package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Transcodes real files, so it needs ffmpeg and is skipped without it.
func TestTranscodeRejectsCraftedFiles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	setup(t.TempDir())
	clip := path("raw", "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-f", "lavfi", "-i", "testsrc=duration=2:size=64x64:rate=10", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("making test clip: %v\n%s", err, out)
	}
	clipBytes, _ := os.ReadFile(clip)
	status := func(id string, raw []byte) string {
		db.Exec(`INSERT INTO videos(id, user_id, title, file_size, visibility, status, quality, created_at) VALUES(?, 1, 't', 0, 'public', 'processing', 'balanced', 0)`, id)
		os.WriteFile(path("raw", id+".tmp"), raw, 0o644)
		transcode(id)
		var s string
		db.QueryRow(`SELECT status FROM videos WHERE id=?`, id).Scan(&s)
		return s
	}

	if s := status("good", clipBytes); s != "ready" {
		t.Fatalf("real video: status %s", s)
	}
	// a playlist that pulls in another file on the server, which ffmpeg follows unless restricted
	playlist := []byte("ffconcat version 1.0\nfile 'clip.mp4'\n")
	os.WriteFile(path("raw", "playlist.tmp"), playlist, 0o644)
	if out, err := exec.Command("ffprobe", append(untrusted, path("raw", "playlist.tmp"))...).CombinedOutput(); err == nil {
		t.Fatalf("ffprobe opened a concat playlist despite the whitelist:\n%s", out)
	}
	if s := status("concat", playlist); s != "failed" {
		t.Fatalf("concat playlist: status %s", s)
	}
	defer func(d time.Duration) { maxDuration = d }(maxDuration)
	maxDuration = time.Second
	if s := status("long", clipBytes); s != "failed" {
		t.Fatalf("over-long video: status %s", s)
	}
}

// Walks the invite and quota rules end to end (no ffmpeg needed).
func TestInviteAndQuota(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	srv := httptest.NewServer(setup(t.TempDir()))
	defer srv.Close()

	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	post := func(c *http.Client, path string, v url.Values) (int, string) {
		r, err := c.PostForm(srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for n, _ := r.Body.Read(buf); n > 0; n, _ = r.Body.Read(buf) {
			b.Write(buf[:n])
		}
		return r.StatusCode, b.String()
	}

	admin := newClient()
	if code, _ := post(admin, "/login", url.Values{"username": {"admin"}, "password": {"password123"}}); code != 303 {
		t.Fatalf("login = %d", code)
	}
	_, html := post(admin, "/settings/invite", nil)
	m := regexp.MustCompile(`token=([0-9a-f]{32})`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no invite in response")
	}

	if code, _ := post(newClient(), "/register", url.Values{"token": {"bogus"}, "username": {"eve"}, "password": {"password123"}}); code != 403 {
		t.Fatalf("bad token register = %d, want 403", code)
	}
	bob := newClient()
	if code, _ := post(bob, "/register", url.Values{"token": {m[1]}, "username": {"bob"}, "password": {"password123"}}); code != 303 {
		t.Fatalf("register = %d", code)
	}
	if code, _ := post(newClient(), "/register", url.Values{"token": {m[1]}, "username": {"carol"}, "password": {"password123"}}); code != 403 {
		t.Fatalf("reused invite = %d, want 403", code)
	}

	opts := url.Values{"filename": {"a.mp4"}, "expires": {"7d"}, "quality": {"balanced"}, "visibility": {"public"}}
	opts.Set("size", "3221225472") // 3 GiB
	if code, _ := post(bob, "/upload/init", opts); code != 413 {
		t.Fatalf("over-quota init = %d, want 413", code)
	}
	opts.Set("size", "1610612736") // 1.5 GiB fits once, not twice
	if code, _ := post(bob, "/upload/init", opts); code != 200 {
		t.Fatalf("first init = %d", code)
	}
	if code, _ := post(bob, "/upload/init", opts); code != 413 {
		t.Fatalf("second init = %d, want 413 (pending upload counts)", code)
	}
	if code, _ := post(newClient(), "/upload/init", opts); code != 401 {
		t.Fatalf("anonymous init = %d, want 401", code)
	}
}

func TestRename(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	srv := httptest.NewServer(setup(t.TempDir()))
	defer srv.Close()
	db.Exec(`INSERT INTO videos(id, user_id, title, file_size, visibility, status, quality, created_at) VALUES('v1', (SELECT id FROM users WHERE username='admin'), 'old', 0, 'public', 'ready', 'balanced', 0)`)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	rename := func(c *http.Client, title string) int {
		r, err := c.PostForm(srv.URL+"/videos/v1/title", url.Values{"title": {title}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := rename(&http.Client{}, "hijack"); code != 401 {
		t.Fatalf("anonymous rename = %d, want 401", code)
	}
	c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"password123"}})
	if code := rename(c, "   "); code != 400 {
		t.Fatalf("blank rename = %d, want 400", code)
	}
	if code := rename(c, "  New name "); code != 200 {
		t.Fatalf("rename = %d", code)
	}
	if v, _ := getVideo("v1"); v.Title != "New name" {
		t.Fatalf("title = %q", v.Title)
	}
}

// An invite link unfurls in Discord with who sent it; a used or bogus one shows no preview.
func TestInvitePreview(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	srv := httptest.NewServer(setup(t.TempDir()))
	defer srv.Close()
	db.Exec(`INSERT INTO invites(token, created_by, created_at, expires_at) VALUES('tok', (SELECT id FROM users WHERE username='admin'), ?, ?)`,
		now(), now()+int64(inviteTTL.Seconds()))

	get := func(path string) (int, string) {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	code, html := get("/register?token=tok")
	for _, want := range []string{`og:title" content="admin invited you to vidra"`, `expires in 7 days.`, `og:image" content="` + srv.URL + `/og.png"`} {
		if code != 200 || !strings.Contains(html, want) {
			t.Fatalf("invite page = %d, missing %s", code, want)
		}
	}
	if code, html := get("/register?token=bogus"); code != 403 || strings.Contains(html, "og:title") {
		t.Fatalf("bogus invite = %d, has preview: %v", code, strings.Contains(html, "og:title"))
	}
	if code, img := get("/og.png"); code != 200 || !strings.HasPrefix(img, "\x89PNG") {
		t.Fatalf("og.png = %d", code)
	}
}

func TestRoles(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	srv := httptest.NewServer(setup(t.TempDir()))
	defer srv.Close()

	client := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(c *http.Client, method, path string, v url.Values) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b := new(strings.Builder)
		io.Copy(b, r.Body)
		return r.StatusCode, b.String()
	}
	userID := func(name string) string {
		var id string
		db.QueryRow(`SELECT id FROM users WHERE username=?`, name).Scan(&id)
		return id
	}

	admin := client()
	do(admin, "POST", "/login", url.Values{"username": {"admin"}, "password": {"password123"}})
	_, html := do(admin, "POST", "/settings/invite", nil)
	tok := regexp.MustCompile(`token=([0-9a-f]{32})`).FindStringSubmatch(html)[1]
	bob := client()
	do(bob, "POST", "/register", url.Values{"token": {tok}, "username": {"bob"}, "password": {"password123"}})

	if code, _ := do(bob, "GET", "/settings", nil); code != 303 {
		t.Fatalf("user GET /settings = %d, want redirect", code)
	}
	if code, _ := do(bob, "POST", "/settings/invite", nil); code != 403 {
		t.Fatalf("user invite = %d, want 403", code)
	}
	if code, _ := do(bob, "POST", "/settings/users/"+userID("bob")+"/role", url.Values{"role": {"admin"}}); code != 403 {
		t.Fatalf("user promoting self = %d, want 403", code)
	}
	if code, page := do(admin, "GET", "/settings", nil); code != 200 || !strings.Contains(page, "bob") {
		t.Fatalf("admin GET /settings = %d, lists bob: %v", code, strings.Contains(page, "bob"))
	}
	if code, _ := do(admin, "POST", "/settings/users/"+userID("admin")+"/role", url.Values{"role": {"user"}}); code != 403 {
		t.Fatalf("admin demoting self = %d, want 403", code)
	}
	if code, _ := do(admin, "POST", "/settings/users/"+userID("bob")+"/role", url.Values{"role": {"owner"}}); code != 400 {
		t.Fatalf("unknown role = %d, want 400", code)
	}
	if code, _ := do(admin, "POST", "/settings/users/"+userID("bob")+"/role", url.Values{"role": {"admin"}}); code != 200 {
		t.Fatalf("promote bob = %d", code)
	}
	if code, _ := do(bob, "GET", "/settings", nil); code != 200 {
		t.Fatalf("promoted bob GET /settings = %d, want 200", code)
	}
}

func TestSharedWithMe(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "password123")
	srv := httptest.NewServer(setup(t.TempDir()))
	defer srv.Close()
	db.Exec(`INSERT INTO users(id, username, pass_hash, created_at) VALUES(50, 'bob', 'x', 0)`)
	for _, v := range []struct{ id, owner, vis string }{{"mine", "admin", "public"}, {"bobs", "bob", "public"}, {"bobs-private", "bob", "private"}} {
		db.Exec(`INSERT INTO videos(id, user_id, title, file_size, visibility, status, quality, created_at) VALUES(?, (SELECT id FROM users WHERE username=?), ?, 0, ?, 'ready', 'balanced', 0)`,
			v.id, v.owner, "title-"+v.id, v.vis)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"password123"}})
	get := func(path string) string {
		r, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	get("/v/mine") // own videos are not "shared"
	get("/v/bobs")
	db.Exec(`INSERT INTO opened(user_id, video_id, opened_at) VALUES((SELECT id FROM users WHERE username='admin'), 'bobs-private', 0)`) // opened before bob made it private

	shared, mine := get("/videos?shared"), get("/videos")
	if !strings.Contains(shared, "title-bobs<") || strings.Contains(shared, "title-mine") || strings.Contains(shared, "title-bobs-private") {
		t.Fatalf("shared list should hold only bob's public video:\n%s", shared)
	}
	if !strings.Contains(mine, "title-mine") || strings.Contains(mine, "title-bobs") {
		t.Fatal("own list should hold only own videos")
	}
}
