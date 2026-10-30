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
"""Cloud-only differential coverage of per-document FTS map reuse."""
import json
import pathlib
import sys
import pymysql

connection = pymysql.connect(host='127.0.0.1', port=2881, user='root',
                             charset='utf8mb4', autocommit=True)
cursor = connection.cursor()
cursor.execute('SET ob_query_timeout=1000000000')
cursor.execute('DROP DATABASE IF EXISTS fts_map_reuse')
cursor.execute('CREATE DATABASE fts_map_reuse')
cursor.execute('USE fts_map_reuse')
results = {}
def norm(value):
    if isinstance(value, float):
        return float(format(value, '.12g'))
    if isinstance(value, bytes):
        return value.decode('utf-8')
    if isinstance(value, (tuple, list)):
        return [norm(item) for item in value]
    return value

texts = ['', 'the an a', 'alpha beta alpha', None,
         '数据库 数据库 全文索引', '中文 OceanBase DATABASE café',
         'alpha ' * 1000, 'beta', ' '.join('term'+chr(97+i//26)+chr(97+i%26) for i in range(500)),
         'omega 数据库', '', 'beta beta']
queries = ['alpha beta', '数据库 全文索引', 'OceanBase', 'termab termza', 'omega', 'missingword']
def capture(table, phase):
    cursor.execute(f'SELECT id, body FROM {table} ORDER BY id')
    results[f'{table}/{phase}/rows'] = norm(cursor.fetchall())
    for index, query in enumerate(queries):
        expr = 'MATCH(body) AGAINST(%s IN NATURAL LANGUAGE MODE)'
        for shape, sql in [
            ('ids', f'SELECT id FROM {table} WHERE {expr} ORDER BY id'),
            ('scores', f'SELECT id, {expr} AS score FROM {table} ORDER BY id'),
            ('count', f'SELECT COUNT(*) FROM {table} WHERE {expr}')]:
            cursor.execute(sql, (query,))
            results[f'{table}/{phase}/{index}/{shape}'] = norm(cursor.fetchall())

for parser in ['ik', 'beng', 'space', 'ngram']:
    table = 'docs_' + parser
    cursor.execute(f'CREATE TABLE {table}(id BIGINT PRIMARY KEY, body TEXT)')
    initial = [(i + 1, texts[i % len(texts)]) for i in range(96)]
    cursor.executemany(f'INSERT INTO {table} VALUES (%s, %s)', initial)
    cursor.execute(f'ALTER TABLE {table} ADD FULLTEXT INDEX ft(body) WITH PARSER {parser}')
    capture(table, 'build')
    cursor.executemany(f'INSERT INTO {table} VALUES (%s,%s)', [(100+i, text) for i, text in enumerate(reversed(texts))])
    capture(table, 'insert')
    cursor.execute(f"UPDATE {table} SET body = CASE MOD(id,4) WHEN 0 THEN '' WHEN 1 THEN 'omega beta 数据库' WHEN 2 THEN NULL ELSE 'alpha alpha' END WHERE id <= 16")
    capture(table, 'update')
    connection.begin()
    cursor.execute(f"UPDATE {table} SET body='rollbackword' WHERE id <= 8")
    cursor.execute(f'DELETE FROM {table} WHERE id > 100')
    connection.rollback()
    capture(table, 'rollback')
    cursor.execute(f'DELETE FROM {table} WHERE MOD(id,3)=0')
    capture(table, 'delete')
connection.close()
pathlib.Path(sys.argv[1]).write_text(json.dumps(results, ensure_ascii=False, sort_keys=True), encoding='utf-8')
print(f'Captured {len(results)} FTS map lifecycle outputs')
