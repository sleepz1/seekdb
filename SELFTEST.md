# M3 self-test

Observed commands from the repository root, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
sg docker -c 'docker build -t minivectordb:m3 .'
```

Exact smoke PASS line:

```text
PASS: build, MySQL wire (go-sql-driver/mysql), empty/any-password auth, ERR, vector ranking, SIGKILL snapshot + WAL recovery, COMMIT/ROLLBACK/disconnect isolation, recovered vector ranking, DELETE/DROP
```

The smoke uses go-sql-driver/mysql v1.8.1 over TCP on an ephemeral port, with
server default :3306 unchanged. It builds both binaries in a temporary directory
and cleans up. No mysql CLI is installed. It verifies M2 CREATE/INSERT/SELECT,
all four vector ranking metrics, authentication, ERR, DELETE and DROP.

A seeded M2 snapshot supplies a legacy table. New autocommit rows and an explicit
BEGIN/INSERT/COMMIT row are written only to WAL: smoke checks the WAL is nonempty
and the snapshot is byte-identical to its original. After SIGKILL/restart, the
legacy table and all committed rows survive; rolled-back and disconnected
uncommitted rows are absent. All vector metrics still rank correctly. Repeated
-query flags execute transaction statements and the final SELECT on one pinned
connection. This verifies crash recovery without a graceful shutdown.

Engine unit tests cover multi-statement commit atomicity at truncated record
boundaries, failed-statement exclusion, read-your-writes, WAL-only recovery, isolation from
other sessions, rollback, an active uncommitted transaction during recovery,
read-only commits without LSN gaps, truncation of an incomplete final record,
append after torn-tail recovery, repeated recovery, malformed complete records,
LSN gaps, legacy snapshot plus redo, conflict rejection, and durable DELETE/DROP.
Existing MySQL wire tests remain passing, including connection reuse after ERR,
COM_PING, handshake database selection, affected rows, empty metadata, UTF-8,
packet continuation/boundary, and invalid packet sequence rejection.

Docker built minivectordb:m3 successfully with CGO=0 and a scratch runtime:
`sha256:758e0813cce8954356d32127bed6a15bddb4600da2e2cd4d2485bb93bc4f3ac4`.
Container runtime was not tested; no image was pushed.

Open: periodic atomic checkpoints/WAL compaction, checksums and process locking;
WAL growth/replay time and full-state cloning; disk-failure/power-loss fault
injection beyond torn-tail tests; official mysqltest pack and full SQL dialect;
prepared statements, TLS, full MySQL session/status semantics. Data directories
require a single server owner. No official contest submission or PR merge.
