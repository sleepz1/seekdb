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
"""Cloud-only cold DBMS_AI_SERVICE regression against a started seekdb.

Run immediately after start_instance, before any other AI package calls. Each
query emits start/end JSON so a server trace can be matched to the slow stage.
The SQL deadline stays at the official 10 seconds; the client allows the server
to return its own timeout. No model endpoint or external AI server is needed.
"""

import argparse
import datetime
import json
import os
import sys
import time

import pymysql


def emit(**record):
    record['utc'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    print(json.dumps(record, ensure_ascii=False, default=str), flush=True)


class Probe:
    def __init__(self):
        self.failures = []

    def query(self, db, stage, statement, args=None, expected=(0,), want=None):
        emit(event='start', stage=stage, connection_id=db.thread_id())
        started = time.monotonic()
        errno, rows = 0, None
        try:
            with db.cursor() as cursor:
                cursor.execute(statement, args)
                rows = cursor.fetchall() if cursor.description else ()
                # Consume CALL's final result set before the next statement.
                while cursor.nextset():
                    pass
        except pymysql.MySQLError as error:
            errno = error.args[0] if error.args else -1
            emit(event='error', stage=stage, errno=errno, message=str(error))
        elapsed = time.monotonic() - started
        ok = errno in expected and (want is None or rows == want)
        emit(event='end', stage=stage, errno=errno, elapsed_s=round(elapsed, 6),
             expected_errno=expected, rows=rows, passed=ok)
        if not ok:
            self.failures.append(stage)
        return ok


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=2881)
    parser.add_argument('--repeat', type=int, default=3)
    options = parser.parse_args()
    if os.environ.get('GITHUB_ACTIONS') != 'true' or sys.platform != 'linux':
        parser.error('requires a disposable Linux GitHub Actions runner')
    if not 1 <= options.repeat <= 20:
        parser.error('--repeat must be between 1 and 20')

    def connect():
        return pymysql.connect(host=options.host, port=options.port, user='root',
                               password='', autocommit=True, charset='utf8mb4',
                               connect_timeout=15, read_timeout=30,
                               write_timeout=15)

    probe = Probe()
    prefix = 'coldai_' + format(os.getpid(), 'x')
    database = prefix + '_db'
    model = prefix + '_model'
    owner = connect()
    try:
        probe.query(owner, 'set_official_deadline',
                    'SET SESSION ob_query_timeout=10000000')
        probe.query(owner, 'session_state',
                    'SELECT @@ob_query_timeout, @@autocommit, CONNECTION_ID()')
        # Preserve the official fresh-database + JSON/LONGTEXT + table DDL order.
        probe.query(owner, 'drop_database', 'DROP DATABASE IF EXISTS ' + database)
        probe.query(owner, 'create_database', 'CREATE DATABASE ' + database)
        probe.query(owner, 'select_database', 'USE ' + database)
        probe.query(owner, 'json_prompt',
                    "SELECT ai_prompt('{0}/{0}/{1}',json_array(1,true),json_object('k',2))")
        probe.query(owner, 'drop_prompt_table',
                    'DROP TABLE IF EXISTS ai_enhance_prompt')
        probe.query(owner, 'create_prompt_table',
                    'CREATE TABLE ai_enhance_prompt(t LONGTEXT,a TEXT)')
        probe.query(owner, 'insert_prompt_table',
                    "INSERT INTO ai_enhance_prompt VALUES('{0}-ok','lob')")
        probe.query(owner, 'lob_prompt', 'SELECT ai_prompt(t,a) FROM ai_enhance_prompt')
        probe.query(owner, 'drop_prompt_table_after_read', 'DROP TABLE ai_enhance_prompt')
        probe.query(owner, 'cold_missing_model_drop',
                    'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (model,),
                    expected=(11118,))
        # Record warm recovery even if the cold call failed. Unexpected errors
        # remain failures; a later success never hides the initial timeout.
        for index in range(options.repeat):
            probe.query(owner, 'warm_missing_model_drop_' + str(index),
                        'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (model,),
                        expected=(11118,))
        probe.query(owner, 'create_model',
                    'CALL DBMS_AI_SERVICE.CREATE_AI_MODEL(%s,%s)',
                    (model, '{"type":"completion","model_name":"cold-probe"}'))
        with connect() as fresh:
            probe.query(fresh, 'new_connection_deadline',
                        'SET SESSION ob_query_timeout=10000000')
            probe.query(fresh, 'new_connection_database', 'USE ' + database)
            probe.query(fresh, 'new_connection_existing_model_drop',
                        'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (model,))
            probe.query(fresh, 'new_connection_missing_model_drop',
                        'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (model,),
                        expected=(11118,))
    finally:
        # Dedicated names only; collect cleanup errors instead of swallowing them.
        probe.query(owner, 'cleanup_model',
                    'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (model,),
                    expected=(0, 11118))
        probe.query(owner, 'cleanup_database', 'DROP DATABASE IF EXISTS ' + database)
        owner.close()
    emit(event='summary', failures=probe.failures, passed=not probe.failures)
    return bool(probe.failures)


if __name__ == '__main__':
    sys.exit(main())
