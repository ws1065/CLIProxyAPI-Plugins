# Automatic CPA supplier selection — 0.5.0

Verified on 2026-10-04 with CPA v8.0.4. This change keeps each rule's target model (`to`) fixed and makes only the supplier automatic.

## Configuration

```yaml
model_map:
  enabled: true
  provider: ""  # omit, leave empty, or use auto
  rules:
    - from: gpt-6-astra
      to: gpt-6.1-sol
```

A non-empty provider key still pins a channel. A rule's empty provider inherits the global value; a per-rule `auto` overrides a globally pinned provider. Automatic mode delegates the mapped model to CPA's native model registry and credential scheduler. CPA excludes unavailable/disabled routes according to its normal model registration and auth rules. The plugin does not infer model ownership from names, probe third-party suppliers, or automatically change `to`.

## Verification results

| Check | Evidence |
|---|---|
| Source model not registered | Isolated CPA lists the target but not gpt-6-astra; mapped request succeeds |
| JSON mapping | HTTP 200; mock upstream receives its real target model; client receives gpt-6-astra |
| Supplier switch | Disable A, enable B, leave the mapping rule untouched; mock observations confirm requests move to B |
| Supplier without target | Remains enabled but receives no mapped requests |
| No supplier has target | HTTP 400 names gpt-6.1-sol; never silently switches models |
| SSE | response.completed received; response model restored; no target-model leak |
| Concurrent requests | 40 JSON requests at concurrency 8 and 20 SSE requests at concurrency 4; all succeed |
| Reasoning | Responses and Chat fields downgraded to medium in isolated upstream observations |
| Exemption / admission | Exempt key bypasses mapping; unofficial client gets 403 without upstream execution |
| Explicit supplier | Existing provider-key routing still succeeds |
| Runtime | No panic or plugin fuse in isolated CPA; test container is removed after completion |
| Go checks | go test -race ./... and go vet ./... pass |
| Real deployment | Non-exempt wulei requests astra: JSON HTTP 200/completed and SSE HTTP 200/response.completed, both visibly astra |
| Real upstream / configured effort | CPA execution log confirms OAuth execution of gpt-6.1-sol; policy retains configured xhigh → high |
| Config page / binary | Automatic-selection hint present; dist and installed binaries have matching SHA-256 |

The integration script records 17 assertions. Unit tests also cover lost policy edits, callback context preservation, upstream HTTP status preservation, native Codex event chunks, SSE fields split byte by byte, CRLF framing, bounded buffering and stream shutdown cleanup.

## Native Codex streaming regression caught during acceptance

Codex's native translator can emit complete `data: {JSON}` events without trailing newlines, whereas OpenAI-compatible translators emit delimited SSE frames. Buffering all native events until `\n\n` corrupts their boundaries and produces `upstream stream closed before first payload`. The bridge now handles native complete events immediately and buffers only fragmented frames; both contracts have regression coverage. Real OAuth acceptance succeeded after this fix.

## Reproduce the isolated suite

```bash
./build.sh
(cd codex-policy && go test -race ./... && go vet ./...)
python3 tests/integration_auto.py
```

Requirements: Docker, curl, Python with PyYAML, and the local `eceasy/cli-proxy-api:latest` image (or `CPA_TEST_IMAGE`). The suite uses random local ports, mock suppliers and synthetic keys. It does not touch production configuration or call a real model provider.

## Reproduce a real request

Use a valid non-exempt CPA access key. The synthetic Codex headers exercise heuristic admission, not client authenticity.

```bash
read -rsp 'CPA key: ' CPA_TEST_KEY
curl -sS --max-time 45 http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer $CPA_TEST_KEY" \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: codex-tui/0.160.0' \
  -H 'Originator: codex_cli_rs' \
  -d '{"model":"gpt-6-astra","instructions":"Reply only OK.","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}],"store":false,"reasoning":{"effort":"xhigh"},"stream":false}'
```

For streaming, use curl `-N` and change `stream` to `true`. Confirm `response.completed` and the original model name, then check CPA execution logs for the actual target. Audit rows say `auto (CPA)` / `AUTO_PROVIDER_SELECTION`; they do not claim a specific supplier was chosen.

## Practical limits

- Automatic mode supports inference; token-count operations return 501. Explicit provider routing continues to use the built-in counting path.
- CPA Home mode does not support plugin-executor routes in this tested version.
- Pending fragmented SSE frames are bounded to 16 MiB. Host scheduling and retries remain subject to CPA's configured policy and upstream availability.
