package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vidra/views"
)

var expiries = map[string]int64{"24h": 24 * 3600, "7d": 7 * 24 * 3600, "30d": 30 * 24 * 3600, "never": 0}

// quality name -> x264 CRF
var crfs = map[string]string{"high": "20", "balanced": "23", "small": "28"}

func path(sub, name string) string { return filepath.Join(dataDir, sub, name) }

// usedBytes counts stored videos plus uploads still in flight, so parallel uploads can't dodge the quota.
func usedBytes(uid int64) int64 {
	var n int64
	db.QueryRow(`SELECT COALESCE((SELECT SUM(file_size) FROM videos WHERE user_id=?),0) + COALESCE((SELECT SUM(size) FROM uploads WHERE user_id=?),0)`, uid, uid).Scan(&n)
	return n
}

func usage(uid int64) views.Usage {
	used := usedBytes(uid)
	return views.Usage{Used: used, Quota: quota, Pct: int(min(used*100/quota, 100))}
}

func uploadPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, "Upload", nil, views.Upload(usage(userFrom(r).ID)))
}

func uploadInit(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	size, _ := strconv.ParseInt(r.FormValue("size"), 10, 64)
	secs, okExp := expiries[r.FormValue("expires")]
	quality := r.FormValue("quality")
	vis := r.FormValue("visibility")
	if vis != "private" {
		vis = "public"
	}
	if _, ok := crfs[quality]; size <= 0 || !okExp || !ok {
		http.Error(w, "invalid upload options", http.StatusBadRequest)
		return
	}
	if free := quota - usedBytes(u.ID); size > free {
		http.Error(w, fmt.Sprintf("Not enough storage: this file is %s but only %s is free.", views.FormatBytes(size), views.FormatBytes(max(free, 0))), http.StatusRequestEntityTooLarge)
		return
	}
	id := newID()
	_, err := db.Exec(`INSERT INTO uploads(id, user_id, filename, size, expires_in, quality, visibility, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, u.ID, filepath.Base(r.FormValue("filename")), size, secs, quality, vis, now())
	if err != nil {
		http.Error(w, "could not start upload", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "chunk": chunkSize})
}

// uploadChunk writes slice N at its offset, so a retried chunk just overwrites itself.
func uploadChunk(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	idx, _ := strconv.ParseInt(r.URL.Query().Get("index"), 10, 64)
	var size int64
	if err := db.QueryRow(`SELECT size FROM uploads WHERE id=? AND user_id=?`, id, userFrom(r).ID).Scan(&size); err != nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	off := idx * chunkSize
	want := min(chunkSize, size-off)
	if idx < 0 || want <= 0 {
		http.Error(w, "bad chunk index", http.StatusBadRequest)
		return
	}
	f, err := os.OpenFile(path("tmp", id+".part"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if st, _ := f.Stat(); off > st.Size() {
		http.Error(w, "missing earlier chunk", http.StatusConflict)
		return
	}
	if n, err := io.Copy(io.NewOffsetWriter(f, off), io.LimitReader(r.Body, want)); err != nil || n != want {
		http.Error(w, "short chunk", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func uploadComplete(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id := r.FormValue("id")
	var name, quality, vis string
	var size, expIn int64
	err := db.QueryRow(`SELECT filename, size, expires_in, quality, visibility FROM uploads WHERE id=? AND user_id=?`, id, u.ID).
		Scan(&name, &size, &expIn, &quality, &vis)
	if err != nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	if st, err := os.Stat(path("tmp", id+".part")); err != nil || st.Size() != size {
		http.Error(w, "upload incomplete", http.StatusBadRequest)
		return
	}
	if err := os.Rename(path("tmp", id+".part"), path("raw", id+".tmp")); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	var exp int64
	if expIn > 0 {
		exp = now() + expIn
	}
	title := strings.TrimSuffix(name, filepath.Ext(name))
	if r := []rune(title); len(r) > 100 {
		title = string(r[:100])
	}
	tx, err := db.Begin()
	if err == nil {
		_, err = tx.Exec(`INSERT INTO videos(id, user_id, title, file_size, visibility, status, quality, created_at, expires_at) VALUES(?,?,?,?,?,'processing',?,?,?)`,
			id, u.ID, title, size, vis, quality, now(), exp)
		if err == nil {
			_, err = tx.Exec(`DELETE FROM uploads WHERE id=?`, id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	if err != nil {
		http.Error(w, "could not save video", http.StatusInternalServerError)
		return
	}
	queue <- id
	writeJSON(w, map[string]string{"url": "/v/" + id})
}

// worker transcodes one video at a time; ffmpeg already uses all cores.
func worker() {
	for id := range queue {
		transcode(id)
	}
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return fmt.Errorf("%s: %w\n%s", name, err, tail)
	}
	return nil
}

// maxDuration caps uploads, so one file can't hold the single transcode worker for hours.
var maxDuration = time.Hour

// untrusted limits how ffmpeg reads an upload. It picks the format from the contents, not the
// extension, so a crafted playlist (HLS, concat) could otherwise fetch URLs or read other local files.
var untrusted = []string{"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,avi,mpegts,flv,asf,mpeg,ogg"}

// checkDuration rejects uploads that are too long, or whose length ffprobe can't tell.
func checkDuration(raw string) error {
	b, err := exec.Command("ffprobe", append(untrusted, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", raw)...).Output()
	if err != nil {
		return fmt.Errorf("ffprobe: %w", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || secs <= 0 {
		return fmt.Errorf("unknown duration %q", strings.TrimSpace(string(b)))
	}
	if d := time.Duration(secs * float64(time.Second)); d > maxDuration {
		return fmt.Errorf("too long: %s", d.Round(time.Second))
	}
	return nil
}

func transcode(id string) {
	raw, out, thumb := path("raw", id+".tmp"), path("videos", id+".mp4"), path("thumbs", id+".jpg")
	var quality string
	if db.QueryRow(`SELECT quality FROM videos WHERE id=?`, id).Scan(&quality) != nil {
		os.Remove(raw) // deleted while queued
		return
	}
	err := checkDuration(raw)
	if err == nil {
		// -t backs up checkDuration when a file's header understates its length.
		// even dimensions + yuv420p: libx264 rejects odd sizes, and browsers/Discord need yuv420p
		args := append(append([]string{"-y"}, untrusted...), "-i", raw, "-t", fmt.Sprint(maxDuration.Seconds()),
			"-c:v", "libx264", "-preset", "fast", "-crf", crfs[quality], "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
			"-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", out)
		err = run("ffmpeg", args...)
	}
	if err == nil {
		err = run("ffmpeg", "-y", "-i", out, "-vf", "thumbnail,scale=640:-2", "-frames:v", "1", thumb)
	}
	os.Remove(raw)
	if err != nil {
		log.Println("transcode", id, err)
		os.Remove(out)
		db.Exec(`UPDATE videos SET status='failed', file_size=0 WHERE id=?`, id)
		return
	}
	st, _ := os.Stat(out)
	var w, h int
	if b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", out).Output(); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d,%d", &w, &h)
	}
	res, _ := db.Exec(`UPDATE videos SET status='ready', file_size=?, width=?, height=? WHERE id=?`, st.Size(), w, h, id)
	if n, _ := res.RowsAffected(); n == 0 { // deleted while transcoding
		os.Remove(out)
		os.Remove(thumb)
	}
}

func cleanup() {
	for _, id := range ids(`SELECT id FROM videos WHERE expires_at>0 AND expires_at<?`, now()) {
		deleteVideo(id)
	}
	for _, id := range ids(`SELECT id FROM uploads WHERE created_at<?`, now()-86400) {
		os.Remove(path("tmp", id+".part"))
		db.Exec(`DELETE FROM uploads WHERE id=?`, id)
	}
	db.Exec(`DELETE FROM sessions WHERE expires_at<?`, now())
}
