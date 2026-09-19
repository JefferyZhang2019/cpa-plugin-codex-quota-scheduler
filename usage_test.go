package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDetectQuotaFailureUsageLimitReachedWithResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if event.AuthID != "auth-1" || !event.ResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureResetsInSeconds(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, now.Add(120*time.Second))
	}
}

func TestDetectQuotaFailureIgnoresGenericRateLimit(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"rate_limit_error"}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestDetectQuotaFailureTopLevelResetFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"},"resets_at":"2026-06-21T12:00:00Z"}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if event.AuthIndex != "idx-1" || !event.ResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureNumericErrorResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":1782043200}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(time.Unix(1782043200, 0).UTC()) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, time.Unix(1782043200, 0).UTC())
	}
}

func TestDetectQuotaFailureNumericTopLevelResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"},"resets_at":1782043200}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(time.Unix(1782043200, 0).UTC()) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, time.Unix(1782043200, 0).UTC())
	}
}

func TestDetectQuotaFailureInvalidResetAtUsesResetsInSeconds(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"not-a-time","resets_in_seconds":120}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, now.Add(120*time.Second))
	}
}

func TestDetectQuotaFailureInvalidResetFieldsFallsBack(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"not-a-time","resets_in_seconds":"later"}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(2*time.Minute)) || event.Reason != "usage_limit_reached_no_reset" {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureUsesFallbackResetWhenMissing(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(2*time.Minute)) || event.Reason != "usage_limit_reached_no_reset" {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureIgnoresMalformedBody(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `not-json`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestDetectQuotaFailureIgnoresNonCodexProvider(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "openai",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestDetectQuotaFailureIgnoresSuccessfulRecord(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   false,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestUsageHandleMarksTemporaryExhaustedByAuthIndex(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 2
	cfg.CircuitOpenDuration = 10 * time.Minute
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	HandleUsageFeedback(store, record, now)
	snapshot := store.Snapshot(now)
	if !snapshot.Accounts[0].TemporaryExhausted || !snapshot.Accounts[0].TemporaryResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("account after first failure = %#v", snapshot.Accounts[0])
	}
	status, available, reason, sortTime := accountQueueState(snapshot.Accounts[0], now)
	if status != QueueStatusFiveHourExhausted || available || reason != "temporary_exhausted" || !sortTime.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("queue after first failure = status=%s available=%t reason=%q sort=%s account=%#v", status, available, reason, sortTime, snapshot.Accounts[0])
	}

	HandleUsageFeedback(store, record, now.Add(time.Second))

	snapshot = store.Snapshot(now.Add(time.Second))
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	account := snapshot.Accounts[0]
	if !account.TemporaryExhausted || !account.TemporaryResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("account = %#v", account)
	}
	if account.Circuit.State != CircuitStateClosed || account.Circuit.FailureCount != 0 {
		t.Fatalf("circuit = %#v, want closed without quota failure count", account.Circuit)
	}
}

func TestUsageQuotaFailureMarksTemporaryExhaustedWithoutOpeningCircuit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	HandleUsageFeedback(store, record, now)

	account := store.Snapshot(now).Accounts[0]
	if !account.TemporaryExhausted {
		t.Fatalf("TemporaryExhausted = false, want true: %#v", account)
	}
	if !account.TemporaryResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("TemporaryResetAt = %s, want quota reset time", account.TemporaryResetAt)
	}
	if account.Circuit.State != CircuitStateClosed || account.Circuit.FailureCount != 0 {
		t.Fatalf("Circuit = %#v, want closed without quota failure count", account.Circuit)
	}
	status, available, reason, sortTime := accountQueueState(account, now)
	if status != QueueStatusFiveHourExhausted || available || reason != "temporary_exhausted" || !sortTime.Equal(account.TemporaryResetAt) {
		t.Fatalf("queue = status=%s available=%t reason=%q sort=%s account=%#v", status, available, reason, sortTime, account)
	}
}

func TestUsageSuccessClearsTemporaryExhaustedWithoutCircuitProbe(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 5 * time.Minute
	cfg.CircuitHalfOpenSuccessThreshold = 2
	store := NewPluginState(cfg)
	account := weeklyAccount("auth-1", 5, time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC), false)
	store.UpsertQuota(account)

	fail := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	HandleUsageFeedback(store, fail, now)

	snapshot := store.Snapshot(now.Add(6 * time.Minute))
	if !snapshot.Accounts[0].TemporaryExhausted || snapshot.Accounts[0].Circuit.EffectiveState != CircuitStateClosed {
		t.Fatalf("account after quota failure = %#v", snapshot.Accounts[0])
	}

	// A real successful request is ground-truth recovery evidence (issue #11):
	// it clears the temporary-exhaustion marker even though the circuit itself
	// never opened. A later 429 would re-mark the account immediately.
	success := pluginapi.UsageRecord{Provider: "codex", AuthID: "auth-1", Failed: false}
	HandleUsageFeedback(store, success, now.Add(6*time.Minute))
	snapshot = store.Snapshot(now.Add(6 * time.Minute))
	if snapshot.Accounts[0].TemporaryExhausted {
		t.Fatalf("account after success = %#v", snapshot.Accounts[0])
	}
	if snapshot.Accounts[0].Circuit.EffectiveState != CircuitStateClosed || snapshot.Accounts[0].Circuit.SuccessCount != 0 {
		t.Fatalf("circuit after success = %#v, want closed without success counting", snapshot.Accounts[0].Circuit)
	}
}

func TestUsageHandleDisabledDoesNotMutateState(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableUsageFeedback = false
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	HandleUsageFeedback(store, record, time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))

	snapshot := store.Snapshot(time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))
	if snapshot.Accounts[0].TemporaryExhausted {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}

func TestUsageLimitWithoutResetSchedulesShortPause(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"}}`,
		},
	}

	HandleUsageFeedback(store, record, now)

	account := store.Snapshot(now).Accounts[0]
	if account.TemporaryResetAt.IsZero() {
		t.Fatal("TemporaryResetAt is zero, want short pause")
	}
	if !account.TemporaryResetAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("TemporaryResetAt = %s, want %s", account.TemporaryResetAt, now.Add(2*time.Minute))
	}
	if account.Circuit.State != CircuitStateClosed || account.Circuit.FailureCount != 0 {
		t.Fatalf("Circuit = %#v, want closed without quota failure count", account.Circuit)
	}
}

func TestUsageFeedbackSuccessClearsTemporaryExhausted(t *testing.T) {
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now})
	store.MarkAccountTemporaryExhausted("team", now.Add(3*time.Hour), usageLimitReachedReason)
	// Circuit is pristine, so the old gate skipped success recording entirely.
	HandleUsageFeedback(store, pluginapi.UsageRecord{Provider: "codex", AuthID: "team", AuthIndex: "idx-team"}, now)
	if account := accountByAuthID(t, store.Snapshot(now), "team"); account.TemporaryExhausted {
		t.Fatalf("TemporaryExhausted survived a successful request: %#v", account)
	}
	var clearedSeen bool
	for _, entry := range store.Snapshot(now).Logs {
		if entry.Event != "circuit.success" {
			continue
		}
		if value, ok := entry.Fields["temporary_exhausted_cleared"]; ok && value == true {
			clearedSeen = true
		}
	}
	if !clearedSeen {
		t.Fatal("circuit.success log missing temporary_exhausted_cleared field")
	}
}
