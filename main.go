package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/a-h/templ"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"vidra/views"
)

const (
	quota      = 2 << 30 // per user, bytes
	chunkSize  = 5 << 20
	sessionTTL = 30 * 24 * time.Hour
	inviteTTL  = 7 * 24 * time.Hour
)

// Pages are templ components (views/*.templ); go generate compiles them to *_templ.go.
//
//go:generate go tool templ generate

// Compiled ahead of time: the browser must have the styles before first paint, or every navigation flashes unstyled.
//
//go:generate npx -y tailwindcss@3.4.16 -c tailwind.config.js -i app.css -o static/app.css --minify
//go:embed static/app.css
var appCSS []byte

// cssVersion busts the browser cache whenever the stylesheet changes, so it can be cached forever.
var cssVersion = fmt.Sprintf("%x", sha256.Sum256(appCSS))[:12]

var (
	db      *sql.DB
	dataDir string
	queue   = make(chan string, 1024) // video ids waiting for ffmpeg
)

func asset(path string) string { return path + "?v=" + cssVersion }

const schema = `
CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pass_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'user', created_at INTEGER NOT NULL); -- role: 'user' or 'admin'

CREATE TABLE IF NOT EXISTS sessions(token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS invites(token TEXT PRIMARY KEY, created_by INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_by INTEGER);
CREATE TABLE IF NOT EXISTS videos(id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, title TEXT NOT NULL, file_size INTEGER NOT NULL,
  visibility TEXT NOT NULL, status TEXT NOT NULL, quality TEXT NOT NULL, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0); -- expires_at 0 = never
CREATE TABLE IF NOT EXISTS opened(user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  video_id TEXT NOT NULL REFERENCES videos(id) ON DELETE CASCADE, opened_at INTEGER NOT NULL, PRIMARY KEY(user_id, video_id));
CREATE TABLE IF NOT EXISTS uploads(id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, filename TEXT NOT NULL, size INTEGER NOT NULL,
  expires_in INTEGER NOT NULL, quality TEXT NOT NULL, visibility TEXT NOT NULL, created_at INTEGER NOT NULL);
`

func main() {
	h := setup(env("DATA_DIR", "./data"))
	go worker()
	go func() {
		for {
			cleanup()
			time.Sleep(time.Hour)
		}
	}()
	addr := env("ADDR", ":8080")
	log.Println("listening on", addr)
	log.Fatal((&http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

func setup(dir string) http.Handler {
	dataDir = dir
	for _, d := range []string{"tmp", "raw", "videos", "thumbs"} {
		must(os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	var err error
	db, err = sql.Open("sqlite", "file:"+filepath.Join(dir, "vidra.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	must(err)
	db.SetMaxOpenConns(1) // one writer; never hold rows open while running another query
	_, err = db.Exec(schema)
	must(err)
	seedAdmin()

	// resume transcodes interrupted by a restart
	for _, id := range ids(`SELECT id FROM videos WHERE status='processing'`) {
		queue <- id
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(appCSS)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) != nil {
			http.Redirect(w, r, "/upload", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
	})
	mux.HandleFunc("GET /login", loginPage)
	mux.HandleFunc("POST /login", login)
	mux.HandleFunc("POST /logout", logout)
	mux.HandleFunc("GET /register", registerPage)
	mux.HandleFunc("POST /register", register)

	mux.HandleFunc("GET /upload", authed(uploadPage))
	mux.HandleFunc("POST /upload/init", authed(uploadInit))
	mux.HandleFunc("POST /upload/chunk", authed(uploadChunk))
	mux.HandleFunc("POST /upload/complete", authed(uploadComplete))

	mux.HandleFunc("GET /videos", authed(library))
	mux.HandleFunc("GET /videos/{id}/card", authed(card))
	mux.HandleFunc("POST /videos/{id}/visibility", authed(toggleVisibility))
	mux.HandleFunc("POST /videos/{id}/title", authed(renameVideo))
	mux.HandleFunc("DELETE /videos/{id}", authed(deleteVideoHandler))

	mux.HandleFunc("GET /settings", adminOnly(settings))
	mux.HandleFunc("POST /settings/invite", adminOnly(newInvite))
	mux.HandleFunc("POST /settings/users/{id}/role", adminOnly(setRole))

	mux.HandleFunc("GET /v/{id}", watch)
	mux.HandleFunc("GET /v/{id}/video.mp4", serveFile("videos", ".mp4", "video/mp4"))
	mux.HandleFunc("GET /v/{id}/thumb.jpg", serveFile("thumbs", ".jpg", "image/jpeg"))

	return secure(sessions(mux))
}

func seedAdmin() {
	var n int
	must(db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n))
	if n > 0 {
		return
	}
	u, p := os.Getenv("ADMIN_USER"), os.Getenv("ADMIN_PASSWORD")
	if u == "" || p == "" {
		log.Fatal("no users yet: set ADMIN_USER and ADMIN_PASSWORD to create the first account")
	}
	_, err := createUser(u, p, "admin")
	must(err)
	log.Println("created admin user", u)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// render writes content inside the page layout. title names the tab and picks the active nav link;
// head adds to <head> and may be nil.
func render(w http.ResponseWriter, r *http.Request, title string, head, content templ.Component) {
	var buf bytes.Buffer
	if err := views.Layout(title, userFrom(r), asset("/app.css"), head).Render(templ.WithChildren(r.Context(), content), &buf); err != nil {
		log.Println("render", title, err)
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// fragment renders a partial for HTMX responses.
func fragment(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		log.Println("fragment", err)
	}
}

// notify makes htmx raise a "toast" event, which the layout shows as a snackbar.
func notify(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(map[string]string{"toast": msg})
	w.Header().Set("HX-Trigger", string(b))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// baseURL is the public origin, used for absolute Open Graph URLs.
func baseURL(r *http.Request) string {
	if b := os.Getenv("BASE_URL"); b != "" {
		return b
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func now() int64 { return time.Now().Unix() }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newID returns a random UUIDv4.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ids runs a query returning one text column and reads it fully before returning.
func ids(query string, args ...any) (out []string) {
	rows, err := db.Query(query, args...)
	if err != nil {
		log.Println("query", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return
}

func hashPassword(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}
