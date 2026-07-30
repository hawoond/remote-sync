# syntax=docker/dockerfile:1

FROM golang:1.25.12-alpine AS build

RUN apk add --no-cache ca-certificates git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/sync-server ./cmd/sync-server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/sync-migrate ./cmd/sync-migrate

FROM alpine:3.23

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 remote-sync \
    && adduser -S -D -H -u 10001 -G remote-sync remote-sync \
    && mkdir -p /app/migrations /var/lib/remote-sync/blobs \
    && chown -R remote-sync:remote-sync /app /var/lib/remote-sync

COPY --from=build --chown=remote-sync:remote-sync \
  /out/sync-server /out/sync-migrate /usr/local/bin/
COPY --chown=remote-sync:remote-sync migrations /app/migrations

USER remote-sync
WORKDIR /app

EXPOSE 8443

ENTRYPOINT ["/usr/local/bin/sync-server"]
