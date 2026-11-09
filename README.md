# MiniVectorDB M2 — MySQL wire

Pure Go SQL/vector server with atomic `snapshot.json` persistence. Requires Go
1.22+; smoke also requires Bash. MySQL wire is the default on :3306; the old
line/JSON protocol and server `-query` option have been removed.

```sh
go build -o /tmp/minivectordb ./cmd/minivectordb
/tmp/minivectordb -addr 127.0.0.1:3306 -data /tmp/minivectordb-data
mysql --protocol=TCP -h 127.0.0.1 -P 3306 -u root --ssl-mode=DISABLED
```

Run SQL in that client:

```sql
CREATE TABLE items (id INT, embedding VECTOR(2));
INSERT INTO items VALUES (1, [1,0]);
SELECT * FROM items ORDER BY DISTANCE(embedding, [1,0]) LIMIT 1;
DELETE FROM items;
DROP TABLE items;
```

Without a mysql CLI, use the real Go MySQL driver client:

```sh
go run ./scripts/mysqlsmoke -addr 127.0.0.1:3306 -query 'SELECT 1'
./scripts/smoke.sh
```

`github.com/go-sql-driver/mysql` DSN example:
`root:any-password@tcp(127.0.0.1:3306)/contest`. Use SQL text without bound
parameters (or driver-side `interpolateParams=true`); server prepared statements
are not implemented. Database names in the connection handshake and COM_INIT_DB
are accepted as a single shared namespace.

Supported SQL: CREATE TABLE with INT/INTEGER, TEXT, FLOAT/DOUBLE and VECTOR(n);
single-row INSERT INTO ... VALUES; SELECT * with optional vector ORDER BY and
LIMIT; whole-table DELETE FROM; DROP TABLE; BEGIN/START TRANSACTION, COMMIT,
ROLLBACK. Vector functions are DISTANCE (alias for EUCLIDEAN), EUCLIDEAN,
COSINE (distance) and DOT (negative inner product), ordered ASC by default,
with optional DESC. Functions currently work in ORDER BY, not projections.
Transactions must share one connection; each smoke-client invocation opens a new
connection. Existing transaction behavior and snapshot persistence are unchanged.
`SELECT 1`, `SELECT VERSION()`, and `SET NAMES utf8[mb4]` are also accepted.

Wire details: protocol 10 greeting, server version `8.0.36-minivectordb`, utf8mb4
collation 45, COM_QUERY, COM_PING, COM_INIT_DB and COM_QUIT. Capabilities are
LONG_PASSWORD, CONNECT_WITH_DB, PROTOCOL_41, TRANSACTIONS, SECURE_CONNECTION and
PLUGIN_AUTH. Results use text rows, VARCHAR metadata (vectors encoded as JSON),
and legacy EOF terminators. Packet continuation and sequence validation are
supported with a 64 MiB incoming message limit. Errors use code 1105/HY000 for
engine errors and 1047/HY000 for unsupported commands.

Authentication is intentionally permissive for the contest: advertise
`mysql_native_password`, ignore client auth bytes and plugin name, accept any
username and empty or arbitrary passwords, and immediately return OK. There is
no credential verification, TLS, compression, multi-statement support or server
prepared-statement support. This is an unauthenticated contest server.
Status reports autocommit; full MySQL session/transaction status semantics and
SQL dialect compatibility remain outside M2.

Wire layout references: [MySQL handshake](https://dev.mysql.com/doc/dev/mysql-server/8.0.46/page_protocol_connection_phase_packets_protocol_handshake_v10.html)
and [text result sets](https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query_response_text_resultset.html).

```sh
sg docker -c 'docker build -t minivectordb:m2 .'
docker run --rm -p 127.0.0.1:3306:3306 -v minivectordb-data:/data minivectordb:m2
```

The multi-stage Docker build uses CGO=0 and a scratch runtime exposing 3306.
The driver smoke runs on the host; the server has no third-party runtime imports.
See [SELFTEST.md](SELFTEST.md) for observed verification. Official mysqltest pack
requirements and dialect are TBD. No official contest submission has been made.
