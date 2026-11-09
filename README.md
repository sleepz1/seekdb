# MiniVectorDB M5 — SQL subset and vector index

Pure Go SQL/vector engine. No external storage engine or vector library. Requires
Go 1.22+; smoke requires Bash. Default mode is a foreground stdin session with
stdout results, stderr logs, no network listener, and data directory `/data`.
The confirmed contest protocol is stdin/stdout; exact round2 pack framing is TBD.

```sh
go build -o minivectordb ./cmd/minivectordb
printf 'SELECT 1;\n' | ./minivectordb -data /tmp/x
./scripts/smoke.sh
```

Temporary protocol (stable until pack reconciliation): SQL statements end at a
semicolon outside SQL single quotes, or a newline outside quotes and balanced
parentheses/brackets. Multiple statements per line are supported. Multiline
CREATE/INSERT works inside parentheses; arbitrary multiline clauses must stay on
one line. SQL doubled quotes (`''`) escape quotes. Comments are not supported.
EOF executes any remaining statement, then exits cleanly; unfinished transaction
writes are discarded. Malformed unterminated quotes consume input until EOF and
produce an error. Each stdin process owns one Session.

Successful writes and transaction commands print `OK <affected>` (zero for DDL
and transaction commands). SELECT prints tab-separated column names, then one row
per line, then `OK <rowcount>`, including empty results. Text cells escape backslash,
tab, newline and carriage return as `\\`, `\t`, `\n`, `\r`; numbers and vectors
use JSON notation (vectors are arrays). SQL errors print `ERROR <message>` and the
loop continues. Every response is flushed immediately. Startup and I/O failures
log to stderr and exit nonzero. There is no stdout banner.

```sql
CREATE TABLE items (id BIGINT PRIMARY KEY, label VARCHAR(32), embedding VECTOR(2));
BEGIN;
INSERT INTO items VALUES (1, 'first', [1,0]);
COMMIT;
SELECT * FROM items ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
```

Supported SQL: CREATE TABLE with exactly one inline BIGINT PRIMARY KEY and at most
one VECTOR(n); INT/INTEGER, TEXT/VARCHAR/VARCHAR(n), FLOAT/DOUBLE remain available
for other columns. INSERT INTO ... VALUES supports multiple rows atomically.
UPDATE ... SET col=literal [, ...] WHERE predicates rejects PK assignments.
DELETE FROM supports predicates or deletes all rows without WHERE. SELECT supports
* or projected columns/distance expressions, optional AS aliases, ordinary-column
or distance ORDER BY with ASC/DESC, and LIMIT. WHERE supports =, <, <=, >, >= and
AND; vectors are rejected. BIGINT compares numerically; UTF-8 text compares in
Unicode code point order. Also: SELECT 1, DROP TABLE, BEGIN/START TRANSACTION,
COMMIT, ROLLBACK. Unsupported SQL returns ERROR and the next statement still runs.
Duplicate primary keys are rejected without partial writes, including duplicate
keys within one multi-row INSERT. Bare and SQL-quoted vector arrays are accepted
in INSERT, UPDATE and distance expressions.
INT/INTEGER/BIGINT use int64 and ParseInt: fractions, exponent notation and values
outside [-9223372036854775808, 9223372036854775807] are rejected. Typed transaction
copies and snapshot decoding preserve integer precision; WAL retains SQL literals.
VARCHAR(n) enforces a positive maximum Unicode rune count, not bytes; bare VARCHAR
and TEXT remain unlimited. Strings require SQL quotes and valid UTF-8.
VECTOR(n) requires a positive fixed dimension, stores []float32, and rounds inputs
to float32. NaN, infinities, null elements, conversion overflow and dimension
mismatch are errors on insertion and query. Distance arithmetic accumulates in
float32 (square root uses Go math then rounds to float32); nonfinite distance
results are errors, including overflow from large finite input values.
L2_DISTANCE is primary; EUCLIDEAN and DISTANCE are aliases. COSINE computes cosine
distance and DOT negative inner product. Filtering precedes candidate selection,
distance sorting and LIMIT. All ORDER BY ties use the explicit primary key
ascending, including DESC. Example projection:
`SELECT id, L2_DISTANCE(embedding, '[1,0]') AS distance FROM items ORDER BY distance LIMIT 2;`

Search defaults to exact. `-ann` sets the mode for new sessions;
`SET search_mode=ann` / `SET search_mode=exact` changes the current session and
`SELECT search_mode` reads it. Mode is session configuration, unaffected by
ROLLBACK and not persisted. `CREATE VECTOR INDEX idx ON items(embedding)`
creates a self-built sign-LSH index using up to eight vector coordinates.
ANN uses the nearest nonempty Hamming shell after filtering; it may return fewer
than k rows and has no recall guarantee. Without an index, the retained M4 ANN
fallback samples every second filtered row. Exact always scans all qualifying
rows, regardless of indexes. Queries without vector ORDER BY are unaffected.
Index metadata is stored in snapshot JSON / WAL DDL, with buckets rebuilt before
Open returns, on transaction copies and after successful writes. Rollback discards
private index changes. Rebuilding and whole-state cloning prioritize correctness;
this is not yet a performance-tuned index.

```sql
SELECT * FROM items WHERE label = 'first' AND id = 1 ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
SET search_mode=ann;
SELECT search_mode;
SET search_mode=exact;
```

Ready flag/handshake, numeric error codes, exact framing and official ANN/recall
requirements are **pending official pack / 待确认**. There is currently no ready
flag or unsolicited ready line: a successful `SELECT 1` response can be used as a
local readiness probe. `ERROR <message>` has no numeric SQL error code; process
startup/I/O failure exits nonzero, while SQL errors allow continued input. These
are temporary local conventions, not a claim of official pack compatibility.

BEGIN/START TRANSACTION creates a private state copy with read-your-writes;
other sessions see only committed state. COMMIT checks the engine version,
appends all successful writes as one newline-terminated JSON WAL record with a
sequential LSN, and fsyncs before publishing state or returning success.
Autocommit writes use the same path with one statement. ROLLBACK and disconnect
discard private writes. A failed statement leaves prior successful statements
in the transaction intact. Concurrent commits use a coarse database-wide version
check; conflicts require ROLLBACK. Nested BEGIN is rejected. COMMIT/ROLLBACK
without an active transaction are no-ops.

Open loads the legacy snapshot, then replays complete WAL records in LSN order.
An incomplete final record is truncated and synced before accepting new writes;
malformed complete records or LSN gaps fail startup. WAL creation is synced along
with its directory. Read-only transactions write no redo. Write/fsync failure
blocks subsequent durable writes until restart; a commit interrupted before its
acknowledgment has an uncertain outcome and may recover if its full record exists.
Only one server/Engine may own a data directory at a time; there is no process lock.

M3 does not rewrite the base snapshot or compact the WAL. New databases recover
entirely from WAL; old databases retain their immutable snapshot base. Keep both
files together. Automatic atomic checkpoints, WAL checksums, process locking,
bounded log growth, and faster recovery remain open work. Transactions still
clone the in-memory database, and redo uses the supported deterministic SQL
statements rather than page-level changes.
Optional local MySQL wire helper replaces stdin mode only when explicitly set:

```sh
./minivectordb -mysql-addr 127.0.0.1:3306 -data /tmp/wire-data
go run ./scripts/mysqlsmoke -addr 127.0.0.1:3306 -query 'SELECT 1'
```

`-addr` is an alias. The retained helper supports protocol 10, COM_QUERY, COM_PING,
COM_INIT_DB and COM_QUIT, JSON vectors and text results. Authentication accepts
any username/password; no TLS, prepared statements or wire multi-statements.
Wire-specific SELECT VERSION() and SET NAMES remain available. Its transaction
commands must share a connection (repeat client `-query` flags). Primary smoke
does not use the wire helper.

```sh
sg docker -c 'docker build -t minivectordb:m3 .'
printf 'SELECT 1;\n' | docker run --rm -i -v minivectordb-data:/data minivectordb:m3
```

Docker uses CGO=0 and scratch, WORKDIR /data, ENTRYPOINT /minivectordb and CMD
`-data /data`; no port exposure is needed. See [SELFTEST.md](SELFTEST.md).
Exact official framing and broader contest SQL dialect remain open. No official
contest submission or PR merge has been made.
