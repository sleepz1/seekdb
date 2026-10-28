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
"""Differential full token, ID and score coverage using the unchanged corpus."""
import json, pathlib, sys
def normalize(v):
    if isinstance(v, float): return float(format(v, '.12g'))
    if isinstance(v, bytes): return v.decode('utf-8')
    if isinstance(v, (list, tuple)): return [normalize(x) for x in v]
    return v
if sys.argv[1] == '--compare':
    before=json.loads(pathlib.Path(sys.argv[2]).read_text())
    after=json.loads(pathlib.Path(sys.argv[3]).read_text())
    assert before.keys() == after.keys()
    different=[key for key in before if before[key] != after[key]]
    assert not different, 'Full semantic output differs: '+str(different)
    print('All '+str(len(before))+' token/ID/score cases exactly match parent')
    sys.exit(0)
import pymysql
connection=pymysql.connect(host='127.0.0.1',port=2881,user='root',charset='utf8mb4',autocommit=True)
cur=connection.cursor()
cur.execute('SET ob_query_timeout=1000000000')
results={}
texts=['','a','The quick BROWN fox the database DATABASE','OceanBase数据库全文索引分词器','数据库 数据库 倒排索引 tokenizer','ÉCOLE café Straße İstanbul ΑΒΓ абв','１２３ＡＢＣ，中文。\n\t中文','😀数据库𠀀测试','中文' * 700]
for parser in ['ik','beng','ngram','space']:
    for i,text in enumerate(texts):
        for detail in [False,True]:
            sql='SELECT TOKENIZE(%s,%s'+(", '[{\"output\":\"all\"}]')" if detail else ')')
            key=f'token/{parser}/{i}/{detail}'
            try:
                cur.execute(sql,(text,parser))
                value=json.loads(cur.fetchone()[0])
                if isinstance(value,list): value=sorted(value,key=lambda x:json.dumps(x,sort_keys=True,ensure_ascii=False))
                elif isinstance(value,dict) and 'tokens' in value: value['tokens']=sorted(value['tokens'],key=lambda x:json.dumps(x,sort_keys=True,ensure_ascii=False))
                results[key]=value
            except pymysql.MySQLError as error:
                results[key]={'error':error.args[0]}
for columns,terms in [('title, content',['全文索引 分词器','OceanBase 稳定 database','missingterm','','数据库 数据库']),('title_en, content_en',['database performance audit','DATABASE','the','+database -performance'])]:
    for text in terms:
        modes=[' IN NATURAL LANGUAGE MODE',' IN BOOLEAN MODE'] if text else [' IN NATURAL LANGUAGE MODE']
        for mode in modes:
            expr=f'MATCH({columns}) AGAINST(%s{mode})'
            for shape,sql in [('count',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE {expr}'),('ids',f'SELECT id FROM fts_large_bench.docs WHERE {expr} ORDER BY id'),('scores',f'SELECT id, {expr} AS score FROM fts_large_bench.docs ORDER BY score DESC,id LIMIT 32'),('threshold',f'SELECT id FROM fts_large_bench.docs WHERE {expr} > 0.5 ORDER BY id'),('zero',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE {expr} = 0'),('limit',f'SELECT COUNT(*) FROM (SELECT id FROM fts_large_bench.docs WHERE {expr} LIMIT 20) t')]:
                key=f'match/{columns}/{text}/{mode}/{shape}'
                try:
                    cur.execute(sql,(text,))
                    results[key]=normalize(cur.fetchall())
                except pymysql.MySQLError as error:
                    results[key]={'error':error.args[0]}
connection.close()
pathlib.Path(sys.argv[1]).write_text(json.dumps(results,ensure_ascii=False,sort_keys=True),encoding='utf-8')
print('Captured '+str(len(results))+' full token/ID/score cases')
