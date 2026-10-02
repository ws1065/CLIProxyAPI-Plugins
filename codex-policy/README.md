# codex-policy

Source for the **CLIProxyAPI (CPA) plugin** `codex-policy`.

The full documentation lives one directory up, where it covers both languages:

- [../README.md](../README.md) — English
- [../README_cn.md](../README_cn.md) — 中文

Source files:

| File | Role |
|---|---|
| `main.go` | Hooks: `model.route`, `request.intercept_before`, `response.intercept_after`, `response.intercept_stream_chunk` |
| `config_fields.go` | Single source of truth for every config key: type, default, description. Feeds both the CPA panel field list and the config page, so docs cannot drift from `defaultConfig()` |
| `config_patch.go` | Online-save: surgical line-range replacement of the plugin block in `config.yaml`, validation, backup, atomic write |
| `management.go` | `/settings`, `/logs` endpoints and the audit ring buffer |
| `pages.go` | Self-contained HTML for the two panel menu pages |
| `regression_test.go`, `config_patch_test.go` | Regression tests (`go test -race ./...`) |
