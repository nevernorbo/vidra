# vidra

A small, self-hostable video-sharing app. Upload a clip, get a link, and share it anywhere. Links embed and play inline in Discord.

Built with Go, HTMX, SQLite and ffmpeg. It runs as a single container and is designed to sit behind a Cloudflare Tunnel.

## Features

- **Chunked uploads**: files are sliced into 5 MB pieces in the browser, so large videos get past Cloudflare's 100 MB request limit. Failed chunks are retried automatically.
- **Automatic transcoding**: everything is converted to H.264 + AAC MP4 with `+faststart`, and a JPEG thumbnail is generated. Raw uploads are deleted afterwards.
- **Discord embeds**: `/v/{uuid}` renders `og:video`, `og:image` and friends server-side.
- **Invite-only**: logged-in users generate single-use invite links that expire after 7 days.
- **Storage quota**: 2 GB per user, counted from SQLite. Uploads that would exceed it are rejected up front.
- **Expiring videos**: choose 24 hours, 7 days, 30 days or never. An hourly job deletes expired videos from disk and the database.
- **Public or private**: toggle visibility per video at any time. Private videos open only for their owner, so they don't embed.
- **Responsive UI**: server-rendered templates, HTMX and Tailwind (via CDN), usable on a phone.

## Quick start

```bash
cp .env.example .env        # set VIDRA_ADMIN_PASSWORD and VIDRA_BASE_URL
mkdir -p /opt/homelab/data/vidra
docker compose up -d --build
```

Then open `http://<host>:8086` and log in with the admin account from your `.env`. Create the data directory yourself first. Otherwise Docker creates it as root and the app can't write to it.

The admin variables are only used to create the first account when the database has no users. After that you can remove them and invite others from the **Invites** page.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `VIDRA_ADMIN_USER` / `VIDRA_ADMIN_PASSWORD` | – | First account, only used while there are no users. |
| `VIDRA_BASE_URL` | derived from the request | Public origin used in Open Graph URLs, e.g. `https://vidra.example.com`. Set it when running behind a tunnel. |
| `DATA_DIR` | `/opt/homelab/data` | Videos and the database live in `$DATA_DIR/vidra`. |
| `VIDRA_PORT` | `8086` | Host port mapped to the container's `8080`. |
| `PUID` / `PGID` | `1000` | User the container runs as. Must own the data directory. |
| `TZ` | `UTC` | Timezone. |

### Cloudflare Tunnel

The compose file joins `homelab-network` and `cloudflare-network` (both expected to exist already, as in `home-infra`). In Cloudflare Zero Trust, add a public hostname pointing at `http://vidra:8080`. If your network names differ, set `DOCKER_NETWORK` and `CLOUDFLARE_NETWORK`.

## How it works

```
browser ──5 MB chunks──▶ /upload/chunk ──▶ data/tmp/{id}.part
                         /upload/complete ─▶ data/raw/{id}.tmp ──ffmpeg──▶ data/videos/{id}.mp4
                                                                          └▶ data/thumbs/{id}.jpg
```

| Route | Purpose |
|---|---|
| `GET /upload` | Drag-and-drop upload page with progress and storage meter |
| `POST /upload/init`, `/chunk`, `/complete` | Chunked upload API |
| `GET /videos` | Library: toggle visibility, copy link, delete |
| `GET /v/{id}` | Player page with Open Graph tags |
| `GET /v/{id}/video.mp4`, `/thumb.jpg` | Direct media (supports `Range`; add `?download` to save) |
| `GET /settings` | Generate invite links |
| `GET /register?token=…` | Sign up with an invite |

Transcoding runs one video at a time in a background worker. Videos still processing when the server restarts are resumed on startup. Each upload's quality maps to an x264 CRF: High 20, Balanced 23, Small 28.

## Development

There is no need for a local Go install. Use the container:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test ./...
```

To click through the UI locally, run `docker compose --profile dev up --watch vidra-dev` and open `http://localhost:8087` (log in as `admin` / `password123`). It rebuilds on every file change and keeps its data in `./data`.

Or with Go 1.26+ installed: `ADMIN_USER=admin ADMIN_PASSWORD=password123 go run .`. This needs `ffmpeg` and `ffprobe` on your `PATH`, and it stores data in `./data`.

Styles are Tailwind, compiled ahead of time into `static/app.css` and embedded in the binary. After changing classes in `templates/` or `templates/app.css`, run `go generate` (needs Node) and commit the result. Docker builds regenerate it anyway.

## Limitations

- No login rate limiting and no CSRF tokens beyond `SameSite=Lax` cookies. This is fine for a small private instance, but add both before exposing it more widely.
- The quota counts the transcoded size, which can be larger than the original, so a user can end up slightly over 2 GB.
- Tailwind and HTMX load from CDNs, so the UI needs internet access in the browser.
