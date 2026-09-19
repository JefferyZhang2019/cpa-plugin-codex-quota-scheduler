package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStreamObserverFramesRateLimitsAcrossChunkSplits(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	var frames [][]byte
	observer := &streamQuotaObserver{}
	// Split the frame at every awkward boundary: before the marker, inside the
	// marker, and inside the JSON payload.
	splits := []string{"event: codex.", "rate_limits\ndata: ", `{"type":"cod`, `ex.rate_limits","rate_limits":{"allowed":true,"limit_rea`, `ched":false,"primary":{"used_percent":25,"window_minutes":300,"reset_after_seconds":10800},"secondary":{"used_percent":10,"window_minutes":10080,"reset_after_seconds":86400}}}`, "\n"}
	for _, split := range splits {
		observer.absorb([]byte(split), func(frame []byte) { frames = append(frames, append([]byte(nil), frame...)) })
	}
	observer.absorb([]byte("\n"), func(frame []byte) { frames = append(frames, append([]byte(nil), frame...)) })
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1 assembled frame (got %#v)", len(frames), frames)
	}
	if !frameLooksLikeRateLimits(frames[0]) {
		t.Fatalf("assembled frame failed prefilter: %s", frames[0])
	}

	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	applyObservedRateLimits(store, "team", "idx-team", frames[0], now)
	account := accountByAuthID(t, store.Snapshot(now), "team")
	if account.Quota.FiveHour == nil || account.Quota.FiveHour.UsedPercent == nil || *account.Quota.FiveHour.UsedPercent != 25 {
		t.Fatalf("five-hour window not observed: %#v", account.Quota.FiveHour)
	}
	if account.Quota.LongWindow == nil || account.Quota.Family != AccountFamilyWeekly {
		t.Fatalf("long window/family not observed: %#v %s", account.Quota.LongWindow, account.Family)
	}
	if !account.LastObservedAt.Equal(now) {
		t.Fatalf("LastObservedAt = %s, want %s", account.LastObservedAt, now)
	}
	if !account.LastSuccessAt.Equal(now) {
		t.Fatalf("observation should refresh cache freshness, LastSuccessAt = %s", account.LastSuccessAt)
	}
}

func TestStreamObserverIgnoresOrdinaryFramesAndBouncesBuffer(t *testing.T) {
	observer := &streamQuotaObserver{}
	var handled int
	observer.absorb([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"), func([]byte) { handled++ })
	observer.absorb([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world this is a long ordinary frame with no quota marker at all\"}\n\n"), func([]byte) { handled++ })
	if handled != 0 {
		t.Fatalf("ordinary frames handled: %d", handled)
	}
	if len(observer.buffer) > len(rateLimitsEventMarker) {
		t.Fatalf("buffer not trimmed for ordinary stream: %d bytes", len(observer.buffer))
	}
	// Marker straddling the next chunk boundary is still detected.
	saw := false
	observer.absorb([]byte("event: codex.rate_lim"), func([]byte) {})
	observer.absorb([]byte("its\ndata: {}\n\n"), func([]byte) { saw = true })
	if !saw {
		t.Fatal("straddled marker frame was not assembled")
	}
}

func TestStreamChunkInterceptIsReadOnlyAndAttributes(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	previous := globalState
	t.Cleanup(func() { globalState = previous })
	globalState = NewPluginState(DefaultConfig())
	globalState.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})

	payload, err := json.Marshal(pluginapi.StreamChunkInterceptRequest{
		RequestID: "req-1", ChunkIndex: 3,
		Body: []byte("event: codex.rate_limits\ndata: {\"rate_limits\":{\"primary\":{\"used_percent\":40,\"window_minutes\":300,\"reset_after_seconds\":3600}}}\n\n"),
		Metadata: map[string]any{"selected_auth_id": "team"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod(pluginabi.MethodResponseInterceptStreamChunk, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("intercept response not ok: %s", raw)
	}
	// Read-only contract: the response result must not carry any body/headers.
	var envelope struct {
		Result pluginapi.StreamChunkInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Body) > 0 || envelope.Result.DropChunk || len(envelope.Result.Headers) > 0 {
		t.Fatalf("interceptor attempted to modify the stream: %#v", envelope.Result)
	}
	if account := accountByAuthID(t, globalState.Snapshot(now), "team"); account.Quota.FiveHour == nil {
		t.Fatal("rate_limits frame was not applied to the attributed account")
	}
}

func TestUsageRecordHeadersObserveQuotaOnSuccessOnly(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.MarkAccountTemporaryExhausted("team", now.Add(2*time.Hour), usageLimitReachedReason)

	// A failed record with a fresh-window header snapshot must NOT clear the
	// marker: the upstream just refused to serve the account.
	HandleUsageFeedback(store, pluginapi.UsageRecord{
		Provider: "codex", AuthID: "team", AuthIndex: "idx-team", Failed: true,
		Failure:        pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`},
		ResponseHeaders: map[string][]string{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"18000"}},
	}, now)
	if account := accountByAuthID(t, store.Snapshot(now), "team"); !account.TemporaryExhausted {
		t.Fatal("failed record incorrectly cleared the temporary marker")
	}

	// A successful record with headers observes the window and refreshes the cache.
	HandleUsageFeedback(store, pluginapi.UsageRecord{
		Provider: "codex", AuthID: "team", AuthIndex: "idx-team", Failed: false,
		ResponseHeaders: map[string][]string{"X-Codex-Primary-Used-Percent": {"0"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"18000"}},
	}, now)
	account := accountByAuthID(t, store.Snapshot(now), "team")
	if account.Quota.FiveHour == nil || account.LastObservedAt.IsZero() {
		t.Fatalf("usage headers not observed: %#v", account.Quota.FiveHour)
	}
	// Same-window full reading: the manual/identity-gated reconciliation does
	// not fire, but the real success itself clears the marker.
	if account.TemporaryExhausted {
		t.Fatal("successful request should have cleared the temporary marker")
	}
}

func TestQuotaSourceStatusProjection(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "polled", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now})
	store.UpsertQuota(AccountState{AuthID: "observed", AuthIndex: "idx-2", Provider: "codex", LastSuccessAt: now, LastObservedAt: now})
	snapshot := store.Snapshot(now)
	payload := BuildStatusPayload(snapshot, BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now))
	byID := map[string]string{}
	for _, account := range payload.Accounts {
		byID[account.AuthID] = account.QuotaSource
	}
	if byID["polled"] != "polled" {
		t.Fatalf("polled account source = %q", byID["polled"])
	}
	if byID["observed"] != "observed" {
		t.Fatalf("observed account source = %q", byID["observed"])
	}
}
