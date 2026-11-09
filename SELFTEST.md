# M2 self-test

Observed commands from the repository root, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
sg docker -c 'docker build -t minivectordb:m2 .'
```

Exact smoke PASS line:

```text
PASS: build, MySQL wire (go-sql-driver/mysql), empty/any-password auth, ERR, vector ranking, SIGKILL snapshot recovery, DELETE/DROP
```

No mysql CLI is installed in the verification environment. The host smoke uses
`github.com/go-sql-driver/mysql` v1.8.1, builds the server and client, uses a
temporary snapshot directory and ephemeral TCP port, and cleans up afterward.
It verifies CREATE/INSERT/SELECT, EUCLIDEAN/DISTANCE/COSINE/DOT ORDER BY with
LIMIT, empty/arbitrary-password authentication, SQL ERR handling, snapshot
recovery following SIGKILL, DELETE, empty SELECT, and DROP.

Wire tests additionally exercise connection reuse after ERR, COM_PING,
handshake database selection, affected rows, empty result metadata, UTF-8 values
requiring length-encoded prefixes, packet continuation including an exact
16,777,215-byte boundary, and invalid packet sequence rejection. Race tests pass.

Manual MySQL connection example (not CLI-verified here):

```sh
mysql --protocol=TCP -h 127.0.0.1 -P 3306 -u root --ssl-mode=DISABLED -e 'SELECT 1'
```

Docker built `minivectordb:m2` successfully with CGO=0 and scratch runtime:
`sha256:ac729104ea23962eda9307b6d3cb84866f8cab2c5babad0da731e86e55f400c3`.
Container runtime was not tested; no image was pushed.

Open: official mysqltest pack unknown; full contest SQL dialect TBD. Server
prepared statements, TLS, full MySQL session/status semantics, and projection
expressions are not implemented. WAL/crash-recovery and transaction semantics
were not deepened. No formal contest submission or PR merge was performed.
