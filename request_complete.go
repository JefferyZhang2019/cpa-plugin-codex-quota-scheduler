package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleRequestComplete consumes the official per-request terminal events
// (schema v2+ RequestLifecyclePlugin). It complements usage.handle rather than
// replacing it: usage records carry the credential attribution and the failure
// body needed for reset parsing, while terminal events cover every intercepted
// request including rejected and canceled ones.
func handleRequestComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &completion); err != nil {
			return nil, err
		}
	}
	handleRequestCompletionEvent(globalState, completion, time.Now())
	return okEnvelope(struct{}{})
}

func handleRequestCompletionEvent(store *PluginState, completion pluginapi.RequestCompletion, now time.Time) {
	if store == nil || completion.RequestID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	switch completion.Outcome {
	case pluginapi.RequestCompletionRejected, pluginapi.RequestCompletionCanceled:
		// These never reach usage.handle; record them so operators can see why
		// requests disappeared before execution.
		store.RecordLog("info", "request.terminal", "请求在执行前被终止", map[string]any{
			"request_id":  completion.RequestID,
			"model":       completion.RequestedModel,
			"outcome":     string(completion.Outcome),
			"status_code": completion.StatusCode,
			"error":       redactSecrets(completion.Error),
		}, now)
	case pluginapi.RequestCompletionSucceeded:
		// Best-effort supplementary success signal: v7.3 terminal events have
		// no dedicated credential field, so only act when the host metadata
		// carries the selected auth id.
		if authID := metadataString(completion.Metadata, "selected_auth_id"); authID != "" {
			store.RecordAccountSuccess(authID, "", now)
		}
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}
