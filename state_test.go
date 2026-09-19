package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReplaceCPAAdmissionPrunesExcludedAccounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "high", Provider: "codex", Priority: 1})
	store.UpsertQuota(AccountState{AuthID: "low", Provider: "codex", Priority: 0})
	store.ReplaceCPAAdmission(CPAAdmissionState{
		Observed: true,
		Priority: 1,
		AuthIDs:  map[string]struct{}{"high": {}},
	})
	snapshot := store.Snapshot(time.Now())
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "high" {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	if store.IsAuthAdmitted("low") || !store.IsAuthAdmitted("high") {
		t.Fatalf("admission = %#v", store.CPAAdmission())
	}
}

func TestReplaceCPAAdmissionReplacesOldTier(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"old": {}}})
	store.UpsertQuota(AccountState{AuthID: "old", Provider: "codex", Priority: 1})
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"new": {}}})
	if store.IsAuthAdmitted("old") || !store.IsAuthAdmitted("new") || len(store.Snapshot(time.Now()).Accounts) != 0 {
		t.Fatalf("state not replaced: %#v", store.Snapshot(time.Now()))
	}
}

func TestCPAAdmissionVersionChangesOnlyWhenAdmissionChanges(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	first := CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}}
	version1 := store.ReplaceCPAAdmission(first)
	if version1 == 0 {
		t.Fatal("first admission version is zero")
	}
	if same := store.ReplaceCPAAdmission(first); same != version1 {
		t.Fatalf("same admission version = %d, want %d", same, version1)
	}
	version2 := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	if version2 <= version1 {
		t.Fatalf("changed admission version = %d, want greater than %d", version2, version1)
	}
}

func TestConditionalRefreshMutationsRejectStaleAdmissionVersion(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	account := AccountState{AuthID: "a", AuthIndex: "idx-a", Provider: "codex", LastSuccessAt: now}
	if store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(account, version, now) {
		t.Fatal("stale success mutation was accepted")
	}
	if store.ApplyQuotaRefreshFailureIfAdmissionCurrent(account, version, RefreshFailureTransient, "failed", now) {
		t.Fatal("stale failure mutation was accepted")
	}
	if store.RecordLogIfAdmissionCurrent("a", version, "warn", "quota.refresh_failed", "failed", nil, now) {
		t.Fatal("stale log mutation was accepted")
	}
	if snapshot := store.Snapshot(now); len(snapshot.Accounts) != 0 || len(snapshot.Logs) != 0 {
		t.Fatalf("snapshot = %#v, want no stale result mutations", snapshot)
	}
}

func TestPluginStateUpsertsAndSnapshotsAccounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Provider:  "codex",
		Priority:  5,
		Family:    AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family:     AccountFamilyWeekly,
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)},
		},
		LastRefreshAt: now,
		LastSuccessAt: now,
	})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].AuthID != "auth-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}

func TestPluginStateClonesResetProbes(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	account := AccountState{
		AuthID: "auth-1",
		ResetProbes: map[WindowKind]ResetProbeState{
			WindowFiveHour: {
				WindowKind: WindowFiveHour,
				Status:     ResetProbeStatusPending,
			},
		},
	}

	store.UpsertQuota(account)
	probe := account.ResetProbes[WindowFiveHour]
	probe.Status = ResetProbeStatusFailed
	account.ResetProbes[WindowFiveHour] = probe

	snapshot := store.Snapshot(now)
	if got := snapshot.Accounts[0].ResetProbes[WindowFiveHour].Status; got != ResetProbeStatusPending {
		t.Fatalf("stored reset probe status after source mutation = %q, want %q", got, ResetProbeStatusPending)
	}

	probe = snapshot.Accounts[0].ResetProbes[WindowFiveHour]
	probe.Status = ResetProbeStatusVerified
	snapshot.Accounts[0].ResetProbes[WindowFiveHour] = probe

	second := store.Snapshot(now)
	if got := second.Accounts[0].ResetProbes[WindowFiveHour].Status; got != ResetProbeStatusPending {
		t.Fatalf("stored reset probe status after snapshot mutation = %q, want %q", got, ResetProbeStatusPending)
	}
}

func TestScheduleResetProbeFromMaturedPreviousReset(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(time.Minute)
	seconds := int64(fiveHourSeconds)
	previous := AccountState{
		AuthID: "auth-1",
		Quota: ParsedQuota{
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt},
		},
	}
	next := scheduleResetProbesFromPrevious(previous, nil, now)
	probe, ok := next[WindowFiveHour]
	if !ok {
		t.Fatal("five-hour reset probe was not scheduled")
	}
	if probe.Status != ResetProbeStatusPending {
		t.Fatalf("Status = %q, want pending", probe.Status)
	}
	if !probe.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %s, want %s", probe.ResetAt, resetAt)
	}
	if !probe.NextCheckAt.Equal(resetAt.Add(10 * time.Minute)) {
		t.Fatalf("NextCheckAt = %s, want %s", probe.NextCheckAt, resetAt.Add(10*time.Minute))
	}
}

func TestScheduleResetProbeKeepsSeparateWindowBaselines(t *testing.T) {
	fiveReset := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	weeklyReset := fiveReset.Add(2 * time.Minute)
	now := weeklyReset.Add(time.Minute)
	fiveSeconds := int64(fiveHourSeconds)
	weeklySeconds := int64(7 * 24 * 60 * 60)
	previous := AccountState{
		AuthID: "auth-1",
		Quota: ParsedQuota{
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &fiveSeconds, ResetAt: fiveReset},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, LimitWindowSeconds: &weeklySeconds, ResetAt: weeklyReset},
		},
	}
	next := scheduleResetProbesFromPrevious(previous, nil, now)
	if !next[WindowFiveHour].ResetAt.Equal(fiveReset) {
		t.Fatalf("five-hour ResetAt = %s, want %s", next[WindowFiveHour].ResetAt, fiveReset)
	}
	if !next[WindowWeekly].ResetAt.Equal(weeklyReset) {
		t.Fatalf("weekly ResetAt = %s, want %s", next[WindowWeekly].ResetAt, weeklyReset)
	}
}

func TestScheduleResetProbePreservesExistingState(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(time.Minute)
	retryAt := resetAt.Add(30 * time.Minute)
	seconds := int64(fiveHourSeconds)
	previous := AccountState{
		AuthID: "auth-1",
		Quota: ParsedQuota{
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt},
		},
	}
	current := map[WindowKind]ResetProbeState{
		WindowFiveHour: {
			WindowKind:  WindowFiveHour,
			Status:      ResetProbeStatusFailed,
			ResetAt:     resetAt,
			NextCheckAt: retryAt,
			Attempts:    2,
			Error:       "kept",
		},
		WindowWeekly: {
			WindowKind:  WindowWeekly,
			Status:      ResetProbeStatusPending,
			ResetAt:     resetAt.Add(time.Hour),
			NextCheckAt: retryAt.Add(time.Hour),
		},
	}

	next := scheduleResetProbesFromPrevious(previous, current, now)
	five := next[WindowFiveHour]
	if five.Status != ResetProbeStatusFailed || five.Attempts != 2 || five.Error != "kept" || !five.NextCheckAt.Equal(retryAt) {
		t.Fatalf("five-hour probe = %#v, want preserved failed probe", five)
	}
	if _, ok := next[WindowWeekly]; !ok {
		t.Fatal("weekly probe was dropped")
	}
}

func TestResetProbeRetryRedactsCredentialsAndEventuallyFails(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.RefreshRetryDelays = []time.Duration{time.Minute}
	credentials := CodexCredentials{
		AccessToken: "access-1",
		IDToken:     "id-1",
	}
	probe := ResetProbeState{
		WindowKind: WindowFiveHour,
		ResetAt:    now.Add(-10 * time.Minute),
		Status:     ResetProbeStatusPending,
	}

	first := markResetProbeRetry(probe, now, errors.New("upstream echoed access-1 and id-1"), credentials, cfg)
	if first.Status != ResetProbeStatusPending {
		t.Fatalf("first retry Status = %q, want pending", first.Status)
	}
	if !first.NextCheckAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first retry NextCheckAt = %s, want %s", first.NextCheckAt, now.Add(time.Minute))
	}
	if first.Error == "" {
		t.Fatal("first retry Error empty, want redacted error")
	}
	for _, leaked := range []string{"access-1", "id-1"} {
		if strings.Contains(first.Error, leaked) {
			t.Fatalf("first retry Error leaked %q: %q", leaked, first.Error)
		}
	}

	second := markResetProbeRetry(first, now.Add(time.Minute), errors.New("still failed"), credentials, cfg)
	if second.Status != ResetProbeStatusFailed {
		t.Fatalf("second retry Status = %q, want failed", second.Status)
	}
	if !second.NextCheckAt.IsZero() {
		t.Fatalf("second retry NextCheckAt = %s, want zero", second.NextCheckAt)
	}
}

func TestNextRefreshDueAtIncludesPendingResetProbe(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(2 * time.Minute)
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID: "auth-1",
		ResetProbes: map[WindowKind]ResetProbeState{
			WindowFiveHour: {
				WindowKind:  WindowFiveHour,
				Status:      ResetProbeStatusPending,
				ResetAt:     resetAt,
				NextCheckAt: resetAt.Add(10 * time.Minute),
			},
		},
		LastSuccessAt: now,
	})
	next := store.NextRefreshDueAt(now)
	if !next.Equal(resetAt.Add(10 * time.Minute)) {
		t.Fatalf("NextRefreshDueAt = %s, want %s", next, resetAt.Add(10*time.Minute))
	}
}

func TestResetProbeDueIgnoredWhenDisabled(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableResetProbe = false
	account := AccountState{
		AuthID:        "auth-1",
		LastSuccessAt: now,
		ResetProbes: map[WindowKind]ResetProbeState{
			WindowFiveHour: {
				WindowKind:  WindowFiveHour,
				Status:      ResetProbeStatusPending,
				ResetAt:     now.Add(-10 * time.Minute),
				NextCheckAt: now,
			},
		},
	}

	due, reason := accountRefreshDue(account, cfg, now)
	if due || reason != "" {
		t.Fatalf("accountRefreshDue = %t, %q; want false with empty reason", due, reason)
	}

	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(account)
	next := store.NextRefreshDueAt(now)
	if next.Equal(now) {
		t.Fatalf("NextRefreshDueAt = %s, want reset probe time ignored when disabled", next)
	}
}

func TestAccountRefreshDueReturnsResetProbeCheckDue(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	account := AccountState{
		AuthID:        "auth-1",
		LastSuccessAt: now,
		ResetProbes: map[WindowKind]ResetProbeState{
			WindowFiveHour: {
				WindowKind:  WindowFiveHour,
				Status:      ResetProbeStatusPending,
				ResetAt:     now.Add(-10 * time.Minute),
				NextCheckAt: now,
			},
		},
	}

	due, reason := accountRefreshDue(account, cfg, now)
	if !due || reason != "reset_probe_check_due" {
		t.Fatalf("accountRefreshDue = %t, %q; want true reset_probe_check_due", due, reason)
	}
}

func TestAccountRefreshDueIgnoresResetConsumedBySuccess(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	sources := []struct {
		name   string
		reason string
		apply  func(*AccountState, time.Time)
	}{
		{name: "five-hour", reason: "five_hour_reset_due", apply: func(account *AccountState, resetAt time.Time) {
			account.Quota.FiveHour = &QuotaWindow{Kind: WindowFiveHour, ResetAt: resetAt}
		}},
		{name: "long-window", reason: "long_window_reset_due", apply: func(account *AccountState, resetAt time.Time) {
			account.Quota.LongWindow = &QuotaWindow{Kind: WindowWeekly, ResetAt: resetAt}
		}},
		{name: "temporary", reason: "temporary_reset_due", apply: func(account *AccountState, resetAt time.Time) {
			account.TemporaryExhausted = true
			account.TemporaryResetAt = resetAt
		}},
	}
	tests := []struct {
		name          string
		resetAt       time.Time
		lastSuccessAt time.Time
		wantDue       bool
		wantReason    bool
	}{
		{name: "pending past trigger is due", resetAt: now.Add(-2 * time.Minute), lastSuccessAt: now.Add(-2 * time.Minute), wantDue: true, wantReason: true},
		{name: "success after trigger consumes", resetAt: now.Add(-10 * time.Minute), lastSuccessAt: now.Add(-time.Minute)},
		{name: "success at trigger consumes", resetAt: now.Add(-10 * time.Minute), lastSuccessAt: now.Add(-9 * time.Minute)},
		{name: "future trigger remains pending but not due", resetAt: now.Add(10 * time.Minute), lastSuccessAt: now.Add(-time.Minute)},
		{name: "zero reset is ignored", lastSuccessAt: now.Add(-time.Minute)},
	}
	for _, source := range sources {
		source := source
		for _, tt := range tests {
			tt := tt
			t.Run(source.name+"/"+tt.name, func(t *testing.T) {
				account := AccountState{AuthID: "auth-1", LastSuccessAt: tt.lastSuccessAt}
				source.apply(&account, tt.resetAt)
				due, reason := accountRefreshDue(account, cfg, now)
				wantReason := ""
				if tt.wantReason {
					wantReason = source.reason
				}
				if due != tt.wantDue || reason != wantReason {
					t.Fatalf("due=%t reason=%q wantDue=%t wantReason=%q", due, reason, tt.wantDue, wantReason)
				}
			})
		}
	}
}

func TestNextRefreshDueAtIgnoresConsumedReset(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	sources := []struct {
		name  string
		apply func(*AccountState, time.Time)
	}{
		{name: "five-hour", apply: func(account *AccountState, resetAt time.Time) {
			account.Quota.FiveHour = &QuotaWindow{Kind: WindowFiveHour, ResetAt: resetAt}
		}},
		{name: "long-window", apply: func(account *AccountState, resetAt time.Time) {
			account.Quota.LongWindow = &QuotaWindow{Kind: WindowWeekly, ResetAt: resetAt}
		}},
		{name: "temporary", apply: func(account *AccountState, resetAt time.Time) {
			account.TemporaryExhausted = true
			account.TemporaryResetAt = resetAt
		}},
	}
	tests := []struct {
		name          string
		resetAt       time.Time
		lastSuccessAt time.Time
		want          time.Time
	}{
		{name: "pending past trigger is due now", resetAt: now.Add(-2 * time.Minute), lastSuccessAt: now.Add(-2 * time.Minute), want: now},
		{name: "success after trigger consumes", resetAt: now.Add(-10 * time.Minute), lastSuccessAt: now.Add(-time.Minute)},
		{name: "success at trigger consumes", resetAt: now.Add(-10 * time.Minute), lastSuccessAt: now.Add(-9 * time.Minute)},
		{name: "future trigger remains pending", resetAt: now.Add(10 * time.Minute), lastSuccessAt: now.Add(-time.Minute), want: now.Add(11 * time.Minute)},
		{name: "zero reset is ignored", lastSuccessAt: now.Add(-time.Minute)},
	}
	for _, source := range sources {
		source := source
		for _, tt := range tests {
			tt := tt
			t.Run(source.name+"/"+tt.name, func(t *testing.T) {
				store := NewPluginState(cfg)
				store.RecordCodexActivity(now)
				account := AccountState{AuthID: "auth-1", LastSuccessAt: tt.lastSuccessAt}
				source.apply(&account, tt.resetAt)
				store.UpsertQuota(account)
				if got := store.NextRefreshDueAt(now); !got.Equal(tt.want) {
					t.Fatalf("next=%s want=%s", got, tt.want)
				}
			})
		}
	}
}

func TestPluginStateNormalizesConfigAtBoundaries(t *testing.T) {
	store := NewPluginState(Config{})
	if store.Config().MaxLogEntries != DefaultConfig().MaxLogEntries || store.Config().LogRetention != DefaultConfig().LogRetention {
		t.Fatalf("initial config = %#v, want normalized defaults", store.Config())
	}

	store.ReplaceConfig(Config{})
	if store.Config().QuotaRefreshInterval != DefaultConfig().QuotaRefreshInterval || store.Config().StaleAfter != DefaultConfig().StaleAfter {
		t.Fatalf("replaced config = %#v, want normalized defaults", store.Config())
	}
}

func TestPluginStateMarksStale(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StaleAfter = time.Minute
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", LastSuccessAt: now.Add(-2 * time.Minute)})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || !snapshot.Accounts[0].Stale {
		t.Fatalf("snapshot = %#v", snapshot.Accounts)
	}
}

func TestPluginStateTemporaryExhaustedIgnoresEmptyAuthID(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.MarkAccountTemporaryExhausted("", now.Add(time.Hour), "usage exhausted")
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want none", snapshot.Accounts)
	}
}

func TestPluginStateMarksTemporaryExhaustedByAuthIndex(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1"})
	store.MarkAccountTemporaryExhaustedByAuthIndex("idx-1", resetAt, "usage exhausted")
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	account := snapshot.Accounts[0]
	if !account.TemporaryExhausted || !account.TemporaryResetAt.Equal(resetAt) || account.LastError != "usage exhausted" {
		t.Fatalf("account = %#v", account)
	}
}

func TestPluginStateOpenCircuitIgnoresEarlySuccess(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 10 * time.Minute
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	failed, ok := store.RecordAccountFailure("auth-1", "", "usage_limit_reached", time.Time{}, now)
	if !ok || failed.Circuit.State != CircuitStateOpen {
		t.Fatalf("failed account = %#v, ok=%t; want open circuit", failed, ok)
	}
	success, ok := store.RecordAccountSuccess("auth-1", "", now.Add(time.Minute))
	if !ok {
		t.Fatalf("RecordAccountSuccess returned ok=false")
	}
	if success.Circuit.State != CircuitStateOpen || success.Circuit.EffectiveState != CircuitStateOpen {
		t.Fatalf("circuit = %#v, want still open before next probe", success.Circuit)
	}
	if success.LastError == "" {
		t.Fatalf("LastError was cleared while circuit is still open: %#v", success)
	}
}

func TestPluginStateHalfOpenSuccessClosesCircuit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 10 * time.Minute
	cfg.CircuitHalfOpenSuccessThreshold = 1
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordAccountFailure("auth-1", "", "usage_limit_reached", time.Time{}, now)

	success, ok := store.RecordAccountSuccess("auth-1", "", now.Add(11*time.Minute))
	if !ok {
		t.Fatalf("RecordAccountSuccess returned ok=false")
	}
	if success.Circuit.State != CircuitStateClosed || success.LastError != "" {
		t.Fatalf("account = %#v, want half-open probe success to close circuit", success)
	}
}

func TestPluginStateHalfOpenPartialSuccessClearsProbeDue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 10 * time.Minute
	cfg.CircuitHalfOpenSuccessThreshold = 2
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now})
	store.RecordAccountFailure("auth-1", "", "usage_limit_reached", time.Time{}, now)

	success, ok := store.RecordAccountSuccess("auth-1", "", now.Add(11*time.Minute))
	if !ok {
		t.Fatalf("RecordAccountSuccess returned ok=false")
	}
	if success.Circuit.State != CircuitStateHalfOpen || success.Circuit.EffectiveState != CircuitStateHalfOpen || success.Circuit.SuccessCount != 1 {
		t.Fatalf("circuit = %#v, want persistent half-open partial success", success.Circuit)
	}
	if !success.Circuit.NextProbeAt.IsZero() {
		t.Fatalf("NextProbeAt = %s, want cleared after partial half-open success", success.Circuit.NextProbeAt)
	}
	if due, reason := accountRefreshDue(success, cfg, now.Add(11*time.Minute)); due || reason == "circuit_probe_due" {
		t.Fatalf("accountRefreshDue = %t, %q; want no repeated circuit probe", due, reason)
	}
}

func TestPluginStateLogRetentionKeepsNewestEntriesByCount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 3
	cfg.LogRetention = 24 * time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		store.RecordLog("info", "event", string(rune('a'+i)), nil, now.Add(time.Duration(i)*time.Minute))
	}

	logs := store.Snapshot(now.Add(5 * time.Minute)).Logs
	if len(logs) != 3 {
		t.Fatalf("logs len = %d, want 3: %#v", len(logs), logs)
	}
	if logs[0].Message != "c" || logs[2].Message != "e" {
		t.Fatalf("logs = %#v, want newest c,d,e", logs)
	}
}

func TestPluginStateLogRetentionDropsEntriesOlderThanRetention(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 10
	cfg.LogRetention = time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	store.RecordLog("info", "event", "old", nil, now.Add(-2*time.Hour))
	store.RecordLog("info", "event", "new", nil, now.Add(-30*time.Minute))

	logs := store.Snapshot(now).Logs
	if len(logs) != 1 || logs[0].Message != "new" {
		t.Fatalf("logs = %#v, want only new log", logs)
	}
}

func TestPluginStateLogRetentionDropsOldEntriesEvenWhenOutOfOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 10
	cfg.LogRetention = time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	store.RecordLog("info", "event", "new-before", nil, now.Add(-30*time.Minute))
	store.RecordLog("info", "event", "old-after", nil, now.Add(-2*time.Hour))
	store.RecordLog("info", "event", "new-after", nil, now.Add(-10*time.Minute))

	logs := store.Snapshot(now).Logs
	if len(logs) != 2 {
		t.Fatalf("logs len = %d, want 2: %#v", len(logs), logs)
	}
	if logs[0].Message != "new-before" || logs[1].Message != "new-after" {
		t.Fatalf("logs = %#v, want only non-expired entries in original order", logs)
	}
}

func TestRecordCodexActivityControlsActiveWindow(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	if store.RefreshActive(now) {
		t.Fatal("RefreshActive before activity = true, want false")
	}
	store.RecordCodexActivity(now)
	if !store.RefreshActive(now.Add(59 * time.Minute)) {
		t.Fatal("RefreshActive inside 1h window = false, want true")
	}
	if store.RefreshActive(now.Add(time.Hour + time.Second)) {
		t.Fatal("RefreshActive after 1h window = true, want false")
	}
}

func TestQuotaRefreshIntervalMakesSuccessfulAccountDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	cfg.StaleAfter = 5 * time.Hour
	account := AccountState{AuthID: "auth-1", LastSuccessAt: now.Add(-31 * time.Minute)}

	due, reason := accountRefreshDue(account, cfg, now)
	if !due || reason != "refresh_interval_due" {
		t.Fatalf("accountRefreshDue = %t, %q; want true refresh_interval_due", due, reason)
	}
}

func TestNextRefreshDueAtDoesNotOwnQuotaRefreshInterval(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	cfg.StaleAfter = 5 * time.Hour
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{AuthID: "auth-1", LastSuccessAt: now.Add(-10 * time.Minute)})

	next := store.NextRefreshDueAt(now)
	if !next.IsZero() {
		t.Fatalf("NextRefreshDueAt = %s, want zero; RefreshController owns interval", next)
	}
}

func TestRecordRefreshFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "request failed", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if updated.Refresh.RetryAttempt != 1 {
		t.Fatalf("RetryAttempt = %d, want 1", updated.Refresh.RetryAttempt)
	}
	if !updated.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", updated.Refresh.NextRetryAt, now.Add(time.Minute))
	}
	if updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
}

func TestFutureRetryPreventsNeverRefreshedDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "request failed", now)

	due := store.DueAccounts(now.Add(30 * time.Second))
	if len(due) != 0 {
		t.Fatalf("due accounts before retry time = %#v, want none", due)
	}

	due = store.DueAccounts(now.Add(time.Minute))
	if len(due) != 1 || due[0].Refresh.DueReason != "retry_due" {
		t.Fatalf("due accounts at retry time = %#v, want retry_due account", due)
	}
}

func TestCircuitProbeControlsRefreshDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	account := AccountState{
		AuthID:   "auth-1",
		Provider: "codex",
		Circuit: CircuitBreakerState{
			State:       CircuitStateOpen,
			NextProbeAt: now.Add(2 * time.Minute),
		},
	}
	if due, reason := accountRefreshDue(account, cfg, now); due || reason != "circuit_wait" {
		t.Fatalf("accountRefreshDue before probe = %t, %q; want false circuit_wait", due, reason)
	}
	if due, reason := accountRefreshDue(account, cfg, now.Add(2*time.Minute)); !due || reason != "circuit_probe_due" {
		t.Fatalf("accountRefreshDue at probe = %t, %q; want true circuit_probe_due", due, reason)
	}
}

func TestClosedCircuitProbeFailureControlsRefreshDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	account := AccountState{
		AuthID:        "auth-1",
		Provider:      "codex",
		LastSuccessAt: now.Add(-time.Hour),
		Circuit: CircuitBreakerState{
			State:          CircuitStateClosed,
			EffectiveState: CircuitStateClosed,
			FailureCount:   1,
			NextProbeAt:    now.Add(2 * time.Minute),
		},
	}
	if due, reason := accountRefreshDue(account, cfg, now); due || reason != "circuit_wait" {
		t.Fatalf("accountRefreshDue before closed probe = %t, %q; want false circuit_wait", due, reason)
	}
	if due, reason := accountRefreshDue(account, cfg, now.Add(2*time.Minute)); !due || reason != "circuit_probe_due" {
		t.Fatalf("accountRefreshDue at closed probe = %t, %q; want true circuit_probe_due", due, reason)
	}
}

func TestRecordRefreshAuthFailureStopsRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureAuth, "please re-login", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if !updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if !updated.Refresh.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt = %s, want zero", updated.Refresh.NextRetryAt)
	}
}

func TestLocalRefreshFailureBlocksAutomaticDueRefresh(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureLocal, "missing access token", now)

	due := store.DueAccounts(now.Add(24 * time.Hour))
	if len(due) != 0 {
		t.Fatalf("due accounts after local failure = %#v, want none", due)
	}
	account := store.Snapshot(now).Accounts[0]
	if ok, reason := accountRefreshDue(account, store.Config(), now); ok || reason != "local_failure" {
		t.Fatalf("accountRefreshDue = %t, %q; want false local_failure", ok, reason)
	}
}

func TestAccountRefreshDueReasons(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	if due, reason := accountRefreshDue(AccountState{AuthID: "a"}, cfg, now); !due || reason != "never_refreshed" {
		t.Fatalf("never refreshed due=%v reason=%q, want true never_refreshed", due, reason)
	}
	stale := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	if due, reason := accountRefreshDue(stale, cfg, now); !due || reason != "stale" {
		t.Fatalf("stale due=%v reason=%q, want true stale", due, reason)
	}
	retry := AccountState{AuthID: "a", LastSuccessAt: now.Add(-time.Hour)}
	retry.Refresh.NextRetryAt = now.Add(-time.Second)
	if due, reason := accountRefreshDue(retry, cfg, now); !due || reason != "retry_due" {
		t.Fatalf("retry due=%v reason=%q, want true retry_due", due, reason)
	}
	authFailed := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	authFailed.Refresh.AuthFailure = true
	if due, reason := accountRefreshDue(authFailed, cfg, now); due || reason != "auth_failure" {
		t.Fatalf("auth failure due=%v reason=%q, want false auth_failure", due, reason)
	}
}

func TestPluginStateDueAccountsAnnotatesReason(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "fresh", LastSuccessAt: now})
	store.UpsertQuota(AccountState{AuthID: "stale", LastSuccessAt: now.Add(-6 * time.Hour)})

	due := store.DueAccounts(now)
	if len(due) != 1 {
		t.Fatalf("due accounts len = %d, want 1: %#v", len(due), due)
	}
	if due[0].AuthID != "stale" || due[0].Refresh.DueReason != "stale" {
		t.Fatalf("due account = %#v, want stale with reason", due[0])
	}
}

func TestResetProbeConstantsAvoidEarlyMisclassification(t *testing.T) {
	if resetProbeAfterResetDelay <= resetProbeCloseThreshold {
		t.Fatalf("resetProbeAfterResetDelay = %s must be greater than resetProbeCloseThreshold = %s", resetProbeAfterResetDelay, resetProbeCloseThreshold)
	}
	if resetProbeAfterResetDelay != 10*time.Minute {
		t.Fatalf("resetProbeAfterResetDelay = %s, want 10m", resetProbeAfterResetDelay)
	}
	if resetProbeCloseThreshold != 3*time.Minute {
		t.Fatalf("resetProbeCloseThreshold = %s, want 3m", resetProbeCloseThreshold)
	}
}

func TestLooksLikeLazyReset(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 10, 0, 0, time.UTC)
	seconds := int64(fiveHourSeconds)
	lazy := QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: now.Add(5 * time.Hour)}
	if !looksLikeLazyReset(now, lazy, seconds) {
		t.Fatal("lazy reset was not detected")
	}
	active := QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: now.Add(5*time.Hour - 10*time.Minute)}
	if looksLikeLazyReset(now, active, seconds) {
		t.Fatal("active reset was misclassified as lazy")
	}
}

func TestMonthlyProbeRequiresWindowSeconds(t *testing.T) {
	monthly := QuotaWindow{Kind: WindowMonthly, ResetAt: time.Now().Add(30 * 24 * time.Hour)}
	if _, ok := probeWindowDuration(monthly); ok {
		t.Fatal("monthly window without limit_window_seconds returned duration, want none")
	}
	seconds := int64(2592000)
	monthly.LimitWindowSeconds = &seconds
	d, ok := probeWindowDuration(monthly)
	if !ok || d != 30*24*time.Hour {
		t.Fatalf("probeWindowDuration = %s, %v; want 720h,true", d, ok)
	}
}

func TestResetProbeUsageEvidence(t *testing.T) {
	if !resetProbeUsageEvidence([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)) {
		t.Fatal("usage evidence not detected")
	}
	if resetProbeUsageEvidence([]byte(`{"id":"probe","usage":{}}`)) {
		t.Fatal("empty usage was accepted")
	}
}

func TestRecordAccountSuccessClearsTemporaryExhausted(t *testing.T) {
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now})
	store.MarkAccountTemporaryExhausted("team", now.Add(3*time.Hour), usageLimitReachedReason)
	if account := accountByAuthID(t, store.Snapshot(now), "team"); !account.TemporaryExhausted {
		t.Fatal("precondition: TemporaryExhausted should be set")
	}
	account, ok := store.RecordAccountSuccess("team", "idx-team", now)
	if !ok {
		t.Fatal("RecordAccountSuccess reported no account")
	}
	if account.TemporaryExhausted || !account.TemporaryResetAt.IsZero() {
		t.Fatalf("temporary exhaustion survived a real request success: %#v", account)
	}
}

func TestQuotaRefreshAutoClearsTemporaryExhaustionOnWindowIdentityChange(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"auth-1": {}}})
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.MarkAccountTemporaryExhausted("auth-1", now.Add(3*time.Hour), usageLimitReachedReason)

	// Upstream reset minted a new five-hour window (reset moved from 15:00 to
	// 17:00) with remaining capacity in both windows.
	fresh := accountByAuthID(t, store.Snapshot(now), "auth-1")
	fresh.Quota = ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: float64Ptr(0), ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(10), ResetAt: now.Add(72 * time.Hour)},
	}
	fresh.Family = AccountFamilyWeekly
	fresh.LastSuccessAt = now
	if !store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(fresh, version, now) {
		t.Fatal("fresh quota refresh was rejected")
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.TemporaryExhausted {
		t.Fatalf("temporary exhaustion survived a confirmed window-identity change: %#v", account)
	}
	status, available, reason, _ := accountQueueState(account, now)
	if status != QueueStatusAvailable || !available || reason != "" {
		t.Fatalf("queue = status=%s available=%t reason=%q, want available", status, available, reason)
	}
}

func TestQuotaRefreshKeepsTemporaryExhaustionOnSameWindowFullReading(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"auth-1": {}}})
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	markReset := now.Add(3 * time.Hour)
	store.MarkAccountTemporaryExhausted("auth-1", markReset, usageLimitReachedReason)

	// K12-style misreport: the quota endpoint shows a full five-hour window,
	// but it is the SAME window the 429 pointed at — no reset happened.
	fresh := accountByAuthID(t, store.Snapshot(now), "auth-1")
	fresh.Quota = ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: float64Ptr(0), ResetAt: markReset},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(10), ResetAt: now.Add(72 * time.Hour)},
	}
	fresh.Family = AccountFamilyWeekly
	fresh.LastSuccessAt = now
	if !store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(fresh, version, now) {
		t.Fatal("fresh quota refresh was rejected")
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if !account.TemporaryExhausted {
		t.Fatal("same-window full reading incorrectly cleared the temporary-exhaustion marker")
	}
}

func TestQuotaRefreshKeepsTemporaryExhaustionWhenWindowStillLimited(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"auth-1": {}}})
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.MarkAccountTemporaryExhausted("auth-1", now.Add(3*time.Hour), usageLimitReachedReason)

	fresh := accountByAuthID(t, store.Snapshot(now), "auth-1")
	fresh.Quota = ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: float64Ptr(100), Exhausted: true, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(10), ResetAt: now.Add(72 * time.Hour)},
	}
	fresh.Family = AccountFamilyWeekly
	fresh.LastSuccessAt = now
	if !store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(fresh, version, now) {
		t.Fatal("fresh quota refresh was rejected")
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if !account.TemporaryExhausted {
		t.Fatal("exhausted fresh window incorrectly cleared the temporary-exhaustion marker")
	}
}

func TestQuotaRefreshAutoClearsViaLongWindowWhenFiveHourAbsent(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"auth-1": {}}})
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.MarkAccountTemporaryExhausted("auth-1", now.Add(3*time.Hour), usageLimitReachedReason)

	fresh := accountByAuthID(t, store.Snapshot(now), "auth-1")
	fresh.Quota = ParsedQuota{
		Family:     AccountFamilyWeekly,
		LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(20), ResetAt: now.Add(48 * time.Hour)},
	}
	fresh.Family = AccountFamilyWeekly
	fresh.LastSuccessAt = now
	if !store.ApplyQuotaRefreshSuccessIfAdmissionCurrent(fresh, version, now) {
		t.Fatal("fresh quota refresh was rejected")
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.TemporaryExhausted {
		t.Fatalf("temporary exhaustion survived long-window fallback evidence: %#v", account)
	}
}
