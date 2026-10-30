package main

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID   int64
	Name string
}

type ctxKey struct{}

var usernameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxKey{}).(*User)
	return u
}

// sessions loads the logged-in user (if any) from the session cookie.
func sessions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err == nil {
			u := &User{}
			err := db.QueryRow(`SELECT u.id, u.username FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token=? AND s.expires_at>?`,
				c.Value, now()).Scan(&u.ID, &u.Name)
			if err == nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) == nil {
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "please log in again", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func createUser(name, password string) (int64, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO users(username, pass_hash, created_at) VALUES(?,?,?)`, name, hash, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func startSession(w http.ResponseWriter, r *http.Request, uid int64) {
	tok := randHex(32)
	db.Exec(`INSERT INTO sessions(token, user_id, expires_at) VALUES(?,?,?)`, tok, uid, now()+int64(sessionTTL.Seconds()))
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: isHTTPS(r), MaxAge: int(sessionTTL.Seconds())})
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/upload", http.StatusSeeOther)
		return
	}
	render(w, r, "login", map[string]any{"Title": "Log in"})
}

func login(w http.ResponseWriter, r *http.Request) {
	var id int64
	var hash string
	err := db.QueryRow(`SELECT id, pass_hash FROM users WHERE username=?`, strings.TrimSpace(r.FormValue("username"))).Scan(&id, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		w.WriteHeader(http.StatusUnauthorized)
		render(w, r, "login", map[string]any{"Title": "Log in", "Error": "Wrong username or password."})
		return
	}
	startSession(w, r, id)
	http.Redirect(w, r, "/upload", http.StatusSeeOther)
}

func logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		db.Exec(`DELETE FROM sessions WHERE token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func inviteValid(token string) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM invites WHERE token=? AND used_by IS NULL AND expires_at>?`, token, now()).Scan(&n)
	return n == 1
}

func registerPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	data := map[string]any{"Title": "Create account", "Token": token}
	if !inviteValid(token) {
		w.WriteHeader(http.StatusForbidden)
		data["Invalid"] = true
	}
	render(w, r, "register", data)
}

func register(w http.ResponseWriter, r *http.Request) {
	token, name, pass := r.FormValue("token"), strings.TrimSpace(r.FormValue("username")), r.FormValue("password")
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		render(w, r, "register", map[string]any{"Title": "Create account", "Token": token, "Error": msg, "Username": name})
	}
	switch {
	case !inviteValid(token):
		fail(http.StatusForbidden, "This invite is invalid, used or expired.")
		return
	case !usernameRE.MatchString(name):
		fail(http.StatusBadRequest, "Username must be 3–32 letters, digits, _ or -.")
		return
	case len(pass) < 8 || len(pass) > 72:
		fail(http.StatusBadRequest, "Password must be 8–72 characters.")
		return
	}
	id, err := createUser(name, pass)
	if err != nil {
		fail(http.StatusConflict, "That username is taken.")
		return
	}
	// the single-use guard: only one request can flip used_by from NULL
	res, _ := db.Exec(`UPDATE invites SET used_by=? WHERE token=? AND used_by IS NULL AND expires_at>?`, id, token, now())
	if n, _ := res.RowsAffected(); n != 1 {
		db.Exec(`DELETE FROM users WHERE id=?`, id)
		fail(http.StatusForbidden, "This invite is invalid, used or expired.")
		return
	}
	startSession(w, r, id)
	http.Redirect(w, r, "/upload", http.StatusSeeOther)
}

type Invite struct {
	Token             string
	ExpiresAt         int64
	UsedBy            sql.NullString
	Active, IsExpired bool
}

func invites(uid int64) (out []Invite) {
	rows, err := db.Query(`SELECT i.token, i.expires_at, u.username FROM invites i LEFT JOIN users u ON u.id=i.used_by
		WHERE i.created_by=? ORDER BY i.created_at DESC LIMIT 20`, uid)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var i Invite
		rows.Scan(&i.Token, &i.ExpiresAt, &i.UsedBy)
		i.IsExpired = i.ExpiresAt <= now()
		i.Active = !i.UsedBy.Valid && !i.IsExpired
		out = append(out, i)
	}
	return
}

func settings(w http.ResponseWriter, r *http.Request) {
	render(w, r, "settings", map[string]any{"Title": "Settings", "Invites": invites(userFrom(r).ID)})
}

func newInvite(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	db.Exec(`INSERT INTO invites(token, created_by, created_at, expires_at) VALUES(?,?,?,?)`,
		randHex(16), u.ID, now(), now()+int64(inviteTTL.Seconds()))
	fragment(w, "invites", map[string]any{"Invites": invites(u.ID), "Base": baseURL(r)})
}
