FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /vidra .

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /vidra /usr/local/bin/vidra
ENV DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
CMD ["vidra"]
