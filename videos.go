package main

import (
	"log"
	"mime"
	"net/http"
	"os"
	"strings"

	"vidra/views"
)

const videoCols = `id, user_id, title, file_size, visibility, status, width, height, created_at, expires_at`

// scanVideo reads videoCols into v, plus any extra columns selected after them.
func scanVideo(v *views.Video, s interface{ Scan(...any) error }, extra ...any) error {
	return s.Scan(append([]any{&v.ID, &v.UserID, &v.Title, &v.Size, &v.Visibility, &v.Status, &v.Width, &v.Height, &v.CreatedAt, &v.ExpiresAt}, extra...)...)
}

func getVideo(id string) (*views.Video, error) {
	v := &views.Video{}
	return v, scanVideo(v, db.QueryRow(`SELECT `+videoCols+` FROM videos WHERE id=?`, id))
}

// getOwned returns the video only when the logged-in user owns it.
func getOwned(r *http.Request) *views.Video {
	v, err := getVideo(r.PathValue("id"))
	if err != nil || v.UserID != userFrom(r).ID {
		return nil
	}
	return v
}

func visibleTo(v *views.Video, u *views.User) bool {
	if v.ExpiresAt > 0 && v.ExpiresAt <= now() {
		return false
	}
	return v.Visibility == "public" || (u != nil && u.ID == v.UserID)
}

func deleteVideo(id string) {
	os.Remove(path("raw", id+".tmp"))
	os.Remove(path("videos", id+".mp4"))
	os.Remove(path("thumbs", id+".jpg"))
	db.Exec(`DELETE FROM videos WHERE id=?`, id)
}

func library(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	shared := r.URL.Query().Has("shared")
	q, args := `SELECT `+videoCols+`, '' FROM videos WHERE user_id=? ORDER BY created_at DESC`, []any{u.ID}
	if shared {
		// other people's videos this user opened from a link, while they can still be watched
		q = `SELECT v.` + strings.ReplaceAll(videoCols, ", ", ", v.") + `, u.username FROM opened o
			JOIN videos v ON v.id=o.video_id JOIN users u ON u.id=v.user_id
			WHERE o.user_id=? AND v.user_id<>o.user_id AND v.visibility='public' AND v.status='ready' AND (v.expires_at=0 OR v.expires_at>?)
			ORDER BY o.opened_at DESC`
		args = []any{u.ID, now()}
	}
	var list []*views.Video
	rows, err := db.Query(q, args...)
	if err != nil {
		log.Println("library", err)
	} else {
		for rows.Next() {
			v := &views.Video{}
			if scanVideo(v, rows, &v.By) == nil {
				list = append(list, v)
			}
		}
		rows.Close()
	}
	w.Header().Set("Cache-Control", "no-store") // Back must refetch, so changes made on the watch page show up
	render(w, r, "Library", nil, views.Library(list, usage(u.ID), shared))
}

func card(w http.ResponseWriter, r *http.Request) {
	v := getOwned(r)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	fragment(w, r, views.VideoCard(v))
}

func toggleVisibility(w http.ResponseWriter, r *http.Request) {
	v := getOwned(r)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	v.Visibility = map[string]string{"public": "private", "private": "public"}[v.Visibility]
	db.Exec(`UPDATE videos SET visibility=? WHERE id=?`, v.Visibility, v.ID)
	notify(w, map[string]string{"public": "Anyone with the link can watch", "private": "Only you can watch this video now"}[v.Visibility])
	fragment(w, r, views.VideoCard(v))
}

func renameVideo(w http.ResponseWriter, r *http.Request) {
	v := getOwned(r)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "The title can’t be empty.", http.StatusBadRequest)
		return
	}
	if t := []rune(title); len(t) > 100 { // same cap as titles taken from file names
		title = string(t[:100])
	}
	db.Exec(`UPDATE videos SET title=? WHERE id=?`, title, v.ID)
	v.Title = title
	notify(w, "Video renamed")
	fragment(w, r, views.VideoCard(v)) // the library swaps the card; the watch page ignores the body
}

func deleteVideoHandler(w http.ResponseWriter, r *http.Request) {
	v := getOwned(r)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	deleteVideo(v.ID)
	notify(w, "Video deleted")
	u := usage(v.UserID)
	u.OOB = true
	fragment(w, r, views.UsageMeter(u)) // the card itself is swapped for nothing; this refreshes the meter
}

func watch(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	v, err := getVideo(r.PathValue("id"))
	if err != nil || !visibleTo(v, u) {
		http.NotFound(w, r)
		return
	}
	if u != nil && u.ID != v.UserID {
		db.Exec(`INSERT INTO opened(user_id, video_id, opened_at) VALUES(?,?,?) ON CONFLICT DO UPDATE SET opened_at=excluded.opened_at`, u.ID, v.ID, now())
	}
	render(w, r, v.Title, views.WatchHead(v, baseURL(r)), views.Watch(v, u != nil && u.ID == v.UserID))
}

func serveFile(sub, ext, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := getVideo(r.PathValue("id"))
		if err != nil || v.Status != "ready" || !visibleTo(v, userFrom(r)) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(path(sub, v.ID+ext))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", ctype)
		if r.URL.Query().Has("download") {
			name := strings.Map(func(c rune) rune {
				if c < 32 || strings.ContainsRune(`/\"`, c) {
					return '_'
				}
				return c
			}, v.Title)
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ext}))
		}
		http.ServeContent(w, r, "", st.ModTime(), f) // handles Range requests for seeking
	}
}
