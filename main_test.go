package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

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
