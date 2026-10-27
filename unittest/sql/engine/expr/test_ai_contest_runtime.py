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

"""Cloud-only integration tests against an already running seekdb.

Requires PyMySQL. Starts only the mock HTTP service, never a database process.
Example: python3 test_ai_contest_runtime.py --host 127.0.0.1 --port 2881 --ai-port 8000
Focused RED/GREEN: add --case '*completion_malformed*' (repeatable).
Known baseline failures are tagged FINDING in test docstrings, not expectedFailure:
their failure must make the process exit nonzero until production is repaired.
"""

import argparse
import fnmatch
import hashlib
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pymysql


CONFIG = None
MOCK = None


def vector(text, dimension=4):
    """A deterministic service fixture; different inputs produce different vectors."""
    seed = int.from_bytes(hashlib.sha256(text.encode()).digest()[:3], "big")
    return [seed + i for i in range(dimension)]


def relevance(text):
    return hashlib.sha256(text.encode()).digest()[0] % 5


def completion_text(prompt):
    return "runtime accepted:" + hashlib.sha256(prompt.encode()).hexdigest()


def decode_json(value):
    if isinstance(value, bytes):
        value = value.decode()
    return json.loads(value)


class MockState:
    def __init__(self):
        self.lock = threading.Lock()
        self.reset(clear_errors=True)

    def reset(self, scenario="normal", clear_errors=False):
        with self.lock:
            self.scenario = scenario
            self.records = []
            if clear_errors:
                self.errors = []

    def snapshot(self):
        with self.lock:
            return list(self.records)

    def reply(self, path, body, headers):
        with self.lock:
            self.records.append({"path": path, "body": body, "headers": dict(headers)})
            scenario = self.scenario
        if headers.get_content_type() != "application/json":
            raise ValueError("expected application/json request")
        native = path in ("/api/generate", "/api/embed")
        if native and headers.get("Authorization"):
            raise ValueError("native Ollama must not send Authorization")
        if not native and headers.get("Authorization") != "Bearer runtime-mock-key":
            raise ValueError("OpenAI/rerank Authorization mismatch")
        if not isinstance(body.get("model"), str) or not body["model"]:
            raise ValueError("missing request model name")
        if scenario == "http_failure":
            return 503, {"error": "injected temporary failure"}
        if path in ("/v1/chat/completions", "/api/generate"):
            if native:
                if body.get("stream") is not False or not isinstance(body.get("prompt"), str):
                    raise ValueError("native generate requires prompt and stream:false")
            else:
                messages = body.get("messages")
                if not isinstance(messages, list) or not any(
                        m.get("role") == "user" and isinstance(m.get("content"), str)
                        for m in messages):
                    raise ValueError("expected OpenAI user message")
            prompt = body["prompt"] if native else next(
                m["content"] for m in messages if m["role"] == "user")
            content = {"complete_null": None, "complete_empty": "",
                       "complete_object": {"unexpected": True}, "complete_number": 17}.get(
                           scenario, completion_text(prompt))
            if scenario == "complete_missing":
                return 200, {"choices": [{"message": {}}]} if not native else {}
            return (200, {"response": content} if native else
                    {"choices": [{"message": {"role": "assistant", "content": content}}]})
        if path in ("/v1/embeddings", "/api/embed"):
            inputs = body.get("input")
            if isinstance(inputs, str):
                inputs = [inputs]
            if not isinstance(inputs, list) or not inputs or not all(
                    isinstance(text, str) for text in inputs):
                raise ValueError("embedding input must contain strings")
            dimension = body.get("dimensions", 4)
            if not isinstance(dimension, int) or dimension <= 0:
                raise ValueError("invalid requested dimension")
            embeddings = [vector(text, dimension) for text in inputs]
            if scenario == "embed_bad_dimension":
                embeddings[0].append(0)
            elif scenario == "embed_bad_element":
                embeddings[0][0] = "not numeric"
            elif scenario == "embed_empty_vector":
                embeddings[0] = []
            if native:
                return 200, {"embeddings": embeddings}
            data = [{"index": i, "embedding": item} for i, item in enumerate(embeddings)]
            if scenario == "embed_duplicate" and len(data) > 1:
                data[-1]["index"] = 0
            elif scenario == "embed_partial":
                data = data[:-1]
            elif scenario == "embed_out_of_range":
                data[0]["index"] = len(data)
            return 200, {"data": list(reversed(data))}
        if path == "/v1/rerank":
            docs = body.get("documents")
            if not isinstance(body.get("query"), str) or not isinstance(docs, list):
                raise ValueError("invalid rerank request")
            if len(docs) > 20 or not all(isinstance(text, str) for text in docs):
                raise ValueError("rerank batch must contain <=20 strings")
            # Equal scores deliberately arrive in reverse local-index order.
            indices = sorted(range(len(docs)), key=lambda i: (relevance(docs[i]), i), reverse=True)
            results = [{"index": i, "relevance_score": relevance(docs[i])} for i in indices]
            if scenario == "rerank_unscored":
                results = [{"index": i} for i in reversed(range(len(docs)))]
            elif scenario == "rerank_empty":
                results = []
            elif scenario == "rerank_partial":
                results = results[:-1]
            elif scenario == "rerank_duplicate" and len(results) > 1:
                results[-1]["index"] = results[0]["index"]
            elif scenario == "rerank_out_of_range":
                results[0]["index"] = len(docs)
            return 200, {"results": results}
        raise ValueError("unexpected endpoint: " + path)


class MockHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])).decode())
            status, payload = MOCK.reply(self.path, body, self.headers)
        except Exception as exc:
            with MOCK.lock:
                MOCK.errors.append(str(exc))
            status, payload = 400, {"error": str(exc)}
        data = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        pass


class AIContestRuntime(unittest.TestCase):
    @classmethod
    def sql(cls, statement, args=None):
        with cls.db.cursor() as cursor:
            cursor.execute(statement, args)
            return cursor.fetchall() if cursor.description else ()

    @classmethod
    def setUpClass(cls):
        cls.db = pymysql.connect(host=CONFIG.host, port=CONFIG.port, user=CONFIG.user,
                                 password="", database=CONFIG.database, charset="utf8mb4",
                                 autocommit=True, connect_timeout=15, read_timeout=90,
                                 write_timeout=30)
        cls.addClassCleanup(cls.db.close)
        cls.prefix = "rtai_" + format(os.getpid(), "x")
        cls.models = {}
        for key, kind, provider, model, path in (
                ("complete", "completion", "openai", "qwen3-32b", "/v1/chat/completions"),
                ("embed", "dense_embedding", "openai", "text-embedding-v1", "/v1/embeddings"),
                ("rerank", "rerank", "siliconflow", "BAAI/bge-reranker-v2-m3", "/v1/rerank"),
                ("ollama_c", "completion", "oLlAmA", "qwen2.5", "/api/generate"),
                ("ollama_e", "dense_embedding", "OLLAMA", "nomic-embed-text", "/api/embed")):
            name = cls.prefix + "_" + key
            endpoint = name + "_ep"
            cls.models[key] = name
            cls.sql("CALL DBMS_AI_SERVICE.CREATE_AI_MODEL(%s,%s)",
                    (name, json.dumps({"type": kind, "model_name": model})))
            cls.addClassCleanup(cls.sql, "CALL DBMS_AI_SERVICE.DROP_AI_MODEL(%s)", (name,))
            endpoint_config = {"ai_model_name": name, "scope": "all",
                               "url": "http://127.0.0.1:%d%s" % (CONFIG.ai_port, path),
                               "provider": provider}
            # Cover both omitted and explicitly empty native credentials.
            if key != "ollama_e":
                endpoint_config["access_key"] = ("" if provider.lower() == "ollama"
                                                 else "runtime-mock-key")
            cls.sql("CALL DBMS_AI_SERVICE.CREATE_AI_MODEL_ENDPOINT(%s,%s)",
                    (endpoint, json.dumps(endpoint_config)))
            cls.addClassCleanup(cls.sql, "CALL DBMS_AI_SERVICE.DROP_AI_MODEL_ENDPOINT(%s)",
                                (endpoint,))
        cls.table = cls.prefix + "_rows"
        cls.sql("CREATE TABLE `%s` (id INT PRIMARY KEY, content TEXT, tpl LONGTEXT)" % cls.table)
        cls.addClassCleanup(cls.sql, "DROP TABLE IF EXISTS `%s`" % cls.table)
        with cls.db.cursor() as cursor:
            cursor.executemany("INSERT INTO `%s` VALUES (%%s,%%s,NULL)" % cls.table,
                               [(i, None if i % 13 == 0 else "row-%03d-中文" % i)
                                for i in range(1, 97)])

    def setUp(self):
        MOCK.reset(clear_errors=True)

    def tearDown(self):
        self.assertEqual([], MOCK.errors, "engine sent an invalid protocol request")

    def scalar(self, statement, args=None):
        rows = self.sql(statement, args)
        self.assertEqual(1, len(rows))
        return rows[0][0]

    def requests(self, suffix=None):
        records = MOCK.snapshot()
        return records if suffix is None else [r for r in records if r["path"].endswith(suffix)]

    def expect_sql_error(self, statement, args=None, must_request=False):
        before = len(self.requests())
        with self.assertRaises(pymysql.err.DatabaseError) as raised:
            self.sql(statement, args)
        self.assertNotIn(raised.exception.args[0], (0, 2002, 2003, 2006, 2013, 2055),
                         "connection failure is not a valid SQL rejection")
        self.assertEqual([], MOCK.errors, "invalid protocol is not the intended SQL rejection")
        self.assertEqual(1, self.scalar("SELECT 1"), "database must survive invalid input")
        if must_request:
            self.assertGreater(len(self.requests()), before, "failure happened before model response")

    def check_complete(self, statement, args=None):
        result = decode_json(self.scalar(statement, args))
        request = self.requests()[-1]
        body = request["body"]
        prompt = body["prompt"] if request["path"] == "/api/generate" else next(
            item["content"] for item in body["messages"] if item["role"] == "user")
        self.assertEqual(completion_text(prompt), result, "provider content must be returned intact")
        return result

    def complete(self, prompt="hello", config=None, model="complete"):
        result = self.check_complete("SELECT AI_COMPLETE(%s,%s,%s) AS completion",
                                     (self.models[model], prompt, config))
        actual_prompt = (self.requests()[-1]["body"]["prompt"] if model == "ollama_c"
                         else self.user_prompt())
        self.assertEqual(prompt, actual_prompt)
        return result

    def embed(self, content, dimension=None, model="embed"):
        args = (self.models[model], content)
        statement = "SELECT AI_EMBED(%s,%s)"
        if dimension is not None:
            args += (dimension,)
            statement = "SELECT AI_EMBED(%s,%s,%s)"
        return decode_json(self.scalar(statement, args))

    def user_prompt(self):
        messages = self.requests("chat/completions")[-1]["body"]["messages"]
        return next(m["content"] for m in messages if m["role"] == "user")

    def test_completion_null_config_and_settings(self):
        self.assertTrue(self.complete())
        body = self.requests()[-1]["body"]
        self.assertNotIn("temperature", body)
        self.assertTrue(self.complete(config=json.dumps({"temperature": 0.2, "max_tokens": 16})))
        body = self.requests()[-1]["body"]
        self.assertEqual((0.2, 16), (body["temperature"], body["max_tokens"]))
        self.assertEqual("qwen3-32b", body["model"])

    def test_completion_json_config(self):
        self.check_complete("SELECT AI_COMPLETE(%s,'hello',JSON_OBJECT('temperature',0.2,'max_tokens',16))",
                    (self.models["complete"],))
        self.assertEqual(16, self.requests()[-1]["body"]["max_tokens"])

    def test_completion_malformed_rejected(self):
        """FINDING: baseline accepts null/object/number content as successful text."""
        for scenario in ("complete_null", "complete_empty", "complete_object",
                         "complete_number", "complete_missing"):
            with self.subTest(scenario=scenario):
                MOCK.reset(scenario)
                self.expect_sql_error("SELECT AI_COMPLETE(%s,'hello',NULL)",
                                      (self.models["complete"],), must_request=True)

    def test_prompt_json_arguments(self):
        result = decode_json(self.scalar("SELECT AI_PROMPT('{0}/{1}',JSON_ARRAY(1,TRUE),"
                                         "JSON_OBJECT('text','你好'))"))
        self.assertEqual("{0}/{1}", result["template"])
        self.assertEqual([[1, True], {"text": "你好"}], result["args"])
        self.check_complete("SELECT AI_COMPLETE(%s,AI_PROMPT('array={0}',JSON_ARRAY(1,TRUE)))",
                    (self.models["complete"],))
        self.assertEqual([1, True], json.loads(self.user_prompt().split("=", 1)[1]))

    def test_prompt_repeated_escape_unicode(self):
        template = "{0}/{0}|{{0}}|\\{1\\}|中文\n"
        self.check_complete("SELECT AI_COMPLETE(%s,AI_PROMPT(%s,%s))",
                    (self.models["complete"], template, "A"))
        self.assertEqual("A/A|{0}|{1}|中文\n", self.user_prompt())

    def test_prompt_nested_and_illegal_index(self):
        self.check_complete("SELECT AI_COMPLETE(%s,AI_PROMPT('outer[{0}]',"
                    "AI_PROMPT('inner[{0}]','你好')))", (self.models["complete"],))
        self.assertEqual("outer[inner[你好]]", self.user_prompt())
        MOCK.reset()
        self.expect_sql_error("SELECT AI_COMPLETE(%s,AI_PROMPT('{99}','only'))",
                              (self.models["complete"],))
        self.assertEqual([], self.requests())

    def test_prompt_longtext_template_and_text_argument(self):
        template = "模" * 24000 + "-{0}"
        argument = "参" * 7000
        self.sql("UPDATE `%s` SET tpl=%%s,content=%%s WHERE id=1" % self.table,
                 (template, argument))
        try:
            self.check_complete("SELECT AI_COMPLETE(%%s,AI_PROMPT(tpl,content)) AS completion FROM `%s` WHERE id=1" %
                     self.table, (self.models["complete"],))
            self.assertEqual(template.replace("{0}", argument), self.user_prompt())
        finally:
            self.sql("UPDATE `%s` SET tpl=NULL,content='row-001-中文' WHERE id=1" % self.table)

    def test_embed_single_default_explicit_and_null(self):
        self.assertEqual(vector("one"), self.embed("one"))
        self.assertEqual(vector("two", 7), self.embed("two", 7))
        self.assertEqual(vector("four", 4), self.embed("four", 4))
        self.assertEqual(4, self.requests()[-1]["body"]["dimensions"])
        before = len(self.requests())
        self.assertIsNone(self.scalar("SELECT AI_EMBED(%s,NULL)", (self.models["embed"],)))
        self.assertIsNone(self.scalar("SELECT AI_EMBED(NULL,'anything')"))
        self.assertEqual(before, len(self.requests()))

    def test_embed_invalid_dimensions(self):
        for dimension in (0, -1, 1.5, "3", None):
            with self.subTest(dimension=dimension):
                self.expect_sql_error("SELECT AI_EMBED(%s,'one',%s)",
                                      (self.models["embed"], dimension))
        self.assertEqual([], self.requests())

    def test_embed_bad_vectors_rejected(self):
        for scenario in ("embed_bad_dimension", "embed_bad_element", "embed_empty_vector",
                         "embed_partial", "embed_out_of_range", "http_failure"):
            with self.subTest(scenario=scenario):
                MOCK.reset(scenario)
                self.expect_sql_error("SELECT AI_EMBED(%s,'one',4)",
                                      (self.models["embed"],), must_request=True)

    def assert_batch(self, where="", limit="", dimension=None):
        expression = "AI_EMBED(%s,content" + (",5)" if dimension else ")")
        rows = self.sql("SELECT id,content," + expression + " FROM `%s` %s ORDER BY id %s" %
                        (self.table, where, limit), (self.models["embed"],))
        expected_ids = [i for i in range(1, 97) if not where or i % 2 == 0]
        if limit:
            expected_ids = expected_ids[:17]
        self.assertEqual(expected_ids, [r[0] for r in rows])
        for row_id, content, result in rows:
            if row_id % 13 == 0:
                self.assertIsNone(result)
            else:
                self.assertEqual(vector(content, 5 if dimension else 4), decode_json(result))
        calls = self.requests("embeddings")
        self.assertTrue(calls)
        self.assertTrue(any(len(r["body"]["input"]) > 1 for r in calls),
                        "multirow SQL must use a multi-input HTTP request")
        if not where and not limit:
            self.assertGreaterEqual(len(calls), 3, "65+ live rows must span multiple chunks")
        for request in calls:
            self.assertNotIn(None, request["body"]["input"])
        return rows

    def test_embed_batch_96_rows_reverse_indices_and_nulls(self):
        self.assert_batch()

    def test_embed_batch_filter_limit_explicit_dimensions(self):
        self.assert_batch(where="WHERE MOD(id,2)=0", limit="LIMIT 17", dimension=5)

    def test_embed_batch_failure_and_recovery(self):
        for scenario in ("embed_duplicate", "embed_partial", "http_failure"):
            with self.subTest(scenario=scenario):
                MOCK.reset(scenario)
                self.expect_sql_error("SELECT AI_EMBED(%%s,content) FROM `%s` WHERE id<10" %
                                      self.table, (self.models["embed"],), must_request=True)
        MOCK.reset()
        self.assert_batch()

    def test_ollama_native_and_provider_alter(self):
        for key in ("ollama_c", "ollama_e"):
            self.sql("CALL DBMS_AI_SERVICE.ALTER_AI_MODEL_ENDPOINT(%s,%s)",
                     (self.models[key] + "_ep", json.dumps({"provider": "OlLaMa"})))
            provider = self.scalar("SELECT provider FROM oceanbase.DBA_OB_AI_MODEL_ENDPOINTS "
                                   "WHERE endpoint_name=%s", (self.models[key] + "_ep",))
            self.assertEqual("ollama", provider.lower())
        for provider in ("openai", "siliconflow"):
            self.expect_sql_error("CALL DBMS_AI_SERVICE.ALTER_AI_MODEL_ENDPOINT(%s,%s)",
                (self.models["ollama_c"] + "_ep", json.dumps({"provider": provider})))
        self.assertTrue(self.complete(model="ollama_c"))
        self.assertEqual(vector("native"), self.embed("native", model="ollama_e"))
        self.assertEqual(["/api/generate", "/api/embed"], [r["path"] for r in self.requests()])
        self.assertIs(self.requests()[0]["body"]["stream"], False)

    def rerank(self, count, objects=False):
        texts = ["document-%03d-中文" % i for i in range(count)]
        docs = [{"id": "id-%03d" % i, "text": text, "keep": i} for i, text in enumerate(texts)]
        result = decode_json(self.scalar("SELECT AI_RERANK(%s,'query',%s,%s) AS ranked",
                    (self.models["rerank"], json.dumps(docs if objects else texts,
                     ensure_ascii=False), "text" if objects else None)))
        return texts, docs, result

    def test_rerank_global_indices_stable_ties_strings_and_objects(self):
        for count in (41, 141):
            for objects in (False, True):
                with self.subTest(count=count, objects=objects):
                    MOCK.reset()
                    texts, docs, result = self.rerank(count, objects)
                    order = sorted(range(count), key=lambda i: (-relevance(texts[i]), i))
                    self.assertEqual(count, len(result))
                    if objects:
                        self.assertEqual([docs[i]["id"] for i in order], [r["id"] for r in result])
                        for original, actual in zip([docs[i] for i in order], result):
                            self.assertEqual(original, {k: actual[k] for k in original})
                            self.assertEqual(relevance(original["text"]), actual["_model_score"])
                    else:
                        self.assertEqual(order, [r["index"] for r in result])
                    calls = self.requests("rerank")
                    self.assertEqual((count + 19) // 20, len(calls))
                    self.assertEqual(texts, [text for r in calls for text in r["body"]["documents"]])

    def test_rerank_incomplete_empty_partial_rejected(self):
        """FINDING: baseline silently accepts empty/partial full-ranking results."""
        for scenario in ("rerank_empty", "rerank_partial", "rerank_duplicate", "rerank_out_of_range"):
            for objects in (False, True):
                with self.subTest(scenario=scenario, objects=objects):
                    MOCK.reset(scenario)
                    docs = [{"id": i, "text": str(i)} for i in range(21)] if objects else [str(i) for i in range(21)]
                    self.expect_sql_error("SELECT AI_RERANK(%s,'query',%s,%s) AS ranked",
                        (self.models["rerank"], json.dumps(docs), "text" if objects else None),
                        must_request=True)

    def test_rerank_unscored_within_and_across_batches(self):
        """FINDING: baseline accepts <=20 unscored rows but fails merging 21."""
        for count in (20, 21):
            with self.subTest(count=count):
                MOCK.reset("rerank_unscored")
                _, _, result = self.rerank(count)
                expected = [i for start in range(0, count, 20)
                            for i in reversed(range(start, min(start + 20, count)))]
                self.assertEqual(expected, [r["index"] for r in result])


def main():
    global CONFIG, MOCK
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=2881)
    parser.add_argument("--user", default="root")
    parser.add_argument("--database", default="test")
    parser.add_argument("--ai-port", type=int, default=8000)
    parser.add_argument("--ai-bind", default="127.0.0.1")
    parser.add_argument("--case", action="append", default=[], help="test name substring or glob; repeatable")
    CONFIG = parser.parse_args()
    MOCK = MockState()
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(AIContestRuntime)
    if CONFIG.case:
        suite = unittest.TestSuite(test for test in suite if any(
            fnmatch.fnmatchcase(test.id(), pattern) or pattern in test.id()
            for pattern in CONFIG.case))
    if suite.countTestCases() == 0:
        parser.error("--case matched no tests")
    server = ThreadingHTTPServer((CONFIG.ai_bind, CONFIG.ai_port), MockHandler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    print("Running real SQL against %s:%d; mock %s:%d; cases=%d" %
          (CONFIG.host, CONFIG.port, CONFIG.ai_bind, CONFIG.ai_port, suite.countTestCases()), flush=True)
    try:
        result = unittest.TextTestRunner(verbosity=2).run(suite)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())

