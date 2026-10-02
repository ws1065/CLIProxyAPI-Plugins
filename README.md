# CLIProxyAPI-Plugins

Plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (**CPA**).

> This repository holds **CPA plugins** — Go shared libraries (`-buildmode=c-shared`) that
> CLIProxyAPI loads at runtime through its plugin host. Nothing here is a standalone proxy or a
> CPA fork; the code only takes effect when placed inside a CPA deployment.

| Plugin | ID | What it does |
|---|---|---|
| **codex-policy** | `codex-policy` | Official-client admission control, reasoning-effort downgrading, transparent model mapping |

`codex-policy` sits in CPA's request path and does three things:

1. **Official-client admission control** — only official Codex clients are served; everyone
   else gets `403 客户端不支持访问` ("client not supported").
2. **Reasoning-effort downgrading** — `xhigh` / `max` / `ultra` are rewritten to `medium`
   without the client noticing.
3. **Model mapping** — the client asks for model A, CPA actually forwards to model B, and the
   response reports model A again, so the client stays unaware.

All three are **per-API-key exemptable**: keys on the exempt list skip admission and skip
rewriting.

Current version: **0.4.4**

---

## Contents

```
CLIProxyAPIPlugins/
├── codex-policy/          plugin source (main.go, config_fields.go, config_patch.go, pages.go, ...)
├── dist/codex-policy.so   prebuilt plugin binary
├── config-example.yaml    sample plugins.configs.codex-policy block
├── build.sh               build script
├── README.md              this file (English)
└── README_cn.md           中文文档
```

## How the three CPA hooks are divided

Model mapping **must** use the ModelRouter hook. Request interceptors run too late: the
executor overwrites the body's `model` field with the already-resolved model
(`helps.SetStringIfDifferent(body, "model", baseModel)`), so a body rewrite is discarded.

| Capability | Hook | When | Purpose |
|---|---|---|---|
| Model mapping | `model.route` | **before** model→provider resolution | Replace the upstream model |
| Admission + effort rewrite | `request.intercept_before` | before credential selection | Gate the client, downgrade effort |
| Restore visible model name | `response.intercept_after`, `response.intercept_stream_chunk` | before the client sees it | Report model A back to the client |

## Build and deploy

```bash
cd CLIProxyAPIPlugins
./build.sh

# plugins/ is usually a bind mount, so a plain copy is enough
cp dist/codex-policy.so ../plugins/linux/amd64/codex-policy.so
docker compose restart cli-proxy-api
```

The plugin is loaded when CPA logs:

```
pluginhost: plugin registered plugin_id=codex-policy plugin_name=codex-policy version=0.4.4
```

Two common traps:

1. `plugins.configs.codex-policy.enabled: false` makes CPA **skip loading entirely and print
   nothing at all** — this is the most common cause of "the plugin disappeared". CPA treats a
   missing `enabled` as `false`, so it must be written explicitly.
2. `model_map.provider` must be a **registry key** (e.g. `openai-compatible-or`), not the `name`
   shown in the config (e.g. `or`). A wrong value produces a single warning line in
   `logs/main.log` and requests silently fall through to the unmapped model.

## Configuration (CPA `config.yaml`)

```yaml
plugins:
  enabled: true
  configs:
    codex-policy:
      enabled: true
      enforce_official_client: true
      reject_status: 403
      reject_message: "客户端不支持访问"
      official_min_score: 5          # needs >= 1 strong signal AND total score >= this
      enforce_all_protocols: true    # admission covers every protocol (recommended)
      apply_to_non_codex: false      # whether *rewriting* also reaches non-Responses traffic
      exempt_api_keys: []            # keys that bypass admission and rewriting
      exempt_bypass_admission: true
      effort:
        enabled: true
        downgrade_from: [xhigh, max, ultra]
        target: medium
        models: []                   # empty = all models
        skip_for_exempt_keys: true
      model_map:
        enabled: true
        provider: "openai-compatible-or"  # registry key, not the display name
        preserve_thinking_suffix: true     # keep the model(xhigh) suffix
        rewrite_response_model: true       # report the client-visible name in responses
        skip_for_exempt_keys: true
        rules:
          - from: "gpt-5.6-sol-demo"      # what the client asks for
            to: "gpt-5.6-luna"            # what CPA actually calls
            provider: ""                  # empty = use the default provider above
            keys: []                       # empty = all keys
      probe_model: ""                # non-empty: answer locally with a policy report
      probe_header: X-Policy-Probe
      probe_header_value: "1"
      log_decisions: true
      log_format: table
      log_buffer_size: 200
```

### The two protocol-scope switches

These are independent and were deliberately decoupled in 0.4.4:

- **`enforce_all_protocols`** (default `true`) — run official-client admission on **every**
  protocol. Before 0.4.4, `apply_to_non_codex: false` caused every non-Responses request to be
  waved through untouched, so `/v1/chat/completions` accepted any client with any valid key
  while the same key got a 403 on `/v1/responses`. Set to `false` to restore the old behaviour.
- **`apply_to_non_codex`** (default `false`) — whether **model mapping and effort downgrading**
  also apply to Claude / Gemini / OpenAI-chat traffic. It no longer has any effect on admission.

Non-inference endpoints such as `/v1/models` do not pass through the interceptor and are
unaffected either way.

### Admission signals

Responses-Lite header `X-OpenAI-Internal-Codex-Responses-Lite: true` (+2, strong), `Originator`
containing `codex` (+2, strong), user agent starting with `codex-tui/` / `codex_cli_rs/` /
`codex-cli/` (+2, strong), `X-Codex-Turn-Metadata` (+1), `X-Codex-Window-Id` or
`X-Client-Request-Id` (+1), `X-Codex-Beta-Features` (+1), a session header (+1), body shape
(+1). At least one **strong** signal is required *and* the total must clear the threshold, so a
single forgeable header is never enough on its own.

## Audit log

`log_format: table` (default) prints one aligned table per request; `log_format: line` prints a
single `key=value` line for grepping.

```
[codex-policy] +-- codex-policy --------------------------------------------
[codex-policy] |  phase      : policy
[codex-policy] |  time       : 2026-10-01 23:12:37.751
[codex-policy] |  request_id : 4497d57e-df5
[codex-policy] |  api_key    : sk-W...NiHm   exempt=false
[codex-policy] |  protocol   : openai-response   client=codex-tui/0.159.3
[codex-policy] |  model      : gpt-5.6-sol-demo -> gpt-5.6-luna  via provider=openai-compatible-or
[codex-policy] |  effort     : xhigh -> medium
[codex-policy] |  admission  : ALLOWED  score=7/5  signals=[responses_lite originator:codex-tui ...]
[codex-policy] |  signals    : responses_lite, originator:codex-tui, ...
[codex-policy] |  actions    : MODEL_MAPPED, EFFORT_DOWNGRADED
[codex-policy] |  result     : FORWARDED
[codex-policy] +-- ----------------------------------------------------------------
```

`phase: route` is the model-routing decision, `phase: policy` is admission plus effort. The two
hooks do not share a request id, so the model line in a policy table comes from a 90-second TTL
route cache.

```bash
docker compose logs -f cli-proxy-api | grep 'codex-policy'
```

CPA's own routing and execution details live in a **log file**, not in docker logs:

```bash
docker exec cli-proxy-api grep 'Use API key' /CLIProxyAPI/logs/main.log | tail   # model actually used
docker exec cli-proxy-api grep 'model router' /CLIProxyAPI/logs/main.log | tail   # routing warnings
```

## Management panel menu

The plugin registers two menu entries (CPA panel → Plugins → codex-policy):

| Menu | Path | Contents |
|---|---|---|
| Request log | `/v0/resource/plugins/codex-policy/logs` | Per request: key, client, requested model, forwarded model, effort, admission score, actions |
| Config reference | `/v0/resource/plugins/codex-policy/config` | Every option's type, default, effective value, description |

Both pages are self-contained HTML with no external dependencies:

```bash
curl -s http://127.0.0.1:8080/v0/resource/plugins/codex-policy/logs   | head -40
curl -s http://127.0.0.1:8080/v0/resource/plugins/codex-policy/config | head -40
```

The same data is available as JSON (`?format=json`) behind CPA's management authentication,
which suits scripted polling:

```
GET /v0/management/plugins/codex-policy/logs?limit=50&format=json
GET /v0/management/plugins/codex-policy/settings?format=json
```

### Saving configuration from the panel

Every field on the config page is editable. Saving is handled by the plugin itself:

```
POST /v0/management/plugins/codex-policy/settings
Authorization: Bearer <plaintext management.secret-key>
Content-Type: application/json

{"official_min_score": 6, "effort.target": "high"}
```

The body is **flat dotted keys** (only the fields you actually changed are submitted); values are
strings or native types. The plugin coerces and validates them against the declarations in
`config_fields.go`, returning 400 without touching disk on an invalid value.

The write procedure:

1. Read `config.yaml` and replace only the **line range** of the
   `plugins.configs.codex-policy` block, leaving every other byte untouched (so your comments and
   formatting elsewhere survive).
2. Add a self-documenting comment above each top-level key, sourced from `config_fields.go`.
3. Validate the result by parsing the YAML, then back up to
   `config.yaml.bak-before-codex-policy`.
4. Replace atomically (when `config.yaml` is a single-file bind mount the host returns `EBUSY`,
   and the plugin falls back to an in-place rewrite).
5. Feed the new content through `configureRaw()`, so everything the plugin itself reads takes
   effect **immediately, with no restart**.

Two caveats:

- `enabled` and `priority` are read by CPA itself. The page marks them "restart required".
- The save endpoint is behind CPA's management authentication and returns 401 without a key.

> CPA's own `GET/PUT/PATCH /v0/management/plugins/<id>/config` is a different implementation: it
> writes configuration and triggers a hot reload, but it **re-serializes the entire
> `config.yaml`**, losing every comment and all indentation. The plugin therefore avoids that
> path and uses `/settings` instead.

### Hot reload: what actually works

Only the plugin's own `/settings` endpoint applies changes immediately. **Hand-editing
`config.yaml` with sed or an editor does not hot-reload** — the host logs no reload event and
behaviour is unchanged until you run `docker restart`. This is the single most common cause of
"I changed the configuration but nothing happened".

Resource routes (`/v0/resource/...`) are unauthenticated by CPA design, so the log page only
stores masked keys (`sk-W...NiHm`). Still, expose the management port only on a trusted network.

## Verifying with curl

```bash
K='<your CPA access key>'
H=(-H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
   -H 'X-OpenAI-Internal-Codex-Responses-Lite: true' -H 'Originator: codex-tui' \
   -H 'User-Agent: codex-tui/0.159.3' -H 'session_id: s-verify')

# M1 model mapping (non-streaming): request sol-demo, upstream is really luna
curl -s http://127.0.0.1:8080/v1/responses "${H[@]}" \
  -d '{"model":"gpt-5.6-sol-demo","input":"say PONG","reasoning":{"effort":"low"},"stream":false}' \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["model"])'

# M2 model mapping + effort downgrade (streaming): xhigh becomes medium, model stays sol-demo
curl -sN http://127.0.0.1:8080/v1/responses "${H[@]}" \
  -d '{"model":"gpt-5.6-sol-demo","input":"say PONG","reasoning":{"effort":"xhigh"},"stream":true}' \
  | grep -o '"effort":"[^"]*"\|"model":"[^"]*"' | sort | uniq -c

# M3 non-official client: expect 403
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'User-Agent: python-requests/2.31' \
  -d '{"model":"gpt-5.6-sol-demo","input":"hi","reasoning":{"effort":"xhigh"}}'

# M4 the same non-official client on chat/completions: also expect 403 (fixed in 0.4.4)
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'User-Agent: python-requests/2.31' \
  -d '{"model":"gpt-5.6-sol-demo","messages":[{"role":"user","content":"hi"}]}'
```

To confirm mapping really happened, read the **actual model** out of CPA's execution log rather
than trusting the `model` field in the response:

```bash
docker exec cli-proxy-api grep 'Use API key' /CLIProxyAPI/logs/main.log | tail -3
```

### Zero-cost verification with the probe

`probe_model` must name a **routable real model** (model resolution happens before interception),
and the request must additionally carry `X-Policy-Probe: 1`. That guard is essential: without
it a probe would truncate real streaming responses, which shows up as
`stream closed before response.completed`.

```bash
# Probe: returns the policy report locally without calling upstream
curl -s http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'X-OpenAI-Internal-Codex-Responses-Lite: true' -H 'Originator: codex-tui' \
  -H 'User-Agent: codex-tui/0.159.3' -H 'session_id: sess-1' \
  -H 'X-Policy-Probe: 1' \
  -d '{"model":"<REAL_MODEL>","input":"ping","reasoning":{"effort":"xhigh"},"stream":false}'
```

**Leave `probe_model` empty in production.** If the model you name also carries real traffic,
any real request that lacks the probe header gets its response replaced by local JSON, presenting
as `stream closed before response.completed`.

## Known limits

- Admission is based on request characteristics, not cryptographic proof. The goal is to deter
  third-party clients, not to withstand targeted forgery.
- `exempt_api_keys` is compared in constant time, but the keys are stored in plaintext in
  `config.yaml`.
- Model mapping can only replace the provider route as a whole
  (`ModelRouteTargetProvider`); it cannot hand a request to a different plugin's executor, and the
  mapping target must be a model that genuinely exists under that provider.
- The client-visible model name is the `model` field in the response. A client cannot be prevented
  from observing other upstream information through fields such as `store: true` or `include`.

## Changelog

### 0.4.4

- **Fixed: non-Responses protocols bypassed admission.** `interceptRequest` used to return
  immediately for any `source_format` that was not codex/responses when
  `apply_to_non_codex: false` (the default), so any client could call `/v1/chat/completions`
  with any valid access key while the same key got a 403 on `/v1/responses`. Admission and rewrite
  scope are now decoupled via the new `enforce_all_protocols` (default `true`).
- New regression tests `TestAdmissionCoversNonCodexProtocols` and
  `TestRewriteAppliesToNonCodexOnlyWhenEnabled`.

### 0.4.3 and earlier

- Fixed a data race in the audit ring buffer, an SSE rewriter panic plus JSON body corruption
  when text contained `"data: "`, silent comma-splitting of malformed YAML, unquoted
  `arrayAsYAML` scalars truncating on `#` / `:`, route-cache cross-attribution caused by keying
  on masked keys, and `splicePluginBlock` stopping at the first blank line and producing duplicate
  keys.

## License

Provided as-is for use with CLIProxyAPI.
