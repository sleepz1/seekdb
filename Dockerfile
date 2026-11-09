# Multi-stage build: compile from the committed sources, run on Ubuntu 24.04.
# Go standard library only; no third-party modules are downloaded.
FROM golang:1.22.12-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOFLAGS=-mod=mod go build -trimpath -ldflags="-s -w" -o /out/minivectordb ./cmd/minivectordb

FROM ubuntu:24.04
WORKDIR /app
COPY --from=build --chmod=755 /out/minivectordb /app/minivectordb
RUN mkdir -p /data
VOLUME ["/data"]
# Foreground process: SQL on stdin, results on stdout, logs on stderr.
# No ports are opened.
ENTRYPOINT ["/app/minivectordb"]
CMD ["--data-dir", "/data"]
