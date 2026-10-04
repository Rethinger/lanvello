# lanvello

one local port serving opencode free models over an openai-compatible api.
no login, no api keys from you: lanvello replays the official client
fingerprint (`opencode/*` user-agent, `x-opencode-*` session headers,
forced streaming, bash/glob/grep/read tool quartet) against
`https://opencode.ai/zen/v1` with `Bearer public`, exactly like the cli
does for its free tier. tor lanes rotate exits when an ip is throttled.

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
```

any harness that accepts an openai-compatible provider then uses:

```
baseURL: http://127.0.0.1:11434/v1
apiKey:  sk-lanv-...
model:   opencode/muse-spark-1.3-contributor-free
```

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
require it. one stable `x-opencode-session` is derived per api key:
minting a fresh session per request burns free quota into 429s.

## limits

429 retires the lane exit until `retry-after` and rotates country, request is
retried on another lane. 502/503/504 retry on another lane. per-request
proof goes to `$dataDir/proof.jsonl`.

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
