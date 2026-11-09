# MiniVectorDB M1

A Go SQL/vector scaffold with TCP access and atomic `snapshot.json` persistence.
Requires Go 1.22+; the smoke script also requires Bash.

Build and start from the repository root:

```sh
go build -o /tmp/minivectordb ./cmd/minivectordb
/tmp/minivectordb -addr 127.0.0.1:3306 -data /tmp/minivectordb-data
```

In another terminal:

```sh
/tmp/minivectordb -addr 127.0.0.1:3306 -query 'CREATE TABLE items (id INT, embedding VECTOR(2))'
/tmp/minivectordb -addr 127.0.0.1:3306 -query 'INSERT INTO items VALUES (1, [1,0])'
/tmp/minivectordb -addr 127.0.0.1:3306 -query 'SELECT * FROM items ORDER BY EUCLIDEAN(embedding, [1,0]) LIMIT 1'
./scripts/smoke.sh
```

Supported subset: CREATE TABLE with INT/INTEGER, TEXT, FLOAT/DOUBLE and VECTOR(n);
single-row INSERT INTO ... VALUES; SELECT * with optional vector ORDER BY and
LIMIT; whole-table DELETE FROM; DROP TABLE; BEGIN/START TRANSACTION, COMMIT,
ROLLBACK. Vector functions are EUCLIDEAN, COSINE (distance) and DOT (negative
inner product), ordered ASC by default, with optional DESC. Transactions must
share one TCP connection; each `-query` invocation opens a new connection.

The current protocol is one SQL statement per line and one JSON response per
line over TCP. MySQL wire support is TBD. Persistence replaces `snapshot.json`
atomically after writes and reloads it on startup.

Build and run with Docker:

```sh
sg docker -c 'docker build -t minivectordb:m1 .'
docker run --rm -p 127.0.0.1:3306:3306 -v minivectordb-data:/data minivectordb:m1
```

See [SELFTEST.md](SELFTEST.md) for observed results. The official mysqltest pack
is unknown, and transaction stubs depth needs further validation. Decision and
validation records are in [conversation.json](conversation.json) and the
identical [agent/conversation.json](agent/conversation.json).
