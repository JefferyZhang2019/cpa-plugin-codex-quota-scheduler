package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Protocol names are the CPA wire formats the chain can be asked to carry. The
// executor declares all of them so a chained model resolved from any client
// protocol is routed to this plugin without an extra translation hop.
const (
	protocolClaude     = "claude"
	protocolOpenAI     = "openai"
	protocolOpenAIResp = "openai-response"
	protocolCodex      = "codex"
)

var retryChainInputFormats = []string{protocolOpenAIResp, protocolCodex, protocolOpenAI, protocolClaude}

// Budgets for the chain. The stall timeout bounds how long one attempt may
// stay silent before the chain gives up on it, the hold timeout bounds how long
// the client waits on a single attempt's handshake frames, and the chain
// deadline bounds the whole request. The frame and byte budgets mirror CPA's own
// bootstrap buffers so the chain holds no more than the built-in path would.
const (
	defaultRetryMaxAttempts         = 4
	maxRetryMaxAttempts             = 16
	defaultRetryStallTimeout        = 60 * time.Second
	defaultRetryHoldTimeout         = 90 * time.Second
	defaultRetryChainDeadline       = 240 * time.Second
	defaultRetryMaxFrames           = 4096
	defaultRetryMaxBytes      int64 = 8 << 20
)

// rpcExecutorRequest mirrors the host's executor.execute_stream payload.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// hostModelExecutionRequest mirrors the host's host.model.execute_stream
// payload. The SDK pinned by this module predates ForcedProvider/AuthID, so they
// are declared here; the host reads the same JSON keys.
type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	ForcedProvider string `json:"forced_provider,omitempty"`
	AuthID         string `json:"auth_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// callHostModel is the host callback used by the chain. callHostCallback
// flattens RPC error envelopes, which would hide the upstream status code, so
// the chain uses this status-aware variant instead.
var callHostModel = func(method string, payload any) (json.RawMessage, error) {
	return nil, fmt.Errorf("host callback %s unavailable", method)
}

// routeRetryChain claims a request only when a chain row applies to the
// requested model. Every other request keeps the built-in path untouched.
func routeRetryChain(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := activeConfig()
	if !cfg.RetryActive() {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "retry chain disabled"})
	}
	if !req.Stream {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "retry chain handles streaming requests only"})
	}
	if _, ok := retryPlanForRequest(cfg, req.RequestedModel); !ok {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "no retry chain for model"})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "retry chain",
	})
}

// retryPlanForRequest resolves the plan for the model a client asked for. The
// thinking-effort suffix a client may append is not part of the model identity,
// so the lookup ignores it and an exact match still wins when a chain row
// deliberately includes it.
func retryPlanForRequest(cfg Config, model string) (RetryPlan, bool) {
	if plan, ok := cfg.RetryPlanFor(model); ok {
		return plan, true
	}
	base, _, ok := splitRetryModelSuffix(model)
	if !ok {
		return RetryPlan{}, false
	}
	return cfg.RetryPlanFor(base)
}

// splitRetryModelSuffix splits "model(high)" into "model" and "high". It mirrors
// CPA's own suffix parsing so the chain re-applies the same effort to a
// fallback model the client asked for on the original one.
func splitRetryModelSuffix(model string) (string, string, bool) {
	trimmed := strings.TrimSpace(model)
	lastOpen := strings.LastIndex(trimmed, "(")
	if lastOpen < 0 || !strings.HasSuffix(trimmed, ")") {
		return trimmed, "", false
	}
	base := strings.TrimSpace(trimmed[:lastOpen])
	if base == "" {
		return trimmed, "", false
	}
	return base, trimmed[lastOpen+1 : len(trimmed)-1], true
}

// executorIdentifier is required for the host to accept this plugin as an
// executor target. It is never advertised as a provider: the plugin registers
// no models, so no client can select it directly.
func executorIdentifier() ([]byte, error) {
	return okEnvelope(map[string]any{"identifier": PluginID})
}

const retryExecutorUnsupported = "codex-fleet-manager retry executor only serves streaming requests"

// executeRetryChainStream starts the chain and returns headers immediately. The
// orchestration goroutine owns the plugin stream and always closes it.
func executeRetryChainStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("retry_executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("retry chain panic: %v", recovered))
			}
		}()
		runRetryChain(context.Background(), req, streamID)
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func executeRetryChainUnsupported(method string) ([]byte, error) {
	return errorEnvelope("retry_executor_error", method+": "+retryExecutorUnsupported), nil
}

// runRetryChain walks the attempt plan and owns the plugin stream lifetime.
func runRetryChain(ctx context.Context, req rpcExecutorRequest, streamID string) {
	cfg := activeConfig()
	requested := retryRequestedModel(req.ExecutorRequest)
	plan, ok := cfg.RetryPlanFor(requested)
	if !ok {
		plan = retrySingleTargetPlan(requested)
	}
	protocol := retryProtocol(req.ExecutorRequest)
	if protocol == "" {
		closePluginStream(streamID, "retry chain: request has no source protocol")
		return
	}
	if cfg.RetryShadow {
		runRetryShadow(ctx, cfg, req, streamID, plan, protocol)
		return
	}

	deadline := time.Now().Add(cfg.RetryChainDeadline)
	attempts := plan.TargetCount()
	globalRetryStats.requestStarted(requested, attempts)
	var lastReason string
	var lastKind retryFailureKind
	var lastAttempt attemptResult

	for index := 0; index < attempts; index++ {
		target, okTarget := plan.Target(index)
		if !okTarget {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			lastReason = "retry chain deadline exceeded before the next attempt"
			break
		}
		state := retryAttemptState{index: index, target: target, requested: plan.Requested}
		globalRetryStats.attemptStarted(requested, index, FormatRetryTarget(target))
		recordRetryLog("info", "retry.attempt_started", "重试链开始尝试", map[string]any{
			"requested_model": requested,
			"attempt":         index + 1,
			"attempt_count":   attempts,
			"target":          FormatRetryTarget(target),
		})
		result := runRetryAttempt(ctx, cfg, req.ExecutorRequest, req.HostCallbackID, state, protocol, streamID, deadline)
		switch {
		case result.committed:
			if result.reason != "" {
				closePluginStream(streamID, result.reason)
				globalRetryStats.requestFinished(requested, false, result.reason)
				return
			}
			globalRetryStats.requestFinished(requested, true, "")
			closePluginStream(streamID, "")
			return
		case result.retryable:
			lastReason = result.reason
			lastKind = result.kind
			lastAttempt = result
			globalRetryStats.retryScheduled(requested, index, FormatRetryTarget(target), string(result.kind), result.reason)
			recordRetryLog("warn", "retry.retry_scheduled", "上游可重试失败，切换到下一个目标", map[string]any{
				"requested_model": requested,
				"attempt":         index + 1,
				"target":          FormatRetryTarget(target),
				"kind":            string(result.kind),
				"reason":          result.reason,
				"remaining":       len(plan.Remaining(index)),
			})
			continue
		default:
			lastReason = result.reason
			lastKind = result.kind
			lastAttempt = result
			globalRetryStats.requestFinished(requested, false, result.reason)
			finishFailedAttempt(streamID, result)
			recordRetryLog("warn", "retry.aborted", "上游失败不可重试，直接返回错误", map[string]any{
				"requested_model": requested,
				"attempt":         index + 1,
				"target":          FormatRetryTarget(target),
				"reason":          result.reason,
			})
			return
		}
	}

	if lastReason == "" {
		lastReason = "retry chain exhausted without an upstream response"
	}
	globalRetryStats.requestFinished(requested, false, lastReason)
	recordRetryLog("warn", "retry.exhausted", "重试链已用尽，返回最后一次错误", map[string]any{
		"requested_model": requested,
		"attempts":        attempts,
		"kind":            string(lastKind),
		"reason":          lastReason,
	})
	finishFailedAttempt(streamID, lastAttempt)
}

// finishFailedAttempt ends the request the way today's non-retrying path would
// have ended it: the frames the failed attempt had already buffered are replayed
// so an in-stream error still reaches the client as an in-stream error, and only
// a failure that produced no frames at all is reported as a transport error.
func finishFailedAttempt(streamID string, result attemptResult) {
	flushed := false
	for _, payload := range result.held {
		if err := emitPluginStream(streamID, payload); err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
		flushed = true
	}
	if flushed && result.heldFailure {
		// The upstream failure frame is already in the stream, so appending
		// another error would duplicate it.
		closePluginStream(streamID, "")
		return
	}
	closePluginStream(streamID, result.reason)
}

// runRetryShadow forwards attempt one byte-for-byte and only records what the
// chain would have done. It never retries and never delays a frame.
func runRetryShadow(ctx context.Context, cfg Config, req rpcExecutorRequest, streamID string, plan RetryPlan, protocol string) {
	state := retryAttemptState{index: 0, target: plan.Requested, requested: plan.Requested}
	result, sniff := runRetryShadowAttempt(ctx, cfg, req.ExecutorRequest, req.HostCallbackID, state, protocol, streamID)
	if result.reason != "" {
		closePluginStream(streamID, result.reason)
		return
	}
	if !sniff.Retryable {
		closePluginStream(streamID, "")
		return
	}
	next, ok := plan.Target(1)
	nextLabel := ""
	if ok {
		nextLabel = FormatRetryTarget(next)
	}
	globalRetryStats.shadowWouldRetry(plan.Key, string(sniff.Kind), sniff.Reason, nextLabel)
	recordRetryLog("info", "retry.shadow_would_retry", "影子模式：该请求本应触发重试", map[string]any{
		"requested_model": plan.Key,
		"kind":            string(sniff.Kind),
		"reason":          sniff.Reason,
		"next_target":     nextLabel,
	})
	closePluginStream(streamID, "")
}

func runRetryShadowAttempt(ctx context.Context, cfg Config, exec pluginapi.ExecutorRequest, hostCallbackID string, state retryAttemptState, protocol string, pluginStreamID string) (attemptResult, RetryClassification) {
	streamID, result, ready := openNestedStream(cfg, exec, hostCallbackID, state, protocol)
	if !ready {
		return result, RetryClassification{}
	}
	defer closeNestedStream(streamID)

	assembler := retryFrameAssembler{protocol: protocol}
	sniff := RetryClassification{}
	for {
		if err := ctx.Err(); err != nil {
			return attemptResult{committed: true, reason: err.Error()}, sniff
		}
		chunk, err, timedOut := readNestedChunk(streamID, cfg.RetryStallTimeout)
		if timedOut {
			return attemptResult{committed: true, reason: fmt.Sprintf("upstream stalled for %s", cfg.RetryStallTimeout)}, sniff
		}
		if err != nil {
			return attemptResult{committed: true, reason: err.Error()}, sniff
		}
		if chunk.Error != "" {
			return attemptResult{committed: true, reason: chunk.Error}, sniff
		}
		if len(chunk.Payload) > 0 {
			result, decisive := assembler.absorb(chunk.Payload)
			if decisive && result.Disposition == frameFailure && !sniff.Retryable {
				sniff = result.Failure
			}
			if errEmit := emitPluginStream(pluginStreamID, chunk.Payload); errEmit != nil {
				return attemptResult{committed: true, reason: errEmit.Error()}, sniff
			}
		}
		if chunk.Done {
			return attemptResult{committed: true}, sniff
		}
	}
}

// retryAttemptState describes one attempt inside the resolved plan.
type retryAttemptState struct {
	index     int
	target    RetryTarget
	requested RetryTarget
}

type attemptResult struct {
	committed bool
	retryable bool
	reason    string
	kind      retryFailureKind
	status    int
	// held carries the frames the attempt buffered before it failed, and
	// heldFailure reports that those frames already contain the upstream
	// failure. A chain that runs out of targets replays them so the client sees
	// the same stream it would see without the chain.
	held        [][]byte
	heldFailure bool
}

// runRetryAttempt performs one execution attempt. It returns committed when the
// attempt produced client-visible output (or finished), retryable when the
// failure justifies the next target, and neither when the failure must be
// returned to the client as-is.
func runRetryAttempt(ctx context.Context, cfg Config, exec pluginapi.ExecutorRequest, hostCallbackID string, state retryAttemptState, protocol string, pluginStreamID string, deadline time.Time) attemptResult {
	nestedID, result, ready := openNestedStream(cfg, exec, hostCallbackID, state, protocol)
	if !ready {
		return result
	}
	nestedOpen := true
	defer func() {
		if nestedOpen {
			closeNestedStream(nestedID)
		}
	}()

	hold := retryHoldBuffer{limitFrames: cfg.RetryMaxFrames, limitBytes: cfg.RetryMaxBytes}
	assembler := retryFrameAssembler{protocol: protocol}
	holdDeadline := time.Now().Add(cfg.RetryHoldTimeout)
	if deadline.Before(holdDeadline) {
		holdDeadline = deadline
	}

	for {
		if err := ctx.Err(); err != nil {
			return attemptResult{reason: err.Error(), kind: retryKindTransport}
		}
		wait := cfg.RetryStallTimeout
		if remaining := time.Until(holdDeadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			// The hold window closed without a decisive frame. A stream that is
			// still silent is handed to the client instead of being retried: the
			// request may simply be slow, and nothing has been committed yet.
			return forwardCommitted(pluginStreamID, nestedID, hold, ctx, &nestedOpen)
		}
		chunk, err, timedOut := readNestedChunk(nestedID, wait)
		if timedOut {
			if len(hold.payloads) == 0 {
				closeNestedStream(nestedID)
				nestedOpen = false
				return attemptResult{
					retryable: true,
					kind:      retryKindTransport,
					reason:    fmt.Sprintf("no upstream output within the %s stall timeout", cfg.RetryStallTimeout),
				}
			}
			return forwardCommitted(pluginStreamID, nestedID, hold, ctx, &nestedOpen)
		}
		if err != nil {
			closeNestedStream(nestedID)
			nestedOpen = false
			classified := classifyAttemptError(err)
			return attemptResult{
				retryable: classified.Retryable,
				reason:    retryFailureText(err.Error(), classified),
				kind:      classified.Kind,
				status:    classified.Status,
				held:      hold.payloads,
			}
		}
		if chunk.Error != "" {
			closeNestedStream(nestedID)
			nestedOpen = false
			classified := classifyRetryFailure(chunk.Error)
			return attemptResult{
				retryable: classified.Retryable,
				reason:    retryFailureText(chunk.Error, classified),
				kind:      classified.Kind,
				status:    classified.Status,
				held:      hold.payloads,
			}
		}
		if len(chunk.Payload) > 0 {
			hold.add(chunk.Payload)
			verdict, decisive := assembler.absorb(chunk.Payload)
			if decisive {
				switch verdict.Disposition {
				case frameFailure:
					closeNestedStream(nestedID)
					nestedOpen = false
					return attemptResult{
						retryable:   verdict.Failure.Retryable,
						reason:      retryFailureText(string(chunk.Payload), verdict.Failure),
						kind:        verdict.Failure.Kind,
						status:      verdict.Failure.Status,
						held:        hold.payloads,
						heldFailure: true,
					}
				default:
					return forwardCommitted(pluginStreamID, nestedID, hold, ctx, &nestedOpen)
				}
			}
			if hold.overBudget() {
				return forwardCommitted(pluginStreamID, nestedID, hold, ctx, &nestedOpen)
			}
		}
		if chunk.Done {
			// The upstream ended before any decisive frame. Flush what we held so
			// the client sees the same partial stream it sees today.
			return forwardCommitted(pluginStreamID, nestedID, hold, ctx, &nestedOpen)
		}
	}
}

// openNestedStream issues one nested execution through the host.
func openNestedStream(cfg Config, exec pluginapi.ExecutorRequest, hostCallbackID string, state retryAttemptState, protocol string) (string, attemptResult, bool) {
	body := retryAttemptBody(cfg, exec, state)
	request := hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocol,
			ExitProtocol:  protocol,
			Model:         retryAttemptModel(exec, state),
			Stream:        true,
			Body:          body,
			Headers:       exec.Headers,
			Query:         exec.Query,
			Alt:           exec.Alt,
		},
		ForcedProvider: state.target.Provider,
		HostCallbackID: hostCallbackID,
	}
	raw, err := callHostModel(pluginabi.MethodHostModelExecuteStream, request)
	if err != nil {
		classified := classifyAttemptError(err)
		return "", attemptResult{
			retryable: classified.Retryable,
			reason:    retryFailureText(err.Error(), classified),
			kind:      classified.Kind,
			status:    classified.Status,
		}, false
	}
	var response pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &response); errDecode != nil {
		return "", attemptResult{reason: "decode host model stream response: " + errDecode.Error(), kind: retryKindTransport}, false
	}
	if response.StatusCode >= 400 {
		classified := classifyRetryStatus(response.StatusCode)
		return "", attemptResult{
			retryable: classified.Retryable,
			reason:    fmt.Sprintf("upstream status %d", response.StatusCode),
			kind:      classified.Kind,
			status:    response.StatusCode,
		}, false
	}
	streamID := strings.TrimSpace(response.StreamID)
	if streamID == "" {
		return "", attemptResult{
			retryable: true,
			kind:      retryKindTransport,
			reason:    "host model stream bridge returned an empty stream id",
		}, false
	}
	return streamID, attemptResult{}, true
}

// forwardCommitted flushes the hold buffer and switches the attempt to
// pass-through. Nothing after this point can trigger a retry.
func forwardCommitted(pluginStreamID string, nestedID string, hold retryHoldBuffer, ctx context.Context, nestedOpen *bool) attemptResult {
	for _, payload := range hold.payloads {
		if err := emitPluginStream(pluginStreamID, payload); err != nil {
			closeNestedStream(nestedID)
			*nestedOpen = false
			return attemptResult{committed: true, reason: err.Error()}
		}
	}
	if err := ctx.Err(); err != nil {
		closeNestedStream(nestedID)
		*nestedOpen = false
		return attemptResult{committed: true, reason: err.Error()}
	}
	for {
		chunk, err, _ := readNestedChunk(nestedID, 0)
		if err != nil {
			closeNestedStream(nestedID)
			*nestedOpen = false
			return attemptResult{committed: true, reason: err.Error()}
		}
		if chunk.Error != "" {
			closeNestedStream(nestedID)
			*nestedOpen = false
			return attemptResult{committed: true, reason: chunk.Error}
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitPluginStream(pluginStreamID, chunk.Payload); errEmit != nil {
				closeNestedStream(nestedID)
				*nestedOpen = false
				return attemptResult{committed: true, reason: errEmit.Error()}
			}
		}
		if chunk.Done {
			closeNestedStream(nestedID)
			*nestedOpen = false
			return attemptResult{committed: true}
		}
	}
}

func retryAttemptModel(exec pluginapi.ExecutorRequest, state retryAttemptState) string {
	if state.index == 0 {
		return strings.TrimSpace(exec.Model)
	}
	if model := strings.TrimSpace(state.target.Model); model != "" {
		return model
	}
	return strings.TrimSpace(exec.Model)
}

// retryAttemptBody rewrites the requested model and, when the attempt switches
// models, drops Codex encrypted reasoning items that a different model cannot
// verify. Attempt zero keeps the body byte-identical.
func retryAttemptBody(cfg Config, exec pluginapi.ExecutorRequest, state retryAttemptState) []byte {
	body := exec.Payload
	if len(body) == 0 {
		body = exec.OriginalRequest
	}
	if state.index == 0 || len(body) == 0 {
		return body
	}
	model := retryAttemptModel(exec, state)
	current := strings.TrimSpace(exec.Model)
	changed := model != "" && !strings.EqualFold(model, current)
	if !changed {
		// The target resolves to the same model, so the only difference this
		// attempt introduces is the provider. Keep the body byte-identical
		// instead of round-tripping it through a map.
		return body
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return body
	}
	encoded, errMarshal := json.Marshal(model)
	if errMarshal == nil {
		object["model"] = json.RawMessage(encoded)
	}
	if cfg.RetryStripReasoning {
		if filtered, ok := stripEncryptedReasoning(object["input"]); ok {
			object["input"] = filtered
		}
	}
	rewritten, errRewrite := json.Marshal(object)
	if errRewrite != nil {
		return body
	}
	return rewritten
}

// stripEncryptedReasoning removes reasoning items that carry encrypted content.
// A different model cannot replay them, and forwarding them fails the request.
func stripEncryptedReasoning(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	kept := make([]json.RawMessage, 0, len(items))
	removed := false
	for _, item := range items {
		if retryItemIsEncryptedReasoning(item) {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if !removed {
		return nil, false
	}
	encoded, errMarshal := json.Marshal(kept)
	if errMarshal != nil {
		return nil, false
	}
	return encoded, true
}

func retryItemIsEncryptedReasoning(item json.RawMessage) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(item, &object); err != nil {
		return false
	}
	var itemType string
	if err := json.Unmarshal(object["type"], &itemType); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(itemType), "reasoning") {
		return false
	}
	var encrypted string
	if err := json.Unmarshal(object["encrypted_content"], &encrypted); err != nil {
		return false
	}
	return strings.TrimSpace(encrypted) != ""
}

func retryRequestedModel(exec pluginapi.ExecutorRequest) string {
	model := strings.TrimSpace(exec.Model)
	if model != "" {
		return model
	}
	if exec.Metadata != nil {
		if raw, ok := exec.Metadata["requested_model"]; ok {
			if text, isText := raw.(string); isText {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

// retryProtocol resolves the wire format carried by this executor route.
func retryProtocol(exec pluginapi.ExecutorRequest) string {
	for _, candidate := range []string{exec.SourceFormat, exec.Format} {
		trimmed := strings.ToLower(strings.TrimSpace(candidate))
		if trimmed == "" {
			continue
		}
		return trimmed
	}
	return ""
}

func retrySingleTargetPlan(model string) RetryPlan {
	return RetryPlan{
		Key:         model,
		Requested:   NormalizeRetryTarget(RetryTarget{Model: model}),
		MaxAttempts: 1,
	}
}

func emitPluginStream(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := callHostModel(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  append([]byte(nil), payload...),
	})
	return err
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHostModel(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

func closeNestedStream(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHostModel(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
}

// readNestedChunk reads one chunk with an optional deadline. A zero or negative
// timeout blocks. The abandoned reader goroutine exits once the host bridge
// notices the closed stream.
func readNestedChunk(streamID string, timeout time.Duration) (pluginapi.HostModelStreamReadResponse, error, bool) {
	type readResult struct {
		raw json.RawMessage
		err error
	}
	results := make(chan readResult, 1)
	go func() {
		raw, err := callHostModel(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
		results <- readResult{raw: raw, err: err}
	}()
	if timeout <= 0 {
		outcome := <-results
		return decodeNestedChunk(outcome.raw, outcome.err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case outcome := <-results:
		response, err, _ := decodeNestedChunk(outcome.raw, outcome.err)
		return response, err, false
	case <-timer.C:
		return pluginapi.HostModelStreamReadResponse{}, nil, true
	}
}

func decodeNestedChunk(raw json.RawMessage, err error) (pluginapi.HostModelStreamReadResponse, error, bool) {
	if err != nil {
		return pluginapi.HostModelStreamReadResponse{}, err, false
	}
	var response pluginapi.HostModelStreamReadResponse
	if errDecode := json.Unmarshal(raw, &response); errDecode != nil {
		return pluginapi.HostModelStreamReadResponse{}, errDecode, false
	}
	return response, nil, false
}

func classifyAttemptError(err error) RetryClassification {
	var callErr *hostCallError
	if errors.As(err, &callErr) && callErr.Status > 0 {
		return classifyRetryStatus(callErr.Status)
	}
	return classifyRetryFailure(err.Error())
}

func retryFailureText(raw string, classification RetryClassification) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		text = "upstream failure"
	}
	if classification.Reason == "" {
		return text
	}
	if strings.Contains(strings.ToLower(text), strings.ToLower(classification.Reason)) {
		return text
	}
	return fmt.Sprintf("%s (%s)", text, classification.Reason)
}

// retryFrameAssembler accumulates stream payloads so a frame split across two
// chunks is classified once, not twice. Handshake frames are held; the first
// decisive frame decides the attempt.
type retryFrameAssembler struct {
	protocol string
	pending  []byte
}

func (a *retryFrameAssembler) absorb(payload []byte) (frameVerdict, bool) {
	if len(payload) == 0 {
		return frameVerdict{Disposition: frameHandshake}, false
	}
	a.pending = append(a.pending, payload...)
	for {
		index := bytes.Index(a.pending, []byte("\n\n"))
		if index < 0 {
			break
		}
		frame := a.pending[:index]
		rest := a.pending[index+2:]
		a.pending = append(a.pending[:0], rest...)
		verdict := classifyFrame(frame, a.protocol)
		if verdict.Disposition != frameHandshake {
			return verdict, true
		}
	}
	// A non-SSE stream delivers whole JSON bodies with no frame terminator. Only
	// a complete body may decide the attempt; a partial one keeps accumulating.
	trimmed := bytes.TrimSpace(a.pending)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		verdict := classifyFrame(trimmed, a.protocol)
		if verdict.Disposition != frameHandshake {
			return verdict, true
		}
		a.pending = a.pending[:0]
	}
	return frameVerdict{Disposition: frameHandshake}, false
}

// retryHoldBuffer keeps buffered payloads so they can be replayed in order when
// the attempt commits.
type retryHoldBuffer struct {
	payloads    [][]byte
	bytes       int64
	frames      int
	limitFrames int
	limitBytes  int64
}

func (b *retryHoldBuffer) add(payload []byte) {
	copied := append([]byte(nil), payload...)
	b.payloads = append(b.payloads, copied)
	b.bytes += int64(len(copied))
	b.frames += countSSEFrames(copied)
}

func (b *retryHoldBuffer) overBudget() bool {
	if b.limitFrames > 0 && b.frames > b.limitFrames {
		return true
	}
	return b.limitBytes > 0 && b.bytes > b.limitBytes
}

// RetryEvent is one observable step of a chain execution.
type RetryEvent struct {
	At        time.Time `json:"at"`
	Model     string    `json:"model,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	Target    string    `json:"target,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	NextTarge string    `json:"next_target,omitempty"`
}

type retryStats struct {
	mu         sync.Mutex
	requests   int
	retries    int
	succeeded  int
	failed     int
	shadowHits int
	events     []RetryEvent
}

var globalRetryStats = &retryStats{}

const retryEventHistory = 40

func (s *retryStats) record(event RetryEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.At.IsZero() {
		event.At = time.Now()
	}
	s.events = append(s.events, event)
	if len(s.events) > retryEventHistory {
		s.events = append([]RetryEvent(nil), s.events[len(s.events)-retryEventHistory:]...)
	}
}

func (s *retryStats) requestStarted(model string, attempts int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	s.record(RetryEvent{Model: model, Attempt: attempts, Outcome: "request"})
}

func (s *retryStats) attemptStarted(model string, index int, target string) {
	s.record(RetryEvent{Model: model, Attempt: index + 1, Target: target, Outcome: "attempt"})
}

func (s *retryStats) retryScheduled(model string, index int, target, kind, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retries++
	s.mu.Unlock()
	s.record(RetryEvent{Model: model, Attempt: index + 1, Target: target, Outcome: "retry", Kind: kind, Reason: reason})
}

func (s *retryStats) requestFinished(model string, ok bool, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if ok {
		s.succeeded++
	} else {
		s.failed++
	}
	s.mu.Unlock()
	outcome := "succeeded"
	if !ok {
		outcome = "failed"
	}
	s.record(RetryEvent{Model: model, Outcome: outcome, Reason: reason})
}

func (s *retryStats) shadowWouldRetry(model, kind, reason, next string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shadowHits++
	s.mu.Unlock()
	s.record(RetryEvent{Model: model, Outcome: "shadow", Kind: kind, Reason: reason, NextTarge: next})
}

type RetryStatusPayload struct {
	Mode       string       `json:"mode"`
	Enabled    bool         `json:"enabled"`
	Chain      []string     `json:"chain"`
	Requests   int          `json:"requests"`
	Retries    int          `json:"retries"`
	Succeeded  int          `json:"succeeded"`
	Failed     int          `json:"failed"`
	ShadowHits int          `json:"shadow_hits"`
	Events     []RetryEvent `json:"events"`
}

func retryStatusSnapshot(cfg Config) RetryStatusPayload {
	payload := RetryStatusPayload{
		Enabled: cfg.RetryEnabled,
		Chain:   make([]string, 0, len(cfg.RetryChain)),
	}
	if cfg.RetryEnabled && len(cfg.RetryChain) > 0 {
		payload.Mode = "live"
		if cfg.RetryShadow {
			payload.Mode = "shadow"
		}
	} else {
		payload.Mode = "off"
	}
	for _, row := range cfg.RetryChain {
		targets := make([]string, 0, len(row.Fallbacks)+1)
		targets = append(targets, strings.TrimSpace(row.Model))
		for _, fallback := range row.Fallbacks {
			targets = append(targets, FormatRetryTarget(fallback))
		}
		payload.Chain = append(payload.Chain, strings.Join(targets, " -> "))
	}
	globalRetryStats.mu.Lock()
	defer globalRetryStats.mu.Unlock()
	payload.Requests = globalRetryStats.requests
	payload.Retries = globalRetryStats.retries
	payload.Succeeded = globalRetryStats.succeeded
	payload.Failed = globalRetryStats.failed
	payload.ShadowHits = globalRetryStats.shadowHits
	if len(globalRetryStats.events) > 0 {
		payload.Events = append([]RetryEvent(nil), globalRetryStats.events...)
	}
	return payload
}

func recordRetryLog(level, event, message string, fields map[string]any) {
	if globalState == nil {
		return
	}
	globalState.RecordLog(level, event, message, fields, time.Now())
}
