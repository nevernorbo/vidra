package main

import (
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
)

type Video struct {
	ID, Title, Visibility, Status      string
	UserID, Size, CreatedAt, ExpiresAt int64
	Width, Height                      int
}

const videoCols = `id, user_id, title, file_size, visibility, status, width, height, created_at, expires_at`

func (v *Video) scan(s interface{ Scan(...any) error }) error {
	return s.Scan(&v.ID, &v.UserID, &v.Title, &v.Size, &v.Visibility, &v.Status, &v.Width, &v.Height, &v.CreatedAt, &v.ExpiresAt)
}

func getVideo(id string) (*Video, error) {
	v := &Video{}
	return v, v.scan(db.QueryRow(`SELECT `+videoCols+` FROM videos WHERE id=?`, id))
}

// getOwned returns the video only when the logged-in user owns it.
func getOwned(r *http.Request) *Video {
	v, err := getVideo(r.PathValue("id"))
	if err != nil || v.UserID != userFrom(r).ID {
		return nil
	}
	return v
}

func (v *Video) visibleTo(u *User) bool {
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
	var list []*Video
	rows, err := db.Query(`SELECT `+videoCols+` FROM videos WHERE user_id=? ORDER BY created_at DESC`, u.ID)
	if err != nil {
		log.Println("library", err)
	} else {
		for rows.Next() {
			v := &Video{}
			if v.scan(rows) == nil {
				list = append(list, v)
			}
		}
		rows.Close()
	}
	render(w, r, "videos", map[string]any{"Title": "Library", "Videos": list, "Usage": usage(u.ID)})
}

func card(w http.ResponseWriter, r *http.Request) {
	v := getOwned(r)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	fragment(w, "card", v)
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
	fragment(w, "card", v)
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
	fragment(w, "card", v) // the library swaps the card; the watch page ignores the body
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
	fragment(w, "usage", u) // the card itself is swapped for nothing; this refreshes the meter
}

func watch(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	v, err := getVideo(r.PathValue("id"))
	if err != nil || !v.visibleTo(u) {
		http.NotFound(w, r)
		return
	}
	render(w, r, "watch", map[string]any{"Title": v.Title, "Video": v, "Owner": u != nil && u.ID == v.UserID})
}

func serveFile(sub, ext, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := getVideo(r.PathValue("id"))
		if err != nil || v.Status != "ready" || !v.visibleTo(userFrom(r)) {
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
