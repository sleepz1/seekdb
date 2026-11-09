# M5 SQL subset self-test

Baseline: `0c33e8fe7c5b7c9b10ec6ae4487f2db21f10df36` (M4), branch
`2026-competition-round2`. No reset/rebase, formal submission or PR merge.

Implementation followed the required sequence, with `go test ./...` passing
after each stage: (1) explicit PK / CREATE constraints, (2) atomic multi-row
INSERT / quoted vectors / UPDATE and DELETE, (3) comparisons / projections /
sorting, (4) self-built vector index.

Final checks, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
cmp conversation.json agent/conversation.json
git diff --check
```

Smoke retains the M3 SIGKILL-after-acknowledgment recovery, immutable snapshot,
COMMIT/ROLLBACK/EOF isolation, flush, errors and continuation checks. M4 int64,
Unicode VARCHAR, finite float32/dimensions, filtering, tie ordering and search
mode checks pass; fixtures now declare explicit BIGINT PRIMARY KEY.

The new M5 stdin section runs the Yuque 2.2 example with INSERT and SELECT each
on one line under existing newline framing. It verifies projected content and
L2 scores, UPDATE/DELETE rollback, duplicate-PK multi-row statement atomicity,
all comparison operators, vector-index ANN/exact and restart from WAL.

Go tests additionally cover missing/multiple/nonconforming PK constraints,
multiple vectors, quoted vectors, multi-row validation failures, forbidden PK
updates, update assignment validation, affected counts, predicates shared across
SELECT/UPDATE/DELETE, explicit PK tie ordering when another BIGINT comes first,
aliases with/without AS, ordinary ordering, invalid queries and literal WHERE
inside UPDATE text. Index tests distinguish bucket candidates from the old
subsample scan, verify exact bypass, filter-before-candidates, transaction
insert/update/delete visibility and rollback, empty-shell fallback, duplicate
index names, wrong columns/tables, committed mutations after WAL replay, and
PK/index metadata plus buckets after snapshot recovery.

All packages pass normal and race tests. No new dependencies, forbidden engines,
secrets or binaries. Optional MySQL wire implementation was not expanded; its
test fixture now declares a PK.

Temporary protocol / open items:

- Official framing, ready flag/handshake, numeric error codes and ANN recall
  requirements: **待确认**, official pack unpublished.
- Current stdin framing: semicolon outside quotes or newline outside quotes and
  balanced parentheses/brackets; arbitrary multiline clauses must be flattened.
  SQL responses flush immediately; stdout TSV + OK/ERROR, stderr logs.
- Sign-LSH probes the closest nonempty filtered Hamming shell. ANN may return
  fewer than k rows; exact remains brute-force exact. No recall guarantee.
- Whole-state clones and index rebuilds are deliberately simple; performance,
  checkpoint/compaction, bounded WAL growth, WAL checksums and process locking
  remain open. Only one Engine/process may own a data directory.
- CREATE now requires an explicit BIGINT PRIMARY KEY. Legacy snapshot tables
  without that metadata remain readable; pre-M5 WAL CREATE statements lacking
  PRIMARY KEY are not migrated by this milestone.
- Docker runtime checks were not rerun. No contest submission or PR merge.
