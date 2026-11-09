#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "$PWD/.m1-smoke.XXXXXX")
pid=''
cleanup() { if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; rm -rf "$tmp"; }
trap cleanup EXIT
go build -o "$tmp/minivectordb" ./cmd/minivectordb
start() {
  "$tmp/minivectordb" -addr 127.0.0.1:0 -data "$tmp/data" >"$tmp/server.log" 2>&1 &
  pid=$!
  for _ in {1..100}; do
    if grep -q 'listening on' "$tmp/server.log"; then addr=$(sed -n 's/.*listening on //p' "$tmp/server.log"); return; fi
    kill -0 "$pid" || { cat "$tmp/server.log"; exit 1; }
    sleep .05
  done
  cat "$tmp/server.log"; exit 1
}
q() { "$tmp/minivectordb" -addr "$addr" -query "$1"; }
start
q 'CREATE TABLE items (id INT, label TEXT, embedding VECTOR(2))'
q "INSERT INTO items VALUES (1, 'first', [1,0])"
q "INSERT INTO items VALUES (2, 'second', [0,1])"
result=$(q 'SELECT * FROM items ORDER BY EUCLIDEAN(embedding, [1,0]) LIMIT 1')
[[ "$result" == *'"rows":[[1,"first",[1,0]]]'* ]]
# SIGKILL exercises recovery without a graceful shutdown.
kill -9 "$pid"; wait "$pid" 2>/dev/null || true; pid=''
start
[[ $(q 'SELECT * FROM items') == *'second'* ]]
q 'DELETE FROM items'
q 'DROP TABLE items'
printf '%s\n' 'PASS: build, TCP SQL, vector ranking, SIGKILL snapshot recovery, DELETE/DROP'
