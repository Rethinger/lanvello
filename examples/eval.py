#!/usr/bin/env python3
"""eval battery for lanvello free models. one dir per model, json results."""
import json
import os
import subprocess
import sys
import time
import urllib.request

BASE = os.environ.get("LANVELLO_URL", "http://127.0.0.1:11447/v1")
KEY = open("/tmp/opencode/evals/key.txt").read().strip().splitlines()[0]
ROOT = "/tmp/opencode/evals"
TIMEOUT = 120

MODELS = [
    "opencode/muse-spark-1.3-contributor-free",
    "opencode/muse-spark-1.2-contributor-free",
]


def call(path, payload=None, stream=False):
    url = BASE + path
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
        method="POST" if data else "GET",
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            body = r.read()
            return r.status, dict(r.headers), body
    except Exception as e:
        return -1, {}, str(e).encode()


def sse_text(body):
    out = []
    for line in body.decode(errors="replace").splitlines():
        line = line.strip()
        if line.startswith("data:"):
            p = line[5:].strip()
            if p == "[DONE]":
                break
            try:
                ev = json.loads(p)
            except Exception:
                continue
            ch = ev.get("choices", [{}])[0]
            d = ch.get("delta", {}).get("content") or ""
            out.append(d)
    return "".join(out)


def run_case(model_dir, name, fn):
    t0 = time.time()
    try:
        ok, detail = fn()
    except Exception as e:
        ok, detail = False, "exc: %s" % e
    dt = round(time.time() - t0, 1)
    res = {"case": name, "ok": bool(ok), "secs": dt, "detail": detail}
    with open(os.path.join(model_dir, name + ".json"), "w") as f:
        json.dump(res, f, indent=1)
    print("%-28s %-6s %5ss %s" % (name, "PASS" if ok else "FAIL", dt, str(detail)[:100]))
    return ok


def main():
    only = sys.argv[1:] or MODELS
    summary = {}
    for model in MODELS:
        if model not in only and "/".join(model.split("/")[-1:]) not in [o.split("/")[-1] for o in only]:
            pass
        short = model.split("/")[-1]
        mdir = os.path.join(ROOT, short)
        os.makedirs(mdir, exist_ok=True)
        print("== %s ==" % model)
        results = {}

        def t_list():
            st, _, body = call("/models")
            if st != 200:
                return False, "http %s" % st
            ids = [m["id"] for m in json.loads(body)["data"]]
            return (model in ids), "models=%d" % len(ids)

        def t_chat():
            st, _, body = call("/chat/completions", {
                "model": model,
                "messages": [{"role": "user", "content": "reply with exactly: ok"}],
                "stream": False})
            if st != 200:
                return False, "http %s %s" % (st, body[:150])
            txt = json.loads(body)["choices"][0]["message"]["content"]
            return len(txt.strip()) > 0, txt.strip()[:80]

        def t_stream():
            st, _, body = call("/chat/completions", {
                "model": model,
                "messages": [{"role": "user", "content": "count to 3"}],
                "stream": True})
            if st != 200:
                return False, "http %s" % st
            txt = sse_text(body)
            return len(txt.strip()) > 0, txt.strip()[:80]

        def t_multiturn():
            p1 = {"model": model, "messages": [
                {"role": "user", "content": "remember the word: zebra"},
                {"role": "assistant", "content": "noted"},
                {"role": "user", "content": "what was the word? answer with the word only"}], "stream": False}
            st, _, body = call("/chat/completions", p1)
            if st != 200:
                return False, "http %s" % st
            txt = json.loads(body)["choices"][0]["message"]["content"]
            return ("zebra" in txt.lower()), txt.strip()[:80]

        def t_code():
            st, _, body = call("/chat/completions", {
                "model": model,
                "messages": [{"role": "user",
                              "content": "output ONLY python code, no markdown, no explanation. code must define fib(n) iteratively and print fib(10). nothing else."}],
                "stream": False, "max_tokens": 1000})
            if st != 200:
                return False, "http %s" % st
            txt = json.loads(body)["choices"][0]["message"]["content"]
            code = txt.replace("```python", "").replace("```", "").strip()
            with open(os.path.join(mdir, "fib.py"), "w") as f:
                f.write(code)
            r = subprocess.run(["python3", os.path.join(mdir, "fib.py")],
                               capture_output=True, text=True, timeout=15)
            return (r.stdout.strip() == "55"), "out=%r err=%r" % (r.stdout.strip()[:40], r.stderr.strip()[:60])

        def t_responses():
            st, _, body = call("/responses", {
                "model": short,
                "input": [{"type": "message", "role": "user",
                           "content": [{"type": "input_text", "text": "reply with exactly: ok"}]}],
                "stream": False})
            if st != 200:
                return False, "http %s %s" % (st, body[:150])
            return True, body[:120].decode(errors="replace")

        def t_badkey():
            url = BASE + "/models"
            req = urllib.request.Request(url, headers={"Authorization": "Bearer wrong"})
            try:
                urllib.request.urlopen(req, timeout=15)
                return False, "no 401"
            except Exception as e:
                return ("401" in str(e)), str(e)[:60]

        for name, fn in [("01-list", t_list), ("02-chat", t_chat),
                         ("03-stream", t_stream), ("04-multiturn", t_multiturn),
                         ("05-code-exec", t_code), ("06-responses", t_responses),
                         ("07-badkey", t_badkey)]:
            results[name] = run_case(mdir, name, fn)
        summary[short] = sum(1 for v in results.values() if v)
    print("summary:", summary)


main()
