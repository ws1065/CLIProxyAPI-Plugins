# CPA model routing verification — 0.4.5

Date: 2026-10-03. Tested against the running CPA deployment with curl.

## Root cause and correction

The configured route targeted `openai-compatible-or`, but `or` was disabled after switching to `subapi`. CPA rejected the plugin proposal with `model router returned unavailable provider`, fell back to resolving the original model and returned `unknown provider for model gpt-6-astra`.

The deployment configuration now uses `model_map.provider: openai-compatible-subapi`. The plugin now checks the available provider list supplied by CPA, reports `PROVIDER_UNAVAILABLE` / `ROUTE_REJECTED` for unavailable targets, and labels successful proposals `ROUTE_PROPOSED` instead of claiming execution succeeded.

## Results

| Check | Result |
|---|---|
| Restricted key, Responses JSON, astra + xhigh | HTTP 200, completed; visible model remains gpt-6-astra |
| Restricted key, Responses SSE, astra + xhigh, two calls | Both HTTP 200, each with one response.completed event |
| Actual execution | CPA execution log: gpt-6.1-sol; plugin policy: openai-compatible-subapi, xhigh → high |
| Non-official client | HTTP 403, client_not_supported |
| Exempt key | Policy log confirms EXEMPT_KEY and no mapping; upstream rejected the synthetic request with reasoning.context validation (not counted as an end-to-end success) |
| Regression checks | go test -race ./... and go vet ./... passed |
| Artifact | dist/codex-policy.so and deployed binary have identical SHA-256 |

## Reproduce

Use a valid non-exempt CPA key and the deployment's configured model alias. Mapping examples use a routable target under the enabled provider. The synthetic Codex headers below exercise the heuristic admission check; they are not proof of client identity.

```bash
read -rsp 'CPA access key: ' CPA_TEST_KEY
export CPA_TEST_KEY
curl -sS --max-time 45 http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer $CPA_TEST_KEY" \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: codex-tui/0.160.0' \
  -H 'Originator: codex_cli_rs' \
  -H 'X-OpenAI-Internal-Codex-Responses-Lite: true' \
  -d '{"model":"gpt-6-astra","input":"Reply only OK","reasoning":{"effort":"xhigh"},"stream":false}'
```

For SSE, replace `"stream":false` with `"stream":true` and add curl `-N`. Verify `response.completed` and the visible response model, then correlate the policy and host execution logs to confirm the actual target. Response model names alone cannot prove mapping.

Configuration changes should be verified inside the container: file watchers can reload in-place writes, while atomic replacement of a single-file bind mount may leave the container on the old inode until restart.
