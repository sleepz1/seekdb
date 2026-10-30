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
ROOT=$PWD
OUT=$ROOT/sdb07-evidence
INSTANCE=$RUNNER_TEMP/sdb07-instance
CLIENT=$ROOT/deps/3rd/u01/obclient/bin/obclient
export LD_LIBRARY_PATH="$ROOT/deps/3rd/u01/obclient/lib:$ROOT/deps/3rd/u01/obclient/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export MYSQL="$CLIENT -h127.0.0.1 -P2881 -uroot -N -s --default-character-set=utf8mb4"
export MYSQL_VERBOSE="$CLIENT -h127.0.0.1 -P2881 -uroot --default-character-set=utf8mb4"
mkdir -p "$OUT/baseline"
git rev-parse HEAD > "$OUT/transport-sha.txt"
git diff --exit-code 5d7919353d73cbbd0012c777b9beb22abf5692eb -- src tools/benchmark
sha256sum tools/benchmark/fts_large_bench* > "$OUT/benchmark-integrity.sha256"
cmake --build build_release --target seekdb -j4 2>&1 | tee "$OUT/baseline/build.log"
python3 .github/script/seekdb/sdb.py start --binary "$ROOT/build_release/src/observer/seekdb" --base-dir "$INSTANCE" --port 2881 --parameter memory_limit=4G --parameter datafile_size=2G --parameter log_disk_size=2G
python3 .github/script/seekdb/sdb.py wait-ready --client "$CLIENT" --base-dir "$INSTANCE" --host 127.0.0.1 --port 2881 --user root --timeout 180
LABEL=official-best-5d791935 ROWS=20000 BATCH=500 ROUNDS=3000 QUERY_ROUNDS=200 SAMPLES=3 WARMUP=30 SKIP_LOAD=0 OUTPUT="$OUT/baseline/report.txt" bash tools/benchmark/fts_large_bench.sh 2>&1 | tee "$OUT/baseline/benchmark.log"
$MYSQL -e "EXPLAIN EXTENDED SELECT COUNT(*) FROM fts_large_bench.docs WHERE MATCH(title,content) AGAINST('全文索引 分词器'); EXPLAIN EXTENDED SELECT COUNT(*) FROM (SELECT id FROM fts_large_bench.docs WHERE MATCH(content) AGAINST('倒排索引 tokenizer') LIMIT 20) t;" > "$OUT/baseline/query-plans.txt"
$MYSQL -e "SELECT * FROM oceanbase.__all_virtual_ddl_task_status;" > "$OUT/baseline/ddl-task-status.txt" 2>&1 || true
"$RUNNER_TEMP/sdb07-venv/bin/python" unittest/storage/fts/test_contest_semantics.py "$OUT/baseline/semantics.json"
python3 .github/script/seekdb/sdb.py stop --base-dir "$INSTANCE"
find "$INSTANCE/log" -maxdepth 1 -type f -name 'seekdb.log*' -exec cp {} "$OUT/baseline/" \;
sha256sum --check "$OUT/benchmark-integrity.sha256"
