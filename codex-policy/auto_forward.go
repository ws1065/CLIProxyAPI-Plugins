package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Auto routing delegates the mapped model to CPA's native model registry and
// auth scheduler. The caller callback ID makes CPA skip this plugin's router
// and interceptors in the nested request, preventing recursion and double work.
var hostRPC = callNativeHost

type autoExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id"`
}

type autoHostRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id"`
}

func hostResult(method string, request, result any) error {
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return err
	}
	raw, err := hostRPC(method, requestBytes)
	if err != nil {
		return err
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode CPA callback %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return env.Error
		}
		return fmt.Errorf("CPA callback %s failed", method)
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}

func autoError(err error) ([]byte, error) {
	status := http.StatusBadGateway
	if e, ok := err.(interface{ StatusCode() int }); ok && e.StatusCode() > 0 {
		status = e.StatusCode()
	}
	return pluginabi.NewErrorEnvelope("auto_route_failed", err.Error(), status)
}

func executeAutoMapping(raw []byte, stream bool) ([]byte, error) {
	var req autoExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.HostCallbackID == "" {
		return autoError(fmt.Errorf("CPA did not supply a callback ID for automatic routing"))
	}
	cfg := currentConfig()
	decision, _ := resolveModelRoute(cfg, pluginapi.ModelRouteRequest{
		SourceFormat: req.SourceFormat, RequestedModel: req.Model, Headers: req.Headers,
	})
	if decision == nil || decision.Provider != "" {
		return pluginabi.NewErrorEnvelope("mapping_changed", "Mapping changed before execution; retry the request", http.StatusConflict)
	}
	// Payload includes the outer admission/effort interceptors' edits. Using
	// OriginalRequest here would silently restore the requested high effort.
	body, err := sjson.SetBytes(req.Payload, "model", decision.Target)
	if err != nil {
		return autoError(err)
	}
	request := autoHostRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: req.SourceFormat, ExitProtocol: req.SourceFormat,
			Model: decision.Target, Stream: stream, Body: body,
			Headers: req.Headers, Query: req.Query, Alt: req.Alt,
		},
		HostCallbackID: req.HostCallbackID,
	}
	visible := ""
	if boolValue(cfg.ModelMap.RewriteResponseModel, true) {
		visible = req.Model
	}
	if !stream {
		var response pluginapi.HostModelExecutionResponse
		if err := hostResult(pluginabi.MethodHostModelExecute, request, &response); err != nil {
			logAutoFailure(cfg, req, decision, err)
			return autoError(err)
		}
		if visible != "" {
			if rewritten := rewriteResponseModel(response.Body, visible); rewritten != nil {
				response.Body = rewritten
			}
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers})
	}
	if req.StreamID == "" {
		return autoError(fmt.Errorf("CPA did not supply a stream ID"))
	}
	var response pluginapi.HostModelStreamResponse
	if err := hostResult(pluginabi.MethodHostModelExecuteStream, request, &response); err != nil {
		logAutoFailure(cfg, req, decision, err)
		return autoError(err)
	}
	if response.StreamID == "" {
		return autoError(fmt.Errorf("CPA returned an empty model stream ID"))
	}
	if !startAutoStream(req, response.StreamID, visible) {
		_ = hostResult(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: response.StreamID}, nil)
		return autoError(fmt.Errorf("Plugin is shutting down"))
	}
	return okEnvelope(map[string]any{"headers": response.Headers})
}

func logAutoFailure(cfg pluginConfig, req autoExecutorRequest, decision *routeDecision, err error) {
	logDecision(cfg, decisionRecord{
		Phase: "execution", ClientKey: clientAPIKey(req.Headers),
		Source: req.SourceFormat, Client: clientUserAgent(req.Headers), ReqModel: req.Model,
		Route: decision, Actions: []string{"AUTO_PROVIDER_SELECTION", "EXECUTION_FAILED"},
		Signals: []string{err.Error()}, Result: "FAILED",
	})
}

var autoStreams = struct {
	mu       sync.Mutex
	stopping bool
	active   map[string]struct{}
	wg       sync.WaitGroup
}{active: make(map[string]struct{})}

func resetAutoStreams() {
	autoStreams.mu.Lock()
	autoStreams.stopping = false
	autoStreams.mu.Unlock()
}

func startAutoStream(req autoExecutorRequest, modelStreamID, visible string) bool {
	autoStreams.mu.Lock()
	defer autoStreams.mu.Unlock()
	if autoStreams.stopping {
		return false
	}
	autoStreams.active[modelStreamID] = struct{}{}
	autoStreams.wg.Add(1)
	go func() {
		defer autoStreams.wg.Done()
		defer func() {
			autoStreams.mu.Lock()
			delete(autoStreams.active, modelStreamID)
			autoStreams.mu.Unlock()
		}()
		forwardAutoStream(req, modelStreamID, visible)
	}()
	return true
}

func stopAutoStreams() {
	autoStreams.mu.Lock()
	autoStreams.stopping = true
	ids := make([]string, 0, len(autoStreams.active))
	for id := range autoStreams.active {
		ids = append(ids, id)
	}
	autoStreams.mu.Unlock()
	for _, id := range ids {
		_ = hostResult(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: id}, nil)
	}
	// All async callbacks must finish before the native library can be unloaded.
	autoStreams.wg.Wait()
}

func forwardAutoStream(req autoExecutorRequest, modelStreamID, visible string) {
	terminalError := ""
	defer func() {
		_ = hostResult(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: modelStreamID}, nil)
		_ = hostResult(pluginabi.MethodHostStreamClose, map[string]any{"stream_id": req.StreamID, "error": terminalError}, nil)
	}()
	emit := func(payload []byte) error {
		if len(payload) == 0 {
			return nil
		}
		return hostResult(pluginabi.MethodHostStreamEmit, map[string]any{
			"stream_id": req.StreamID, "payload": payload,
		}, nil)
	}
	var rewriter sseFrameRewriter
	for {
		var chunk pluginapi.HostModelStreamReadResponse
		if err := hostResult(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: modelStreamID}, &chunk); err != nil {
			terminalError = err.Error()
			return
		}
		payload := chunk.Payload
		if visible != "" && isCodexFormat(req.SourceFormat) {
			var err error
			payload, err = rewriter.push(payload, visible, chunk.Done)
			if err != nil {
				terminalError = err.Error()
				return
			}
		} else if visible != "" {
			if rewritten := rewriteResponseModel(payload, visible); rewritten != nil {
				payload = rewritten
			}
		}
		if err := emit(payload); err != nil {
			terminalError = err.Error()
			return
		}
		if chunk.Error != "" {
			terminalError = chunk.Error
			return
		}
		if chunk.Done {
			return
		}
	}
}

// Native stream chunks can split an SSE model field at any byte. Rewrite
// complete frames, preserve their delimiters, and bound pending memory.
type sseFrameRewriter struct{ pending []byte }

func (r *sseFrameRewriter) push(payload []byte, visible string, done bool) ([]byte, error) {
	// Codex's native translator emits one complete data event per chunk without
	// line delimiters. CPA's HTTP framer accepts that contract. Do not concatenate
	// these events into one invalid JSON data line while waiting for "\n\n".
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("data:")) && !bytes.Contains(payload, []byte("\n")) {
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if gjson.ValidBytes(data) || bytes.Equal(data, []byte("[DONE]")) {
			if len(r.pending) == 0 {
				if rewritten := rewriteResponseModel(payload, visible); rewritten != nil {
					return rewritten, nil
				}
				return payload, nil
			}
			if !hasSSEDataLine(string(r.pending)) {
				frame := append(r.pending, '\n')
				frame = append(frame, payload...)
				frame = append(frame, '\n', '\n')
				r.pending = nil
				if rewritten := rewriteResponseModel(frame, visible); rewritten != nil {
					return rewritten, nil
				}
				return frame, nil
			}
		}
	}
	r.pending = append(r.pending, payload...)
	var output []byte
	for {
		end, width := bytes.Index(r.pending, []byte("\n\n")), 2
		if crlf := bytes.Index(r.pending, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
			end, width = crlf, 4
		}
		if end < 0 {
			break
		}
		frame := r.pending[:end+width]
		if rewritten := rewriteResponseModel(frame, visible); rewritten != nil {
			frame = rewritten
		}
		output = append(output, frame...)
		r.pending = r.pending[end+width:]
	}
	if len(r.pending) > 16<<20 {
		return nil, fmt.Errorf("Automatic routing SSE frame exceeded 16 MiB")
	}
	if done && len(r.pending) > 0 {
		frame := r.pending
		if rewritten := rewriteResponseModel(frame, visible); rewritten != nil {
			frame = rewritten
		}
		output = append(output, frame...)
		r.pending = nil
	}
	if len(r.pending) == 0 {
		r.pending = nil
	}
	return output, nil
}

func providerLabel(provider string) string {
	if strings.TrimSpace(provider) == "" || provider == "auto" {
		return "auto (CPA)"
	}
	return provider
}
