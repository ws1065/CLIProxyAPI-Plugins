package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func autoTestConfig(t *testing.T) {
	t.Helper()
	previous := configuredRaw()
	t.Cleanup(func() {
		raw, _ := yaml.Marshal(previous)
		if err := configureRaw(raw); err != nil {
			t.Error(err)
		}
	})
	if err := configureRaw([]byte(`log_decisions: false
exempt_api_keys: [exempt-test]
model_map:
  rules:
    - from: gpt-6-astra
      to: gpt-6.1-sol
`)); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticProviderRoute(t *testing.T) {
	autoTestConfig(t)
	for _, tc := range []struct {
		name, key, format string
		handled           bool
	}{
		{"missing source still maps", "restricted", "openai-response", true},
		{"exempt still skips", "exempt-test", "openai-response", false},
		{"scope still applies", "restricted", "openai", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(pluginapi.ModelRouteRequest{
				RequestedModel: "gpt-6-astra", SourceFormat: tc.format,
				Headers:            http.Header{"Authorization": {"Bearer " + tc.key}},
				AvailableProviders: []string{"openai-compatible-renamed", "codex"},
			})
			out, err := routeModel(raw)
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Result pluginapi.ModelRouteResponse `json:"result"`
			}
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatal(err)
			}
			if env.Result.Handled != tc.handled {
				t.Fatalf("route=%s", out)
			}
			if tc.handled && (env.Result.TargetKind != pluginapi.ModelRouteTargetSelf || env.Result.Target != pluginID) {
				t.Fatalf("pinned provider: %s", out)
			}
		})
	}
}

func TestAutomaticForwardPreservesPolicyAndHostContext(t *testing.T) {
	autoTestConfig(t)
	previous := hostRPC
	t.Cleanup(func() { hostRPC = previous })
	called := false
	hostRPC = func(method string, raw []byte) ([]byte, error) {
		called = true
		if method != pluginabi.MethodHostModelExecute {
			t.Errorf("method=%s", method)
		}
		var req autoHostRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "gpt-6.1-sol" || req.ForcedProvider != "" || req.HostCallbackID != "callback-test" {
			t.Errorf("wrong delegation: %+v", req)
		}
		if readEffort(req.Body) != "medium" {
			t.Errorf("policy edits lost: %s", req.Body)
		}
		if req.Headers.Get("Authorization") != "Bearer restricted" {
			t.Error("lost frontend context")
		}
		return okEnvelope(pluginapi.HostModelExecutionResponse{StatusCode: 200, Body: []byte(`{"model":"gpt-6.1-sol","status":"completed"}`)})
	}
	raw, _ := json.Marshal(autoExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "gpt-6-astra", SourceFormat: "openai-response", Headers: http.Header{"Authorization": {"Bearer restricted"}},
			Payload: []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"medium"}}`), OriginalRequest: []byte(`{"reasoning":{"effort":"xhigh"}}`)},
		HostCallbackID: "callback-test",
	})
	out, err := executeAutoMapping(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.ExecutorResponse `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !called || string(env.Result.Payload) != `{"model":"gpt-6-astra","status":"completed"}` {
		t.Fatalf("out=%s", out)
	}
}

func TestAutomaticForwardKeepsHTTPFailure(t *testing.T) {
	autoTestConfig(t)
	previous := hostRPC
	t.Cleanup(func() { hostRPC = previous })
	hostRPC = func(string, []byte) ([]byte, error) {
		return pluginabi.NewErrorEnvelope("model_not_found", "unknown provider for model gpt-6.1-sol", 400)
	}
	raw, _ := json.Marshal(autoExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gpt-6-astra", SourceFormat: "openai-response", Payload: []byte(`{}`)}, HostCallbackID: "callback-test"})
	out, err := executeAutoMapping(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 400 || !strings.Contains(env.Error.Message, "gpt-6.1-sol") {
		t.Fatalf("failure hidden: %s", out)
	}
}

func TestAutoSSEByteSplits(t *testing.T) {
	for _, delimiter := range []string{"\n\n", "\r\n\r\n"} {
		input := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"upstream\"}}" + delimiter + "data: [DONE]" + delimiter
		var r sseFrameRewriter
		var out []byte
		for i := range []byte(input) {
			part, err := r.push([]byte(input[i:i+1]), "visible", false)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, part...)
		}
		part, err := r.push(nil, "visible", true)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, part...)
		want := strings.ReplaceAll(input, "upstream", "visible")
		if string(out) != want {
			t.Fatalf("SSE corrupted:\ngot %q\nwant %q", out, want)
		}
	}
	var r sseFrameRewriter
	if _, err := r.push(bytes.Repeat([]byte("x"), (16<<20)+1), "visible", false); err == nil {
		t.Fatal("unbounded frame accepted")
	}
}

func TestAutoStreamShutdownClosesNestedStream(t *testing.T) {
	previous := hostRPC
	resetAutoStreams()
	readStarted := make(chan struct{})
	closed := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	hostRPC = func(method string, raw []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostModelStreamRead:
			close(readStarted)
			<-closed
			return okEnvelope(pluginapi.HostModelStreamReadResponse{Done: true})
		case pluginabi.MethodHostModelStreamClose:
			once.Do(func() { close(closed) })
		case pluginabi.MethodHostStreamClose:
			close(finished)
		}
		return okEnvelope(map[string]any{})
	}
	t.Cleanup(func() { stopAutoStreams(); hostRPC = previous; resetAutoStreams() })
	if !startAutoStream(autoExecutorRequest{StreamID: "out"}, "nested", "visible") {
		t.Fatal("not started")
	}
	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream not read")
	}
	stopAutoStreams()
	select {
	case <-finished:
	default:
		t.Fatal("downstream not closed")
	}
	if startAutoStream(autoExecutorRequest{StreamID: "out2"}, "nested2", "visible") {
		t.Fatal("accepted stream after quiesce")
	}
}

// OAuth Codex's native translator emits complete data events with no trailing
// newline. Buffering until a delimiter joins them into invalid JSON and yields
// "upstream stream closed before first payload" at CPA's HTTP framer.
func TestAutoSSENativeCodexEvents(t *testing.T) {
	var r sseFrameRewriter
	for _, event := range []string{"response.created", "response.completed"} {
		payload := []byte(`data: {"type":"` + event + `","response":{"model":"upstream"}}`)
		out, err := r.push(payload, "visible", false)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(string(payload), "upstream", "visible")
		if string(out) != want || len(r.pending) != 0 {
			t.Fatalf("native event buffered/corrupted: %q", out)
		}
	}
	if out, err := r.push(nil, "visible", true); err != nil || len(out) != 0 {
		t.Fatalf("unexpected trailing data: %q %v", out, err)
	}
	var withEvent sseFrameRewriter
	_, _ = withEvent.push([]byte("event: response.completed"), "visible", false)
	out, err := withEvent.push([]byte(`data: {"type":"response.completed","response":{"model":"upstream"}}`), "visible", false)
	if err != nil || !strings.Contains(string(out), "\ndata: ") || !strings.Contains(string(out), `"model":"visible"`) {
		t.Fatalf("native metadata lost: %q %v", out, err)
	}
}
