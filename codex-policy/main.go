package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

static int policy_host_call(cliproxy_host_api* host, const char* method,
    const uint8_t* request, size_t length, cliproxy_buffer* response) {
    return host->call(host->host_ctx, method, request, length, response);
}
static void policy_host_free(cliproxy_host_api* host, void* ptr, size_t length) {
    host->free_buffer(ptr, length);
}

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

// pluginID is the key under plugins.configs.<pluginID> in config.yaml.
const pluginID = "codex-policy"

// pluginVersion is reported in the management menu and the audit log.
const pluginVersion = "0.5.0"

var nativeHost struct {
	mu  sync.RWMutex
	api C.cliproxy_host_api
}

var state = pluginState{config: defaultConfig()}

// routeTTL bounds how long a model.route decision stays available to the
// before-auth interceptor, which runs later in the same request but has no
// access to the router's return value.
const routeTTL = 90 * time.Second

var routeCache = struct {
	mu      sync.Mutex
	entries map[string]routeDecision
}{entries: map[string]routeDecision{}}

type routeDecision struct {
	From     string
	Target   string
	Provider string
	At       time.Time
}

type pluginState struct {
	mu     sync.RWMutex
	config pluginConfig
	// raw keeps every key the host passed in, including host-only keys such as
	// "enabled" and "priority" that pluginConfig does not model.
	raw map[string]any
}

// effortConfig controls the reasoning-effort rewrite.
type effortConfig struct {
	Enabled       *bool    `yaml:"enabled"`
	Target        string   `yaml:"target"`
	DowngradeFrom []string `yaml:"downgrade_from"`
	Models        []string `yaml:"models"`
	SkipForExempt *bool    `yaml:"skip_for_exempt_keys"`
}

// modelRule maps one client-visible model onto another model of a provider.
type modelRule struct {
	From     string   `yaml:"from"`
	To       string   `yaml:"to"`
	Provider string   `yaml:"provider"`
	Keys     []string `yaml:"keys"`
}

// modelMapConfig controls the ModelRouter hook used for client-transparent
// model forwarding.
type modelMapConfig struct {
	Enabled              *bool       `yaml:"enabled"`
	Provider             string      `yaml:"provider"`
	Rules                []modelRule `yaml:"rules"`
	PreserveSuffix       *bool       `yaml:"preserve_thinking_suffix"`
	RewriteResponseModel *bool       `yaml:"rewrite_response_model"`
	SkipForExemptKeys    *bool       `yaml:"skip_for_exempt_keys"`
}

// pluginConfig is the plugins.configs.codex-policy section.
type pluginConfig struct {
	EnforceOfficialClient *bool          `yaml:"enforce_official_client"`
	RejectStatus          int            `yaml:"reject_status"`
	RejectMessage         string         `yaml:"reject_message"`
	OfficialMinScore      int            `yaml:"official_min_score"`
	ApplyToNonCodex       *bool          `yaml:"apply_to_non_codex"`
	EnforceAllProtocols   *bool          `yaml:"enforce_all_protocols"`
	ExemptAPIKeys         []string       `yaml:"exempt_api_keys"`
	ExemptBypassAdmission *bool          `yaml:"exempt_bypass_admission"`
	Effort                effortConfig   `yaml:"effort"`
	ModelMap              modelMapConfig `yaml:"model_map"`
	ProbeModel            string         `yaml:"probe_model"`
	ProbeHeader           string         `yaml:"probe_header"`
	ProbeHeaderValue      string         `yaml:"probe_header_value"`
	ConfigPath            string         `yaml:"config_path"`
	LogDecisions          *bool          `yaml:"log_decisions"`
	LogFormat             string         `yaml:"log_format"`
	LogBufferSize         *int           `yaml:"log_buffer_size"`
}

func defaultConfig() pluginConfig {
	t := true
	enforce := true
	applyNonCodex := false
	enforceAll := true
	logDecisions := true
	return pluginConfig{
		EnforceOfficialClient: &enforce,
		RejectStatus:          http.StatusForbidden,
		RejectMessage:         "客户端不支持访问",
		OfficialMinScore:      5,
		ApplyToNonCodex:       &applyNonCodex,
		EnforceAllProtocols:   &enforceAll,
		ExemptBypassAdmission: &t,
		Effort: effortConfig{
			Enabled:       &t,
			Target:        "medium",
			DowngradeFrom: []string{"xhigh", "max", "ultra"},
			SkipForExempt: &t,
		},
		ModelMap: modelMapConfig{
			Enabled:              &t,
			PreserveSuffix:       &t,
			RewriteResponseModel: &t,
			SkipForExemptKeys:    &t,
		},
		ProbeModel:       "",
		ProbeHeader:      "X-Policy-Probe",
		ProbeHeaderValue: "1",
		LogDecisions:     &logDecisions,
		LogFormat:        "table",
		LogBufferSize:    intPtr(200),
	}
}

func intPtr(value int) *int {
	return &value
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	RequestInterceptor    bool     `json:"request_interceptor"`
	ModelRouter           bool     `json:"model_router"`
	ResponseInterceptor   bool     `json:"response_interceptor"`
	StreamChunkIntercept  bool     `json:"response_stream_interceptor"`
	ManagementAPI         bool     `json:"management_api"`
}

type probeResult struct {
	Admitted       bool     `json:"admitted"`
	Score          int      `json:"score"`
	MinScore       int      `json:"min_score"`
	Signals        []string `json:"signals"`
	Exempt         bool     `json:"exempt"`
	SourceFormat   string   `json:"source_format"`
	RequestedModel string   `json:"requested_model"`
	RoutedModel    string   `json:"routed_model,omitempty"`
	RouteProvider  string   `json:"route_provider,omitempty"`
	OriginalEffort string   `json:"original_effort"`
	FinalEffort    string   `json:"final_effort"`
	Rewritten      bool     `json:"rewritten"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		nativeHost.mu.Lock()
		nativeHost.api = *host
		nativeHost.mu.Unlock()
	}
	resetAutoStreams()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { stopAutoStreams() }

func callNativeHost(method string, payload []byte) ([]byte, error) {
	nativeHost.mu.RLock()
	api := nativeHost.api
	nativeHost.mu.RUnlock()
	if api.call == nil || api.free_buffer == nil {
		return nil, fmt.Errorf("CPA host callback is unavailable")
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	var input unsafe.Pointer
	if len(payload) > 0 {
		input = C.CBytes(payload)
		defer C.free(input)
	}
	var out C.cliproxy_buffer
	rc := C.policy_host_call(&api, name, (*C.uint8_t)(input), C.size_t(len(payload)), &out)
	if out.ptr != nil {
		defer C.policy_host_free(&api, out.ptr, out.len)
	}
	if out.len > 1<<30 {
		return nil, fmt.Errorf("CPA callback response too large")
	}
	if out.ptr != nil && out.len > 0 {
		// Preserve the host's error envelope, including HTTP status.
		return C.GoBytes(out.ptr, C.int(out.len)), nil
	}
	return nil, fmt.Errorf("CPA host callback %s failed (code=%d)", method, rc)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequest(request)
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)
	case pluginabi.MethodRequestInterceptAfter, pluginabi.MethodRequestComplete:
		return passthroughRequest(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return executeAutoMapping(request, false)
	case pluginabi.MethodExecutorExecuteStream:
		return executeAutoMapping(request, true)
	case pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		return pluginabi.NewErrorEnvelope("unsupported_operation", "Automatic supplier selection supports inference requests only", http.StatusNotImplemented)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		stopAutoStreams()
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	return configureRaw(req.ConfigYAML)
}

// configureRaw normalizes and installs a plugin config block. It is used both
// by the host's reconfigure call and after the management API rewrites
// config.yaml in place, so both paths clamp values identically.
func configureRaw(configYAML []byte) error {
	cfg := defaultConfig()
	if len(configYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(configYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg.RejectMessage = strings.TrimSpace(cfg.RejectMessage)
	if cfg.RejectMessage == "" {
		cfg.RejectMessage = "客户端不支持访问"
	}
	if cfg.RejectStatus < 200 || cfg.RejectStatus > 599 {
		cfg.RejectStatus = http.StatusForbidden
	}
	if cfg.OfficialMinScore < 1 {
		cfg.OfficialMinScore = 1
	}
	cfg.Effort.Target = strings.ToLower(strings.TrimSpace(cfg.Effort.Target))
	for i := range cfg.Effort.DowngradeFrom {
		cfg.Effort.DowngradeFrom[i] = strings.ToLower(strings.TrimSpace(cfg.Effort.DowngradeFrom[i]))
	}
	cfg.ProbeModel = strings.TrimSpace(cfg.ProbeModel)
	cfg.ConfigPath = strings.TrimSpace(cfg.ConfigPath)
	cfg.LogFormat = strings.ToLower(strings.TrimSpace(cfg.LogFormat))
	if cfg.LogFormat != "line" {
		cfg.LogFormat = "table"
	}
	cfg.ModelMap.Provider = strings.ToLower(strings.TrimSpace(cfg.ModelMap.Provider))
	for i := range cfg.ModelMap.Rules {
		cfg.ModelMap.Rules[i].From = strings.TrimSpace(cfg.ModelMap.Rules[i].From)
		cfg.ModelMap.Rules[i].To = strings.TrimSpace(cfg.ModelMap.Rules[i].To)
		cfg.ModelMap.Rules[i].Provider = strings.ToLower(strings.TrimSpace(cfg.ModelMap.Rules[i].Provider))
	}

	state.mu.Lock()
	state.config = cfg
	state.raw = map[string]any{}
	if len(configYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(configYAML, &state.raw); errUnmarshal != nil {
			state.raw = map[string]any{}
		}
	}
	state.mu.Unlock()
	logf("configured: enforce_official_client=%v min_score=%d reject_status=%d reject_message=%q effort.enabled=%v downgrade_from=%v target=%q model_map.enabled=%v rules=%d default_provider=%q rewrite_response_model=%v exempt_keys=%d probe_model=%q",
		boolValue(cfg.EnforceOfficialClient, true), cfg.OfficialMinScore, cfg.RejectStatus, cfg.RejectMessage,
		boolValue(cfg.Effort.Enabled, true), cfg.Effort.DowngradeFrom, cfg.Effort.Target,
		boolValue(cfg.ModelMap.Enabled, true), len(cfg.ModelMap.Rules), providerLabel(cfg.ModelMap.Provider),
		boolValue(cfg.ModelMap.RewriteResponseModel, true), len(cfg.ExemptAPIKeys), cfg.ProbeModel)
	for _, rule := range cfg.ModelMap.Rules {
		provider := rule.Provider
		if provider == "" {
			provider = cfg.ModelMap.Provider
		}
		logf("model-map rule: %s -> %s (provider=%s, keys=%d)", rule.From, rule.To, providerLabel(provider), len(rule.Keys))
	}
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "local",
			GitHubRepository: "local/codex-policy",
			ConfigFields:     configFields(),
		},
		Capabilities: registrationCapability{
			Executor: true, ExecutorModelScope: "static",
			ExecutorInputFormats:  []string{"openai-response", "openai", "claude", "gemini"},
			ExecutorOutputFormats: []string{"openai-response", "openai", "claude", "gemini"},
			RequestInterceptor:    true,
			ModelRouter:           true,
			ResponseInterceptor:   true,
			StreamChunkIntercept:  true,
			ManagementAPI:         true,
		},
	}
}

// interceptRequest applies admission control and the reasoning-effort rewrite
// before credential selection.
func interceptRequest(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := currentConfig()
	isCodex := isCodexFormat(req.SourceFormat)
	// The official-client restriction is a property of the *client*, not of the
	// endpoint it happens to call, so admission is evaluated on every protocol
	// unless the operator explicitly opts out via enforce_all_protocols=false.
	// apply_to_non_codex keeps its narrower meaning: whether model/effort
	// rewriting also reaches non-Responses traffic.
	admissionApplies := isCodex || boolValue(cfg.EnforceAllProtocols, true)
	rewriteApplies := isCodex || boolValue(cfg.ApplyToNonCodex, false)
	if !admissionApplies {
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
	}

	clientKey := clientAPIKey(req.Headers)
	exempt := isExempt(cfg, clientKey)
	route := lookupRouteDecision(clientKey, req.SourceFormat, firstNonEmpty(req.RequestedModel, req.Model))
	score, signals, strong := detectOfficialClient(req)
	admitted := strong && score >= cfg.OfficialMinScore
	if !admitted && exempt && boolValue(cfg.ExemptBypassAdmission, true) {
		admitted = true
		signals = append(signals, "exempt_key_bypass")
	}

	if !admitted {
		logDecision(cfg, decisionRecord{
			Phase: "policy", RequestID: req.RequestID, ClientKey: clientKey, Exempt: exempt,
			Source: req.SourceFormat, Client: clientUserAgent(req.Headers),
			ReqModel: firstNonEmpty(req.RequestedModel, req.Model), Score: score, MinScore: cfg.OfficialMinScore,
			Signals: signals, Admitted: false, Route: route, Result: "REJECTED",
		})
		if boolValue(cfg.EnforceOfficialClient, true) {
			status := cfg.RejectStatus
			if status <= 0 {
				status = http.StatusForbidden
			}
			return terminated(status, map[string]any{
				"error": map[string]any{
					"message": cfg.RejectMessage,
					"type":    "invalid_request_error",
					"code":    "client_not_supported",
				},
			})
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body})
	}

	body := req.Body
	originalEffort := readEffort(body)
	finalEffort := originalEffort
	rewritten := false
	if rewriteApplies && shouldRewriteEffort(cfg, req, exempt) && isDowngradeTarget(cfg, originalEffort) {
		path := "reasoning.effort"
		if gjson.GetBytes(body, "reasoning_effort").Exists() {
			path = "reasoning_effort"
		}
		if updated, errSet := sjson.SetBytes(body, path, cfg.Effort.Target); errSet == nil {
			body = updated
			finalEffort = cfg.Effort.Target
			rewritten = true
		}
	}
	actions := make([]string, 0, 3)
	if route != nil {
		actions = append(actions, "MODEL_MAPPED")
		if route.Provider == "" {
			actions = append(actions, "AUTO_PROVIDER_SELECTION")
		}
	}
	if rewritten {
		actions = append(actions, "EFFORT_DOWNGRADED")
	}
	if exempt {
		actions = append(actions, "EXEMPT_KEY")
	}
	if len(actions) == 0 {
		actions = append(actions, "NONE")
	}
	logDecision(cfg, decisionRecord{
		Phase: "policy", RequestID: req.RequestID, ClientKey: clientKey, Exempt: exempt,
		Source: req.SourceFormat, Client: clientUserAgent(req.Headers),
		ReqModel:   firstNonEmpty(req.RequestedModel, req.Model),
		EffortFrom: originalEffort, EffortTo: finalEffort, EffortDone: rewritten,
		Score: score, MinScore: cfg.OfficialMinScore, Signals: signals, Admitted: true,
		Route: route, Actions: actions, Result: "FORWARDED",
	})

	if isProbeRequest(cfg, req) {
		return terminated(http.StatusOK, map[string]any{
			"probe": probeResult{
				Admitted:       true,
				Score:          score,
				MinScore:       cfg.OfficialMinScore,
				Signals:        signals,
				Exempt:         exempt,
				SourceFormat:   req.SourceFormat,
				RequestedModel: firstNonEmpty(req.RequestedModel, req.Model),
				RoutedModel:    routeTarget(route),
				RouteProvider:  routeProvider(route),
				OriginalEffort: originalEffort,
				FinalEffort:    finalEffort,
				Rewritten:      rewritten,
			},
		})
	}

	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: body})
}

// routeModel implements the ModelRouter hook. It runs before model-to-provider
// resolution and credential selection, which is the only place where the
// upstream model can be replaced: request interceptors run later and the
// executors overwrite any body "model" field with the routed model.
func routeModel(raw []byte) ([]byte, error) {
	response := pluginapi.ModelRouteResponse{}
	var req pluginapi.ModelRouteRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg := currentConfig()
	decision, rule := resolveModelRoute(cfg, req)
	if decision == nil {
		return okEnvelope(response)
	}
	// The host validates provider availability after this hook returns. Never
	// claim that a rejected proposal was routed or cache it as an applied map.
	if decision.Provider != "" && req.AvailableProviders != nil {
		available := false
		for _, provider := range req.AvailableProviders {
			if strings.EqualFold(strings.TrimSpace(provider), decision.Provider) {
				available = true
				break
			}
		}
		if !available {
			logDecision(cfg, decisionRecord{
				Phase: "route", ClientKey: clientAPIKey(req.Headers),
				Source: req.SourceFormat, Client: clientUserAgent(req.Headers), ReqModel: req.RequestedModel,
				Route: decision, Actions: []string{"PROVIDER_UNAVAILABLE"},
				Signals: append([]string{"configured_provider=" + decision.Provider}, req.AvailableProviders...),
				Result:  "ROUTE_REJECTED",
			})
			logf("模型映射未应用 / model mapping not applied: model=%q target=%q configured_provider=%q available_providers=%v; 请将 model_map.provider 设置为启用的提供商", req.RequestedModel, decision.Target, decision.Provider, req.AvailableProviders)
			return okEnvelope(response)
		}
	}
	response.Handled = true
	response.TargetKind = pluginapi.ModelRouteTargetProvider
	response.Target = decision.Provider
	response.TargetModel = decision.Target
	if decision.Provider == "" {
		response.TargetKind = pluginapi.ModelRouteTargetSelf
		response.Target = pluginID
	}
	response.Reason = "codex-policy model_map: " + rule
	rememberRouteDecision(routeCacheKey(clientAPIKey(req.Headers), req.SourceFormat, req.RequestedModel), *decision)
	actions := []string{"MODEL_MAPPING_PROPOSED"}
	if decision.Provider == "" {
		actions = append(actions, "AUTO_PROVIDER_SELECTION")
	}
	logDecision(cfg, decisionRecord{
		Phase: "route", ClientKey: clientAPIKey(req.Headers), Exempt: isExempt(cfg, clientAPIKey(req.Headers)),
		Source: req.SourceFormat, Client: clientUserAgent(req.Headers), ReqModel: req.RequestedModel,
		Route: decision, Admitted: true, Actions: actions, Result: "ROUTE_PROPOSED",
	})
	return okEnvelope(response)
}

// resolveModelRoute finds the first mapping rule that applies to the request.
func resolveModelRoute(cfg pluginConfig, req pluginapi.ModelRouteRequest) (*routeDecision, string) {
	if !boolValue(cfg.ModelMap.Enabled, true) || len(cfg.ModelMap.Rules) == 0 {
		return nil, ""
	}
	if !isCodexFormat(req.SourceFormat) && !boolValue(cfg.ApplyToNonCodex, false) {
		return nil, ""
	}
	clientKey := clientAPIKey(req.Headers)
	if isExempt(cfg, clientKey) && boolValue(cfg.ModelMap.SkipForExemptKeys, true) {
		return nil, ""
	}
	baseModel, suffix := splitThinkingSuffix(strings.TrimSpace(req.RequestedModel))
	for _, rule := range cfg.ModelMap.Rules {
		if rule.From == "" || rule.To == "" {
			continue
		}
		if !matchesModel(rule.From, baseModel) {
			continue
		}
		if len(rule.Keys) > 0 && !keyListed(rule.Keys, clientKey) {
			continue
		}
		provider := rule.Provider
		if provider == "" {
			provider = cfg.ModelMap.Provider
		}
		if provider == "auto" {
			provider = ""
		}
		target := rule.To
		if suffix != "" && boolValue(cfg.ModelMap.PreserveSuffix, true) {
			target += suffix
		}
		return &routeDecision{From: baseModel, Target: target, Provider: provider, At: time.Now()}, rule.From + " -> " + rule.To
	}
	return nil, ""
}

func keyListed(keys []string, clientKey string) bool {
	if strings.TrimSpace(clientKey) == "" {
		return false
	}
	for _, candidate := range keys {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(clientKey)) == 1 {
			return true
		}
	}
	return false
}

// splitThinkingSuffix splits "model(effort)" into its model and suffix parts.
func splitThinkingSuffix(model string) (string, string) {
	open := strings.LastIndex(model, "(")
	if open < 0 || !strings.HasSuffix(model, ")") || open >= len(model)-1 {
		return model, ""
	}
	return strings.TrimSpace(model[:open]), model[open:]
}

// interceptResponse restores the client-visible model name on non-streaming
// responses so a mapped request stays indistinguishable from a direct one.
func interceptResponse(raw []byte) ([]byte, error) {
	response := pluginapi.ResponseInterceptResponse{}
	var req pluginapi.ResponseInterceptRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg := currentConfig()
	visible := clientVisibleModel(cfg, req.Model, req.RequestedModel)
	if visible == "" || len(req.Body) == 0 {
		return okEnvelope(response)
	}
	body := rewriteResponseModel(req.Body, visible)
	if body == nil {
		return okEnvelope(response)
	}
	response.Body = body
	return okEnvelope(response)
}

// interceptStreamChunk applies the same restore to streamed SSE chunks.
func interceptStreamChunk(raw []byte) ([]byte, error) {
	response := pluginapi.StreamChunkInterceptResponse{}
	var req pluginapi.StreamChunkInterceptRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg := currentConfig()
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		return okEnvelope(response)
	}
	visible := clientVisibleModel(cfg, req.Model, req.RequestedModel)
	if visible == "" || len(req.Body) == 0 {
		return okEnvelope(response)
	}
	body := rewriteResponseModel(req.Body, visible)
	if body == nil {
		return okEnvelope(response)
	}
	response.Body = body
	return okEnvelope(response)
}

// clientVisibleModel returns the model name the client asked for when a route
// mapping replaced it, and an empty string when nothing was mapped.
func clientVisibleModel(cfg pluginConfig, routed, requested string) string {
	if !boolValue(cfg.ModelMap.RewriteResponseModel, true) {
		return ""
	}
	routed = strings.TrimSpace(routed)
	requested = strings.TrimSpace(requested)
	if routed == "" || requested == "" || routed == requested {
		return ""
	}
	return requested
}

// rewriteResponseModel overwrites every model field of a JSON or SSE payload.
// It returns nil when the payload carries no model field at all.
func rewriteResponseModel(payload []byte, visible string) []byte {
	if len(payload) == 0 || !strings.Contains(string(payload), "\"model\"") {
		return nil
	}
	trimmed := strings.TrimLeft(string(payload), " \t\r\n")
	// A plain JSON document is always rewritten as JSON, even when its text
	// happens to contain "data: " (echoed input, model output, ...). Sniffing
	// for that substring instead corrupts the body by splicing the prefix back
	// in front of the rewritten JSON.
	if looksLikeJSON(trimmed) {
		return rewriteJSONModel([]byte(trimmed), visible)
	}
	if !hasSSEDataLine(trimmed) {
		return nil
	}
	return rewriteSSE(payload, visible)
}

// looksLikeJSON reports whether the payload is a bare JSON document rather
// than an SSE stream.
func looksLikeJSON(trimmed string) bool {
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return gjson.Valid(trimmed)
}

// hasSSEDataLine reports whether at least one line carries an SSE data field.
func hasSSEDataLine(payload string) bool {
	for _, line := range strings.Split(payload, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			return true
		}
	}
	return false
}

func rewriteJSONModel(payload []byte, visible string) []byte {
	if !gjson.ValidBytes(payload) {
		return nil
	}
	updated := payload
	changed := false
	for _, path := range []string{"model", "response.model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, errSet := sjson.SetBytes(updated, path, visible)
		if errSet != nil {
			continue
		}
		updated = next
		changed = true
	}
	if !changed {
		return nil
	}
	return updated
}

func rewriteSSE(payload []byte, visible string) []byte {
	lines := strings.Split(string(payload), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if trimmed == "" || trimmed == "[DONE]" || !gjson.Valid(trimmed) {
			continue
		}
		updated := rewriteJSONModel([]byte(trimmed), visible)
		if updated == nil {
			continue
		}
		// Guard the index: a payload can mix SSE and bare-JSON lines, and a
		// missing "data:" would otherwise slice with -1 and panic, which the
		// host turns into a fused (permanently disabled) plugin.
		idx := strings.Index(line, "data:")
		if idx < 0 {
			continue
		}
		prefix := line[:idx]
		ending := ""
		if strings.HasSuffix(line, "\r") {
			ending = "\r"
		}
		lines[i] = prefix + "data: " + string(updated) + ending
		changed = true
	}
	if !changed {
		return nil
	}
	return []byte(strings.Join(lines, "\n"))
}

func passthroughRequest(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// isProbeRequest reports whether the request carries the explicit probe marker.
// A matching model name alone is never enough: a probe model that also serves
// real traffic would silently truncate live streaming responses.
func isProbeRequest(cfg pluginConfig, req pluginapi.RequestInterceptRequest) bool {
	if cfg.ProbeModel == "" || !matchesModel(cfg.ProbeModel, firstNonEmpty(req.RequestedModel, req.Model)) {
		return false
	}
	header := strings.TrimSpace(cfg.ProbeHeader)
	if header == "" {
		return false
	}
	return strings.TrimSpace(headerValue(req.Headers, header)) == cfg.ProbeHeaderValue
}

func terminated(status int, payload map[string]any) ([]byte, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: headers,
		ResponseBody:    body,
	})
}

// detectOfficialClient scores the request against signals emitted by the
// official Codex clients. A strong signal (responses-lite header, originator,
// or codex user agent) is required; the score is an additional sanity check.
func detectOfficialClient(req pluginapi.RequestInterceptRequest) (int, []string, bool) {
	score := 0
	strong := false
	signals := make([]string, 0, 8)

	if strings.EqualFold(strings.TrimSpace(headerValue(req.Headers, "X-OpenAI-Internal-Codex-Responses-Lite")), "true") {
		score += 2
		strong = true
		signals = append(signals, "responses_lite")
	}
	originator := strings.ToLower(strings.TrimSpace(headerValue(req.Headers, "Originator")))
	if originator != "" && strings.Contains(originator, "codex") {
		score += 2
		strong = true
		signals = append(signals, "originator:"+originator)
	}
	userAgent := strings.ToLower(strings.TrimSpace(headerValue(req.Headers, "User-Agent")))
	if userAgent != "" && (strings.HasPrefix(userAgent, "codex-tui/") || strings.HasPrefix(userAgent, "codex_cli_rs/") || strings.HasPrefix(userAgent, "codex-cli/") || strings.HasPrefix(userAgent, "codex/")) {
		score += 2
		strong = true
		signals = append(signals, "user_agent:"+truncate(userAgent, 48))
	}
	if raw := strings.TrimSpace(headerValue(req.Headers, "X-Codex-Turn-Metadata")); raw != "" && gjson.Valid(raw) && gjson.Parse(raw).IsObject() {
		score++
		signals = append(signals, "turn_metadata")
	}
	if headerValue(req.Headers, "X-Codex-Window-Id") != "" || headerValue(req.Headers, "X-Client-Request-Id") != "" {
		score++
		signals = append(signals, "window_or_client_request_id")
	}
	if headerValue(req.Headers, "X-Codex-Beta-Features") != "" {
		score++
		signals = append(signals, "beta_features")
	}
	if headerValue(req.Headers, "Session_id") != "" || headerValue(req.Headers, "Session-Id") != "" || headerValue(req.Headers, "Thread-Id") != "" {
		score++
		signals = append(signals, "session_header")
	}
	if bodySignal(req.Body) {
		score++
		signals = append(signals, "body_shape")
	}
	return score, signals, strong
}

func bodySignal(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if gjson.GetBytes(body, "store").Type == gjson.False {
		return true
	}
	for _, path := range []string{"prompt_cache_key", "client_metadata", "text.verbosity", "include", "service_tier"} {
		if gjson.GetBytes(body, path).Exists() {
			return true
		}
	}
	if instructions := gjson.GetBytes(body, "instructions").String(); strings.HasPrefix(instructions, "You are Codex") {
		return true
	}
	return false
}

func shouldRewriteEffort(cfg pluginConfig, req pluginapi.RequestInterceptRequest, exempt bool) bool {
	if !boolValue(cfg.Effort.Enabled, true) {
		return false
	}
	if exempt && boolValue(cfg.Effort.SkipForExempt, true) {
		return false
	}
	if len(cfg.Effort.Models) == 0 {
		return true
	}
	model := firstNonEmpty(req.RequestedModel, req.Model)
	for _, pattern := range cfg.Effort.Models {
		if matchesModel(pattern, model) {
			return true
		}
	}
	return false
}

func isDowngradeTarget(cfg pluginConfig, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return false
	}
	for _, candidate := range cfg.Effort.DowngradeFrom {
		if candidate != "" && candidate == effort {
			return true
		}
	}
	return false
}

func readEffort(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	if effort := gjson.GetBytes(body, "reasoning.effort"); effort.Type == gjson.String {
		return effort.String()
	}
	if effort := gjson.GetBytes(body, "reasoning_effort"); effort.Type == gjson.String {
		return effort.String()
	}
	return ""
}

func isExempt(cfg pluginConfig, clientKey string) bool {
	if clientKey == "" {
		return false
	}
	for _, candidate := range cfg.ExemptAPIKeys {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(clientKey)) == 1 {
			return true
		}
	}
	return false
}

func clientAPIKey(headers http.Header) string {
	value := strings.TrimSpace(headerValue(headers, "Authorization"))
	if value == "" {
		return ""
	}
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

// headerValue is a case-insensitive header lookup that tolerates maps that
// were not canonicalized by net/http.
func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func isCodexFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "codex", "responses", "openai-response", "openai-responses", "response":
		return true
	default:
		return false
	}
}

// matchesModel supports '*' wildcards, e.g. "gpt-*".
func matchesModel(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	segments := strings.Split(pattern, "*")
	rest := model
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		idx := strings.Index(rest, segment)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false
		}
		rest = rest[idx+len(segment):]
	}
	last := segments[len(segments)-1]
	if last != "" && !strings.HasSuffix(model, last) {
		return false
	}
	return true
}

func currentConfig() pluginConfig {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.config
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncate(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}

// decisionRecord is the single audit row rendered for every request the
// plugin touches. It carries both the routing decision (model mapping) and the
// admission/effort decision so one record answers "which key asked for which
// model at which effort, and what did the plugin do about it".
type decisionRecord struct {
	Phase      string
	RequestID  string
	ClientKey  string
	Exempt     bool
	Source     string
	Client     string
	ReqModel   string
	EffortFrom string
	EffortTo   string
	EffortDone bool
	Score      int
	MinScore   int
	Signals    []string
	Admitted   bool
	Route      *routeDecision
	Actions    []string
	Result     string
}

func logDecision(cfg pluginConfig, rec decisionRecord) {
	appendLogEntry(logEntry{
		Time:        pluginNow(),
		Phase:       rec.Phase,
		RequestID:   shortID(rec.RequestID),
		APIKey:      clientAPIKeyLabel(rec.ClientKey),
		Exempt:      rec.Exempt,
		Protocol:    rec.Source,
		Client:      truncate(rec.Client, 64),
		Model:       rec.ReqModel,
		RoutedModel: routeTarget(rec.Route),
		Provider:    routeProvider(rec.Route),
		Effort:      rec.EffortFrom,
		EffortFinal: rec.EffortTo,
		Score:       rec.Score,
		MinScore:    rec.MinScore,
		Admitted:    rec.Admitted,
		Actions:     rec.Actions,
		Signals:     rec.Signals,
		Result:      rec.Result,
	})
	if !boolValue(cfg.LogDecisions, true) {
		return
	}
	if cfg.LogFormat == "line" {
		logf("phase=%s req=%s key=%s exempt=%v format=%s client=%q model=%s routed=%s routed_provider=%s effort=%s->%s rewritten=%v admitted=%v score=%d/%d signals=%v actions=%v result=%s",
			rec.Phase, shortID(rec.RequestID), clientAPIKeyLabel(rec.ClientKey), rec.Exempt, rec.Source, truncate(rec.Client, 40),
			rec.ReqModel, routeTarget(rec.Route), routeProvider(rec.Route),
			firstNonEmpty(rec.EffortFrom, "-"), firstNonEmpty(rec.EffortTo, "-"),
			rec.EffortDone, rec.Admitted, rec.Score, rec.MinScore, rec.Signals, rec.Actions, rec.Result)
		return
	}
	logTable(rec)
}

func effortCell(rec decisionRecord) string {
	from := firstNonEmpty(rec.EffortFrom, "-")
	if !rec.EffortDone {
		return from + " (unchanged)"
	}
	return from + " -> " + firstNonEmpty(rec.EffortTo, "-")
}

func modelCell(rec decisionRecord) string {
	target := routeTarget(rec.Route)
	if target == "" {
		return fmt.Sprintf("%s (no mapping applied)", rec.ReqModel)
	}
	return fmt.Sprintf("%s -> %s  via provider=%s", rec.ReqModel, target, routeProvider(rec.Route))
}

func logTable(rec decisionRecord) {
	admission := "REJECTED"
	if rec.Admitted {
		admission = fmt.Sprintf("ALLOWED  score=%d/%d  signals=[%s]", rec.Score, rec.MinScore, strings.Join(rec.Signals, " "))
	}
	signals := strings.Join(rec.Signals, ", ")
	if signals == "" {
		signals = "-"
	}
	rows := [][2]string{
		{"phase", rec.Phase},
		{"time", time.Now().Format("2006-01-02 15:04:05.000")},
		{"request_id", shortID(rec.RequestID)},
		{"api_key", fmt.Sprintf("%s   exempt=%v", clientAPIKeyLabel(rec.ClientKey), rec.Exempt)},
		{"protocol", fmt.Sprintf("%s   client=%s", rec.Source, firstNonEmpty(truncate(rec.Client, 44), "unknown"))},
		{"model", modelCell(rec)},
		{"effort", effortCell(rec)},
		{"admission", admission},
		{"signals", signals},
		{"actions", strings.Join(rec.Actions, ", ")},
		{"result", rec.Result},
	}
	width := 0
	for _, row := range rows {
		if len(row[0]) > width {
			width = len(row[0])
		}
	}
	logf("+-- codex-policy %s", strings.Repeat("-", 60))
	for _, row := range rows {
		logf("|  %-*s : %s", width, row[0], row[1])
	}
	logf("+-- %s", strings.Repeat("-", 60))
}

func shortID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func clientUserAgent(headers http.Header) string {
	return headerValue(headers, "User-Agent")
}

func clientAPIKeyLabel(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "<none>"
	}
	if len(key) > 10 {
		return key[:4] + "..." + key[len(key)-4:]
	}
	return key
}

func routeTarget(route *routeDecision) string {
	if route == nil {
		return ""
	}
	return route.Target
}

func routeProvider(route *routeDecision) string {
	if route == nil {
		return ""
	}
	return providerLabel(route.Provider)
}

func routeCacheKey(key, format, model string) string {
	// The raw secret must not be used as a map key, and the masked form used
	// for display (first 4 + last 4) is not unique: two keys sharing those
	// eight characters would collide and cross-attribute one client's route
	// decision to another in the audit log. A digest is stable and unique.
	digest := sha256.Sum256([]byte(key))
	// Strip the thinking suffix so the router (which sees the raw client model)
	// and the interceptor (which may see it with or without the suffix) agree.
	baseModel, _ := splitThinkingSuffix(model)
	return hex.EncodeToString(digest[:16]) + "|" +
		strings.ToLower(strings.TrimSpace(format)) + "|" +
		strings.ToLower(strings.TrimSpace(baseModel))
}

func rememberRouteDecision(key string, decision routeDecision) {
	routeCache.mu.Lock()
	defer routeCache.mu.Unlock()
	now := time.Now()
	for cached, entry := range routeCache.entries {
		if now.Sub(entry.At) > routeTTL {
			delete(routeCache.entries, cached)
		}
	}
	routeCache.entries[key] = decision
}

func lookupRouteDecision(key, format, model string) *routeDecision {
	routeCache.mu.Lock()
	defer routeCache.mu.Unlock()
	entry, ok := routeCache.entries[routeCacheKey(key, format, model)]
	if !ok || time.Since(entry.At) > routeTTL {
		return nil
	}
	return &entry
}

func logf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "[codex-policy] %s\n", fmt.Sprintf(format, args...))
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
