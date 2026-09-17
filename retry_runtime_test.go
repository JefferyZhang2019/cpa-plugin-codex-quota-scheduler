package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeRetryAttempt scripts one nested upstream execution.
type fakeRetryAttempt struct {
	status int   // >= 400 answers with an HTTP status instead of a stream
	err    error // a transport failure raised by execute_stream
	chunks []pluginapi.HostModelStreamReadResponse
}

type fakeNestedRequest struct {
	Model    string
	Provider string
	Body     []byte
}

type fakeRetryHost struct {
	mu       sync.Mutex
	attempts []fakeRetryAttempt
	cursor   int
	requests []fakeNestedRequest
	streams  map[string][]pluginapi.HostModelStreamReadResponse
	emitted  map[string][][]byte
	closed   map[string]string
	nextID   int
}

func installFakeRetryHost(t *testing.T) *fakeRetryHost {
	t.Helper()
	host := &fakeRetryHost{
		streams: map[string][]pluginapi.HostModelStreamReadResponse{},
		emitted: map[string][][]byte{},
		closed:  map[string]string{},
	}
	previous := callHostModel
	callHostModel = host.call
	t.Cleanup(func() { callHostModel = previous })
	return host
}

func (h *fakeRetryHost) push(attempt fakeRetryAttempt) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attempts = append(h.attempts, attempt)
}

func (h *fakeRetryHost) call(method string, payload any) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case pluginabi.MethodHostModelExecuteStream:
		request, _ := payload.(hostModelExecutionRequest)
		h.requests = append(h.requests, fakeNestedRequest{
			Model:    request.Model,
			Provider: request.ForcedProvider,
			Body:     append([]byte(nil), request.Body...),
		})
		if h.cursor >= len(h.attempts) {
			return nil, fmt.Errorf("no scripted attempt for %s", method)
		}
		attempt := h.attempts[h.cursor]
		h.cursor++
		if attempt.err != nil {
			return nil, attempt.err
		}
		if attempt.status >= 400 {
			return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: attempt.status})
		}
		h.nextID++
		streamID := fmt.Sprintf("nested-%d", h.nextID)
		h.streams[streamID] = attempt.chunks
		return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: streamID})
	case pluginabi.MethodHostModelStreamRead:
		request, _ := payload.(pluginapi.HostModelStreamReadRequest)
		queue := h.streams[request.StreamID]
		if len(queue) == 0 {
			return json.Marshal(pluginapi.HostModelStreamReadResponse{Done: true})
		}
		next := queue[0]
		h.streams[request.StreamID] = queue[1:]
		return json.Marshal(next)
	case pluginabi.MethodHostModelStreamClose:
		request, _ := payload.(pluginapi.HostModelStreamCloseRequest)
		delete(h.streams, request.StreamID)
		return json.Marshal(map[string]any{"ok": true})
	case pluginabi.MethodHostStreamEmit:
		request, _ := payload.(rpcStreamEmitRequest)
		h.emitted[request.StreamID] = append(h.emitted[request.StreamID], append([]byte(nil), request.Payload...))
		return json.Marshal(map[string]any{"ok": true})
	case pluginabi.MethodHostStreamClose:
		request, _ := payload.(rpcStreamCloseRequest)
		h.closed[request.StreamID] = request.Error
		return json.Marshal(map[string]any{"ok": true})
	default:
		return nil, fmt.Errorf("unexpected host method %s", method)
	}
}

func (h *fakeRetryHost) snapshot() (requests []fakeNestedRequest, emitted [][]byte, closeError string, closeCalled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	requests = append(requests, h.requests...)
	emitted = append(emitted, h.emitted["plugin-stream"]...)
	closeError, closeCalled = h.closed["plugin-stream"]
	return requests, emitted, closeError, closeCalled
}

func (h *fakeRetryHost) nestedEmits() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for streamID, payloads := range h.emitted {
		if strings.HasPrefix(streamID, "nested-") && len(payloads) > 0 {
			out = append(out, streamID)
		}
	}
	return out
}

func retryFrames(payloads ...string) []pluginapi.HostModelStreamReadResponse {
	chunks := make([]pluginapi.HostModelStreamReadResponse, 0, len(payloads)+1)
	for _, payload := range payloads {
		chunks = append(chunks, pluginapi.HostModelStreamReadResponse{Payload: []byte(payload)})
	}
	return append(chunks, pluginapi.HostModelStreamReadResponse{Done: true})
}

const (
	retryCreatedFrame  = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	retryDeltaFrame    = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	retryCapacityFrame = "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"},\"sequence_number\":2}\n\n"
	retryDoneFrame     = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"
)

func retryTestConfig(row RetryChainRow) Config {
	cfg := DefaultConfig()
	cfg.RetryEnabled = true
	cfg.RetryChain = []RetryChainRow{row}
	return cfg
}

func retryTestRequest(model string) rpcExecutorRequest {
	return rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           model,
			Format:          protocolCodex,
			SourceFormat:    protocolCodex,
			Stream:          true,
			Payload:         []byte(`{"model":"` + model + `","stream":true,"input":[]}`),
			OriginalRequest: []byte(`{"model":"` + model + `","stream":true,"input":[]}`),
		},
		StreamID:       "plugin-stream",
		HostCallbackID: "host-callback",
	}
}

func resetRetryTestStats(t *testing.T) *retryStats {
	t.Helper()
	previous := globalRetryStats
	globalRetryStats = &retryStats{}
	t.Cleanup(func() { globalRetryStats = previous })
	return globalRetryStats
}

func retryStatsOf(t *testing.T) (retries, shadowHits, failed, succeeded int) {
	t.Helper()
	globalRetryStats.mu.Lock()
	defer globalRetryStats.mu.Unlock()
	return globalRetryStats.retries, globalRetryStats.shadowHits, globalRetryStats.failed, globalRetryStats.succeeded
}

func TestRunRetryChainRetriesAfterHeldCapacityFrame(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryDeltaFrame, retryDoneFrame)})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, emitted, closeError, closeCalled := host.snapshot()
	if len(requests) != 2 {
		t.Fatalf("attempts = %d, want 2", len(requests))
	}
	if requests[0].Model != "gpt-5.6-sol" || requests[1].Model != "gpt-5.6-luna" {
		t.Fatalf("attempt models = %q, %q", requests[0].Model, requests[1].Model)
	}
	if !closeCalled || closeError != "" {
		t.Fatalf("close = %q called=%v, want a clean close", closeError, closeCalled)
	}
	stream := string(joinPayloads(emitted))
	if !strings.Contains(stream, "response.output_text.delta") {
		t.Fatalf("second attempt output missing from stream: %q", stream)
	}
	if strings.Contains(stream, "at capacity") {
		t.Fatalf("the abandoned attempt leaked into the client stream: %q", stream)
	}
	if nested := host.nestedEmits(); len(nested) != 0 {
		t.Fatalf("emitted to nested stream ids %v", nested)
	}
	if retries, _, _, succeeded := retryStatsOf(t); retries != 1 || succeeded != 1 {
		t.Fatalf("stats retries=%d succeeded=%d", retries, succeeded)
	}
}

func TestRunRetryChainDoesNotRetryAfterContentCommitted(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryDeltaFrame, retryCapacityFrame, retryDoneFrame)})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, emitted, closeError, closeCalled := host.snapshot()
	if len(requests) != 1 {
		t.Fatalf("attempts = %d, want 1: content was already committed", len(requests))
	}
	if !closeCalled || closeError != "" {
		t.Fatalf("close = %q called=%v", closeError, closeCalled)
	}
	stream := string(joinPayloads(emitted))
	for _, want := range []string{"response.created", "hello", "at capacity", "response.completed"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream is missing %q: %q", want, stream)
		}
	}
	if nested := host.nestedEmits(); len(nested) != 0 {
		t.Fatalf("emitted to nested stream ids %v", nested)
	}
}

func TestRunRetryChainReturnsFailureAfterEveryTargetFails(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, emitted, closeError, closeCalled := host.snapshot()
	if len(requests) != 2 {
		t.Fatalf("attempts = %d, want 2", len(requests))
	}
	if !closeCalled {
		t.Fatal("the plugin stream was never closed")
	}
	stream := string(joinPayloads(emitted))
	if !strings.Contains(stream, "at capacity") {
		t.Fatalf("the last in-stream failure never reached the client: %q", stream)
	}
	if closeError != "" {
		t.Fatalf("close error = %q, want the held failure frame to speak for itself", closeError)
	}
	if _, _, failed, _ := retryStatsOf(t); failed != 1 {
		t.Fatalf("failed stats = %d", failed)
	}
}

func TestRunRetryChainReportsTransportFailureWithoutBufferedOutput(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{err: fmt.Errorf("dial tcp 10.0.0.1:443: connection reset by peer")})
	host.push(fakeRetryAttempt{err: fmt.Errorf("dial tcp 10.0.0.1:443: connection reset by peer")})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, _, closeError, closeCalled := host.snapshot()
	if len(requests) != 2 {
		t.Fatalf("attempts = %d, want 2: a transport failure before output is retryable", len(requests))
	}
	if !closeCalled || closeError == "" {
		t.Fatalf("close = %q called=%v, want a transport error for the client", closeError, closeCalled)
	}
	if !strings.Contains(closeError, "connection reset") {
		t.Fatalf("close error lost the upstream cause: %q", closeError)
	}
}

func TestRunRetryChainRetriesStatusFailures(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{status: 503})
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryDeltaFrame, retryDoneFrame)})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, emitted, closeError, _ := host.snapshot()
	if len(requests) != 2 {
		t.Fatalf("attempts = %d, want 2: 503 is retryable", len(requests))
	}
	if closeError != "" {
		t.Fatalf("close error = %q", closeError)
	}
	if !strings.Contains(string(joinPayloads(emitted)), "hello") {
		t.Fatal("the fallback attempt never reached the client")
	}
}

func TestRunRetryChainStopsAtMaxAttempts(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	for index := 0; index < 4; index++ {
		host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	}
	cfg := retryTestConfig(RetryChainRow{
		Model: "gpt-5.6-sol",
		Fallbacks: []RetryTarget{
			{Model: "gpt-5.6-luna"},
			{Model: "gpt-5.5"},
			{Model: "gpt-4.1"},
		},
	})
	cfg.RetryMaxAttempts = 2
	currentConfig.Store(cfg)

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, _, _, _ := host.snapshot()
	if len(requests) != 2 {
		t.Fatalf("attempts = %d, want the configured budget of 2", len(requests))
	}
	if requests[1].Model != "gpt-5.6-luna" {
		t.Fatalf("second attempt model = %q", requests[1].Model)
	}
}

func TestRunRetryChainShadowNeverRetries(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	frames := retryFrames(retryCreatedFrame, retryDeltaFrame, retryCapacityFrame, retryDoneFrame)
	host.push(fakeRetryAttempt{chunks: frames})
	cfg := retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	})
	cfg.RetryShadow = true
	currentConfig.Store(cfg)

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, emitted, closeError, closeCalled := host.snapshot()
	if len(requests) != 1 {
		t.Fatalf("attempts = %d, want 1 in shadow mode", len(requests))
	}
	if !closeCalled || closeError != "" {
		t.Fatalf("close = %q called=%v", closeError, closeCalled)
	}
	if len(emitted) != len(frames)-1 {
		t.Fatalf("emitted %d payloads, want %d", len(emitted), len(frames)-1)
	}
	for index, payload := range emitted {
		if string(payload) != string(frames[index].Payload) {
			t.Fatalf("shadow rewrote payload %d: %q != %q", index, payload, frames[index].Payload)
		}
	}
	if _, shadowHits, _, _ := retryStatsOf(t); shadowHits != 1 {
		t.Fatalf("shadow hits = %d, want the chain to record what it would have done", shadowHits)
	}
}

func TestRunRetryChainIgnoresRequestWithoutChainRow(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	currentConfig.Store(retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	}))

	runRetryChain(context.Background(), retryTestRequest("some-other-model"), "plugin-stream")

	requests, _, _, _ := host.snapshot()
	if len(requests) != 1 {
		t.Fatalf("attempts = %d, want 1: an unconfigured model keeps today's behavior", len(requests))
	}
	if requests[0].Model != "some-other-model" {
		t.Fatalf("model = %q", requests[0].Model)
	}
}

func TestRetryAttemptBodyRewritesModelAndStripsReasoning(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","content":"hi"},{"type":"reasoning","encrypted_content":"secret","summary":[]}],"store":false}`
	exec := pluginapi.ExecutorRequest{Model: "gpt-5.6-sol", Payload: []byte(body)}
	cfg := DefaultConfig()
	cfg.RetryStripReasoning = true

	first := retryAttemptBody(cfg, exec, retryAttemptState{index: 0, target: RetryTarget{Model: "gpt-5.6-sol"}, requested: RetryTarget{Model: "gpt-5.6-sol"}})
	if string(first) != body {
		t.Fatalf("attempt zero rewrote the body: %s", first)
	}

	sameModel := retryAttemptBody(cfg, exec, retryAttemptState{index: 1, target: RetryTarget{Provider: "openai-compatible-zed2api", Model: "gpt-5.6-sol"}, requested: RetryTarget{Model: "gpt-5.6-sol"}})
	if string(sameModel) != body {
		t.Fatalf("a provider-only fallback rewrote the body: %s", sameModel)
	}

	switched := retryAttemptBody(cfg, exec, retryAttemptState{index: 1, target: RetryTarget{Model: "gpt-5.6-luna"}, requested: RetryTarget{Model: "gpt-5.6-sol"}})
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(switched, &parsed); err != nil {
		t.Fatalf("rewritten body is not an object: %v", err)
	}
	var model string
	if err := json.Unmarshal(parsed["model"], &model); err != nil || model != "gpt-5.6-luna" {
		t.Fatalf("model = %q err=%v", model, err)
	}
	var items []map[string]any
	if err := json.Unmarshal(parsed["input"], &items); err != nil {
		t.Fatalf("input is not an array: %v", err)
	}
	if len(items) != 1 || items[0]["type"] != "message" {
		t.Fatalf("encrypted reasoning survived the model switch: %s", parsed["input"])
	}
	var store any
	if err := json.Unmarshal(parsed["store"], &store); err != nil || store != false {
		t.Fatalf("unrelated fields were lost: %v", err)
	}

	cfg.RetryStripReasoning = false
	kept := retryAttemptBody(cfg, exec, retryAttemptState{index: 1, target: RetryTarget{Model: "gpt-5.6-luna"}, requested: RetryTarget{Model: "gpt-5.6-sol"}})
	if !strings.Contains(string(kept), "encrypted_content") {
		t.Fatalf("strip disabled still dropped reasoning: %s", kept)
	}
}

func TestRetryAttemptBodyFallsBackToOriginalRequest(t *testing.T) {
	exec := pluginapi.ExecutorRequest{
		Model:           "gpt-5.6-sol",
		OriginalRequest: []byte(`{"model":"gpt-5.6-sol"}`),
	}
	cfg := DefaultConfig()
	got := retryAttemptBody(cfg, exec, retryAttemptState{index: 1, target: RetryTarget{Model: "gpt-5.5"}, requested: RetryTarget{Model: "gpt-5.6-sol"}})
	if !strings.Contains(string(got), "gpt-5.5") {
		t.Fatalf("original request was not rewritten: %s", got)
	}
}

func TestRunRetryChainUsesChainDeadline(t *testing.T) {
	cleanupIntegrationGlobals(t)
	resetRetryTestStats(t)
	host := installFakeRetryHost(t)
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryCapacityFrame)})
	host.push(fakeRetryAttempt{chunks: retryFrames(retryCreatedFrame, retryDeltaFrame, retryDoneFrame)})
	cfg := retryTestConfig(RetryChainRow{
		Model:     "gpt-5.6-sol",
		Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}},
	})
	cfg.RetryChainDeadline = time.Nanosecond
	currentConfig.Store(cfg)

	runRetryChain(context.Background(), retryTestRequest("gpt-5.6-sol"), "plugin-stream")

	requests, _, closeCalled := requestsOf(t, host)
	if closeCalled != true {
		t.Fatal("the plugin stream was never closed")
	}
	if len(requests) != 0 {
		t.Fatalf("attempts = %d, want 0 once the deadline has passed", len(requests))
	}
}

func requestsOf(t *testing.T, host *fakeRetryHost) ([]fakeNestedRequest, [][]byte, bool) {
	t.Helper()
	requests, emitted, _, closeCalled := host.snapshot()
	return requests, emitted, closeCalled
}

func joinPayloads(payloads [][]byte) []byte {
	var builder strings.Builder
	for _, payload := range payloads {
		builder.Write(payload)
	}
	return []byte(builder.String())
}
