package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Nested host execution overwrites Alias with the routing target. Capture the
// outer request before that happens and join usage using the shared HTTP trace
// plus client HMAC. Persist only identity metadata, never bodies or credentials.
type originalModel struct {
	Model string
	At    time.Time
}

var originalModels = struct {
	sync.Mutex
	Entries map[string]originalModel
}{Entries: make(map[string]originalModel)}

var modelJournalMu sync.Mutex

func clientUsageHash(key, secret string) string {
	index := hmac.New(sha256.New, []byte(secret))
	index.Write([]byte("cap-token-usage-tracker/api-key-index/v1"))
	digest := hmac.New(sha256.New, index.Sum(nil))
	digest.Write([]byte(key))
	return hex.EncodeToString(digest.Sum(nil))[:32]
}

func rememberRequestedModel(req pluginapi.RequestInterceptRequest) {
	cfg := currentConfig()
	key := clientAPIKey(req.Headers)
	model := strings.TrimSpace(firstNonEmpty(req.RequestedModel, req.Model))
	if cfg.RequestedModelLogDir == "" || key == "" || req.TraceID == "" || model == "" {
		return
	}
	id := req.TraceID + ":" + clientUsageHash(key, cfg.TrackerAPIKeySecret)
	now := time.Now()
	originalModels.Lock()
	defer originalModels.Unlock()
	// Keep records after completion: usage delivery is asynchronous, and retries
	// may publish more than one native usage event for one inbound request.
	if len(originalModels.Entries) >= 100000 {
		for k, v := range originalModels.Entries {
			if now.Sub(v.At) > 24*time.Hour {
				delete(originalModels.Entries, k)
			}
		}
		if len(originalModels.Entries) >= 100000 {
			return
		}
	}
	if old, ok := originalModels.Entries[id]; ok && now.Sub(old.At) <= 24*time.Hour {
		if old.Model != model {
			// A trace collision must never relabel either request.
			old.Model = ""
			originalModels.Entries[id] = old
		}
		return
	}
	originalModels.Entries[id] = originalModel{Model: model, At: now}
}

type requestedModelEvent struct {
	RequestID      string `json:"request_id,omitempty"`
	TraceID        string `json:"trace_id,omitempty"`
	Time           string `json:"time"`
	APIKeyHash     string `json:"api_key_hash"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model"`
}

func recordRequestedModelUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	if cfg.RequestedModelLogDir == "" || record.APIKey == "" || record.TraceID == "" || record.RequestedAt.IsZero() {
		return okEnvelope(map[string]any{})
	}
	hash := clientUsageHash(record.APIKey, cfg.TrackerAPIKeySecret)
	originalModels.Lock()
	original, found := originalModels.Entries[record.TraceID+":"+hash]
	originalModels.Unlock()
	if !found || original.Model == "" || time.Since(original.At) > 24*time.Hour {
		return okEnvelope(map[string]any{})
	}
	event := requestedModelEvent{
		RequestID: record.RequestID, TraceID: record.TraceID,
		Time: record.RequestedAt.UTC().Format(time.RFC3339Nano), APIKeyHash: hash,
		Provider: record.Provider, Model: record.Model, RequestedModel: original.Model,
	}
	line, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	modelJournalMu.Lock()
	defer modelJournalMu.Unlock()
	if err = os.MkdirAll(cfg.RequestedModelLogDir, 0700); err == nil {
		path := filepath.Join(cfg.RequestedModelLogDir, record.RequestedAt.UTC().Format("2006-01-02")+".jsonl")
		var file *os.File
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			_, err = file.Write(append(line, '\n'))
			if err == nil {
				err = file.Sync()
			}
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		// Recording failures must not change inference or disable the policy.
		logf("requested-model journal write failed: %s", fmt.Sprint(err))
	}
	return okEnvelope(map[string]any{})
}
