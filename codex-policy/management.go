package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// logEntry is one row of the request audit log exposed by the plugin menu.
// It only ever holds masked API keys, because resource routes are served
// without management authentication.
type logEntry struct {
	Time        string   `json:"time"`
	Phase       string   `json:"phase"`
	RequestID   string   `json:"request_id,omitempty"`
	APIKey      string   `json:"api_key"`
	Exempt      bool     `json:"exempt"`
	Protocol    string   `json:"protocol"`
	Client      string   `json:"client"`
	Model       string   `json:"model"`
	RoutedModel string   `json:"routed_model,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Effort      string   `json:"effort"`
	EffortFinal string   `json:"effort_final,omitempty"`
	Score       int      `json:"score"`
	MinScore    int      `json:"min_score"`
	Admitted    bool     `json:"admitted"`
	Actions     []string `json:"actions"`
	Signals     []string `json:"signals"`
	Result      string   `json:"result"`
}

type logStats struct {
	Total     int `json:"total"`
	Admitted  int `json:"admitted"`
	Rejected  int `json:"rejected"`
	Mapped    int `json:"mapped"`
	Downgrade int `json:"downgraded"`
	Exempt    int `json:"exempt"`
}

var decisionLog = struct {
	mu      sync.Mutex
	entries []logEntry
	stats   logStats
}{entries: []logEntry{}}

// appendLogEntry keeps a bounded in-memory history for the plugin menu log
// page. The buffer is intentionally small and never persisted to disk.
func appendLogEntry(entry logEntry) {
	limit := intValue(currentConfig().LogBufferSize, 200)
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	decisionLog.mu.Lock()
	defer decisionLog.mu.Unlock()
	decisionLog.entries = append(decisionLog.entries, entry)
	if len(decisionLog.entries) > limit {
		decisionLog.entries = decisionLog.entries[len(decisionLog.entries)-limit:]
	}
	decisionLog.stats.Total++
	if entry.Phase != "policy" {
		return
	}
	switch entry.Result {
	case "FORWARDED":
		decisionLog.stats.Admitted++
	case "REJECTED":
		decisionLog.stats.Rejected++
	}
	if entry.Exempt {
		decisionLog.stats.Exempt++
	}
	for _, action := range entry.Actions {
		switch action {
		case "MODEL_MAPPED":
			decisionLog.stats.Mapped++
		case "EFFORT_DOWNGRADED":
			decisionLog.stats.Downgrade++
		}
	}
}

// snapshotLog returns the newest entries in chronological order.
func snapshotLog(limit int, onlyPolicy bool) ([]logEntry, logStats) {
	decisionLog.mu.Lock()
	// Copy under the lock: sharing the backing array would still race with the
	// append path once the buffer wraps. The ring is bounded by log_buffer_size.
	all := make([]logEntry, len(decisionLog.entries))
	copy(all, decisionLog.entries)
	stats := decisionLog.stats
	decisionLog.mu.Unlock()
	filtered := make([]logEntry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if onlyPolicy && all[i].Phase != "policy" {
			continue
		}
		filtered = append(filtered, all[i])
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered, stats
}

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/codex-policy/logs", Description: "Request audit log as JSON."},
			// NOTE: "/plugins/<id>/config" is reserved by the host, which serves it
			// with its own whole-document rewriter. We deliberately use "/settings"
			// so this handler stays reachable and can splice the YAML block without
			// destroying comments elsewhere in config.yaml.
			{Method: http.MethodGet, Path: "/plugins/codex-policy/settings", Description: "Effective plugin configuration as JSON."},
			{Method: http.MethodPost, Path: "/plugins/codex-policy/settings", Description: "Update plugin configuration: rewrites only the codex-policy block of config.yaml, preserving comments, and applies in-process."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/logs", Menu: "请求日志", Description: "查看每个请求的密钥、模型、思考强度和处理动作"},
			{Path: "/config", Menu: "配置说明", Description: "查看全部配置项、默认值与当前生效值"},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	path := strings.TrimSuffix(strings.TrimSpace(req.Path), "/")
	limit := 100
	if rawLimit := strings.TrimSpace(req.Query.Get("limit")); rawLimit != "" {
		if parsed, errParse := strconv.Atoi(rawLimit); errParse == nil && parsed > 0 {
			limit = parsed
		}
	}
	switch {
	case strings.HasSuffix(path, "/settings"), strings.HasSuffix(path, "/config"):
		// "/config" is the unauthenticated menu resource route (read-only).
		// "/settings" is the authenticated management route and also accepts POST.
		if req.Method != http.MethodGet && !strings.HasSuffix(path, "/settings") {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "the config page is read-only; POST to /v0/management/plugins/codex-policy/settings to save"})
		}
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			payload, errApply := applyConfigValues(req.Body)
			if errApply != nil {
				return jsonResponse(http.StatusBadRequest, map[string]any{"error": errApply.Error()})
			}
			return jsonResponse(http.StatusOK, payload)
		}
		if wantsJSON(req) {
			return jsonResponse(http.StatusOK, effectiveConfigPayload())
		}
		return htmlResponse(http.StatusOK, renderConfigPage())
	case strings.HasSuffix(path, "/logs"):
		entries, stats := snapshotLog(limit, true)
		if wantsJSON(req) {
			return jsonResponse(http.StatusOK, map[string]any{
				"plugin":  pluginID,
				"version": pluginVersion,
				"stats":   stats,
				"entries": entries,
			})
		}
		return htmlResponse(http.StatusOK, renderLogsPage(entries, stats, limit))
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route: " + req.Path})
	}
}

func wantsJSON(req pluginapi.ManagementRequest) bool {
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.Path)), ".json") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(req.Query.Get("format")), "json")
}

func effectiveConfigPayload() map[string]any {
	raw, errMarshal := yaml.Marshal(currentConfig())
	if errMarshal != nil {
		return map[string]any{"error": errMarshal.Error()}
	}
	current := configuredRaw()
	defaults := yamlMap(defaultConfig())
	fields := make([]map[string]any, 0, len(configFieldDocs))
	for _, doc := range configFieldDocs {
		fields = append(fields, map[string]any{
			"key":         doc.Key,
			"type":        string(doc.Type),
			"enum":        doc.Enum,
			"description": doc.Description,
			"default":     doc.Default,
			"current":     valueAt(current, doc.Key),
			"default_raw": valueAt(defaults, doc.Key),
		})
	}
	return map[string]any{
		"plugin":         pluginID,
		"version":        pluginVersion,
		"current":        current,
		"fields":         fields,
		"effective_yaml": string(raw),
	}
}

func yamlMap(cfg pluginConfig) map[string]any {
	raw, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if errUnmarshal := yaml.Unmarshal(raw, &out); errUnmarshal != nil {
		return map[string]any{}
	}
	return out
}

// configuredRaw returns every key the host passed to the plugin, merged with
// the typed config so keys absent from config.yaml still show their defaults.
func configuredRaw() map[string]any {
	state.mu.RLock()
	defer state.mu.RUnlock()
	merged := yamlMap(state.config)
	for key, value := range state.raw {
		// Deep copy: callers mutate the result via setDotted, and a shallow
		// copy would alias state.raw's nested maps, mutating live plugin state
		// without the lock and leaving raw/config disagreeing when a later
		// validation or write step fails.
		merged[key] = deepCopyValue(value)
	}
	return merged
}

func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = deepCopyValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return value
	}
}

// valueAt resolves a dotted config path such as "effort.target".
func valueAt(root map[string]any, path string) any {
	var cursor any = root
	for _, part := range strings.Split(path, ".") {
		asMap, ok := cursor.(map[string]any)
		if !ok {
			return nil
		}
		cursor, ok = asMap[part]
		if !ok {
			return nil
		}
	}
	return cursor
}

func jsonResponse(status int, payload any) ([]byte, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func htmlResponse(status int, body string) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(body),
	})
}

func intValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func pluginNow() string {
	return time.Now().Format("2006-01-02 15:04:05.000")
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func resultClass(result string) string {
	switch result {
	case "FORWARDED":
		return "ok"
	case "REJECTED":
		return "warn"
	default:
		return "mute"
	}
}

func renderValue(value any) string {
	if value == nil {
		return "-"
	}
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return `""`
		}
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case []any:
		if len(typed) == 0 {
			return "[]"
		}
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, renderValue(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+": "+renderValue(typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(typed)
	}
}

func sameValue(a, b any) bool {
	return renderValue(a) == renderValue(b)
}

// yamlScalar renders a value as a single-line YAML scalar. Plain rendering is
// not safe here: an unquoted model name containing "#" or ":" would parse back
// as a different value (or start a comment), so the rules textarea would
// silently lose data on a save/reload round-trip.
func yamlScalar(value any) string {
	switch typed := value.(type) {
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, yamlScalar(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, yamlScalar(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		raw, errMarshal := yaml.Marshal(value)
		if errMarshal != nil || strings.Contains(string(raw), "\n") {
			// Multi-line output (nested mapping) cannot be inlined safely.
			return `""`
		}
		return strings.TrimRight(string(raw), "\n")
	}
}
