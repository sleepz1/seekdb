#!/usr/bin/env bash
# Copyright (c) 2026 OceanBase.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -euo pipefail
REPAIR_SHA=$(git rev-parse HEAD)
PARENT_SHA=5d7919353d73cbbd0012c777b9beb22abf5692eb
ORIGINAL_SHA=44c0ff58d66dfeb5c7adccd125421c879ddd6484
ROOT=$PWD
OUT=$ROOT/sdb07-evidence
INSTANCE=$RUNNER_TEMP/sdb07-instance
CLIENT=$ROOT/deps/3rd/u01/obclient/bin/obclient
export LD_LIBRARY_PATH="$ROOT/deps/3rd/u01/obclient/lib:$ROOT/deps/3rd/u01/obclient/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export MYSQL="$CLIENT -h127.0.0.1 -P2881 -uroot -N -s --default-character-set=utf8mb4"
export MYSQL_VERBOSE="$CLIENT -h127.0.0.1 -P2881 -uroot --default-character-set=utf8mb4"
export OBMYSQL_MS0=127.0.0.1 OBMYSQL_MS0_DEV=127.0.0.1 OBMYSQL_PORT=2881
regression_status=0
mkdir -p "$OUT"
git diff --name-only "$PARENT_SHA" "$REPAIR_SHA" -- src > "$OUT/changed-sources.txt"
sha256sum tools/benchmark/fts_large_bench* > "$OUT/benchmark-integrity.sha256"
for variant in parent repair; do
  case $variant in
    parent) source_sha=$PARENT_SHA;;
    original) source_sha=$ORIGINAL_SHA;;
    repair) source_sha=$REPAIR_SHA;;
  esac
  while IFS= read -r path; do git checkout "$source_sha" -- "$path"; done < "$OUT/changed-sources.txt"
  mkdir -p "$OUT/$variant"
  echo "$source_sha" > "$OUT/$variant/source-sha.txt"
  cmake --build build_release --target seekdb -j4 2>&1 | tee "$OUT/$variant/build.log"
  python3 .github/script/seekdb/sdb.py start --binary "$ROOT/build_release/src/observer/seekdb" --base-dir "$INSTANCE" --port 2881 --parameter memory_limit=4G --parameter datafile_size=2G --parameter log_disk_size=2G
  python3 .github/script/seekdb/sdb.py wait-ready --client "$CLIENT" --base-dir "$INSTANCE" --host 127.0.0.1 --port 2881 --user root --timeout 180
  LABEL=$variant ROWS=20000 BATCH=500 ROUNDS=3000 QUERY_ROUNDS=200 SAMPLES=3 WARMUP=30 SKIP_LOAD=0 OUTPUT="$OUT/$variant/report.txt" bash tools/benchmark/fts_large_bench.sh 2>&1 | tee "$OUT/$variant/benchmark.log"
  python3 tools/benchmark/fts_large_bench_score.py "$OUT/$variant/report.txt" --json > "$OUT/$variant/repository-score.json"
  "$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_contest_semantics.py "$OUT/$variant/semantics.json"
  if [[ $variant != parent ]]; then
    "$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_contest_semantics.py --compare "$OUT/parent/semantics.json" "$OUT/$variant/semantics.json"
  fi
  "$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_word_map_reuse.py "$OUT/$variant/word-map.json"
  if [[ $variant != parent ]]; then
    cmp "$OUT/parent/word-map.json" "$OUT/$variant/word-map.json"
  fi
  $MYSQL -e 'CREATE DATABASE IF NOT EXISTS test'
  for test_case in tokenize_function parser_properties; do
    suite="$ROOT/tools/deploy/mysql_test/test_suite/fts_index"
    test_out="$OUT/$variant/mysqltest-$test_case"
    mkdir -p "$test_out/tmp"
    sha256sum "$suite/t/$test_case.test" "$suite/r/mysql/$test_case.result" > "$test_out/source.sha256"
    if (cd "$ROOT/tools/deploy"
      timeout --signal=TERM 300s "$ROOT/deps/3rd/u01/obclient/bin/mysqltest" --host=127.0.0.1 --port=2881 --user=root --database=test \
        --tmpdir="$test_out/tmp" --logdir="$test_out" --silent \
        --test-file="$suite/t/$test_case.test" --result-file="$suite/r/mysql/$test_case.result" \
        --timer-file="$test_out/timer" --tail-lines=20
    ) 2>&1 | tee "$test_out/console.log"; then
      echo PASS > "$test_out/status.txt"
    else
      echo FAIL > "$test_out/status.txt"
      regression_status=1
    fi
    sha256sum --check "$test_out/source.sha256"
  done
  python3 .github/script/seekdb/sdb.py stop --base-dir "$INSTANCE"
  cp "$INSTANCE/log/seekdb.log" "$OUT/$variant/seekdb.log" || true
  rm -rf "$INSTANCE"
done
sha256sum --check "$OUT/benchmark-integrity.sha256"
exit "$regression_status"
