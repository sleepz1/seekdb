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
            sql='SELECT TOKENIZE(%s,%s'+(", '[{\"output\":\"all\"}]')" if detail else ')')+' AS tokens'
            key=f'token/{parser}/{i}/{detail}'
            try:
                cur.execute(sql,(text,parser))
                value=json.loads(cur.fetchone()[0])
                if isinstance(value,list): value=sorted(value,key=lambda x:json.dumps(x,sort_keys=True,ensure_ascii=False))
                elif isinstance(value,dict) and 'tokens' in value: value['tokens']=sorted(value['tokens'],key=lambda x:json.dumps(x,sort_keys=True,ensure_ascii=False))
                results[key]=value
            except pymysql.MySQLError as error:
                results[key]={'error':error.args[0]}
long_query='database performance '+' '.join('missingterm'+str(i) for i in range(300))
for columns,terms in [('title, content',['全文索引 分词器','OceanBase 稳定 database','missingterm','','数据库 数据库']),('title_en, content_en',['database performance audit','DATABASE','the','+database -performance',long_query])]:
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
                if text == long_query and mode == ' IN NATURAL LANGUAGE MODE' and shape in ['count','ids']:
                    assert isinstance(results[key],list) and results[key], 'Long natural-language query must succeed: '+key
                    if shape == 'count': assert results[key][0][0] > 0, 'Long query must find matching documents'
# Reusing one parameterized statement must not reuse the membership-only plan
# for a nonzero threshold. Include aliases, derived tables, HAVING and scores
# that are used solely for ordering.
expr="MATCH(title_en,content_en) AGAINST(%s)"
for threshold in [0,0.5,0,2,0]:
    cur.execute(f'SELECT id FROM fts_large_bench.docs WHERE {expr} > %s ORDER BY id',('database performance',threshold))
    results[f'plan-reuse/{threshold}']=normalize(cur.fetchall())
for name,sql,params in [
    ('alias',f'SELECT id,{expr} AS score FROM fts_large_bench.docs WHERE {expr} ORDER BY score DESC,id LIMIT 32',('database','database')),
    ('order-only',f'SELECT id FROM fts_large_bench.docs WHERE {expr} ORDER BY {expr} DESC,id LIMIT 32',('database','database')),
    ('having',f'SELECT id,{expr} AS score FROM fts_large_bench.docs HAVING score > 0.5 ORDER BY score DESC,id LIMIT 32',('database',)),
    ('nested',f'SELECT id FROM (SELECT id,{expr} AS score FROM fts_large_bench.docs) t WHERE score > 0.5 ORDER BY id',('database',)),
    ('and',f"SELECT id FROM fts_large_bench.docs WHERE {expr} AND MATCH(content) AGAINST('数据库') ORDER BY id",('database',)),
]:
    try:
        cur.execute(sql,params)
        results['shape/'+name]=normalize(cur.fetchall())
    except pymysql.MySQLError as error:
        results['shape/'+name]={'error':error.args[0]}
# SDB-07 regression cases for the changed code paths (compared against the parent):
# 1) inline utf8mb4_general_ci / binary string compare used by sort, group by,
#    distinct, comparison predicates and FTS aux index build;
# 2) casedown skip for already-lowercase ASCII and CJK tokens;
# 3) BOOL(MATCH) relevance filter evaluated directly.
cmp_values=['a','A','b','B','ab','aB','Ab','a ','a  ','a\t','','  ','z','Z','_','0','9',
            '数据','数据库','数据 ','數據','数','䀀','鿿','é','É','e','E','ß','ss','ẞ','Ａ','ａ','ＡＢ',
            'İ','i','I','ı','Σ','σ','ς','😀','😀a','𠀀','a😀','数据😀','数据a','数据A','A数据','a数据',
            '中文 ','中文','中文\t','\u3000','x\u3000','abc'*30,'ABC'*30,'abc'*29+'abd','数据库'*20,'数据库'*19+'数据底']
cur.execute('CREATE DATABASE IF NOT EXISTS sdb07_regress')
cur.execute('DROP TABLE IF EXISTS sdb07_regress.cmp')
cur.execute('CREATE TABLE sdb07_regress.cmp(id INT PRIMARY KEY, s VARCHAR(256) COLLATE utf8mb4_general_ci, b VARBINARY(1024), t TEXT COLLATE utf8mb4_general_ci)')
for i,v in enumerate(cmp_values):
    cur.execute('INSERT INTO sdb07_regress.cmp VALUES(%s,%s,%s,%s)',(i,v,v.encode('utf-8'),v+' '+cmp_values[(i*7)%len(cmp_values)]+' OceanBase 全文索引 Database'))
for name,sql in [
    ('order-ci','SELECT id FROM sdb07_regress.cmp ORDER BY s,id'),
    ('order-ci-desc','SELECT id FROM sdb07_regress.cmp ORDER BY s DESC,id'),
    ('order-bin','SELECT id FROM sdb07_regress.cmp ORDER BY b,id'),
    ('group-ci','SELECT MIN(id),COUNT(*) FROM sdb07_regress.cmp GROUP BY s ORDER BY MIN(id)'),
    ('distinct-ci','SELECT COUNT(DISTINCT s) FROM sdb07_regress.cmp'),
    ('pairs-ci','SELECT a.id,b.id,a.s<b.s,a.s=b.s,a.s>b.s FROM sdb07_regress.cmp a, sdb07_regress.cmp b ORDER BY a.id,b.id'),
    ('pairs-bin','SELECT a.id,b.id,a.b<b.b,a.b=b.b FROM sdb07_regress.cmp a, sdb07_regress.cmp b ORDER BY a.id,b.id'),
]:
    try:
        cur.execute(sql)
        results['cmp/'+name]=normalize(cur.fetchall())
    except pymysql.MySQLError as error:
        results['cmp/'+name]={'error':error.args[0]}
for parser in ['ik','beng','ngram','space']:
    idx='ft_'+parser
    try:
        cur.execute(f'ALTER TABLE sdb07_regress.cmp ADD FULLTEXT INDEX {idx}(t) WITH PARSER {parser}')
        for q in ['数据','DATA','database','oceanbase','全文索引','a','é','ss','😀']:
            for mode in [' IN NATURAL LANGUAGE MODE',' IN BOOLEAN MODE']:
                cur.execute(f'SELECT id, ROUND(MATCH(t) AGAINST(%s{mode}),9) AS sc FROM sdb07_regress.cmp WHERE MATCH(t) AGAINST(%s{mode}) ORDER BY sc DESC,id',(q,q))
                results[f'cmp-fts/{parser}/{q}/{mode}']=normalize(cur.fetchall())
        cur.execute(f'ALTER TABLE sdb07_regress.cmp DROP INDEX {idx}')
    except pymysql.MySQLError as error:
        results[f'cmp-fts/{parser}/error']={'error':error.args[0]}
for i,text in enumerate(['abc数据库def','ABC数据库DEF','Abc數據庫Def','ÀÉÎõü数据','ＡＢＣ数据ａｂｃ','İSTANBUL istanbul','ΣΊΣΥΦΟΣ σίσυφος','ǅungla ǈ ǋ','ⅫⅻⒶⓐ','x'*300+'数据']):
    for parser in ['ik','beng','ngram','space']:
        try:
            cur.execute('SELECT TOKENIZE(%s,%s)',(text,parser))
            results[f'casedown/{parser}/{i}']=sorted(json.loads(cur.fetchone()[0]),key=lambda x:json.dumps(x,sort_keys=True,ensure_ascii=False))
        except pymysql.MySQLError as error:
            results[f'casedown/{parser}/{i}']={'error':error.args[0]}
expr_cn="MATCH(title, content) AGAINST(%s)"
expr_en="MATCH(title_en, content_en) AGAINST(%s IN BOOLEAN MODE)"
for name,sql,params in [
    ('or',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE {expr_cn} OR id < 10',('全文索引 分词器',)),
    ('not',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE NOT ({expr_cn})',('全文索引 分词器',)),
    ('and-filter',f'SELECT id FROM fts_large_bench.docs WHERE {expr_cn} AND id %% 97 = 3 ORDER BY id',('数据库',)),
    ('bool-neg',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE {expr_en}',('+database -performance',)),
    ('bool-only-neg',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE {expr_en}',('-database',)),
    ('bool-limit',f'SELECT id FROM fts_large_bench.docs WHERE {expr_en} ORDER BY id LIMIT 50',('database audit',)),
    ('is-true',f'SELECT COUNT(*) FROM fts_large_bench.docs WHERE ({expr_cn}) IS TRUE',('分词器',)),
]:
    try:
        cur.execute(sql,params)
        results['boolfilter/'+name]=normalize(cur.fetchall())
    except pymysql.MySQLError as error:
        results['boolfilter/'+name]={'error':error.args[0]}
cur.execute('DROP DATABASE IF EXISTS sdb07_regress')
connection.close()
pathlib.Path(sys.argv[1]).write_text(json.dumps(results,ensure_ascii=False,sort_keys=True),encoding='utf-8')
print('Captured '+str(len(results))+' full token/ID/score cases')
