# M4 local hardening self-test

Baseline: `18525b313dfda305fbd4541db3bd3ba16a0dc174` (M3b), branch
`2026-competition-round2`. No reset/rebase, formal contest submission or PR merge.

Commands run from repository root, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
cmp conversation.json agent/conversation.json
git diff --check
```

Go and race suites: PASS for `cmd/minivectordb`, `internal/db`, and
`internal/mysqlwire`; `scripts/mysqlsmoke` has no test files.
Smoke output:

```text
PASS: stdin/stdout, types, vector ranking, error continuation, SIGKILL snapshot + WAL recovery, COMMIT/ROLLBACK/EOF isolation, DELETE/DROP
PASS: M4 int64 WAL round-trip, Unicode VARCHAR, finite float32/dimensions, filter-before-TopK, PK ties, exact/ann session and CLI, ERROR continuation
```

Smoke builds in a temporary directory, uses stdin pipes/FIFO, and retains M3's
SIGKILL-after-acknowledgment test, immutable snapshot check, COMMIT/ROLLBACK and
EOF isolation. M4 restarts and compares full stdout against expected output for
both int64 extremes, equality filtering, PK ties and distinct ANN/exact results.
It checks Unicode length errors, NaN/±Inf/overflow/dimension errors, unsupported
SQL followed by SELECT 1, and `-ann` mode readback.

Focused Go tests additionally verify typed int64/[]float32 values after transaction
copies, WAL replay and JSON snapshot recovery; float32 rounding of 16777217 to
16777216; out-of-range/fractional integer rejection; supplementary Unicode runes;
invalid vector elements; query-vector validation; filter-before-TopK using an
excluded nearest row; ASC/DESC ties at int64 extremes; quoted AND in string
literals; session isolation and mode switching. Existing WAL corruption, torn
record, repeat recovery, conflicts and transaction atomicity tests remain green.

Temporary conventions — **pending official pack / 待确认**:

- Primary path: stdin SQL → stdout TSV/results/status; stderr logs. No listener
  unless the optional wire address is explicitly supplied.
- Framing: semicolon outside SQL quotes or newline outside quotes/balanced
  parentheses/brackets; EOF executes pending SQL. Results end in `OK <rowcount>`;
  writes/commands use `OK <affected>`. Text escapes and JSON vectors follow README.
- Ready flag/handshake: none currently; local `SELECT 1` probe. Official ready
  flag name, timing and handshake are 待确认. No stdout banner.
- Errors: `ERROR <message>` then continue; no numeric SQL error codes yet.
  Startup/I/O errors log to stderr and exit nonzero. Official codes are 待确认.
- Exact is default. `-ann` or `SET search_mode=ann` enables a separate every-second
  filtered-candidate scan; `SELECT search_mode` exposes mode. No index or recall
  guarantee; fewer than k results are possible. SET is session-only, not rolled
  back or logged. Official search/recall requirements are 待确认.
- First BIGINT column is the temporary PK for ascending equal-distance ties;
  uniqueness is not enforced. Duplicate/no PK ties preserve insertion order.

Open: official pack reconciliation and broader SQL; ANN quality/performance;
float32 arithmetic overflow returns ERROR; checkpoint/compaction, WAL checksums,
process locking, bounded WAL/replay/cloning cost and disk-failure injection.
Only one process/Engine may own a data directory. M4 did not run Docker build or
container runtime checks. Optional wire remains unchanged except shared engine
semantics. No forbidden engines/libraries, new dependencies, secrets or binaries.
