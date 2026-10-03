# lanvello

one local port serving opencode free models over an openai-compatible api.
no login, no api keys from you: lanvello replays the official client
fingerprint (`opencode/*` user-agent, `x-opencode-*` session headers,
forced streaming, bash/glob/grep/read tool quartet) against
`https://opencode.ai/zen/v1` with `Bearer public`, exactly like the cli
does for its free tier. tor lanes rotate exits when an ip is throttled.

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
model:   opencode/muse-spark-1.3-contributor-free
```

## endpoints

- `GET /v1/models` — free ids (`muse-spark-*-contributor-free`,
  `union-alpha`, `jev-1.13-free`, `longcat/space-bunny/fledge/mimo/ling/nemotron`
  free builds)
- `POST /v1/chat/completions` — openai chat. responses-backed models
  (muse-spark) are translated chat -> responses upstream and back to
  openai sse (or aggregated when `stream:false`); the rest pass through
  to `/zen/v1/chat/completions`
- `POST /v1/responses` — native responses passthrough to
  `/zen/v1/responses` (fingerprint injected)
- `POST /v1/messages` — anthropic-native passthrough (union-alpha)
- `GET /healthz`

auth: bearer `sk-lanv-...` from `key add`. fresh data dir with zero keys
runs open (single-user localhost); the moment one key exists, all `/v1/*`
require it. one stable `x-opencode-session` is derived per api key:
minting a fresh session per request burns free quota into 429s.

## limits

429 retires the lane exit until `retry-after` and rotates country, request is
retried on another lane. 502/503/504 retry on another lane. per-request
proof goes to `$dataDir/proof.jsonl`.

## tor lanes

- explicit socks list in config `socks: ["127.0.0.1:9050"]`, or
- local tor processes per lane (`tor` binary + generated torrc with
  `ExitNodes {cc}`), or
- `--no-tor` / `LANVELLO_NO_TOR=1` for direct egress.

`countries.txt` in the data dir overrides pools like lingling:
line 1 primary, line 2 fallback, two-letter codes, `#` comments.

## deploy

single static binary, one dep (`golang.org/x/net` for socks5), state in
one dir (`LANVELLO_DATA_DIR`):

```
docker build -t lanvello .
docker run --rm -p 127.0.0.1:11434:11434 \
  -v lanvello-state:/state -e LANVELLO_DATA_DIR=/state lanvello
```

see `deploy/lanvello.service` for systemd.
override target with `LANVELLO_BASE_URL` if zen ever moves.
