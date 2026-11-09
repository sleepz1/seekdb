FROM golang:1.22 AS builder
WORKDIR /app
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /minivectordb ./cmd/minivectordb
FROM scratch
COPY --from=builder /minivectordb /minivectordb
WORKDIR /data
ENTRYPOINT ["/minivectordb"]
CMD ["-data", "/data"]
