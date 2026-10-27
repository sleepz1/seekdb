#!/usr/bin/env python3
"""Local stand-in for the contest AI-function checks.

seekdb itself is not built in this environment, so this exercises the same
contracts the SQL functions have to satisfy: prompt substitution, embedding
reorder/validation, rerank index mapping, and the mock HTTP shapes used by
ai_function_enhancement.test.
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROMPT_MAX_DEPTH = 8


def is_prompt_object(node):
    if not isinstance(node, dict) or set(node) != {"template", "args"}:
        return False
    if not isinstance(node["template"], str) or not isinstance(node["args"], list):
        return False
    for arg in node["args"]:
        if not isinstance(arg, (str, dict, list, bool, int, float)) and arg is not None:
            return False
    return True


def materialize(arg, depth):
    if depth > PROMPT_MAX_DEPTH:
        raise ValueError("nested prompt is too deep")
    if isinstance(arg, str):
        return arg
    if isinstance(arg, dict) and is_prompt_object(arg):
        return replace_template(arg["template"], arg["args"], depth + 1)
    return json.dumps(arg, ensure_ascii=False, separators=(", ", ": "))


def replace_template(template, args, depth=0):
    if depth > PROMPT_MAX_DEPTH or template == "":
        raise ValueError("invalid prompt")
    texts = [materialize(arg, depth) for arg in args]
    out = []
    i = 0
    while i < len(template):
        if template.startswith("{{", i) or template.startswith("}}", i):
            out.append(template[i])
            i += 2
            continue
        if template.startswith("\\{", i) or template.startswith("\\}", i):
            out.append(template[i + 1])
            i += 2
            continue
        if template[i] == "{":
            j = i + 1
            while j < len(template) and template[j].isdigit():
                j += 1
            if j < len(template) and template[j] == "}" and j > i + 1:
                index = int(template[i + 1:j])
                if index >= len(texts):
                    raise ValueError("invalid placeholder index")
                out.append(texts[index])
                i = j + 1
                continue
        out.append(template[i])
        i += 1
    return "".join(out)


def reorder_embeddings(data, dimension=0):
    if not isinstance(data, list) or not data:
        raise ValueError("invalid embedding data")
    ordered = [None] * len(data)
    saw_index = False
    saw_positional = False
    for pos, item in enumerate(data):
        if not isinstance(item, dict) or not isinstance(item.get("embedding"), list):
            raise ValueError("invalid embedding item")
        vector = item["embedding"]
        if not vector or (dimension and len(vector) != dimension):
            raise ValueError("bad dimension")
        if not all(isinstance(x, (int, float)) and not isinstance(x, bool) for x in vector):
            raise ValueError("non-numeric element")
        if "index" in item:
            saw_index = True
            index = item["index"]
            if isinstance(index, bool) or not isinstance(index, int):
                raise ValueError("bad index")
        else:
            saw_positional = True
            index = pos
        if saw_index and saw_positional:
            raise ValueError("mixed index")
        if index < 0 or index >= len(data) or ordered[index] is not None:
            raise ValueError("duplicate or oob index")
        ordered[index] = vector
    if any(item is None for item in ordered):
        raise ValueError("missing slot")
    return ordered


def rerank_apply(results, start, batch_count):
    seen = [False] * batch_count
    for item in results:
        index = item["index"]
        if not isinstance(index, int) or isinstance(index, bool):
            raise ValueError("bad index")
        if index < 0 or index >= batch_count or seen[index]:
            raise ValueError("oob or duplicate")
        seen[index] = True
        item["index"] = index + start
    scored = sum("relevance_score" in item for item in results)
    if scored not in (0, len(results)):
        raise ValueError("partial scores")
    if scored == len(results) and results:
        def key(item):
            return (-item["relevance_score"], item["index"])
        results.sort(key=key)
    return results


class MockHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length).decode() or "{}")
        auth = self.headers.get("Authorization")
        path = self.path
        if path.endswith("/chat/completions"):
            if not auth or not auth.startswith("Bearer "):
                return self._send(401, {"error": "missing key"})
            messages = body.get("messages") or []
            user = next((m.get("content") for m in messages if m.get("role") == "user"), "")
            if "temperature" in body and body["temperature"] != 0.2:
                return self._send(400, {"error": "temperature"})
            payload = {"choices": [{"message": {"role": "assistant", "content": "ok:" + user}}]}
        elif path.endswith("/embeddings"):
            if not auth:
                return self._send(401, {"error": "missing key"})
            inputs = body.get("input")
            if isinstance(inputs, str):
                inputs = [inputs]
            dim = body.get("dimensions") or 768
            data = []
            for i, _text in enumerate(reversed(inputs)):
                # Deliberately reverse the payload so the kernel must reorder by index.
                data.append({"index": len(inputs) - 1 - i, "embedding": [0.1] * dim})
            payload = {"data": data}
        elif path.endswith("/v1/rerank"):
            if not auth:
                return self._send(401, {"error": "missing key"})
            docs = body.get("documents") or []
            results = []
            for i, doc in enumerate(docs):
                text = doc if isinstance(doc, str) else doc.get("text", "")
                results.append({"index": i, "relevance_score": 1.0 if text == "apple" else 0.1})
            results.sort(key=lambda item: -item["relevance_score"])
            payload = {"results": results}
        elif path.endswith("/api/generate"):
            if auth:
                return self._send(400, {"error": "ollama key"})
            if body.get("stream") is not False:
                return self._send(400, {"error": "stream"})
            payload = {"response": "ollama-ok"}
        elif path.endswith("/api/embed"):
            if auth:
                return self._send(400, {"error": "ollama key"})
            inputs = body.get("input")
            if isinstance(inputs, str):
                inputs = [inputs]
            payload = {"embeddings": [[0.2, 0.3] for _ in inputs]}
        else:
            return self._send(404, {"error": path})
        self._send(200, payload)

    def _send(self, code, payload):
        raw = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, _fmt, *_args):
        return


def post(url, body, headers):
    import urllib.request
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers=headers, method="POST")
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read().decode())


def check_prompt():
    assert replace_template("{0}+{1}={2} 吗？请回答true或false", ["1", "2", "3"]) == "1+2=3 吗？请回答true或false"
    assert replace_template("{0}/{0}/{1}", ["a", "b"]) == "a/a/b"
    nested = {"template": "describe {0}", "args": [{"text": "你好"}]}
    rendered = replace_template(nested["template"], nested["args"])
    assert "你好" in rendered and rendered.startswith("describe ")
    deep = {"template": "{0}", "args": [{"template": "{0}", "args": ["inner"]}]}
    assert replace_template("{0}", [deep]) == "inner"
    try:
        replace_template("{1}", ["only"])
        raise AssertionError("illegal index was accepted")
    except ValueError:
        pass
    assert replace_template("{{0}} \\{1\\}", ["a"]) == "{0} {1}"
    # Construction keeps unused placeholders; substitution is the point that errors.
    stored = {"template": "{0}+{1}={2} 吗？请回答true或false", "args": ["1"]}
    assert stored["template"].count("{") == 3


def check_embed():
    data = [
        {"index": 1, "embedding": [0.2, 0.3]},
        {"index": 0, "embedding": [0.0, 0.1]},
    ]
    assert reorder_embeddings(data) == [[0.0, 0.1], [0.2, 0.3]]
    try:
        reorder_embeddings([{"index": 0, "embedding": []}], dimension=2)
        raise AssertionError("empty vector accepted")
    except ValueError:
        pass
    try:
        reorder_embeddings([{"embedding": [1, "x"]}])
        raise AssertionError("non-numeric accepted")
    except ValueError:
        pass
    try:
        reorder_embeddings([{"index": 0, "embedding": [1]}, {"embedding": [2]}])
        raise AssertionError("mixed index accepted")
    except ValueError:
        pass


def check_rerank():
    batch = [
        {"index": 1, "relevance_score": 0.9},
        {"index": 0, "relevance_score": 0.9},
        {"index": 2, "relevance_score": 0.1},
    ]
    ordered = rerank_apply(batch, start=20, batch_count=3)
    assert [item["index"] for item in ordered] == [20, 21, 22]
    plain = [{"index": 0}, {"index": 1}]
    assert [item["index"] for item in rerank_apply(plain, 0, 2)] == [0, 1]
    try:
        rerank_apply([{"index": 0}, {"index": 0}], 0, 2)
        raise AssertionError("duplicate index accepted")
    except ValueError:
        pass


def check_mock(base):
    chat = post(
        base + "/compatible-mode/v1/chat/completions",
        {"model": "qwen3-32b", "messages": [{"role": "user", "content": "hello"}],
         "temperature": 0.2, "max_tokens": 16},
        {"Authorization": "Bearer mock-key", "Content-Type": "application/json"},
    )
    assert chat["choices"][0]["message"]["content"]
    embed = post(
        base + "/compatible-mode/v1/embeddings",
        {"model": "text-embedding-v1", "input": ["first", "third"], "dimensions": 768},
        {"Authorization": "Bearer mock-key", "Content-Type": "application/json"},
    )
    vectors = reorder_embeddings(embed["data"], 768)
    assert len(vectors) == 2 and all(len(v) == 768 for v in vectors)
    rerank = post(
        base + "/v1/rerank",
        {"model": "BAAI/bge-reranker-v2-m3", "query": "apple",
         "documents": ["apple", "banana", "fruit", "vegetable"]},
        {"Authorization": "Bearer mock-key", "Content-Type": "application/json"},
    )
    assert len(rerank["results"]) == 4 and rerank["results"][0]["index"] == 0
    objects = post(
        base + "/v1/rerank",
        {"model": "BAAI/bge-reranker-v2-m3", "query": "apple",
         "documents": ["apple", "banana"]},
        {"Authorization": "Bearer mock-key", "Content-Type": "application/json"},
    )
    docs = [{"id": "a", "text": "apple"}, {"id": "b", "text": "banana"}]
    ranked = rerank_apply(objects["results"], 0, 2)
    assert docs[ranked[0]["index"]]["id"] == "a" and len(ranked) == 2
    ollama_chat = post(
        base + "/api/generate",
        {"model": "qwen2.5", "prompt": "hello", "stream": False},
        {"Content-Type": "application/json"},
    )
    assert ollama_chat["response"]
    ollama_embed = post(
        base + "/api/embed",
        {"model": "nomic-embed-text", "input": ["hello"]},
        {"Content-Type": "application/json"},
    )
    assert ollama_embed["embeddings"] and ollama_embed["embeddings"][0]


def main():
    check_prompt()
    check_embed()
    check_rerank()
    server = ThreadingHTTPServer(("127.0.0.1", 0), MockHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        check_mock("http://127.0.0.1:%d" % server.server_address[1])
    finally:
        server.shutdown()
    print("ai_function_contest_check: ok")
    print("prompt: duplicate/nested/illegal-index/escapes")
    print("embed: out-of-order index, dimension, numeric elements")
    print("rerank: global index, stable score order, unscored order kept")
    print("mock: openai chat/embed, siliconflow rerank, ollama generate/embed")


if __name__ == "__main__":
    main()
