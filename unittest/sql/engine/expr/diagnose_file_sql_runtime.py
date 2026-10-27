#!/usr/bin/env python3
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
"""Collect diagnostics after unchanged mysqltest failure, never a passing gate."""
import json
import os
from pathlib import Path
import sys
import pymysql

if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_OS") != "Linux":
    raise SystemExit("Disposable Linux Actions runner required")
base = Path(os.environ["RUNNER_TEMP"]).resolve() / "sdb04-instance"
data = base / "data_for_mysqltest_file_sql"
logdir = Path(os.environ["GITHUB_WORKSPACE"]) / "official-mysqltest-evidence"
for case in ("ai_prompt", "ai_function_enhancement", "file_sql_basic"):
    log = logdir / case / "console.log"
    if log.is_file():
        print("CASE CONSOLE", case, flush=True)
        print("\n".join(log.read_text(errors="replace").splitlines()[-80:]), flush=True)
db = pymysql.connect(host="127.0.0.1", port=2881, user="root", password="",
                     database="test", charset="utf8mb4", autocommit=True,
                     read_timeout=30, connect_timeout=10)
try:
    with db.cursor() as cur:
        cur.execute("SET SESSION enable_file_sql=ON")
        probes = [
            ("csv_count", "SELECT COUNT(*) AS n FROM read_csv(%s)", "basic.csv"),
            ("csv_count_alias", "SELECT COUNT(*) AS n FROM read_csv(%s) f", "basic.csv"),
            ("csv_star", "SELECT * FROM read_csv(%s) f LIMIT 1", "basic.csv"),
            ("csv_projection", "SELECT id,name FROM read_csv(%s) f LIMIT 1", "basic.csv"),
            ("csv_options", "SELECT id FROM read_csv(%s,header=true,sample_size=-1) f LIMIT 1", "basic.csv"),
            ("json_count", "SELECT COUNT(*) AS n FROM read_json(%s) f", "basic.jsonl"),
            ("parquet_count", "SELECT COUNT(*) AS n FROM read_parquet(%s) f", "simple.parquet"),
        ]
        for label, query, filename in probes:
            path = data / filename
            if not path.is_file():
                print(json.dumps({"probe": label, "fixture_missing": True}), flush=True)
                continue
            try:
                cur.execute(query, (str(path),))
                print(json.dumps({"probe": label, "rows": cur.fetchall()}, default=str), flush=True)
            except pymysql.err.DatabaseError as err:
                print(json.dumps({"probe": label, "error": err.args}), flush=True)
            cur.execute("SELECT 1")
            assert cur.fetchone()[0] == 1
        try:
            cur.execute("SHOW COLUMNS FROM oceanbase.`GV$OB_SQL_AUDIT`")
            available = {row[0].lower() for row in cur.fetchall()}
            wanted = [name for name in ("ret_code", "plan_id", "plan_type", "query_sql", "trace_id")
                      if name in available]
            if "query_sql" in available and wanted:
                query = "SELECT " + ",".join(wanted) + " FROM oceanbase.`GV$OB_SQL_AUDIT` WHERE query_sql LIKE %s"
                if "request_time" in available:
                    query += " ORDER BY request_time DESC"
                cur.execute(query + " LIMIT 12", ("%read_csv%",))
                for row in cur.fetchall():
                    print(json.dumps({"audit": dict(zip(wanted, row))}, default=str), flush=True)
        except pymysql.err.DatabaseError as err:
            print(json.dumps({"audit_error": err.args}), flush=True)
finally:
    db.close()
