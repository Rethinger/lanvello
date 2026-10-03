# lanvello

lightweight universal gateway: one localhost port speaks openai-compatible api
and routes models to configured upstreams (opencode gateway, ikhdev, any
openai-compatible base), rotating tor egress lanes like lingling when tor is
present. no opencode cli in the hot path: at runtime it only reuses the login
token, never spawns the editor cli.

```
go build -o bin/lanvello ./cmd/lanvello
./bin/lanvello serve --listen 127.0.0.1:11434 --lanes 5
./bin/lanvello key add --name aider   # prints sk-lanv-... once + baseURL hint
./bin/lanvello lanes status
./bin/lanvello tui
```

any harness that accepts an openai-compatible provider then uses:

```
baseURL: http://127.0.0.1:11434/v1
apiKey:  sk-lanv-...
model:   ikhdev/free-mimo-v2.6-cline   # or opencode/<id> via gateway token
```

## endpoints

- `GET /v1/models` — static catalog of known free ids + `lanvello/auto`
- `POST /v1/chat/completions` — openai chat, sse passthrough
- `POST /v1/responses` — same handler (openai responses-style bodies pass through)
- `POST /v1/messages` — minimal anthropic -> openai mapping, then same path
- `GET /healthz`

auth: bearer `sk-lanv-...` from `key add`. fresh data dir with zero keys
runs open (single-user localhost); the moment one key exists, all `/v1/*`
require it.

## upstreams

model id prefix decides the upstream:

- `ikhdev/...` with `LANVELLO_UPSTREAMS_JSON` entry
  `{"modelPrefix":"ikhdev/","baseURL":"https://api.ikhdev.xyz/v1","token":"..."}`
  forwards to `<baseURL>/chat/completions` with the prefix stripped, plain
  openai headers.
- anything else goes to the opencode-style gateway
  `<gatewayURL>/ai/v1/proxy/openai/v1/chat/completions` with
  `User-Agent: opencode/<version>` spoof headers and `LANVELLO_GATEWAY_TOKEN`
  (or token auto-read from `~/.local/share/opencode/opencode.db` account
  created by `opencode auth login`, via the sqlite3 cli, no cgo).

429 retires the lane exit until `retry-after` and rotates country, request is
retried on another lane. 502/503/504 retry on another lane. 500 relays as-is.
per-request proof goes to `$dataDir/proof.jsonl`.

## tor lanes

- explicit socks list in config `socks: ["127.0.0.1:9050"]`, or
- local tor processes per lane (`tor` binary + generated torrc with
  `ExitNodes {cc}`), or
- `--no-tor` / `LANVELLO_NO_TOR=1` for direct egress.

`countries.txt` in the data dir overrides pools like lingling:
line 1 primary, line 2 fallback, two-letter codes, `#` comments.

## deploy

single static binary, state in one dir (`LANVELLO_DATA_DIR`):

```
docker build -t lanvello .
docker run --rm -p 127.0.0.1:11434:11434 \
  -e LANVELLO_GATEWAY_TOKEN=... -e LANVELLO_UPSTREAMS_JSON='[...]' \
  -v lanvello-state:/state -e LANVELLO_DATA_DIR=/state lanvello
```

see `deploy/lanvello.service` for systemd.
