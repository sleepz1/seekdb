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
# Same-runner benchmark, semantics and perf profile for SDB-07 candidates.
set -uo pipefail
ROOT=$PWD
OUT=$ROOT/sdb07-evidence
INSTANCE=$RUNNER_TEMP/sdb07-instance
CLIENT=$ROOT/deps/3rd/u01/obclient/bin/obclient
export LD_LIBRARY_PATH="$ROOT/deps/3rd/u01/obclient/lib:$ROOT/deps/3rd/u01/obclient/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export MYSQL="$CLIENT -h127.0.0.1 -P2881 -uroot -N -s --default-character-set=utf8mb4"
export MYSQL_VERBOSE="$CLIENT -h127.0.0.1 -P2881 -uroot --default-character-set=utf8mb4"
export OBMYSQL_MS0=127.0.0.1 OBMYSQL_MS0_DEV=127.0.0.1 OBMYSQL_PORT=2881
HEAD_SHA=$(git rev-parse HEAD)
VARIANTS_FILE=$ROOT/unittest/storage/fts/sdb07_claude_variants.txt
mkdir -p "$OUT"
echo "$HEAD_SHA" > "$OUT/head-sha.txt"
sha256sum tools/benchmark/fts_large_bench* > "$OUT/benchmark-integrity.sha256"
status=0
notice() { echo "::notice title=$1::$2"; }

perf_cmd() {
  local name=$1; shift
  local pid
  pid=$(pgrep -f "$ROOT/build_release/src/observer/seekdb" | head -1)
  if command -v perf >/dev/null 2>&1 && sudo perf --version >/dev/null 2>&1 && [[ -n "$pid" ]]; then
    # exec keeps the recorded pid equal to perf itself, so SIGINT reaches perf directly
    # (sudo does not relay signals sent from its own process group).
    sudo rm -f "$PROF/$name.pid"
    sudo sh -c "echo \$\$ > '$PROF/$name.pid'; exec perf record -F 999 -g -p $pid -o '$PROF/$name.data'" >/dev/null 2>&1 &
    sleep 2
    "$@"
    local ppid; ppid=$(cat "$PROF/$name.pid" 2>/dev/null || true)
    [[ -n "$ppid" ]] && sudo kill -INT "$ppid" 2>/dev/null || true
    for _ in $(seq 1 60); do [[ -n "$ppid" ]] && sudo kill -0 "$ppid" 2>/dev/null || break; sleep 1; done
    [[ -n "$ppid" ]] && sudo kill -9 "$ppid" 2>/dev/null || true
    sudo timeout 600 perf report -i "$PROF/$name.data" --no-children --sort sym --stdio -g none --percent-limit 0.3 2>/dev/null | head -150 > "$PROF/$name.flat.txt" || true
    sudo timeout 600 perf report -i "$PROF/$name.data" --children --sort sym --stdio -g none --percent-limit 1 2>/dev/null | head -200 > "$PROF/$name.children.txt" || true
    sudo rm -f "$PROF/$name.data"
  else
    "$@"
  fi
}

run_sql_file() { $MYSQL < "$1" > /dev/null; }

rebuild_with_timeline() {
  local prof=$1 name=$2 idx=$3 def=$4
  $MYSQL -e "USE fts_large_bench; ALTER TABLE docs DROP INDEX $idx;" 2>&1 | tail -2
  sleep 3
  local s e s_ts e_ts
  s_ts=$(date '+%Y-%m-%d %H:%M:%S.%6N')
  s=$(date +%s%N)
  perf_cmd "build_$name" $MYSQL -e "SET ob_query_timeout = 100000000000; SET ob_trx_timeout = 100000000000; USE fts_large_bench; ALTER TABLE docs ADD FULLTEXT INDEX $def;"
  e=$(date +%s%N)
  e_ts=$(date '+%Y-%m-%d %H:%M:%S.%6N')
  echo "rebuild_${name}_ms $(( (e-s)/1000000 )) start $s_ts end $e_ts" >> "$prof/rebuild.txt"
  # Server log lines inside the DDL window: state switches, child tasks and waits.
  cat "$INSTANCE"/log/seekdb.log* 2>/dev/null | awk -v s="[$s_ts" -v e="[$e_ts" 'substr($0,1,28) >= s && substr($0,1,28) <= e' \
    | grep -v "ddl_task_status" | head -400000 | gzip > "$prof/ddllog-$name.gz" || true
  zcat "$prof/ddllog-$name.gz" | grep -E "switch_status|switch status|task_status|ObFtsIndexBuildTask|ObIndexBuildTask|wait_trans_end|finish|succ to" \
    | cut -c1-400 | head -3000 > "$prof/ddl-timeline-$name.txt" || true
}

profile_variant() {
  PROF=$OUT/$1/profile; mkdir -p "$PROF"
  local tok="$PROF/tok.sql" q="$PROF/q.sql"
  IK_TEXT="OceanBase是一款非常稳定的数据库，全文索引的分词器热路径优化包括解析器实例复用、停用词全局单例、内存池化数据结构等技术，在大量文档索引和查询场景下可以显著降低 CPU 开销。"
  { echo "SET NAMES utf8mb4;"; for i in $(seq 1 6000); do echo "SELECT tokenize('$IK_TEXT', 'ik');"; done; } > "$tok"
  perf_cmd tokenize_ik run_sql_file "$tok"
  { echo "SET NAMES utf8mb4;"; for i in $(seq 1 1000); do echo "SELECT COUNT(*) FROM fts_large_bench.docs WHERE MATCH(title, content) AGAINST('全文索引 分词器');"; done; } > "$q"
  perf_cmd query_cn run_sql_file "$q"
  { echo "SET NAMES utf8mb4;"; for i in $(seq 1 1000); do echo "SELECT COUNT(*) FROM (SELECT id FROM fts_large_bench.docs WHERE MATCH(content) AGAINST('倒排索引 tokenizer') LIMIT 20) t;"; done; } > "$q"
  perf_cmd query_limit run_sql_file "$q"
  $MYSQL -e "SET NAMES utf8mb4; EXPLAIN EXTENDED SELECT COUNT(*) FROM fts_large_bench.docs WHERE MATCH(title, content) AGAINST('全文索引 分词器'); EXPLAIN EXTENDED SELECT COUNT(*) FROM (SELECT id FROM fts_large_bench.docs WHERE MATCH(content) AGAINST('倒排索引 tokenizer') LIMIT 20) t;" > "$PROF/plans.txt" 2>&1
  rebuild_with_timeline "$PROF" ik_all fti_all "fti_all(title, content) WITH PARSER ik"
  rebuild_with_timeline "$PROF" beng fti_en_beng "fti_en_beng(title_en, content_en) WITH PARSER beng"
}

# Steady-state rebuild timings (no perf), repeated to average out runner noise.
repeat_builds() {
  local dir=$1 out=$1/build-repeat.txt summary=""
  : > "$out"
  for spec in "ik_all|rep_ik_all(title, content) WITH PARSER ik" "ik_content|rep_ik_content(content) WITH PARSER ik" "beng|rep_beng(title_en, content_en) WITH PARSER beng"; do
    local name=${spec%%|*} total=0
    for r in 1 2 3; do
      local idx=rep_${name}_$r def=${spec#*|}; def=${def/rep_${name}(/rep_${name}_$r(}
      sleep 2
      local s e; s=$(date +%s%N)
      $MYSQL -e "SET ob_query_timeout = 100000000000; SET ob_trx_timeout = 100000000000; USE fts_large_bench; ALTER TABLE docs ADD FULLTEXT INDEX $def;" 2>&1 | tail -2
      e=$(date +%s%N)
      local ms=$(( (e-s)/1000000 )); total=$((total+ms))
      echo "$name run$r ${ms}ms $($MYSQL -e "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema='fts_large_bench' AND index_name='$idx'" 2>/dev/null | tail -1)" >> "$out"
      $MYSQL -e "USE fts_large_bench; ALTER TABLE docs DROP INDEX $idx;" 2>&1 | tail -2
    done
    summary="$summary $name=$((total/3))ms"
  done
  notice "$(basename "$dir") rebuild-avg" "$summary"
}

run_variant() {
  local label=$1 sha=$2 profile=$3
  local dir=$OUT/$label
  mkdir -p "$dir"
  if [[ "$sha" == HEAD ]]; then sha=$HEAD_SHA; fi
  while IFS= read -r path; do
    if git cat-file -e "$sha:$path" 2>/dev/null; then git checkout "$sha" -- "$path"; else rm -f "$path"; fi
  done < "$OUT/changed-sources.txt"
  echo "$sha" > "$dir/source-sha.txt"
  local t0=$(date +%s)
  if ! cmake --build build_release --target seekdb -j4 > "$dir/build.log" 2>&1; then
    grep -E "error|Error" "$dir/build.log" | head -40 > "$dir/build-errors.txt"
    notice "$label build" "FAILED: $(head -c 1500 "$dir/build-errors.txt" | tr '\n' ' ')"
    status=1; return 1
  fi
  echo "build_minutes $(( ($(date +%s)-t0)/60 ))" > "$dir/build-time.txt"
  tail -5 "$dir/build.log" > "$dir/build-tail.txt"; rm -f "$dir/build.log"
  rm -rf "$INSTANCE"
  python3 .github/script/seekdb/sdb.py start --binary "$ROOT/build_release/src/observer/seekdb" --base-dir "$INSTANCE" --port 2881 --parameter memory_limit=4G --parameter datafile_size=2G --parameter log_disk_size=2G
  python3 .github/script/seekdb/sdb.py wait-ready --client "$CLIENT" --base-dir "$INSTANCE" --host 127.0.0.1 --port 2881 --user root --timeout 180
  LABEL=$label ROWS=20000 BATCH=500 ROUNDS=3000 QUERY_ROUNDS=200 SAMPLES=3 WARMUP=30 SKIP_LOAD=0 OUTPUT="$dir/report.txt" bash tools/benchmark/fts_large_bench.sh > "$dir/benchmark.log" 2>&1
  python3 tools/benchmark/fts_large_bench_score.py "$dir/report.txt" --json > "$dir/repository-score.json" 2>&1
  notice "$label metrics" "$(grep -E '_sec:|_avg_ms:|_hits:' "$dir/report.txt" | tr -s ' ' | tr '\n' ';')"
  "$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_contest_semantics.py "$dir/semantics.json" > "$dir/semantics.log" 2>&1 || { status=1; notice "$label semantics" "capture failed $(tail -c 800 "$dir/semantics.log")"; }
  if [[ "$label" != base && -f "$OUT/base/semantics.json" ]]; then
    if "$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_contest_semantics.py --compare "$OUT/base/semantics.json" "$dir/semantics.json" > "$dir/semantics-compare.log" 2>&1; then
      notice "$label semantics" "MATCH base: $(tail -1 "$dir/semantics-compare.log")"
    else
      status=1; notice "$label semantics" "DIFF: $(tail -c 1500 "$dir/semantics-compare.log")"
    fi
  fi
  $MYSQL -e 'CREATE DATABASE IF NOT EXISTS test'
  for test_case in tokenize_function parser_properties; do
    suite="$ROOT/tools/deploy/mysql_test/test_suite/fts_index"
    test_out="$dir/mysqltest-$test_case"; mkdir -p "$test_out/tmp"
    if (cd "$ROOT/tools/deploy" && timeout --signal=TERM 300s "$ROOT/deps/3rd/u01/obclient/bin/mysqltest" --host=127.0.0.1 --port=2881 --user=root --database=test \
        --tmpdir="$test_out/tmp" --logdir="$test_out" --silent --test-file="$suite/t/$test_case.test" --result-file="$suite/r/mysql/$test_case.result" \
        --timer-file="$test_out/timer" --tail-lines=20) > "$test_out/console.log" 2>&1; then
      echo PASS > "$test_out/status.txt"
    else
      echo FAIL > "$test_out/status.txt"; status=1; notice "$label mysqltest" "$test_case FAIL"
    fi
    rm -rf "$test_out/tmp"
  done
  repeat_builds "$dir"
  if [[ "$profile" == 1 ]]; then profile_variant "$label"; fi
  python3 .github/script/seekdb/sdb.py stop --base-dir "$INSTANCE"
  grep -E "ddl|DDL" "$INSTANCE/log/seekdb.log" 2>/dev/null | grep -E "INFO|WARN" | tail -4000 | gzip > "$dir/seekdb-ddl.log.gz" || true
  rm -rf "$INSTANCE"
}

# variants file: label sha profile(0/1); first line must be base
git diff --name-only 5d7919353d73cbbd0012c777b9beb22abf5692eb "$HEAD_SHA" -- src > "$OUT/changed-sources.txt"
while read -r label sha profile; do
  [[ -z "$label" || "$label" == \#* ]] && continue
  run_variant "$label" "$sha" "$profile" || true
done < "$VARIANTS_FILE"
while IFS= read -r path; do git checkout "$HEAD_SHA" -- "$path" 2>/dev/null || true; done < "$OUT/changed-sources.txt"
sha256sum --check "$OUT/benchmark-integrity.sha256" || status=1
exit $status
