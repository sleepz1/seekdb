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
"""Cloud-only capture: compare this JSON with the pinned build's capture."""
import json
import pathlib
import sys
import pymysql

connection = pymysql.connect(host='127.0.0.1', port=2881, user='root',
                             charset='utf8mb4', autocommit=True)
cursor = connection.cursor()
results = {}
texts = [
    '', 'a', 'AbC THE database DATABASE', 'ONE two THREE four FIVE',
    'ÉCOLE café Straße İstanbul ΑΒΓ абв', '１２３ＡＢＣ，中文。\n\t中文',
    '😀数据库𠀀测试', 'x\x00Y z', 'CAFÉ 中文 AbC',
]
for length in [63, 64, 65, 83, 84, 85, 127, 128, 129, 255, 256, 257, 512]:
    texts.extend(['A' * length, 'A' * length + ' bbb CCCC'])
    texts.extend(['É' * length, 'É' * length + ' bbb CCCC'])
    texts.extend(['A' + 'É' * length + 'B', 'A' + '中文' * length + 'B bbb CCCC'])
texts.extend(['A' * 257 + ' bbb ' + 'B' * 256 + ' CCCC',
              'bbb ' + 'É' * 257 + ' CCCC ' + 'É' * 256 + ' ddd'])
charset_collations = [
    ('latin1', 'latin1_swedish_ci'), ('latin1', 'latin1_bin'),
    ('gbk', 'gbk_chinese_ci'), ('gbk', 'gbk_bin'),
    ('utf8mb4', 'utf8mb4_bin'), ('utf8mb4', 'utf8mb4_general_ci'),
    ('utf8mb4', 'utf8mb4_unicode_ci'),
]
for charset, collation in charset_collations:
    for index, text in enumerate(texts):
        expression = f'CONVERT(%s USING {charset}) COLLATE {collation}'
        metadata_key = f'input/{collation}/{index}'
        try:
            cursor.execute(f'SELECT CHARSET({expression}) AS input_charset, COLLATION({expression}) AS input_collation, '
                           f'LENGTH({expression}) AS input_length, HEX({expression}) AS input_hex', (text,) * 4)
            metadata = list(cursor.fetchone())
            assert metadata[0] == charset and metadata[1] == collation, metadata
            results[metadata_key] = metadata
        except pymysql.MySQLError as error:
            results[metadata_key] = {'error': error.args[0]}
        for detail in [False, True]:
            sql = f'SELECT TOKENIZE({expression}, %s'
            sql += ", '[{\"output\":\"all\"}]')" if detail else ')'
            sql += ' AS tokens'
            key = f'{collation}/{index}/{detail}'
            try:
                cursor.execute(sql, (text, 'beng'))
                raw_value = cursor.fetchone()[0]
                value = None if raw_value is None else json.loads(raw_value)
                if isinstance(value, list):
                    value.sort(key=lambda item: json.dumps(item, sort_keys=True, ensure_ascii=False))
                elif isinstance(value, dict) and 'tokens' in value:
                    value['tokens'].sort(key=lambda item: json.dumps(item, sort_keys=True, ensure_ascii=False))
                results[key] = value
            except pymysql.MySQLError as error:
                results[key] = {'error': error.args[0]}
connection.close()
pathlib.Path(sys.argv[1]).write_text(json.dumps(results, ensure_ascii=False, sort_keys=True),
                                   encoding='utf-8')
