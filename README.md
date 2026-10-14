# lanvello

one binary, two ways at opencode's free models, egress always through tor lanes.

**`lanvello proxy` — the mode that works.** a local http proxy the real opencode
client is pointed at. opencode leases a free-tier session to the *running client*
and to nobody else: replaying the fingerprint (user-agent, `x-opencode-*`, tool
set) gets `403 FreeTierError`, and so does a session id copied from a live run,
even seconds later on the same machine. so the client keeps making its own calls
and the lanes move underneath it — a 429 on a spent exit parks that exit for the
reset window the far end named and replays the very same request on another one.

**`lanvello serve` — the openai-compatible api.** it serves free models again,
but not by replaying the client's fingerprint: the free tier leases its session
to the running opencode client, so `serve` asks that client to make the call and
translates the answer back into openai shapes (streamed chunks included).

```
./bin/lanvello serve --listen 127.0.0.1:11434 --lanes 5
inference: opencode client at http://127.0.0.1:49374 (the free-tier session lives there)
```

- `/v1/models` lists what the client itself can drive (its own list, current).
- `/v1/chat/completions` runs the caller's messages through a client session,
  created per request and dropped again, so the client's session list stays
  clean. `stream:false` aggregates; `stream:true` relays deltas as they arrive.
- one request per client session means requests are concurrent, and different
  models can be asked for in parallel (each session is pinned to its model).
- the caller's own system prompt, history and tool definitions cross the
  boundary through a spool: `serve` parks the request at
  `<dataDir>/bridge/<sessionId>.json` next to the session it created, and a
  plugin inside the client — `examples/lanvello-bridge.ts`, installed in
  `~/.config/opencode/plugins/` — rebuilds the model call from it, so the
  answer comes from the caller's prompt and the caller's tools, not the
  client's. without the plugin the bridge still answers, with the client's own
  agent and its own tools.
- tools are the api caller's to run. the plugin injects the caller's schemas
  into the client (replacing the client's own, so a tool-less api request
  cannot run anything in the client's directory), blocks the call the model
  makes, records it, and `serve` returns it as an openai `tool_calls` answer
  with `finish_reason: "tool_calls"` — including when the caller asked for
  `stream:true`. the caller appends the result as a `role: "tool"` message and
  the next request carries it back into the client.
- `--client` makes a missing client fatal; `--no-client` keeps the old replay
  path, which the free tier answers with `403 FreeTierError` (see the notes at
  the bottom). without a client, `serve` says so on startup instead of serving
  errors.

## install

prebuilt binaries for linux / macos / windows (amd64, arm64, armv7, 386) —
every `v*` tag builds them in ci and attaches them to
[releases](https://github.com/Rethinger/lanvello/releases), no toolchain
needed:

```
tar -xzf lanvello_0.1.0_linux_amd64.tar.gz   # .zip on windows
cd lanvello_0.1.0_linux_amd64 && ./lanvello version
```

each archive also carries the README; `SHA256SUMS.txt` in the release
verifies the download. macOS may refuse a freshly downloaded binary —
unblock it once with `xattr -dr com.apple.quarantine lanvello`.

or build from source (pure go, no cgo):

```
go build -o bin/lanvello ./cmd/lanvello
./bin/lanvello serve --listen 127.0.0.1:11434 --lanes 5
./bin/lanvello key add --name aider   # prints sk-lanv-... once + baseURL hint
./bin/lanvello lanes status
./bin/lanvello tui
cp examples/lanvello-bridge.ts ~/.config/opencode/plugins/   # serve: caller's prompt + tools
```

any harness that accepts an openai-compatible provider then uses:

```
baseURL: http://127.0.0.1:11434/v1
apiKey:  sk-lanv-...
model:   opencode/muse-spark-1.3-contributor-free
```

## proxy

```
./bin/lanvello proxy --listen 127.0.0.1:8888 --lanes 5
./bin/lanvello proxy --print-ca      # the CA path, for NODE_EXTRA_CA_CERTS
```

point the client at it (the proxy prints both lines on start):

```
HTTPS_PROXY=http://[IP_ADDRESS]:8888 \
NODE_EXTRA_CA_CERTS=~/.local/state/lanvello/ca.pem \
opencode
```

the long-running client daemon — `opencode serve --service`, the process
`lanvello serve` drives — needs the same variables where *it* is started,
plus loopback kept out of the proxy:

```
NO_PROXY=127.0.0.1,localhost,::1 \
HTTPS_PROXY=http://[IP_ADDRESS]:8888 \
NODE_EXTRA_CA_CERTS=~/.local/state/lanvello/ca.pem \
opencode serve --service
```

a daemon launched without the proxy answers `403 FreeTierError` to every
session it runs, and that is the error `serve` hands back. a daemon launched
*with* the proxy but without `NO_PROXY` looks like something else again: the
client calls its own api on `127.0.0.1`, those calls go out through a lane
too, land on the exit's own loopback, and the session dies before it starts —
the proxy log shows `tls handshake opencode.ai: EOF` and the client reports
`getaddrinfo ETIMEOUT`.

what happens on the wire:

```
opencode --CONNECT opencode.ai--> lanvello proxy
                                    |  tls terminated with a ca minted in the data dir
                                    |  request replayed per lane
                                    +--> lane 2 {de} --socks--> opencode.ai  200
                                    +--> 429 retry-after 8760 -> lane 2 parked 146m, replay lane 3 {se}  200
```

- `--print-ca` prints `ca.pem`; the client only ever sees a leaf minted for the
  host it asked for, and the CA never leaves the data dir.
- 429, 502, 503 and 504 park the exit for as long as the far end's
  `retry-after` says (up to 6h) and replay the same request on the next lane
  *before* a byte reaches the client: a spent exit costs a retry, not a failure.
  `--quiet` drops the per-request lane log.
- `socks: ["127.0.0.1:52001", ...]` in the config reuses lanes someone else
  spawned (a running `lanvello serve`, say); without it the proxy spawns its own
  tor, exactly like serve.
- loopback targets bypass the lanes: the client's own local server is not
  the internet, and tor would refuse it anyway.

## endpoints

- `GET /v1/models` — the live free catalog, fetched upstream through a lane
  and cached for a minute; only ids the free tier actually serves are
  advertised (`union-alpha` and `jev-1.13-free` are not among them).
  each entry carries capability metadata — `reasoning`,
  `reasoning_options` (selectable effort levels), `limit`
  (context/output), `tool_call`, `attachment`, `cost`, `input`/`output`
  modalities, plus `context_length`/`max_output_tokens` aliases — merged
  in from opencode's catalog mirror (`models.opencode.ai`), which is
  fetched through a lane, cached on disk for 6h and refreshed in the
  background. the embedded table for the known free models is the
  offline fallback, so a dead mirror never breaks the list
- `POST /v1/chat/completions` — openai chat. responses-backed models
  (muse-spark) are translated chat -> responses upstream and back to
  openai sse (or aggregated when `stream:false`); the rest pass through
  to `/zen/v1/chat/completions`
- `POST /v1/responses` — native responses passthrough to
  `/zen/v1/responses` (fingerprint injected)
- `POST /v1/messages` — anthropic-native. body, tools, tool_choice and
  `thinking.budget_tokens` are translated to openai chat upstream and the
  answer comes back as an anthropic message or as the full anthropic sse
  sequence (`message_start` … `message_stop`). unknown model names
  (`claude-*`) land on the default free model instead of failing.
- `GET /healthz`

unknown model ids are substituted with the default free model rather than
passed upstream, so a harness configured for another provider still works.

## streaming and usage

streams are relayed event by event with a flush per event — the first token
reaches the client while the model is still generating (no buffering of the
whole answer). `stream:false` aggregates the same stream into one completion
and keeps `id`, `created` and the full `usage` object, including
`completion_tokens_details.reasoning_tokens`.

effort is passed through in the spelling each endpoint accepts:
`reasoning_effort` on chat/messages, `reasoning{effort,summary}` on
responses. `thinking.budget_tokens` from anthropic bodies is folded into
`reasoning_effort`. effort is observable in the returned usage: the same
prompt on `muse-spark-1.3` costs ~40 reasoning tokens at `minimal` and
~600 at `high`. reasoning text is only visible where upstream sends it in
the clear (`reasoning_content` on chat models); the responses path ships
reasoning encrypted, so there only the token counts show up.

auth: bearer `sk-lanv-...` from `key add`. fresh data dir with zero keys
runs open (single-user localhost); the moment one key exists, all `/v1/*`
require it. on a public endpoint set `requireKey` in the config (or
`LANVELLO_REQUIRE_KEY=1`) so an empty data dir refuses instead of serving
open. keys.json is re-read when it changes, so `key add` / `key revoke`
take effect without a restart. one stable `x-opencode-session` is derived
per api key: minting a fresh session per request burns free quota into 429s.

## limits

429 retires the lane exit until `retry-after`; with tor lanes the process is
respawned in the background for a fresh exit and the lane returns once it is
up, while the request is retried on another lane. 502/503/504 retry on
another lane. per-request proof goes to `$dataDir/proof.jsonl`.

a lane stays busy for the whole answer, not just while the request is in
flight, so N lanes means N concurrent answers; the rest wait in the queue up
to `waitBudgetS` (default 90) and `/v1/models` up to `catalogBudgetS`
(default 20). timeouts are split: 3 minutes for response headers, 10 minutes
without any bytes in flight — a model that thinks for a long time is not cut,
a dead socket is not waited on forever. if the client hangs up mid-stream the
upstream request is closed instead of running to completion for free.

## tor lanes

first run with tor enabled downloads the linux expert bundle (~30mb,
once) into `$dataDir/tools` when no system tor exists — same idea as
lingling, no root needed. then one tor process per lane, pinned with
`ExitNodes {cc}`. there is no direct mode: without at least one healthy
tor lane the server refuses to start. an explicit socks list in config
(`socks: ["127.0.0.1:9050"]`) can replace spawned processes.

`countries.txt` in the data dir overrides pools like lingling:
line 1 primary, line 2 fallback, two-letter codes, `#` comments.

per-model exits: `modelLanes: {"fledge-alpha-free": "de"}` in config pins a
model family to an exit country; requests for other models use the
least-loaded lane. 429 handling is per exit ip with `retry-after` respected,
exactly the free-tier bypass. note that a lane held by a long answer cannot
answer a catalog call until it frees up, hence `catalogBudgetS`.

every byte leaves through a lane, including the model catalog and the
capability mirror: there is no code path in `internal/upstream` that
dials the free tier directly.

## harness integration

`examples/lanvello-ext.mjs` registers the provider for pi-family
harnesses (omp, prime-agent) and fetches the model list from the
gateway at load — an async factory, so new free models appear without
editing anything:

```
LANVELLO_API_KEY=<key> prime-agent -e examples/lanvello-ext.mjs
```

omp can also discover the list on its own via models.yml:

```yaml
providers:
  lanvello:
    baseUrl: http://127.0.0.1:11448/v1
    apiKey: sk-lanv-...        # literal value: omp does not expand ${ENV} here
    api: openai-completions
    discovery:
      type: openai-models-list
    modelOverrides:
      opencode/fledge-alpha-free:
        reasoning: true
        thinking:
          mode: effort
          efforts: [low, high, max]
          defaultLevel: low
        compat:
          supportsReasoningEffort: true
```

omp resolves reasoning flags from its own bundled catalog, which does
not know the free models, so effort levels are declared per model id
in `modelOverrides` (the list itself stays dynamic via `discovery`).
`compat.supportsReasoningEffort: true` is required: without it omp
treats an unrecognized model id as effort-incapable and silently
drops `reasoning_effort` from the request.

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

## reverse-engineering notes (2026-10-13)

what the official client does today, read off the running binary and the live
wire — the next person to touch the free tier needs this:

- the provider moved: `https://models.opencode.ai/api.json` now serves
  `"api": "https://opencode.ai/inference/v1"` for provider `opencode`. `/zen/v1`
  still answers but it is the older path; the client uses `/inference/v1`.
- request shape: user agent `opencode/<channel>/<version>/<client>` (today
  `opencode/dev/0.0.0-dev-20455/cli`), `authorization: Bearer public`,
  `x-opencode-client`, `x-opencode-project`, `x-opencode-session` (the root
  session id), `x-opencode-session-id`, `x-session-affinity`, `x-session-id`,
  `b3` and `traceparent`.
- session ids are minted locally: `ses_` + `hex48(~(Date.now()*4096+counter))`
  + 14 base62 characters from random bytes. there is no signature in them; the
  timestamp decodes back to the moment the session was made (counter 1).
- the model transport is a websocket (`@opencode/SessionModelTransport`,
  55 minute rotation, 15s connect budget, plain http after 5 stream failures);
  the free models are served over plain http today.
- the gate is a server-side lease on the session, not a fingerprint: bytes
  replayed from another process — same headers in the same order and case, same
  body, the client's own TLS stack, same machine — answer `403 FreeTierError`,
  while the same bytes carrying the client's own session answer `200`. no
  header, tool set, body, protocol, exit or address moves that line, and the
  lease does not survive the client's request (a session id taken off the wire
  is refused seconds later). this is why `proxy` exists and why `serve` cannot
  come back.
- the free list moved with it: `fledge-alpha-free` answers RegionError,
  `mimo-v2.5-free` and `exo-free` are deprecated (410); current free ids include
  `mimo-v2.6-flash-free`, `step-5-preview-free`, `cat-jam-free`,
  `muse-spark-1.3-contributor-free`, `nemotron-3-ultra-free`,
  `nemotron-3.5-lightning-free`, `space-bunny-free`, `ling-3.0/3.1-flash`.
                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                