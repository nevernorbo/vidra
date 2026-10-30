# Rebuild the stylesheet from the templates so the image never ships a stale static/app.css
FROM node:22-alpine AS css
WORKDIR /src
COPY tailwind.config.js ./
COPY templates ./templates
RUN npx -y tailwindcss@3.4.16 -c tailwind.config.js -i templates/app.css -o static/app.css --minify

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /src/static/app.css static/app.css
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /vidra .

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /vidra /usr/local/bin/vidra
ENV DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
CMD ["vidra"]
