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
"""Real-instance regressions for reviewed AI-column boundary conditions."""
import json
import os
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pymysql


class Reply(BaseHTTPRequestHandler):
    fail = False

    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        prompt = next((m.get('content', '') for m in body.get('messages', [])
                       if m.get('role') == 'user'), '')
        if self.fail:
            status, reply = 400, {'error': {'message': 'controlled offline model'}}
        elif self.path.endswith('/embeddings'):
            inputs = body['input']
            if isinstance(inputs, str):
                inputs = [inputs]
            status, reply = 200, {'data': [
                {'index': i, 'embedding': [1e-30, -2.5e20, 0.25, 0.0]}
                for i, _value in enumerate(inputs)]}
        else:
            if 'attack-number' in prompt:
                content = "0, content='injected' -- "
            elif 'overflow-int' in prompt:
                content = '999'
            elif 'normal-int' in prompt:
                content = '7'
            elif 'null-content' in prompt:
                content = None
            elif 'empty-content' in prompt:
                content = ''
            elif 'object-content' in prompt:
                content = {'unexpected': True}
            else:
                content = 'value:' + prompt
            status, reply = 200, {'choices': [{'message': {'content': content}}]}
        data = json.dumps(reply).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class AIColumnRegressions(unittest.TestCase):
    @classmethod
    def connect(cls):
        return pymysql.connect(host='127.0.0.1', port=2881, user='root', password='',
                               database='test', autocommit=True, read_timeout=30)

    @classmethod
    def sql(cls, query, args=None, db=None):
        with (db or cls.db).cursor() as cursor:
            cursor.execute(query, args)
            return cursor.fetchall() if cursor.description else ()

    @classmethod
    def setUpClass(cls):
        cls.db = cls.connect()
        cls.addClassCleanup(cls.db.close)
        cls.server = ThreadingHTTPServer(('127.0.0.1', 19129), Reply)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.addClassCleanup(cls.server.server_close)
        cls.addClassCleanup(cls.server.shutdown)
        cls.prefix = 'rtaicol_' + format(os.getpid(), 'x')
        cls.models = {}
        for kind, path in [('completion', '/v1/chat/completions'), ('dense_embedding', '/v1/embeddings')]:
            name = cls.prefix + '_' + kind
            cls.models[kind] = name
            cls.sql('CALL DBMS_AI_SERVICE.CREATE_AI_MODEL(%s,%s)',
                    (name, json.dumps({'type': kind, 'model_name': 'mock-column'})))
            cls.addClassCleanup(cls.sql, 'CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)', (name,))
            cls.sql('CALL DBMS_AI_SERVICE.CREATE_AI_MODEL_ENDPOINT(%s,%s)',
                    (name + '_ep', json.dumps({'ai_model_name': name, 'provider': 'openai',
                     'url': 'http://127.0.0.1:19129' + path, 'access_key': 'column-mock-key',
                     'request_model_name': 'mock-column'})))
            cls.addClassCleanup(cls.sql, 'CALL DBMS_AI_SERVICE.DROP_AI_MODEL_ENDPOINT(%s)', (name + '_ep',))

    def setUp(self):
        Reply.fail = False
        self.table = self.prefix + '_docs'
        self.sql('SET SESSION ai_column_strict_read=OFF')
        self.sql('DROP TABLE IF EXISTS ' + self.table)

    def tearDown(self):
        Reply.fail = False
        self.sql('SET SESSION ai_column_strict_read=OFF')
        self.sql('DROP TABLE IF EXISTS ' + self.table)

    def create(self, kind='completion', sql_type='varchar(400)'):
        func = 'AI_EMBED' if kind == 'dense_embedding' else 'AI_COMPLETE'
        self.sql("CREATE TABLE %s (id BIGINT PRIMARY KEY, content VARCHAR(200), "
                 "result %s AI COLUMN (%s('%s',content)) "
                 "WITH(sync_mode='async',max_retries=0))" %
                 (self.table, sql_type, func, self.models[kind]))

    def wait_count(self, query, expected, timeout=20):
        end = time.monotonic() + timeout
        actual = None
        while time.monotonic() < end:
            actual = self.sql(query)[0][0]
            if actual == expected:
                return
            time.sleep(0.1)
        self.fail('timed out: %s; expected=%s actual=%s' % (query, expected, actual))

    def test_a_strict_wildcard_and_cached_unrelated_column(self):
        self.create()
        ordinary = self.prefix + '_ordinary'
        self.sql('CREATE TABLE %s(id INT PRIMARY KEY,result VARCHAR(20))' % ordinary)
        self.addCleanup(self.sql, 'DROP TABLE IF EXISTS ' + ordinary)
        self.sql("INSERT INTO %s VALUES(1,'ordinary-ok')" % ordinary)
        self.assertEqual(self.sql('SELECT result FROM ' + ordinary)[0][0], 'ordinary-ok')
        Reply.fail = True
        self.sql("INSERT INTO %s(id,content) VALUES(1,'offline')" % self.table)
        self.sql('SET SESSION ai_column_strict_read=ON')
        with self.assertRaises(pymysql.MySQLError) as error:
            self.sql('SELECT * FROM ' + self.table)
        self.assertEqual(error.exception.args[0], 11120)
        for _attempt in range(2):
            self.assertEqual(self.sql('SELECT result FROM ' + ordinary)[0][0], 'ordinary-ok')
        self.sql("INSERT INTO %s(id,content) VALUES(2,'offline-two')" % self.table)
        self.sql("UPDATE %s SET content='offline-new' WHERE id=1" % self.table)
        self.sql('DELETE FROM %s WHERE id=2' % self.table)
        with self.assertRaises(pymysql.MySQLError) as correlated:
            self.sql('SELECT id FROM %s d WHERE EXISTS(SELECT 1 FROM %s o WHERE o.result=d.result)' %
                     (self.table, ordinary))
        self.assertEqual(correlated.exception.args[0], 11120)

    def test_b_retry_all_129_rows(self):
        self.create()
        self.sql('INSERT INTO %s(id,content) VALUES ' % self.table + ','.join(
            "(%d,'bulk-%d')" % (i, i) for i in range(129)))
        self.wait_count('SELECT COUNT(*) FROM %s WHERE result IS NOT NULL' % self.table, 129, 45)
        Reply.fail = True
        self.sql('ALTER TABLE %s RETRY AI COLUMN result' % self.table)
        self.assertEqual(self.sql('SELECT COUNT(*) FROM %s WHERE result IS NULL' % self.table)[0][0], 129)

    def test_c_retry_reads_locked_dependency_version(self):
        self.create()
        self.sql("INSERT INTO %s(id,content) VALUES(1,'old')" % self.table)
        self.wait_count('SELECT COUNT(*) FROM %s WHERE result IS NOT NULL' % self.table, 1)
        writer = self.connect()
        retry_db = self.connect()
        self.addCleanup(writer.close)
        self.addCleanup(retry_db.close)
        writer.begin()
        self.sql("UPDATE %s SET content='new' WHERE id=1" % self.table, db=writer)
        errors = []
        started = threading.Event()
        def retry():
            try:
                started.set()
                self.sql('ALTER TABLE %s RETRY AI COLUMN result WHERE id=1' % self.table, db=retry_db)
            except Exception as exc:
                errors.append(exc)
        thread = threading.Thread(target=retry, daemon=True)
        thread.start()
        started.wait(5)
        time.sleep(0.7)
        writer.commit()
        thread.join(30)
        self.assertFalse(thread.is_alive(), 'retry did not release row lock')
        self.assertFalse(errors, repr(errors))
        self.wait_count('SELECT COUNT(*) FROM %s WHERE result IS NOT NULL' % self.table, 1)
        self.assertEqual(self.sql('SELECT result FROM ' + self.table)[0][0], 'value:new')

    def test_d_integer_model_text_cannot_change_business_columns(self):
        self.create(sql_type='bigint')
        self.sql("INSERT INTO %s(id,content) VALUES(1,'attack-number'),(2,'keep-business')" % self.table)
        time.sleep(2)
        self.assertEqual(self.sql('SELECT id,content FROM %s ORDER BY id' % self.table),
                         ((1, 'attack-number'), (2, 'keep-business')))
        self.assertIsNone(self.sql('SELECT result FROM %s WHERE id=1' % self.table)[0][0])

        self.sql('DROP TABLE ' + self.table)
        self.create()
        self.sql("INSERT INTO %s(id,content) VALUES(1,'null-content'),(2,'empty-content'),"
                 "(3,'object-content'),(4,'typed-valid')" % self.table)
        self.wait_count('SELECT COUNT(*) FROM %s WHERE id=4 AND result IS NOT NULL' % self.table, 1)
        self.wait_count("SELECT COUNT(*) FROM oceanbase.__all_ai_column_task WHERE table_name='%s'" %
                        self.table, 0)
        self.assertEqual(self.sql('SELECT COUNT(*) FROM %s WHERE id<4 AND result IS NULL' % self.table)[0][0], 3)

        self.sql('DROP TABLE ' + self.table)
        self.create(sql_type='tinyint')
        self.sql("INSERT INTO %s(id,content) VALUES(1,'overflow-int'),(2,'normal-int')" % self.table)
        self.wait_count('SELECT COUNT(*) FROM %s WHERE id=2 AND result=7' % self.table, 1)
        self.wait_count("SELECT COUNT(*) FROM oceanbase.__all_ai_column_task WHERE table_name='%s'" %
                        self.table, 0)
        self.assertIsNone(self.sql('SELECT result FROM %s WHERE id=1' % self.table)[0][0])

    def test_z_scientific_embedding_does_not_block_following_completion(self):
        self.create(kind='dense_embedding', sql_type='vector(4)')
        self.sql("INSERT INTO %s(id,content) VALUES(1,'scientific')" % self.table)
        self.wait_count('SELECT COUNT(*) FROM %s WHERE result IS NOT NULL' % self.table, 1)
        values = json.loads(self.sql('SELECT result FROM ' + self.table)[0][0])
        self.assertAlmostEqual(values[0] / 1e-30, 1.0, places=5)
        self.assertAlmostEqual(values[1] / -2.5e20, 1.0, places=5)
        self.sql('DROP TABLE ' + self.table)
        self.create()
        self.sql("INSERT INTO %s(id,content) VALUES(1,'after-vector')" % self.table)
        self.wait_count('SELECT COUNT(*) FROM %s WHERE result IS NOT NULL' % self.table, 1)
        self.assertEqual(self.sql('SELECT result FROM ' + self.table)[0][0], 'value:after-vector')


if __name__ == '__main__':
    if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_OS') != 'Linux':
        raise SystemExit('This suite requires a disposable Linux Actions runner.')
    unittest.main(verbosity=2)
