# lanvello

A local gateway for driving the free models your running opencode client has
access to, from anything that speaks the openai api — plus the client-side
plugin that carries an api caller's own prompt, history and tool schemas into
that client, and a local proxy that keeps the client's own traffic moving when
the far end limits it.

Two modes, one binary:

- **`lanvello serve`** — an openai-compatible api on localhost. it opens a
  session inside the running opencode client (that client is what holds the
  free-tier lease), prompts it, and translates the answer back into openai or
  anthropic shapes, streaming included.
- **`lanvello proxy`** — a local http proxy the real client is pointed at. a
  spent exit costs a retry instead of a failed request: the request is replayed
  on another lane before a byte reaches the client.

## status: read this first

**Not authorized.** Nothing here is an api key, a whitelist entry, or an
agreement with opencode or opencode.ai. The project reads the running client and
reuses the access that client already has, and it moves requests to a different
egress when the far end limits one. If you would rather not depend on that — or
do not consider it acceptable — do not run it. That call is the operator's, and
the operator is you.

**The free tier is a lease on the running client.** `serve` exists because the
gate refuses bytes replayed from any other process — same headers, same body,
same machine — with `403 FreeTierError`, and refuses a session id taken off the
wire seconds earlier. What was measured is in the notes at the bottom.

**Refusals are a normal failure mode, not a bug.** The far end hands `403
FreeTierError` and `429` to the egress it distrusts, and that includes most tor
exits: some requests come back with an empty answer. Rotation turns that into a
retry, and it needs a pool of exits to rotate to (more lanes, or your own
`socks:` list).

**What is on the wire.** No fingerprint is forged: the proxy forwards your
client's own requests (only hop-by-hop headers are stripped). TLS is terminated
locally — a CA in your data dir mints a leaf for the host the client asked for —
so the far end's status codes are readable instead of being an opaque stream.
That CA never leaves the machine, and nothing here calls home.

## install

Prebuilt binaries for linux / macos / windows (amd64, arm64, armv7, 386) —
every `v*` tag builds them in ci and attaches them to
[releases](https://github.com/Rethinger/lanvello/releases), no toolchain
needed:

```
tar -xzf lanvello_0.1.0_linux_amd64.tar.gz   # .zip on windows
cd lanvello_0.1.0_linux_amd64 && ./lanvello version
```

Each archive also carries this README; `SHA256SUMS.txt` in the release verifies
the download. macOS may refuse a freshly downloaded binary — unblock it once
with `xattr -dr com.apple.quarantine lanvello`.

Or build from source (pure go, no cgo):

```
go build -o bin/lanvello ./cmd/lanvello
cp examples/lanvello-bridge.ts ~/.config/opencode/plugins/   # serve: the caller's prompt and tools
```

## serve: the openai-compatible api

```
./bin/lanvello serve --listen 127.0.0.1:11434 --lanes 5
inference: opencode client at http://127.0.0.1:49374 (the free-tier session lives there)
```

Any harness that accepts an openai-compatible provider then uses:

```
baseURL: http://127.0.0.1:11434/v1
apiKey:  sk-lanv-...      # ./bin/lanvello key add --name aider
model:   opencode/mimo-v2.6-flash-free
```

- `/v1/models` lists what the client itself can drive (its own list, current).
- `/v1/chat/completions` runs the caller's messages through a client session,
  created per request and dropped again, so the client's session list stays
  clean. `stream:false` aggregates; `stream:true` relays deltas as they arrive.
- One request per client session means requests are concurrent, and different
  models can be asked for in parallel (each session is pinned to its model).
- `--client` makes a missing client fatal; `--no-client` keeps the replay path,
  which the free tier answers with `403 FreeTierError` (see the notes). Without
  a client, `serve` says so on startup instead of serving errors.

### what crosses the boundary

The client's own http api takes nothing but text, so the caller's prompt,
history and tools travel through a spool instead:

- `serve` writes `<dataDir>/bridge/<sessionId>.json` next to the session it
  created: system prompt, history, tool schemas.
- `examples/lanvello-bridge.ts`, installed in `~/.config/opencode/plugins/`,
  reads that file in the client and rebuilds the model call from it — the
  caller's system prompt replaces the client's agent prompt, the caller's
  history replaces the session's, and the caller's tools replace the client's.
- The caller's tool calls are blocked inside the client and recorded to
  `<sessionId>.calls.jsonl`; `serve` returns them as an openai `tool_calls`
  answer with `finish_reason: "tool_calls"`, streaming included. The caller runs
  the tool and appends the result as a `role: "tool"` message.
- A tool-less api request replaces the client's tools with nothing, so it cannot
  run anything in the client's directory.
- Without the plugin the bridge still answers — with the client's own agent and
  its own tools.

The spool directory is `$LANVELLO_BRIDGE_DIR`, defaulting to the gateway's own
`<dataDir>/bridge`, so a moved data dir moves both ends.

## proxy: keep the client's own traffic moving

```
./bin/lanvello proxy --listen 127.0.0.1:8888 --lanes 5
./bin/lanvello proxy --print-ca      # the CA path, for NODE_EXTRA_CA_CERTS
```

Point the client at it (the proxy prints both lines on start):

```
HTTPS_PROXY=http://127.0.0.1:8888 \
NODE_EXTRA_CA_CERTS=~/.local/state/lanvello/ca.pem \
opencode
```

The long-running client daemon — `opencode serve --service`, the process
`lanvello serve` drives — needs the same variables where *it* is started, plus
loopback kept out of the proxy:

```
NO_PROXY=127.0.0.1,localhost,::1 \
HTTPS_PROXY=http://127.0.0.1:8888 \
NODE_EXTRA_CA_CERTS=~/.local/state/lanvello/ca.pem \
opencode serve --service
```

All three are needed, and each one missing looks like a different bug:

- **no proxy**: every session the daemon runs answers `403 FreeTierError`.
- **proxy but no `NO_PROXY`**: the client calls its own api on `127.0.0.1`, those
  calls go out through a lane too, land on the exit's own loopback, and the
  session dies before it starts — the proxy log shows `tls handshake
  opencode.ai: EOF` and the client reports `getaddrinfo ETIMEOUT`.
- **`NODE_EXTRA_CA_CERTS` not pointing at `ca.pem`**: the client refuses the
  intercepted certificate and nothing reaches the proxy at all.

What happens on the wire:

```
opencode --CONNECT opencode.ai--> lanvello proxy
                                    |  tls terminated with a ca minted in the data dir
                                    |  request replayed per lane
                                    +--> lane 2 {de} --socks--> opencode.ai  200
                                    +--> 429 retry-after 8760 -> lane 2 parked 146m, replay lane 3 {se}  200
```

- `--print-ca` prints `ca.pem`; the client only ever sees a leaf minted for the
  host it asked for.
- `429`, `502`, `503` and `504` park the exit for as long as the far end's
  `retry-after` says (up to 6h) and replay the same request on the next lane
  *before* a byte reaches the client. `403 FreeTierError` is treated the same
  way — that body arrives brotli-encoded, so it is decoded before it is matched,
  and only that refusal rotates: any other `403` is an answer the caller asked
  for and reaches the client untouched.
- Rotating leaves the lane unusable for a moment, so a request waits (30s) for
  the fresh exit rather than handing back the refusal it already has.
- `--quiet` drops the per-request lane log.
- `socks: ["127.0.0.1:52001", ...]` in the config reuses lanes someone else
  spawned (a running `lanvello serve`, say); without it the proxy spawns its own
  tor, exactly like serve.
- Loopback targets bypass the lanes: the client's own local server is not the
  internet, and tor would refuse it anyway.

## when an answer comes back empty

`HTTP 200` with an empty `content` and `finish_reason: "error"`, usually within a
second, is not a gateway bug: it means the client's own session was refused
before it produced any text. In order, check:

1. `serve`'s startup line. `inference: opencode client at http://…` — the client
   was found. `replay upstream only` — no running client was visible, so nothing
   free-tier can answer.
2. The daemon's environment (`/proc/<pid>/environ` for
   `opencode … serve --service`). All three variables from the section above
   must be there. This is the most common cause.
3. The proxy log: `403 POST opencode.ai/inference/… lane 2{de}` (the far end
   refused that exit), `… refused the free tier -- replaying on …` (it was caught
   and retried), `no lane available` (every lane is parked or rotating — add
   lanes).
4. The client's own log, `~/.local/share/opencode/log/opencode.log`:
   `Failed to drain Session` names the cause behind it — `403 FreeTierError`,
   `429`, or `getaddrinfo ETIMEOUT` (that one is missing `NO_PROXY`).
5. `lanvello lanes status` and `$dataDir/proof.jsonl` for which exits are parked
   and why.

A refusal cannot be fixed on the client side: the far end decides which egress it
accepts, and that set moves. What helps is a bigger or better pool of exits.

## endpoints

- `GET /v1/models` — the live free catalog, fetched upstream through a lane and
  cached for a minute; only ids the free tier actually serves are advertised.
  Each entry carries capability metadata — `reasoning`, `reasoning_options`
  (selectable effort levels), `limit` (context/output), `tool_call`,
  `attachment`, `cost`, `input`/`output` modalities, plus
  `context_length`/`max_output_tokens` aliases — merged in from opencode's
  catalog mirror (`models.opencode.ai`), fetched through a lane, cached on disk
  for 6h and refreshed in the background. The embedded table for the known free
  models is the offline fallback, so a dead mirror never breaks the list.
- `POST /v1/chat/completions` — openai chat. responses-backed models
  (muse-spark) are translated chat -> responses upstream and back to openai sse
  (or aggregated when `stream:false`); the rest pass through to
  `/zen/v1/chat/completions`.
- `POST /v1/responses` — native responses passthrough to `/zen/v1/responses`.
- `POST /v1/messages` — anthropic-native. Body, tools, tool_choice and
  `thinking.budget_tokens` are translated to openai chat upstream and the answer
  comes back as an anthropic message or as the full anthropic sse sequence
  (`message_start` … `message_stop`). Unknown model names (`claude-*`) land on
  the default free model instead of failing.
- `GET /healthz`.

Unknown model ids are substituted with the default free model rather than passed
upstream, so a harness configured for another provider still works.

## streaming and usage

Streams are relayed event by event with a flush per event — the first token
reaches the client while the model is still generating. `stream:false`
aggregates the same stream into one completion and keeps `id`, `created` and the
full `usage` object, including `completion_tokens_details.reasoning_tokens`.

Effort is passed through in the spelling each endpoint accepts:
`reasoning_effort` on chat/messages, `reasoning{effort,summary}` on responses.
`thinking.budget_tokens` from anthropic bodies is folded into
`reasoning_effort`. Effort is observable in the returned usage: the same prompt
on `muse-spark-1.3` costs ~40 reasoning tokens at `minimal` and ~600 at `high`.
Reasoning text is only visible where upstream sends it in the clear
(`reasoning_content` on chat models); the responses path ships reasoning
encrypted, so there only the token counts show up.

## auth and keys

Bearer `sk-lanv-...` from `lanvello key add`. A fresh data dir with zero keys
runs open (single-user localhost); the moment one key exists, all `/v1/*`
require it. On a public endpoint set `requireKey` in the config (or
`LANVELLO_REQUIRE_KEY=1`) so an empty data dir refuses instead of serving open.
`keys.json` is re-read when it changes, so `key add` / `key revoke` take effect
without a restart. In replay mode (`--no-client`) one stable `x-opencode-session`
is derived per api key, because minting a fresh session per request burns free
quota into `429`s.

## limits

`429` retires the lane exit until `retry-after`; with tor lanes the process is
respawned in the background for a fresh exit and the lane returns once it is up,
while the request is retried on another lane. `502`/`503`/`504` retry on another
lane. `403 FreeTierError` is handled like a `429` (see the proxy section).
Per-request proof goes to `$dataDir/proof.jsonl`.

A lane stays busy for the whole answer, not just while the request is in flight,
so N lanes means N concurrent answers; the rest wait in the queue up to
`waitBudgetS` (default 90) and `/v1/models` up to `catalogBudgetS` (default 20).
Timeouts are split: 3 minutes for response headers, 10 minutes without any bytes
in flight — a model that thinks for a long time is not cut, a dead socket is not
waited on forever. If the client hangs up mid-stream the upstream request is
closed instead of running to completion.

## tor lanes

First run with tor enabled downloads the linux expert bundle (~30mb, once) into
`$dataDir/tools` when no system tor exists — same idea as
[lingling](https://github.com/huranth/lingling), no root needed. Then one tor
process per lane, pinned with `ExitNodes {cc}`. There is no direct mode: without
at least one healthy tor lane the server refuses to start. An explicit socks list
in config (`socks: ["127.0.0.1:9050"]`) can replace spawned processes.

`countries.txt` in the data dir overrides the pool like lingling: line 1
primary, line 2 fallback, two-letter codes, `#` comments.

Per-model exits: `modelLanes: {"mimo-v2.6-flash-free": "de"}` in config pins a
model family to an exit country; requests for other models use the least-loaded
lane. `429` handling is per exit ip with `retry-after` respected. A lane held by
a long answer cannot answer a catalog call until it frees up, hence
`catalogBudgetS`.

Every byte leaves through a lane, including the model catalog and the capability
mirror: there is no code path in `internal/upstream` that dials the free tier
directly.

## harness integration

`examples/lanvello-ext.mjs` registers the provider for pi-family harnesses (omp,
prime-agent) and fetches the model list from the gateway at load — an async
factory, so new free models appear without editing anything:

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
      opencode/mimo-v2.6-flash-free:
        reasoning: true
        thinking:
          mode: effort
          efforts: [low, high, max]
          defaultLevel: low
        compat:
          supportsReasoningEffort: true
```

omp resolves reasoning flags from its own bundled catalog, which does not know
the free models, so effort levels are declared per model id in `modelOverrides`
(the list itself stays dynamic via `discovery`).
`compat.supportsReasoningEffort: true` is required: without it omp treats an
unrecognized model id as effort-incapable and silently drops `reasoning_effort`
from the request.

## deploy

Single static binary, one dep (`golang.org/x/net` for socks5; `brotli` for the
refusal body), state in one dir (`LANVELLO_DATA_DIR`):

```
docker build -t lanvello .
docker run --rm -p 127.0.0.1:11434:11434 \
  -v lanvello-state:/state -e LANVELLO_DATA_DIR=/state lanvello
```

See `deploy/lanvello.service` for systemd. Override the target with
`LANVELLO_BASE_URL` if the provider's host moves.

## reverse-engineering notes (2026-10-13, 2026-10-14)

What the official client does today, read off the running binary and the live
wire. Recorded while building this, because anyone touching the free tier needs
it:

- the provider moved: `https://models.opencode.ai/api.json` now serves
  `"api": "https://opencode.ai/inference/v1"` for provider `opencode`. `/zen/v1`
  still answers but it is the older path; the client uses `/inference/v1`.
- request shape: user agent `opencode/<channel>/<version>/<client>` (today
  `opencode/dev/0.0.0-dev-20455/cli`), `authorization: Bearer public`,
  `x-opencode-client`, `x-opencode-project`, `x-opencode-session` (the root
  session id), `x-opencode-session-id`, `x-session-affinity`, `x-session-id`,
  `b3` and `traceparent`.
- session ids are minted locally: `ses_` + `hex48(~(Date.now()*4096+counter))` +
  14 base62 characters from random bytes. There is no signature in them; the
  timestamp decodes back to the moment the session was made (counter 1).
- the model transport is a websocket (`@opencode/SessionModelTransport`, 55
  minute rotation, 15s connect budget, plain http after 5 stream failures); the
  free models are served over plain http today.
- the gate is a server-side lease on the session, not a fingerprint: bytes
  replayed from another process — same headers in the same order and case, same
  body, the client's own TLS stack, same machine — answer `403 FreeTierError`,
  while the same bytes carrying the client's own session answer `200`. No
  header, tool set, body, protocol, exit or address moves that line, and the
  lease does not survive the client's request (a session id taken off the wire
  is refused seconds later). This is why `proxy` exists and why `serve` drives
  the client instead of replaying it.
- the `403 FreeTierError` body arrives **brotli-encoded** (`Content-Encoding:
  br`, no content-length) because the client asks for `br`. Anything that wants
  to read that refusal has to decode it; a raw-byte match sees nothing, which is
  how a rotation rule can silently never fire.
- the refusal is handed out per *exit*, not per fingerprint: the same request
  over fresh exits in several countries (us, de, nl, fr, ro) is refused while the
  connection itself is fine, and an exit refused a minute ago passes later. So it
  is treated like a `429`.
- measured on a 2–3 lane tor fleet: every fresh exit was refused within a minute
  of traffic, with a few requests still passing. The refusal tracks egress
  reputation, so the size and the *kind* of egress decide whether a request gets
  an answer — tor exits are the cheapest and the most distrusted.
- the free list moved too: `fledge-alpha-free` answers RegionError,
  `mimo-v2.5-free` and `exo-free` are deprecated (410); current free ids include
  `mimo-v2.6-flash-free`, `step-5-preview-free`, `cat-jam-free`,
  `muse-spark-1.3-contributor-free`, `nemotron-3-ultra-free`,
  `nemotron-3.5-lightning-free`, `space-bunny-free`, `ling-3.0/3.1-flash`.
