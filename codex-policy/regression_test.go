package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The audit ring buffer is written by every request (route + policy phase)
// and read by the menu handler. Without a lock this races in production.
func TestDecisionLogConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				appendLogEntry(logEntry{Phase: "policy", APIKey: "sk-test", Result: "FORWARDED"})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				snapshotLog(50, true)
			}
		}()
	}
	wg.Wait()
}

// rewriteResponseModel must not corrupt a JSON body that merely contains the
// text "data: ", and must handle SSE payloads that carry extra lines.
func TestRewriteResponseModelDispatch(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "json containing data text",
			payload: `{"model":"up","note":"see data: here"}`,
			want:    `{"model":"visible","note":"see data: here"}`,
		},
		{
			name:    "sse data line",
			payload: "data: {\"model\":\"up\"}\n",
			want:    "data: {\"model\":\"visible\"}\n",
		},
		{
			name:    "sse with event prefix",
			payload: "event: message\ndata: {\"model\":\"up\"}\n",
			want:    "event: message\ndata: {\"model\":\"visible\"}\n",
		},
	}
	for _, c := range cases {
		out := rewriteResponseModel([]byte(c.payload), "visible")
		if string(out) != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, string(out), c.want)
		}
	}
}

// A bare-JSON line inside an SSE payload must be left alone instead of
// panicking on a -1 slice index (which the host turns into a fused plugin).
func TestRewriteSSEIgnoresBareJSONLine(t *testing.T) {
	payload := "data: {\"model\":\"up\"}\n{\"model\":\"up2\"}\n"
	out := rewriteResponseModel([]byte(payload), "visible")
	if want := "data: {\"model\":\"visible\"}\n{\"model\":\"up2\"}\n"; string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

// configuredRaw feeds setDotted, so its nested maps must not alias state.raw.
func TestConfiguredRawDoesNotAliasState(t *testing.T) {
	if err := configureRaw([]byte("effort:\n  enabled: true\n  target: medium\n")); err != nil {
		t.Fatalf("configureRaw: %v", err)
	}
	merged := configuredRaw()
	if err := setDotted(merged, "effort.target", "high"); err != nil {
		t.Fatalf("setDotted: %v", err)
	}
	state.mu.RLock()
	rawTarget := valueAt(state.raw, "effort.target")
	state.mu.RUnlock()
	if rawTarget == "high" {
		t.Errorf("state.raw.effort.target mutated to %q without the lock", rawTarget)
	}
}

// Comma-separated single-line lists (including hyphenated keys) must still
// parse, while malformed YAML must be rejected instead of silently split.
func TestCoerceValueArrayForms(t *testing.T) {
	docs := map[string]configFieldDoc{
		"exempt_api_keys":       {Key: "exempt_api_keys", Type: "array"},
		"effort.downgrade_from": {Key: "effort.downgrade_from", Type: "array"},
		"model_map.rules":       {Key: "model_map.rules", Type: "array"},
	}
	for _, c := range []struct {
		key  string
		text string
	}{
		{"exempt_api_keys", "sk-abc, sk-def"},
		{"exempt_api_keys", "sk-WWuPtLxxx"},
		{"effort.downgrade_from", "xhigh, max, ultra"},
		{"model_map.rules", "- from: a\n  to: b\n"},
	} {
		if _, err := coerceValue(docs[c.key], c.text); err != nil {
			t.Errorf("%s <- %q: unexpected error %v", c.key, c.text, err)
		}
	}
	if _, err := coerceValue(docs["model_map.rules"], "from: a\n  to: b"); err == nil {
		t.Error("malformed yaml was accepted; rules would silently stop matching")
	}
}

// Real OpenAI Responses SSE shapes. The stream must survive byte-for-byte
// apart from the model rewrite in response.created / response.completed.
func TestRewriteRealResponsesSSE(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-luna","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"PONG"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-luna","status":"completed"}}` + "\n\n" +
		"data: [DONE]\n"
	out := rewriteResponseModel([]byte(stream), "gpt-5.6-sol-demo")
	if out == nil {
		t.Fatal("stream was not rewritten at all")
	}
	got := string(out)
	if strings.Contains(got, "gpt-5.6-luna") {
		t.Errorf("upstream model leaked to client:\n%s", got)
	}
	if n := strings.Count(got, `"model":"gpt-5.6-sol-demo"`); n != 2 {
		t.Errorf("want 2 rewritten model fields, got %d:\n%s", n, got)
	}
	// Structure must be preserved: same event names, same data lines, same terminator.
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.completed",
		`"delta":"PONG"`,
		"data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q from stream:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != strings.Count(stream, "\n") {
		t.Errorf("line count changed: got %d, want %d", strings.Count(got, "\n"), strings.Count(stream, "\n"))
	}
}

// Two keys sharing the masked form (first 4 + last 4) must not share a route
// decision, and the raw secret must never appear in a cache key.
func TestRouteCacheKeyIsolation(t *testing.T) {
	keyA := "sk-WWuPtL-middle-one-NiHm"
	keyB := "sk-WWuPtL-middle-two-NiHm"
	if clientAPIKeyLabel(keyA) != clientAPIKeyLabel(keyB) {
		t.Fatal("test premise broken: keys should share the masked label")
	}
	keyAHash := routeCacheKey(keyA, "openai-response", "gpt-5.6-sol-demo")
	keyBHash := routeCacheKey(keyB, "openai-response", "gpt-5.6-sol-demo")
	if keyAHash == keyBHash {
		t.Errorf("distinct keys produced the same cache key %q", keyAHash)
	}
	for _, secret := range []string{keyA, keyB} {
		if strings.Contains(keyAHash, secret) || strings.Contains(keyBHash, secret) {
			t.Errorf("cache key leaks the raw api key: %q", keyAHash)
		}
	}
}

// A decision stored under the raw client model must still be found when the
// interceptor looks it up with a thinking suffix attached (or vice versa).
func TestRouteCacheLookupRoundTrip(t *testing.T) {
	routeCache.mu.Lock()
	routeCache.entries = map[string]routeDecision{}
	routeCache.mu.Unlock()
	key := "sk-round-trip-1234"
	rememberRouteDecision(routeCacheKey(key, "openai-response", "gpt-5.6-sol-demo"),
		routeDecision{From: "gpt-5.6-sol-demo", Target: "gpt-5.6-luna", Provider: "p", At: time.Now()})
	for _, model := range []string{"gpt-5.6-sol-demo", "gpt-5.6-sol-demo(xhigh)"} {
		got := lookupRouteDecision(key, "openai-response", model)
		if got == nil {
			t.Errorf("lookup with model %q returned nil", model)
			continue
		}
		if got.Target != "gpt-5.6-luna" {
			t.Errorf("lookup with model %q returned target %q", model, got.Target)
		}
	}
}

// A blank line inside the plugin block must not truncate the replaced range,
// otherwise the keys after the blank line survive and get duplicated.
func TestSplicePluginBlockWithBlankLine(t *testing.T) {
	sample := strings.Join([]string{
		"plugins:",
		"    configs:",
		"        codex-policy:",
		"            enabled: true",
		"",
		"            effort:",
		"                enabled: true",
		"                target: medium",
		"other-setting: true",
	}, "\n") + "\n"
	block, err := renderPluginBlock(map[string]any{"enabled": true, "effort": map[string]any{"enabled": true, "target": "high"}})
	if err != nil {
		t.Fatalf("renderPluginBlock: %v", err)
	}
	updated, err := splicePluginBlock([]byte(sample), block)
	if err != nil {
		t.Fatalf("splicePluginBlock: %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(updated, &root); err != nil {
		t.Fatalf("result is not valid yaml: %v\n%s", err, updated)
	}
	cfg, okMap := root["plugins"].(map[string]any)["configs"].(map[string]any)[pluginID].(map[string]any)
	if !okMap {
		t.Fatalf("plugin block is not a mapping: %#v\n%s", root["plugins"], updated)
	}
	effort, okEffort := cfg["effort"].(map[string]any)
	if !okEffort {
		t.Fatalf("effort is not a mapping: %#v\n%s", cfg, updated)
	}
	if effort["target"] != "high" {
		t.Errorf("target = %v, want high (stale block survived?)\n%s", effort["target"], updated)
	}
	if root["other-setting"] != true {
		t.Errorf("other-setting lost\n%s", updated)
	}
}

// A non-Responses protocol must still be gated. Previously interceptRequest
// returned early for every non-codex source format, so /v1/chat/completions
// accepted any client while /v1/responses returned 403 for the same key+UA.
func TestAdmissionCoversNonCodexProtocols(t *testing.T) {
	if err := configureRaw([]byte("official_min_score: 5\nexempt_api_keys: []\n")); err != nil {
		t.Fatalf("configureRaw: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"request_id":      "r1",
		"source_format":   "openai-chat",
		"requested_model": "gpt-5.6-sol",
		"headers":         map[string][]string{"User-Agent": {"python-requests/2.31"}},
		"body":            []byte(`{"model":"gpt-5.6-sol","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, errIntercept := interceptRequest(payload)
	if errIntercept != nil {
		t.Fatalf("interceptRequest: %v", errIntercept)
	}
	if status, errDecode := terminatedStatus(out); errDecode != nil || status != http.StatusForbidden {
		t.Errorf("non-codex protocol was not gated (status=%d err=%v): %s", status, errDecode, out)
	}

	// The opt-out must restore the old pass-through behaviour.
	if err := configureRaw([]byte("official_min_score: 5\nenforce_all_protocols: false\n")); err != nil {
		t.Fatalf("configureRaw: %v", err)
	}
	out, err = interceptRequest(payload)
	if err != nil {
		t.Fatalf("interceptRequest: %v", err)
	}
	if status, errDecode := terminatedStatus(out); errDecode != nil || status != 0 {
		t.Errorf("enforce_all_protocols=false did not disable the gate (status=%d): %s", status, out)
	}
	if err := configureRaw([]byte("official_min_score: 5\n")); err != nil {
		t.Fatalf("restore config: %v", err)
	}
}

// terminatedStatus decodes the plugin envelope and reports the HTTP status the
// host will return, or 0 when the request is passed through untouched.
func terminatedStatus(out []byte) (int, error) {
	var envelope struct {
		Result struct {
			Terminate    bool   `json:"Terminate"`
			StatusCode   int    `json:"StatusCode"`
			ResponseBody string `json:"ResponseBody"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return 0, err
	}
	if !envelope.Result.Terminate {
		return 0, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Result.ResponseBody)
	if err != nil {
		return envelope.Result.StatusCode, err
	}
	if !strings.Contains(string(decoded), "client_not_supported") {
		return envelope.Result.StatusCode, errors.New("terminated without client_not_supported: " + string(decoded))
	}
	return envelope.Result.StatusCode, nil
}

// passedBody decodes the pass-through request body from the plugin envelope.
func passedBody(out []byte) (string, error) {
	var envelope struct {
		Result struct {
			Body string `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Result.Body)
	return string(decoded), err
}

// apply_to_non_codex must keep governing only the rewrite stage, not admission.
func TestRewriteAppliesToNonCodexOnlyWhenEnabled(t *testing.T) {
	if err := configureRaw([]byte("official_min_score: 1\napply_to_non_codex: false\neffort:\n  downgrade_from: [xhigh]\n  target: medium\n")); err != nil {
		t.Fatalf("configureRaw: %v", err)
	}
	body := []byte(`{"model":"m","reasoning":{"effort":"xhigh"}}`)
	payload, err := json.Marshal(map[string]any{
		"request_id": "r2", "source_format": "openai-chat", "requested_model": "m",
		"headers": map[string][]string{"User-Agent": {"codex-tui/0.160.0"}, "Originator": {"codex_cli_rs"}},
		"body":    body,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := interceptRequest(payload)
	if err != nil {
		t.Fatalf("interceptRequest: %v", err)
	}
	if status, errDecode := terminatedStatus(out); errDecode != nil || status != 0 {
		t.Fatalf("official-looking client was rejected (status=%d): %s", status, out)
	}
	if passed, errDecode := passedBody(out); errDecode != nil || passed != string(body) {
		t.Errorf("body should pass through untouched (got=%q err=%v): %s", passed, errDecode, out)
	}
	if err := configureRaw([]byte("official_min_score: 5\n")); err != nil {
		t.Fatalf("restore config: %v", err)
	}
}
