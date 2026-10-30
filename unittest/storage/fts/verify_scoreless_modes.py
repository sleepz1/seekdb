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
"""Cloud-only follow-up: verify real scalar/vector plans and full outputs."""
import json
import pathlib
import re
import sys

import pymysql

out = pathlib.Path(sys.argv[1])
repair = '--repair' in sys.argv[2:]
out.mkdir(parents=True, exist_ok=True)
connection = pymysql.connect(host='127.0.0.1', port=2881, user='root',
                             charset='utf8mb4', autocommit=True)
cur = connection.cursor()
cur.execute('SET ob_query_timeout=1000000000')
results = {}
differences = {}
def distinct_word(number):
    return 'missingterm'+''.join(chr(97+(number//(26**power))%26) for power in [2, 1, 0])


terms = ['database performance', 'missingterm',
         'database performance ' + ' '.join(distinct_word(i) for i in range(300)),
         'database', 'database performance ' + ' '.join(distinct_word(i) for i in range(6)),
         'database performance ' + ' '.join(distinct_word(i) for i in range(7)),
         'database database performance']
cur.execute("SELECT TOKENIZE(%s,'beng') AS tokens", (terms[2],))
tokens = json.loads(cur.fetchone()[0])
assert isinstance(tokens, list) and len(set(tokens)) > 256, 'Long query did not produce >256 distinct tokens'
(out / 'long-query-tokens.json').write_text(json.dumps(tokens, ensure_ascii=False), encoding='utf-8')
for batch_size in [0, 256]:
    hint = f"/*+ OPT_PARAM('rowsets_max_rows', {batch_size}) */"
    expr = 'MATCH(title_en,content_en) AGAINST(%s IN NATURAL LANGUAGE MODE)'
    for term_idx, term in enumerate(terms):
        shapes = {
            'count': f'SELECT {hint} COUNT(*) FROM fts_large_bench.docs WHERE {expr}',
            'ids': f'SELECT {hint} id FROM fts_large_bench.docs WHERE {expr} ORDER BY id',
            'scores': f'SELECT {hint} id,{expr} AS score FROM fts_large_bench.docs ORDER BY score DESC,id LIMIT 32',
            'limited_count': f'SELECT {hint} COUNT(*) FROM (SELECT id FROM fts_large_bench.docs WHERE {expr} LIMIT 20 OFFSET 10) t',
            'offset': f'SELECT {hint} id FROM fts_large_bench.docs WHERE {expr} ORDER BY id LIMIT 20 OFFSET 10',
        }
        for shape, sql in shapes.items():
            cur.execute('EXPLAIN EXTENDED ' + sql, (term,))
            plan = '\n'.join(str(value) for row in cur.fetchall() for value in row)
            key = f'{batch_size}-{term_idx}-{shape}'
            (out / (key+'.plan.txt')).write_text(plan, encoding='utf-8')
            # ob_sql_plan.cpp prints rowset for operators only when its size > 1.
            if batch_size == 256:
                assert 'rowset=256' in plan, 'Vector plan was not selected: '+key
            else:
                assert not re.search(r'rowset=\d+', plan), 'Scalar plan was not selected: '+key
            if repair and shape == 'count' and term_idx != 1:
                assert 'calc_relevance=false' in plan, 'Membership optimization not selected: '+key
            cur.execute(sql, (term,))
            rows = cur.fetchall()
            if shape == 'count' and term_idx != 1:
                assert rows and rows[0][0] > 0, 'Expected successful nonempty count: '+key
            if batch_size == 256:
                scalar = results[f'0-{term_idx}-{shape}']
                if scalar != rows:
                    differences[key] = {'scalar': scalar, 'vector': rows}
            results[key] = rows
            (out / 'outputs.json').write_text(json.dumps(results, ensure_ascii=False, sort_keys=True), encoding='utf-8')
connection.close()
(out / 'outputs.json').write_text(json.dumps(results, ensure_ascii=False, sort_keys=True), encoding='utf-8')
(out / 'scalar-vector-differences.json').write_text(json.dumps(differences, ensure_ascii=False, sort_keys=True), encoding='utf-8')
assert not differences, 'Scalar/vector output differs: ' + ', '.join(differences)
print(f'Scalar/vector plans and all {len(results)} full outputs verified')
