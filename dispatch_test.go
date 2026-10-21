package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func withGlobalRefresherForTest(t *testing.T, store *PluginState, refresher *QuotaRefresher) {
	t.Helper()

	refresherMu.Lock()
	previousState := globalState
	previousRefresher := globalRefresher
	previousDefaultStatePath := defaultStatePath
	globalState = store
	globalRefresher = refresher
	defaultStatePath = func() string { return t.TempDir() + "\\state.json" }
	refresherMu.Unlock()

	t.Cleanup(func() {
		if refresher != nil {
			refresher.Stop()
		}
		refresherMu.Lock()
		globalState = previousState
		globalRefresher = previousRefresher
		defaultStatePath = previousDefaultStatePath
		refresherMu.Unlock()
	})
}

func lifecyclePayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
	if err != nil {
		t.Fatalf("marshal lifecycle payload: %v", err)
	}
	return raw
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

func TestPluginRegisterStartsRefresherWithoutStartupRefreshByDefault(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	host := &fakeHostClient{}
	refresher := NewQuotaRefresher(host, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)

	if _, err := handleMethod(pluginabi.MethodPluginRegister, lifecyclePayload(t, "quota_refresh_interval: 1h\n")); err != nil {
		t.Fatalf("plugin.register returned error: %v", err)
	}

	refresher.mu.Lock()
	running := refresher.running
	refresher.mu.Unlock()
	if !running {
		t.Fatal("refresher running = false, want true")
	}
	if refreshed := waitForCondition(t, 50*time.Millisecond, func() bool { return host.listCallCount() > 0 }); refreshed {
		t.Fatalf("ListAuths calls = %d, want 0 startup refreshes", host.listCallCount())
	}
}

func TestPluginRegisterRefreshesOnStartupWhenConfigured(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON:  map[string]json.RawMessage{"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`)},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 1),
		releaseDo: make(chan struct{}),
	}
	refresher := NewQuotaRefresher(host, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)

	if _, err := handleMethod(pluginabi.MethodPluginRegister, lifecyclePayload(t, "quota_refresh_interval: 1h\nrefresh_on_startup: true\n")); err != nil {
		t.Fatalf("plugin.register returned error: %v", err)
	}

	refresher.wg.Wait()
	if host.listCallCount() != 0 {
		t.Fatalf("ListAuths calls = %d, want no scan before scheduler admission", host.listCallCount())
	}
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "low", Provider: "codex", Priority: 1},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.doStarted:
		t.Fatal("candidate-only pick started a host call without authoritative roster publication")
	case <-time.After(50 * time.Millisecond):
	}
	close(host.releaseDo)
	refresher.wg.Wait()
	snapshot := store.Snapshot(time.Now())
	if len(snapshot.Accounts) != 0 {
		t.Fatalf("accounts = %#v, candidates must not create roster state", snapshot.Accounts)
	}
}

func TestSchedulerPickReplacesAdmissionBeforeRefresh(t *testing.T) {
	s5store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, s5store, nil)
	before := s5store.CPAAdmission()
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 1},
		{ID: "low", Provider: "codex", Priority: 0},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	if got := s5store.CPAAdmission(); !equalCPAAdmission(got, before) {
		t.Fatalf("candidates mutated admission: before=%#v after=%#v", before, got)
	}
}

func TestSchedulerAdmissionLogDeduplicatesCandidateCounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, store, nil)
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "low", Provider: "codex", Priority: 1},
		{ID: "other", Provider: "openai", Priority: 99},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	for _, entry := range store.Snapshot(time.Now()).Logs {
		if entry.Event == "scheduler.cpa_admission_updated" {
			t.Fatalf("candidate-derived admission log remains: %#v", entry)
		}
	}
}

func TestSchedulerPickPublishesAdmissionOnlyOnce(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, store, nil)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"a": {}}})
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	if store.CPAAdmission().Observed {
		t.Fatal("pick published candidate admission")
	}
}

func TestSchedulerPickPublishesWhileOlderHostCallIsBlocked(t *testing.T) {
	now := time.Now()
	store := NewPluginState(DefaultConfig())
	host := &fakeHostClient{releaseDo: make(chan struct{})}
	withGlobalRefresherForTest(t, store, NewQuotaRefresher(host, store, func() time.Time { return now }))
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "b", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"b": {}}})
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex"}}})
	done := make(chan error, 1)
	go func() { _, err := handleSchedulerPick(raw); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot pick blocked on unrelated host state")
	}
}

func TestConcurrentSchedulerPicksCoalesceLatestAdmission(t *testing.T) {
	s5store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, s5store, nil)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}, {ID: "b", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"a": {}, "b": {}}})
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: id, Provider: "codex"}}})
			_, _ = handleSchedulerPick(raw)
		}(id)
	}
	wg.Wait()
	if s5store.CPAAdmission().Observed {
		t.Fatal("concurrent candidates mutated authoritative admission")
	}
}

func TestSchedulerPickRecordsCodexActivityAndRequestsDueRefresh(t *testing.T) {
	s5store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, s5store, nil)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"a": {}}})
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	if !s5store.Snapshot(time.Now()).LastCodexActivityAt.IsZero() {
		t.Fatal("snapshot pick synchronously mutated activity state")
	}
}

func TestSchedulerPickRefreshesOnlyActivePriorityCandidates(t *testing.T) {
	s5store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, s5store, nil)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "high", Instance: 1, Cache: CacheFresh}, {ID: "low", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"high": {}}})
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "high", Provider: "codex"}, {ID: "low", Provider: "codex"}}})
	response, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(response, &env); err != nil {
		t.Fatal(err)
	}
	var picked pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &picked); err != nil {
		t.Fatal(err)
	}
	if picked.AuthID != "high" {
		t.Fatalf("picked %q outside authoritative tier", picked.AuthID)
	}
}

func TestLogSchedulerDecisionIncludesDetailedFallbackReason(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		Handled:         true,
		DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
		Reason:          "fallback_fill_first",
		Ordered: []ScheduledAccount{
			{AuthID: "five-hour", QueueStatus: QueueStatusFiveHourExhausted, UnavailableReason: "five_hour_exhausted"},
			{AuthID: "weekly", QueueStatus: QueueStatusLongWindowExhausted, UnavailableReason: "weekly_exhausted"},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	logs := store.Snapshot(now).Logs
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	fields := logs[0].Fields
	if fields["reason"] != "fallback_fill_first" || fields["fallback"] != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("fields = %#v, want fallback reason and builtin", fields)
	}
	if fields["ordered_count"] != 2 {
		t.Fatalf("ordered_count = %#v, want 2; fields=%#v", fields["ordered_count"], fields)
	}
	if fields["unavailable_summary"] == "" {
		t.Fatalf("unavailable_summary empty; fields=%#v", fields)
	}
}

func TestLogSchedulerDecisionIncludesSelectedAccountContext(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		AuthID:  "auth-1",
		Handled: true,
		Reason:  "selected",
		Ordered: []ScheduledAccount{
			{AuthID: "auth-1", CPAPriority: 8, SchedulerPriority: 3, QueueStatus: QueueStatusAvailable, Available: true},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	fields := store.Snapshot(now).Logs[0].Fields
	if fields["auth_id"] != "auth-1" || fields["selected_queue_status"] != string(QueueStatusAvailable) || fields["ordered_count"] != 1 {
		t.Fatalf("fields = %#v, want selected account context", fields)
	}
	if fields["selected_cpa_priority"] != 8 || fields["selected_scheduler_priority"] != 3 {
		t.Fatalf("priority fields = %#v, want distinct CPA and scheduler priorities", fields)
	}
}
