package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The response-embedded quota observer. Codex streams carry a
// `codex.rate_limits` event frame with the same window shape the generic
// wham/usage endpoint reports. Observing those frames keeps each account's
// quota cache fresh as a side effect of real traffic, defers the polling
// refresh (accounts without recent observations), and feeds the
// reconciliation paths with evidence bound to an actual response.
//
// The interceptor is strictly read-only: it always returns an empty response,
// so no byte of any stream is modified, held, or delayed beyond this handler.

const (
	rateLimitsEventMarker = "codex.rate_limits"
	sseFrameDelimiter     = "\n\n"
	// maxObserverBufferBytes bounds the cross-chunk frame buffer. Frames longer
	// than this are dropped; rate_limits frames are a few hundred bytes.
	maxObserverBufferBytes = 1 << 20
)

// streamQuotaObserver buffers partial SSE frames across stream chunks for one
// concurrent stream. Instances are cheap; the registry keeps one per RequestID
// and drops them on completion.
type streamQuotaObserver struct {
	buffer []byte
}

type streamObserverRegistry struct {
	mu        sync.Mutex
	observers map[string]*streamQuotaObserver
}

func newStreamObserverRegistry() *streamObserverRegistry {
	return &streamObserverRegistry{observers: make(map[string]*streamQuotaObserver)}
}

func (r *streamObserverRegistry) observerFor(requestID string) *streamQuotaObserver {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o, ok := r.observers[requestID]; ok {
		return o
	}
	o := &streamQuotaObserver{}
	r.observers[requestID] = o
	return o
}

func (r *streamObserverRegistry) forget(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.observers, requestID)
}

// globalStreamObservers tracks in-flight observed streams.
var globalStreamObservers = newStreamObserverRegistry()

func handleStreamChunkIntercept(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			// A malformed intercept request must never disturb the stream.
			return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
		}
	}
	observeStreamChunk(globalState, &req, time.Now())
	// Read-only: always return an empty response regardless of what was seen.
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

func observeStreamChunk(store *PluginState, req *pluginapi.StreamChunkInterceptRequest, now time.Time) {
	if store == nil || req == nil || req.RequestID == "" || req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		return
	}
	body := req.Body
	if len(body) == 0 && len(req.HistoryChunks) == 0 {
		return
	}
	observer := globalStreamObservers.observerFor(req.RequestID)
	authID := metadataString(req.Metadata, "selected_auth_id")
	authIndex := metadataString(req.Metadata, "selected_auth_index")
	observed := observer.absorb(body, func(frame []byte) {
		applyObservedRateLimits(store, authID, authIndex, frame, now)
	})
	if observed {
		globalStreamObservers.forget(req.RequestID)
	}
}

// absorb feeds one chunk into the framer and invokes handle for every
// complete SSE frame. It reports whether the stream already produced a
// rate_limits observation, letting the caller drop per-stream state early.
func (o *streamQuotaObserver) absorb(chunk []byte, handle func(frame []byte)) bool {
	if len(chunk) > 0 {
		if len(o.buffer)+len(chunk) > maxObserverBufferBytes {
			o.buffer = o.buffer[:0]
		}
		o.buffer = append(o.buffer, chunk...)
	}
	observed := false
	for {
		idx := bytes.Index(o.buffer, []byte(sseFrameDelimiter))
		if idx < 0 {
			break
		}
		frame := o.buffer[:idx]
		rest := o.buffer[idx+len(sseFrameDelimiter):]
		if frameLooksLikeRateLimits(frame) {
			handle(frame)
			observed = true
		}
		o.buffer = append(o.buffer[:0], rest...)
	}
	if !bytes.Contains(o.buffer, []byte(rateLimitsEventMarker)) && len(o.buffer) > len(rateLimitsEventMarker) {
		// No marker in the incomplete remainder: keep only a tail long enough
		// to complete a marker straddling the next chunk boundary.
		o.buffer = append(o.buffer[:0], o.buffer[len(o.buffer)-len(rateLimitsEventMarker):]...)
	}
	return observed
}

// frameLooksLikeRateLimits is the cheap prefilter (mirrors the host's
// codexQuotaEventKind): scan the first bytes of the frame for the marker
// before any JSON parsing happens.
func frameLooksLikeRateLimits(frame []byte) bool {
	scanned := frame
	if len(scanned) > 256 {
		scanned = scanned[:256]
	}
	return bytes.Contains(scanned, []byte(rateLimitsEventMarker))
}

// codexRateLimitsEvent mirrors the payload of a `codex.rate_limits` SSE event.
type codexRateLimitsEvent struct {
	RateLimits *codexRateLimitsPayload `json:"rate_limits"`
}

type codexRateLimitsPayload struct {
	Allowed      *bool                 `json:"allowed"`
	LimitReached *bool                 `json:"limit_reached"`
	Primary      *codexRateLimitWindow `json:"primary"`
	Secondary    *codexRateLimitWindow `json:"secondary"`
}

type codexRateLimitWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	WindowMinutes     *float64 `json:"window_minutes"`
	ResetAfterSeconds *float64 `json:"reset_after_seconds"`
	ResetAt           *float64 `json:"reset_at"`
}

func applyObservedRateLimits(store *PluginState, authID, authIndex string, frame []byte, now time.Time) {
	if authID == "" && authIndex == "" {
		return
	}
	payload := sseEventData(frame)
	if len(payload) == 0 {
		return
	}
	var event codexRateLimitsEvent
	if err := json.Unmarshal(payload, &event); err != nil || event.RateLimits == nil {
		return
	}
	quota := ParsedQuota{}
	if window := observedQuotaWindow(event.RateLimits.Primary, event.RateLimits, now); window != nil {
		quota.FiveHour = window
	}
	if window := observedQuotaWindow(event.RateLimits.Secondary, event.RateLimits, now); window != nil {
		quota.LongWindow = window
		quota.Family = familyForWindowMinutes(window.LimitWindowSeconds)
	}
	if quota.FiveHour == nil && quota.LongWindow == nil {
		return
	}
	store.ObserveAccountQuota(authID, authIndex, quota, now)
	publishObservedSchedulerState()
}

// sseEventData extracts the JSON payload of an SSE frame: the first data: line.
func sseEventData(frame []byte) []byte {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		return bytes.TrimSpace(trimmed[len("data:"):])
	}
	return nil
}

func observedQuotaWindow(window *codexRateLimitWindow, limits *codexRateLimitsPayload, now time.Time) *QuotaWindow {
	if window == nil || window.UsedPercent == nil {
		return nil
	}
	out := &QuotaWindow{UsedPercent: window.UsedPercent}
	if window.WindowMinutes != nil && *window.WindowMinutes > 0 {
		seconds := int64(*window.WindowMinutes * 60)
		out.LimitWindowSeconds = &seconds
	}
	switch {
	case window.ResetAt != nil && *window.ResetAt > 0:
		out.ResetAt = time.Unix(int64(*window.ResetAt), 0).UTC()
	case window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0:
		out.ResetAt = now.Add(time.Duration(*window.ResetAfterSeconds * float64(time.Second)))
	}
	if out.ResetAt.IsZero() {
		// Without a reset deadline the observation cannot drive scheduling.
		return nil
	}
	if (limits.Allowed != nil && !*limits.Allowed) || (limits.LimitReached != nil && *limits.LimitReached) || *window.UsedPercent >= 100 {
		out.Exhausted = true
	}
	return out
}

func familyForWindowMinutes(seconds *int64) AccountFamily {
	if seconds == nil {
		return ""
	}
	const weekSeconds = int64(7 * 24 * time.Hour / time.Second)
	switch {
	case *seconds <= weekSeconds:
		return AccountFamilyWeekly
	default:
		return AccountFamilyMonthly
	}
}

// publishObservedSchedulerState republishes the scheduler snapshot after an
// observation changed cached quota state.
func publishObservedSchedulerState() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher == nil {
		return
	}
	publishSchedulerState(globalState, highestTierSet(refresher.runtimeRoster()), time.Now())
}

// applyObservedCodexHeaders consumes the X-Codex-* quota snapshot the host
// attaches to usage records (populated on the WebSocket transport, where the
// host itself parses codex.rate_limits frames). Only successful records run
// the observation path: a failed record may coexist with a freshly minted
// window that the upstream still refuses to serve, and the failure marking
// must win.
func applyObservedCodexHeaders(store *PluginState, record pluginapi.UsageRecord, now time.Time) {
	if store == nil || !strings.EqualFold(record.Provider, "codex") || record.Failed {
		return
	}
	quota, ok := codexQuotaFromHeaders(record.ResponseHeaders, now)
	if !ok {
		return
	}
	if _, observed := store.ObserveAccountQuota(record.AuthID, record.AuthIndex, quota, now); observed {
		publishObservedSchedulerState()
	}
}

// codexQuotaFromHeaders parses the normalized X-Codex-* header set into
// quota windows. Field names mirror the host's ParseCodexQuotaEventHeaders.
func codexQuotaFromHeaders(headers map[string][]string, now time.Time) (ParsedQuota, bool) {
	if len(headers) == 0 {
		return ParsedQuota{}, false
	}
	get := func(name string) string {
		for key, values := range headers {
			if !strings.EqualFold(key, name) {
				continue
			}
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		}
		return ""
	}
	allowed := get("X-Codex-Allowed")
	limitReached := get("X-Codex-Limit-Reached")
	limited := allowed == "false" || limitReached == "true"

	quota := ParsedQuota{}
	if window := quotaWindowFromHeaders(get, "X-Codex-Primary", limited, now); window != nil {
		quota.FiveHour = window
	}
	if window := quotaWindowFromHeaders(get, "X-Codex-Secondary", limited, now); window != nil {
		quota.LongWindow = window
		quota.Family = familyForWindowMinutes(window.LimitWindowSeconds)
	}
	return quota, quota.FiveHour != nil || quota.LongWindow != nil
}

func quotaWindowFromHeaders(get func(string) string, prefix string, limited bool, now time.Time) *QuotaWindow {
	usedText := get(prefix + "-Used-Percent")
	if usedText == "" {
		return nil
	}
	used, err := strconv.ParseFloat(usedText, 64)
	if err != nil || used < 0 || used > 100 {
		return nil
	}
	window := &QuotaWindow{UsedPercent: &used}
	if minutesText := get(prefix + "-Window-Minutes"); minutesText != "" {
		if minutes, err := strconv.ParseFloat(minutesText, 64); err == nil && minutes > 0 {
			seconds := int64(minutes * 60)
			window.LimitWindowSeconds = &seconds
		}
	}
	if resetText := get(prefix + "-Reset-At"); resetText != "" {
		if unix, err := strconv.ParseFloat(resetText, 64); err == nil && unix > 0 {
			window.ResetAt = time.Unix(int64(unix), 0).UTC()
		}
	}
	if window.ResetAt.IsZero() {
		if afterText := get(prefix + "-Reset-After-Seconds"); afterText != "" {
			if after, err := strconv.ParseFloat(afterText, 64); err == nil && after >= 0 {
				window.ResetAt = now.Add(time.Duration(after * float64(time.Second)))
			}
		}
	}
	if window.ResetAt.IsZero() {
		return nil
	}
	if limited || used >= 100 {
		window.Exhausted = true
	}
	return window
}
