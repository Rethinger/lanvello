# examples

## lanvello-ext.mjs

pi-family provider extension (omp, prime-agent) pointing at a local
lanvello gateway. key comes from `LANVELLO_API_KEY`:

```
export LANVELLO_API_KEY=$(./bin/lanvello key add --name x | head -n 1)
omp -p --model lanvello/fledge-alpha-free -e examples/lanvello-ext.mjs "..."
prime-agent -p --model lanvello/fledge-alpha-free -e examples/lanvello-ext.mjs --autonomous "..."
```

omp also accepts the same provider via `~/.omp/agent/models.yml`
(see docs in omp repo), prime-agent only via `-e`.

## eval.py

api battery against a running lanvello: one dir per model, json per
case (list, chat, stream, multiturn memory, code exec, native
responses, bad-key 401). needs the server key in `key.txt`:

```
./bin/lanvello serve --config lanvello-tor.json &
LANVELLO_URL=http://127.0.0.1:11448/v1 python3 examples/eval.py
```
