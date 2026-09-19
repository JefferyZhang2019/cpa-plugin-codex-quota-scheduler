package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func cleanupIntegrationGlobals(t *testing.T) {
	t.Helper()

	previousState := globalState
	previousConfig, hadConfig := currentConfig.Load().(Config)
	refresherMu.Lock()
	previousRefresher := globalRefresher
	globalRefresher = nil
	refresherMu.Unlock()
	previousRefreshSoon := managementRefreshSoon
	managementRefreshSoon = func() {}
	currentConfig = atomic.Value{}

	t.Cleanup(func() {
		refresherMu.Lock()
		testRefresher := globalRefresher
		if testRefresher != nil && testRefresher != previousRefresher {
			testRefresher.Stop()
		}
		globalRefresher = previousRefresher
		refresherMu.Unlock()

		globalState = previousState
		currentConfig = atomic.Value{}
		if hadConfig {
			currentConfig.Store(previousConfig)
		}
		managementRefreshSoon = previousRefreshSoon
	})
}

func TestHandleMethodRegisterEnvelope(t *testing.T) {
	cleanupIntegrationGlobals(t)

	raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"aGFuZGxlX2VuYWJsZWQ6IHRydWUK"}`))
	if err != nil {
		t.Fatalf("handle register: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}
}

func TestHandleMethodRegisterPreservesConfigWhenStateFileMissing(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "missing-state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	rawReq, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("handle_enabled: false\nmonthly_mode: priority\nquota_refresh_interval: 45s\nmax_refresh_concurrency: 2\n")})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, rawReq); err != nil {
		t.Fatalf("handle register: %v", err)
	}

	cfg := globalState.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval.String() != "45s" || cfg.MaxRefreshConcurrency != 2 {
		t.Fatalf("config = %#v, want lifecycle config preserved", cfg)
	}
}

func TestHandleMethodRegisterLoadsPersistedAnnotations(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	if err := SavePluginDiskState(defaultStatePath(), PluginDiskState{
		Config: DefaultConfig(),
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "Personal", GroupID: "team"},
		},
		Groups: map[string]GroupAnnotation{
			"team": {Name: "Team"},
		},
	}); err != nil {
		t.Fatalf("SavePluginDiskState returned error: %v", err)
	}

	rawReq, err := json.Marshal(lifecycleRequest{})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, rawReq); err != nil {
		t.Fatalf("handle register: %v", err)
	}

	state := globalState.Annotations()
	if state.Accounts["auth:auth-1"].Alias != "Personal" {
		t.Fatalf("account annotations = %#v, want persisted alias", state.Accounts)
	}
	if state.Groups["team"].Name != "Team" {
		t.Fatalf("group annotations = %#v, want persisted group", state.Groups)
	}
}

func TestHandleMethodRegisterIgnoresInvalidPersistedAnnotations(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	if err := os.WriteFile(defaultStatePath(), []byte("{not-json"), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	rawReq, err := json.Marshal(lifecycleRequest{})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	raw, err := handleMethod(pluginabi.MethodPluginRegister, rawReq)
	if err != nil {
		t.Fatalf("handle register: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}
}

func TestHandleMethodSchedulerPickFallbackEnvelope(t *testing.T) {
	cleanupIntegrationGlobals(t)

	globalState = NewPluginState(DefaultConfig())
	rawReq, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "missing", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	raw, err := handleMethod(pluginabi.MethodSchedulerPick, rawReq)
	if err != nil {
		t.Fatalf("handle scheduler pick: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}

	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode scheduler response: %v", err)
	}
	if !resp.Handled || resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("expected fill-first fallback, got %+v", resp)
	}
}

func TestSixAccountIncidentUsesPluginPriorityFallthroughWhenCPAPrioritiesMatch(t *testing.T) {
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	exhaustedA := weeklyAccount("exhausted-a", 0, now.Add(24*time.Hour), true)
	exhaustedA.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	exhaustedA.Annotation.SchedulerPriority = 1
	exhaustedB := weeklyAccount("exhausted-b", 0, now.Add(48*time.Hour), true)
	exhaustedB.Quota.FiveHour.ResetAt = now.Add(2 * time.Hour)
	exhaustedB.Annotation.SchedulerPriority = 1
	usableA := weeklyAccount("usable-a", 0, now.Add(6*time.Hour), false)
	usableB := weeklyAccount("usable-b", 0, now.Add(12*time.Hour), false)
	usableC := weeklyAccount("usable-c", 0, now.Add(18*time.Hour), false)
	usableD := weeklyAccount("usable-d", 0, now.Add(30*time.Hour), false)

	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		exhaustedA,
		exhaustedB,
		usableA,
		usableB,
		usableC,
		usableD,
	}}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: exhaustedA.AuthID, Provider: "codex", Priority: 0},
		{ID: exhaustedB.AuthID, Provider: "codex", Priority: 0},
		{ID: usableA.AuthID, Provider: "codex", Priority: 0},
		{ID: usableB.AuthID, Provider: "codex", Priority: 0},
		{ID: usableC.AuthID, Provider: "codex", Priority: 0},
		{ID: usableD.AuthID, Provider: "codex", Priority: 0},
	}}

	decision := PickCodexAccount(req, snapshot, now)
	if !decision.Handled || decision.AuthID != usableA.AuthID || decision.DelegateBuiltin != "" {
		t.Fatalf("decision = %#v, want first usable plugin-priority-0 account without fallback", decision)
	}
	if len(decision.Ordered) != 6 {
		t.Fatalf("ordered account count = %d, want 6: %#v", len(decision.Ordered), decision.Ordered)
	}
	gotIDs := make([]string, 0, len(decision.Ordered))
	for _, account := range decision.Ordered {
		gotIDs = append(gotIDs, account.AuthID)
	}
	wantIDs := []string{"usable-a", "usable-b", "usable-c", "usable-d", "exhausted-a", "exhausted-b"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ordered IDs = %#v, want %#v", gotIDs, wantIDs)
	}
	if decision.Ordered[4].SchedulerPriority != 1 || decision.Ordered[5].SchedulerPriority != 1 {
		t.Fatalf("excluded plugin tier = %#v, want two plugin-priority-1 accounts", decision.Ordered[4:])
	}
	if decision.Ordered[4].Available || decision.Ordered[5].Available {
		t.Fatalf("excluded plugin tier unexpectedly usable: %#v", decision.Ordered[4:])
	}
	if decision.Ordered[0].AuthID != usableA.AuthID || decision.Ordered[0].SchedulerPriority != 0 || !decision.Ordered[0].Available {
		t.Fatalf("first usable lower plugin tier account = %#v, want %q", decision.Ordered[0], usableA.AuthID)
	}
}

func TestSixAccountIncidentMixedCPAPrioritiesExposeOnlyMaximumTier(t *testing.T) {
	legacy := DefaultConfig()
	legacy.ScheduleAcrossPriorities = false
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	exhaustedA := weeklyAccount("exhausted-a", 1, now.Add(24*time.Hour), true)
	exhaustedA.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	exhaustedB := weeklyAccount("exhausted-b", 1, now.Add(48*time.Hour), true)
	exhaustedB.Quota.FiveHour.ResetAt = now.Add(2 * time.Hour)
	usableA := weeklyAccount("usable-a", 0, now.Add(6*time.Hour), false)
	usableB := weeklyAccount("usable-b", 0, now.Add(12*time.Hour), false)
	usableC := weeklyAccount("usable-c", 0, now.Add(18*time.Hour), false)
	usableD := weeklyAccount("usable-d", 0, now.Add(30*time.Hour), false)

	snapshot := StateSnapshot{Config: legacy, Now: now, Accounts: []AccountState{
		exhaustedA,
		exhaustedB,
		usableA,
		usableB,
		usableC,
		usableD,
	}}
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: exhaustedA.AuthID, Provider: "codex", Priority: 1},
		{ID: exhaustedB.AuthID, Provider: "codex", Priority: 1},
		{ID: usableA.AuthID, Provider: "codex", Priority: 0},
		{ID: usableB.AuthID, Provider: "codex", Priority: 0},
		{ID: usableC.AuthID, Provider: "codex", Priority: 0},
		{ID: usableD.AuthID, Provider: "codex", Priority: 0},
	}}

	decision := PickCodexAccount(req, snapshot, now)
	if !decision.Handled || decision.AuthID != "" || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("decision = %#v, want fill-first fallback from exhausted maximum CPA tier", decision)
	}
	if len(decision.Ordered) != 2 {
		t.Fatalf("ordered account count = %d, want only 2 maximum-tier accounts: %#v", len(decision.Ordered), decision.Ordered)
	}
	gotIDs := []string{decision.Ordered[0].AuthID, decision.Ordered[1].AuthID}
	wantIDs := []string{"exhausted-a", "exhausted-b"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ordered IDs = %#v, want %#v", gotIDs, wantIDs)
	}
	for _, account := range decision.Ordered {
		if account.CPAPriority != 1 || account.Available {
			t.Fatalf("ordered account = %#v, want unavailable CPA-priority-1 account", account)
		}
	}
}
