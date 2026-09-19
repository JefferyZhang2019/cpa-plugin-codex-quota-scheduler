package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementRegisterExposesStatusResourceAndRoutes(t *testing.T) {
	resp := RegisterManagement()
	resources := map[string]pluginapi.ResourceRoute{}
	for _, resource := range resp.Resources {
		resources[resource.Path] = resource
	}
	if _, ok := resources["/status"]; !ok {
		t.Fatalf("missing status resource in %#v", resp.Resources)
	}
	if resources["/status"].Menu == "" {
		t.Fatalf("status resource Menu is empty: %#v", resources["/status"])
	}
	if resources["/status"].Menu != "Codex Scheduler" {
		t.Fatalf("status resource Menu = %q, want English static label", resources["/status"].Menu)
	}
	if _, ok := resources["/status-data"]; ok {
		t.Fatalf("status-data resource is still registered: %#v", resp.Resources)
	}
	if len(resources) != len(resp.Resources) {
		t.Fatalf("resources = %#v", resp.Resources)
	}
	if !isResourcePath("/v0/resource" + managementBasePath + "/status") {
		t.Fatal("registered status Resource path is not recognized as Resource")
	}
	if !resourceRouteAllowed(http.MethodGet, "/status") {
		t.Fatal("GET /status is not allowed at the Resource boundary")
	}
	for _, mutation := range []struct{ method, path string }{
		{http.MethodPost, "/refresh"},
		{http.MethodPut, "/settings"},
		{http.MethodPatch, "/annotations/account"},
	} {
		if resourceRouteAllowed(mutation.method, mutation.path) {
			t.Fatalf("%s %s crossed the Resource boundary", mutation.method, mutation.path)
		}
	}

	paths := map[string]string{}
	for _, route := range resp.Routes {
		paths[route.Method+" "+route.Path] = route.Path
		if route.Method == http.MethodGet && route.Path == "/plugins/codex-quota-scheduler/status" && route.Menu != "" {
			t.Fatalf("management status route Menu = %q, want empty", route.Menu)
		}
	}
	for _, key := range []string{
		"GET /plugins/codex-quota-scheduler/status",
		"GET /plugins/codex-quota-scheduler/settings",
		"PUT /plugins/codex-quota-scheduler/settings",
		"POST /plugins/codex-quota-scheduler/refresh",
		"POST /plugins/codex-quota-scheduler/refresh/account",
		"GET /plugins/codex-quota-scheduler/logs",
		"GET /plugins/codex-quota-scheduler/export",
		"POST /plugins/codex-quota-scheduler/import",
		"GET /plugins/codex-quota-scheduler/annotations",
		"PUT /plugins/codex-quota-scheduler/annotations",
		"PATCH /plugins/codex-quota-scheduler/annotations/account",
		"PATCH /plugins/codex-quota-scheduler/annotations/group",
	} {
		if paths[key] == "" {
			t.Fatalf("missing route %s in %#v", key, paths)
		}
	}
}

func TestManagementRoutesDispatchFullCPAPaths(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 5, AuthIDs: map[string]struct{}{"auth-1": {}}})
	store.UpsertQuota(weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false))

	refreshes := 0
	refreshOne := ""
	previousRefreshSoon := managementRefreshSoon
	previousRefreshOneSoon := managementRefreshOneSoon
	managementRefreshSoon = func() { refreshes++ }
	managementRefreshOneSoon = func(authID string) { refreshOne = authID }
	t.Cleanup(func() {
		managementRefreshSoon = previousRefreshSoon
		managementRefreshOneSoon = previousRefreshOneSoon
	})

	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   []byte
		want   int
	}{
		{
			name:   "management status",
			method: http.MethodGet,
			path:   "/v0/management/plugins/codex-quota-scheduler/status",
			query:  url.Values{"format": []string{"json"}},
			want:   http.StatusOK,
		},
		{
			name:   "resource status",
			method: http.MethodGet,
			path:   "/v0/resource/plugins/codex-quota-scheduler/status",
			want:   http.StatusOK,
		},
		{
			name:   "management refresh",
			method: http.MethodPost,
			path:   "/v0/management/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusAccepted,
		},
		{
			name:   "management refresh one",
			method: http.MethodPost,
			path:   "/v0/management/plugins/codex-quota-scheduler/refresh/account",
			body:   []byte(`{"auth_id":"auth-1"}`),
			want:   http.StatusAccepted,
		},
		{
			name:   "resource mutation hidden",
			method: http.MethodGet,
			path:   "/v0/resource/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
				Method: tt.method,
				Path:   tt.path,
				Query:  tt.query,
				Body:   tt.body,
			}, now)
			if resp.StatusCode != tt.want {
				t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, tt.want, resp.Body)
			}
		})
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if refreshOne != "auth-1" {
		t.Fatalf("refreshOne = %q, want auth-1", refreshOne)
	}
}

func TestManagementRefreshAccountRejectsOutsideAdmission(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"high": {}}})

	called := false
	previousRefreshOneSoon := managementRefreshOneSoon
	managementRefreshOneSoon = func(string) { called = true }
	t.Cleanup(func() { managementRefreshOneSoon = previousRefreshOneSoon })

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/plugins/codex-quota-scheduler/refresh/account",
		Body:   []byte(`{"auth_id":"low"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusConflict, resp.Body)
	}
	if called {
		t.Fatal("management refresh callback was called for excluded auth")
	}
}

func TestManagementUsesActiveRosterOnly(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	for _, account := range []AccountState{
		weeklyAccount("active-dormant", 9, now.Add(24*time.Hour), false),
		weeklyAccount("removed-cache", 9, now.Add(24*time.Hour), false),
		weeklyAccount("lower-tier-cache", 1, now.Add(24*time.Hour), false),
	} {
		store.UpsertQuota(account)
	}

	lifecycle := ManagementLifecycleSnapshot{Roster: ActiveRoster{
		Capability: CapabilityA, Confirmed: true, HighestPriority: 9,
		Generation: 7, Instances: []string{"active-dormant"},
		Health: RosterHealthy, BackgroundAllowed: true,
	}}
	resp := HandleManagementRequestWithLifecycle(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now, lifecycle)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AuthID != "active-dormant" {
		t.Fatalf("authoritative accounts = %#v", body.Accounts)
	}
	if body.Accounts[0].CPAPriority != 9 || body.RefreshState != "sleeping" {
		t.Fatalf("dormant active-tier card = %#v, refresh=%q", body.Accounts[0], body.RefreshState)
	}
}

func TestManagementStatusExposesNonSensitiveRosterAdmissionDiagnostics(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	version := store.ReplaceCPAAdmission(CPAAdmissionState{
		Observed: true,
		Priority: 0,
		AuthIDs:  map[string]struct{}{"codex-a": {}, "codex-b": {}},
	})
	lifecycle := ManagementLifecycleSnapshot{Roster: ActiveRoster{
		Capability: CapabilityA, Confirmed: true, HighestPriority: 0,
		Generation: 7, Instances: []string{"codex-a", "codex-b"},
		Entries: []RosterEntry{{ID: "codex-a"}, {ID: "codex-b"}},
		Health:  RosterHealthy, BackgroundAllowed: true,
	}}

	resp := HandleManagementRequestWithLifecycle(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now, lifecycle)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatal(err)
	}
	if !body.Roster.AdmissionObserved || body.Roster.AdmissionVersion != version || body.Roster.AdmissionPriority != 0 || body.Roster.AdmittedAuthCount != 2 {
		t.Fatalf("admission diagnostics = %#v, want observed version=%d priority=0 count=2", body.Roster, version)
	}
	if body.Roster.RosterEntryCount != 2 || body.Roster.RosterInstanceCount != 2 {
		t.Fatalf("roster diagnostics = %#v, want entry/instance count 2/2", body.Roster)
	}
	var raw struct {
		Roster map[string]json.RawMessage `json:"roster"`
	}
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]struct{}{
		"capability": {}, "health": {}, "confirmed": {}, "provisional": {},
		"degraded": {}, "fail_closed": {}, "waiting_roster": {}, "background_allowed": {},
		"highest_priority": {}, "generation": {}, "admission_observed": {}, "admission_version": {},
		"admission_priority": {}, "admitted_auth_count": {}, "roster_entry_count": {},
		"roster_instance_count": {}, "credential_ambiguous": {}, "risk_option_enabled": {},
		"risk_option_available": {}, "warning": {}, "risk_warning": {},
	}
	for key := range raw.Roster {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("unexpected roster diagnostic key %q in %s", key, resp.Body)
		}
	}
	rawBody := string(resp.Body)
	for _, sensitive := range []string{`"codex-a"`, `"codex-b"`, `"auth_ids"`, `"auth_index"`, `"entries"`, `"instances"`, `"credentials"`, `"path"`} {
		if strings.Contains(rawBody, sensitive) {
			t.Fatalf("sensitive roster detail %s leaked in %s", sensitive, resp.Body)
		}
	}
}

func TestManagementExposesRosterLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	cases := []struct {
		name      string
		roster    ActiveRoster
		ambiguous bool
		check     func(t *testing.T, got RosterLifecyclePayload)
	}{
		{"healthy", ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true}, false, func(t *testing.T, got RosterLifecyclePayload) {
			if got.Capability != CapabilityA || got.Health != RosterHealthy || !got.Confirmed || got.Degraded || got.FailClosed || got.WaitingRoster {
				t.Fatalf("healthy lifecycle = %#v", got)
			}
		}},
		{"degraded", ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterDegraded, DegradedSince: now.Add(-time.Minute), BackgroundAllowed: true}, false, func(t *testing.T, got RosterLifecyclePayload) {
			if !got.Degraded || got.FailClosed || got.Warning == "" {
				t.Fatalf("degraded lifecycle = %#v", got)
			}
		}},
		{"fail-closed", ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed}, false, func(t *testing.T, got RosterLifecyclePayload) {
			if !got.FailClosed || got.Degraded || got.Warning == "" {
				t.Fatalf("fail-closed lifecycle = %#v", got)
			}
		}},
		{"waiting-provisional", ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, ConfirmedAt: now.Add(-time.Hour)}, false, func(t *testing.T, got RosterLifecyclePayload) {
			if !got.Provisional || !got.WaitingRoster || !got.RiskOptionAvailable || got.RiskOptionEnabled || got.RiskWarning == "" {
				t.Fatalf("waiting lifecycle = %#v", got)
			}
		}},
		{"credential-ambiguous", ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, ConfirmedAt: now.Add(-time.Hour)}, true, func(t *testing.T, got RosterLifecyclePayload) {
			if !got.CredentialAmbiguous || got.RiskOptionAvailable || got.Warning == "" {
				t.Fatalf("credential ambiguous lifecycle = %#v", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := buildCurrentStatusPayloadWithLifecycle(store, now, ManagementLifecycleSnapshot{Roster: tc.roster, CredentialAmbiguous: tc.ambiguous})
			tc.check(t, payload.Roster)
		})
	}

	cfg := store.Config()
	cfg.ProbeOnProvisionalRoster = true
	store.ReplaceConfig(cfg)
	payload := buildCurrentStatusPayloadWithLifecycle(store, now, ManagementLifecycleSnapshot{Roster: ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, ConfirmedAt: now.Add(-time.Hour)}})
	if !payload.Roster.RiskOptionEnabled || !payload.Roster.RiskOptionAvailable || payload.Roster.RiskWarning == "" {
		t.Fatalf("enabled provisional risk = %#v", payload.Roster)
	}
}

func TestManagementRoundTripsProvisionalRiskOption(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProbeOnProvisionalRoster = true
	store := NewPluginState(cfg)
	page := string(RenderStatusHTML(buildCurrentStatusPayloadWithLifecycle(store, time.Now(), ManagementLifecycleSnapshot{Roster: ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, ConfirmedAt: time.Now().Add(-time.Hour)}})))
	for _, want := range []string{
		`id="probeOnProvisionalRoster"`,
		"document.getElementById('probeOnProvisionalRoster').checked=s.probe_on_provisional_roster===true",
		"probe_on_provisional_roster:document.getElementById('probeOnProvisionalRoster').checked",
		`id="rosterLifecycleWarning"`,
		"renderRosterLifecycle",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("status UI missing %q", want)
		}
	}
}

func TestStatusJSONOrdersAccountsBySchedulerOrder(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	later := weeklyAccount("later", 5, now.Add(48*time.Hour), false)
	later.LastSuccessAt = now
	earlier := weeklyAccount("earlier", 5, now.Add(24*time.Hour), false)
	earlier.LastSuccessAt = now
	store.UpsertQuota(later)
	store.UpsertQuota(earlier)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if strings.Contains(string(resp.Body), "action_token") {
		t.Fatalf("status JSON exposed action token: %s", resp.Body)
	}
	var body struct {
		Accounts []struct {
			AuthID string `json:"auth_id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 2 || body.Accounts[0].AuthID != "earlier" {
		t.Fatalf("accounts = %#v, want earlier first", body.Accounts)
	}
}

func TestStatusJSONMovesUnavailableAccountsBehindAvailableAccounts(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	exhausted := weeklyAccount("exhausted", 5, now.Add(25*time.Hour), true)
	available := weeklyAccount("available", 5, now.Add(48*time.Hour), false)
	available.LastSuccessAt = now
	store.UpsertQuota(exhausted)
	store.UpsertQuota(available)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2; accounts=%#v", len(body.Accounts), body.Accounts)
	}
	if body.Accounts[0].AuthID != "available" || body.Accounts[1].AuthID != "exhausted" {
		t.Fatalf("accounts = %#v, want available then exhausted", body.Accounts)
	}
	if !body.Accounts[0].Available {
		t.Fatalf("available account = %#v, want available=true", body.Accounts[0])
	}
	if body.Accounts[1].Available || body.Accounts[1].UnavailableReason == "" {
		t.Fatalf("unavailable account = %#v, want available=false with reason", body.Accounts[1])
	}
}

func TestStatusPayloadExplainsEmptyQueueBeforeFirstRequest(t *testing.T) {
	now := time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	payload := BuildStatusPayload(store.Snapshot(now), nil)

	if payload.RefreshActive {
		t.Fatalf("RefreshActive = true, want false before first request")
	}
	if payload.EmptyState.Reason != "sleeping_no_activity" {
		t.Fatalf("EmptyState reason = %q, want sleeping_no_activity; payload=%#v", payload.EmptyState.Reason, payload.EmptyState)
	}
	for _, want := range []string{"1h0m0s", "发送第一次 Codex 请求"} {
		if !strings.Contains(payload.EmptyState.Message, want) {
			t.Fatalf("EmptyState message missing %q: %q", want, payload.EmptyState.Message)
		}
	}
}

func TestManagementReportsLongWindowExhaustionAheadOfTemporaryFeedback(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	account := weeklyAccount("weekly", 0, now.Add(6*time.Hour), false)
	account.Quota.FiveHour = nil
	account.Quota.LongWindow.UsedPercent = &used
	account.Quota.LongWindow.Exhausted = true
	account.TemporaryExhausted = true
	account.TemporaryResetAt = now.Add(time.Hour)
	snapshot := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{account}, Now: now}

	payload := BuildStatusPayload(snapshot, BuildOrderedAccounts(requestWithCandidates("weekly"), snapshot, now))
	if len(payload.Accounts) != 1 || payload.Accounts[0].UnavailableReason != "weekly_exhausted" {
		t.Fatalf("accounts = %#v", payload.Accounts)
	}
}

func TestStatusPayloadExplainsNoCodexAuthAfterAuthScan(t *testing.T) {
	now := time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.RecordAuthScan(0, now)
	payload := BuildStatusPayload(store.Snapshot(now), nil)

	if payload.CodexAuthCount != 0 || payload.LastAuthScanText != "2026-06-29T09:00:00Z" {
		t.Fatalf("auth scan fields = count %d text %q", payload.CodexAuthCount, payload.LastAuthScanText)
	}
	if payload.EmptyState.Reason != "no_codex_auth" {
		t.Fatalf("EmptyState reason = %q, want no_codex_auth; payload=%#v", payload.EmptyState.Reason, payload.EmptyState)
	}
	if !strings.Contains(payload.EmptyState.Message, "认证文件中没有 Codex 账号") {
		t.Fatalf("EmptyState message = %q, want no auth explanation", payload.EmptyState.Message)
	}
}

func TestStatusPayloadNotesStaleAccountWhileSleeping(t *testing.T) {
	now := time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now.Add(-6 * time.Hour)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(account)
	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)

	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(payload.Accounts))
	}
	note := payload.Accounts[0].StatusNote
	for _, want := range []string{"账号额度已过期", "休眠状态", "发送一次 Codex 请求"} {
		if !strings.Contains(note, want) {
			t.Fatalf("StatusNote missing %q: %q", want, note)
		}
	}
}

func TestStatusJSONIncludesSchedulerSummary(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	cfg.HandleEnabled = false
	store := NewPluginState(cfg)
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	store.UpsertQuota(account)
	store.RecordSelection("auth-1", "selected")

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if body.NextAuthID != "auth-1" || body.MonthlyMode != MonthlyModePriority || body.HandleEnabled || body.LastSelected != "auth-1" || body.LastReason != "selected" {
		t.Fatalf("payload summary = %#v", body)
	}
}

func TestStatusJSONKeepsCPAAndSchedulerPrioritiesDistinct(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 3, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	account.Annotation.SchedulerPriority = 8
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].CPAPriority != 3 || body.Accounts[0].SchedulerPriority != 8 {
		t.Fatalf("accounts = %#v, want cpa_priority=3 and scheduler_priority=8", body.Accounts)
	}
}

func TestStatusNextAuthIDScansLowerPluginPriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	highBlocked := weeklyAccount("high-blocked", 5, now.Add(24*time.Hour), false)
	highBlocked.Stale = true
	highBlocked.Annotation.SchedulerPriority = 1
	lowAvailable := weeklyAccount("low-available", 5, now.Add(time.Hour), false)
	lowAvailable.LastSuccessAt = now
	store.UpsertQuota(highBlocked)
	store.UpsertQuota(lowAvailable)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if body.NextAuthID != "low-available" {
		t.Fatalf("NextAuthID = %q, want lower plugin priority account", body.NextAuthID)
	}
	if len(body.Accounts) != 2 || body.Accounts[0].AuthID != "low-available" || body.Accounts[0].CPAPriority != 5 || body.Accounts[0].SchedulerPriority != 0 || body.Accounts[1].AuthID != "high-blocked" || body.Accounts[1].SchedulerPriority != 1 {
		t.Fatalf("Accounts = %#v, want availability-first order with distinct status priorities", body.Accounts)
	}
}

func TestManagementStatusExcludesActiveTrialConsistently(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	trialAccount := weeklyAccount("trial", 0, now.Add(time.Hour), false)
	trialAccount.Instance = 101
	selectable := weeklyAccount("selectable", 0, now.Add(3*time.Hour), false)
	selectable.Instance = 102
	used := 100.0
	knownRecovery := weeklyAccount("known-recovery", 0, now.Add(2*time.Hour), false)
	knownRecovery.Instance = 103
	knownRecovery.Quota.LongWindow.UsedPercent = &used
	knownRecovery.Quota.LongWindow.Exhausted = true

	trials := NewTrialRegistry()
	if !trials.TryBegin(trialAccount.Instance, now) {
		t.Fatal("TryBegin returned false for a new trial")
	}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		trialAccount, selectable, knownRecovery,
	}}
	ordered := buildOrderedAccounts(
		requestWithCandidates("trial", "selectable", "known-recovery"), snapshot, now, trials,
	)
	payload := BuildStatusPayload(snapshot, ordered)

	wantOrder := []string{"selectable", "known-recovery", "trial"}
	for i, want := range wantOrder {
		if ordered[i].AuthID != want {
			t.Errorf("ordered[%d].AuthID = %q, want %q; ordered=%#v", i, ordered[i].AuthID, want, ordered)
		}
	}
	var trialScheduled ScheduledAccount
	for _, account := range ordered {
		if account.AuthID == trialAccount.AuthID {
			trialScheduled = account
			break
		}
	}
	if trialScheduled.selectionClass != Excluded {
		t.Errorf("trial selectionClass = %v, want Excluded", trialScheduled.selectionClass)
	}
	if trialScheduled.Available {
		t.Error("trial Available = true, want false")
	}
	if trialScheduled.QueueStatus != QueueStatusUnavailable || trialScheduled.UnavailableReason != "quota_probe_wait" {
		t.Errorf("trial queue state = (%q, %q), want unavailable quota_probe_wait", trialScheduled.QueueStatus, trialScheduled.UnavailableReason)
	}
	if !trialScheduled.SortTime.IsZero() {
		t.Errorf("trial SortTime = %v, want unknown recovery", trialScheduled.SortTime)
	}
	if payload.NextAuthID != selectable.AuthID {
		t.Errorf("NextAuthID = %q, want %q", payload.NextAuthID, selectable.AuthID)
	}
	for _, account := range payload.Accounts {
		if account.AuthID == trialAccount.AuthID && account.Available {
			t.Error("status trial Available = true, want false")
		}
	}
}

func TestStatusJSONIncludesEmptyLastSelectionFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	lastSelected, ok := body["last_selected"]
	if !ok || lastSelected != "" {
		t.Fatalf("last_selected = %#v, present=%t; body=%s", lastSelected, ok, resp.Body)
	}
	lastReason, ok := body["last_reason"]
	if !ok || lastReason != "" {
		t.Fatalf("last_reason = %#v, present=%t; body=%s", lastReason, ok, resp.Body)
	}
}

func TestStatusHTMLRedactsSensitiveFieldsAndEscapesUserFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: `<script>alert("x")</script>`, Tags: []string{"team"}, GroupID: "ops"},
		},
		Groups: map[string]GroupAnnotation{
			"ops": {Name: "Ops & Finance"},
		},
	})
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	lower := strings.ToLower(html)
	if !strings.Contains(html, "codex-quota-scheduler") {
		t.Fatalf("html missing plugin id: %s", html)
	}
	for _, want := range []string{"Codex 额度调度器", "调度设置", "账号队列", "保存设置", "刷新额度", "账号卡片"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing Chinese UI text %q: %s", want, html)
		}
	}
	if strings.Contains(html, "<table") {
		t.Fatalf("html still renders table layout: %s", html)
	}
	for _, forbidden := range []string{"access_token", "bearer ", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("html contains sensitive field %q: %s", forbidden, html)
		}
	}
	if strings.Contains(html, "<script>alert") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "Ops &amp; Finance") {
		t.Fatalf("html did not escape user fields: %s", html)
	}
}

func TestStatusHTMLUsesManagementAPIActionsModalProgressAndLogs(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	store.UpsertQuota(account)
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	lower := strings.ToLower(html)
	for _, want := range []string{"quota-bar", "editDialog", "logList", "openEdit", "exportLogs", "codex-quota-scheduler-logs.json", "maxLogEntries", "logRetention", "refreshOneQuota", "refreshStatus", "renderAccounts", "renderMetrics", "metricNextAuthID", "metricMonthlyMode", "metricLastSelected", "managementKey", "loadStatus", "MANAGEMENT_BASE", "/v0/management/plugins/codex-quota-scheduler", "authHeaders()", "localeSelect", "TRANSLATIONS", "codex-quota-scheduler-locale-v1", "Scheduler Settings", "Account Queue", "INLINE_TRANSLATIONS", "Reset credits", "Refresh Quota", `id="editSchedulerPriority"`, "account.schedulerPriority", "scheduler_priority", "Plugin priority", "插件优先级"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing marker %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{"RESOURCE_ENDPOINT", "PUBLIC_STATUS_BASE", "requestPublicStatus", "metricSchedulerState", "metrics.scheduler", "/v0/resource/plugins/codex-quota-scheduler/status?action", "requestPlugin(action,options)", "details class=\"editor\"", "action_token"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("html still contains removed marker %q: %s", forbidden, html)
		}
	}
	for _, forbidden := range []string{"bearer ", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("html contains sensitive field %q: %s", forbidden, html)
		}
	}
}

func TestStatusHTMLStaticCardShowsDistinctPriorityBadges(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {SchedulerPriority: 8},
		},
	})
	account := weeklyAccount("auth-1", 3, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	for _, want := range []string{
		`<span class="badge">CPA 优先级 3</span>`,
		`<span class="badge">插件优先级 8</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static account card missing priority badge %q: %s", want, html)
		}
	}
}

func TestStatusPageValidatesSchedulerPriorityBeforePatch(t *testing.T) {
	page := renderStatusPageForTest(t, NewPluginState(DefaultConfig()))
	for _, want := range []string{
		"valueAsNumber",
		"Number.isSafeInteger(schedulerPriority)",
		"error.schedulerPriorityInteger",
		"Plugin priority must be a safe integer.",
		"插件优先级必须是安全整数。",
		"scheduler_priority:schedulerPriority",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing scheduler priority validation marker %q", want)
		}
	}
	if strings.Contains(page, "Number.parseInt(document.getElementById('editSchedulerPriority').value,10)||0") {
		t.Fatal("page still coerces invalid scheduler priority to zero")
	}
	validation := strings.Index(page, "if(!Number.isSafeInteger(schedulerPriority))")
	patch := strings.Index(page, "requestManagement('/annotations/account'")
	if validation < 0 || patch < 0 || validation > patch {
		t.Fatalf("scheduler priority validation must return before PATCH: validation=%d patch=%d", validation, patch)
	}
}

func TestStatusPageUsesCollapsedSettingsAndNoHardReload(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	if !strings.Contains(page, `<details class="section collapsible" id="settingsPanel" hidden>`) &&
		!strings.Contains(page, `<details class="panel collapsible" id="settingsPanel" hidden>`) {
		t.Fatalf("page does not render settings as collapsed details")
	}
	if strings.Contains(page, `id="settingsPanel" open`) {
		t.Fatalf("settings panel is open by default")
	}
	if strings.Contains(page, "window.location.reload") {
		t.Fatalf("page still contains hard reload")
	}
	if strings.Contains(page, "schedulePageRefresh") {
		t.Fatalf("page still contains scheduled hard reload helper")
	}
	for _, want := range []string{
		`summary-toggle`,
		`hasManagementKey`,
		`requestManagement('/status'`,
		`refreshStatus`,
		`loadStatus`,
		`startStatusPolling`,
		`默认配置已经都设置好了，正常情况下不需要手动设置。`,
		`活跃刷新窗口`,
		`重置后刷新延迟`,
		`刷新失败重试间隔`,
		`启动时刷新额度`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing collapsed/public refresh marker %q", want)
		}
	}
	settingsStart := strings.Index(page, `id="settingsPanel"`)
	refreshStart := strings.Index(page, `id="refreshQuota"`)
	if settingsStart < 0 || refreshStart < 0 {
		t.Fatalf("page missing settings panel or refresh button")
	}
	if refreshStart > settingsStart {
		t.Fatalf("refresh quota button is still inside or after the settings panel")
	}
	if strings.Contains(page, "requestPublicStatus") || strings.Contains(page, "PUBLIC_STATUS_BASE") {
		t.Fatalf("page still contains public status fallback")
	}
	if strings.Contains(page, `refreshStatus({fillSettings:true}).catch`) {
		t.Fatalf("page still auto-loads protected status on startup")
	}
}

func TestStatusPageShowsResetProbeWarningOnlyAfterProtectedLoadWhenDisabled(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	warningStart := strings.Index(page, `id="resetProbeWarning"`)
	settingsStart := strings.Index(page, `id="settingsPanel"`)
	if warningStart < 0 {
		t.Fatalf("page missing reset probe warning: %s", page)
	}
	if settingsStart < 0 {
		t.Fatalf("page missing settings panel: %s", page)
	}
	if warningStart > settingsStart {
		t.Fatalf("reset probe warning renders after settings panel")
	}
	for _, want := range []string{
		`id="resetProbeWarning" hidden`,
		`updateResetProbeWarning`,
		`statusLoaded&&STATUS.settings&&STATUS.settings.enable_reset_probe!==true`,
		`data-i18n="resetProbe.warningTitle"`,
		`data-i18n="resetProbe.warningBody"`,
		`name="enable_reset_probe"`,
		`id="enableResetProbe"`,
		`data-i18n="settings.enableResetProbe"`,
		`enable_reset_probe:document.getElementById('enableResetProbe').checked`,
		`document.getElementById('enableResetProbe').checked=s.enable_reset_probe===true`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing reset probe marker %q", want)
		}
	}
}

func TestManagementEnglishResetProbeCopy(t *testing.T) {
	page := renderStatusPageForTest(t, NewPluginState(DefaultConfig()))
	for _, want := range []string{
		"Probe performs read-only checks at the quota refresh interval with a 30-minute minimum, even while normal refresh is dormant",
		"sends one tiny request only after detecting a lazy reset window",
		"This may consume a small amount of quota.",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("English reset-probe disclosure missing %q", want)
		}
	}
}

func TestManagementChineseResetProbeCopy(t *testing.T) {
	page := renderStatusPageForTest(t, NewPluginState(DefaultConfig()))
	for _, want := range []string{
		"即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短 30 分钟",
		"只有检测到延迟启动的重置窗口时，才发送一次极小请求",
		"可能消耗少量额度",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("Chinese reset-probe disclosure missing %q", want)
		}
	}
}

func TestStatusPageUsesPlainChineseProbeAndMonthlyCopy(t *testing.T) {
	page := renderStatusPageForTest(t, NewPluginState(DefaultConfig()))
	for _, want := range []string{
		"自动激活新的额度周期",
		"即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短 30 分钟",
		"账号列表未确认时仍允许额度探测（高风险）",
		"通常应保持关闭",
		"月度账号使用方式",
		"优先使用月度账号",
		`'settings.enableResetProbe':'Enable automatic reset probe'`,
		`'settings.enableResetProbeHelp':'Probe performs read-only checks at the quota refresh interval with a 30-minute minimum`,
		`'settings.provisionalProbe':'Allow quota probes when the account roster is unconfirmed (high risk)'`,
		`'settings.provisionalProbeHelp':'When CPA temporarily cannot confirm the current accounts and priorities`,
		`'settings.monthlyMode':'Monthly mode'`,
		`'settings.monthlyPriority':'Prefer Monthly'`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("status page missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"启用自动 reset probe",
		"启用 provisional roster Probe 风险模式",
		"Monthly 模式",
		"优先使用 Monthly",
	} {
		if strings.Contains(page, unwanted) {
			t.Fatalf("status page still contains mixed-language copy %q", unwanted)
		}
	}
}

func TestStatusPageDoesNotClobberDirtySettingsOnProtectedPoll(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{
		"let settingsDirty=false",
		"settingsFocusedOrDirty",
		"settingsPanel.addEventListener('input'",
		"fillSettings:true",
		"!settingsFocusedOrDirty()",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing dirty settings guard marker %q", want)
		}
	}
}

func TestDynamicAccountRenderingUsesChineseBaseText(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{"addBadge('可用'", "'5 小时额度'", "'刷新额度'", "'编辑'", "renderEmptyState", "暂无账号数据"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing localized dynamic rendering marker %q", want)
		}
	}
	for _, forbidden := range []string{"addBadge('Available'", "addQuota(account.five_hour,'5-hour quota'", "'No account data yet.'"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("dynamic account rendering still contains English base text %q", forbidden)
		}
	}
}

func TestDynamicAccountRenderingShowsResetCreditExpiry(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{"resetCreditSummary", "reset_credits", "expires_at"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing reset credit expiry marker %q", want)
		}
	}
}

func TestDynamicAccountRenderingShowsEmptyStateAndStatusNote(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{"renderEmptyState", "STATUS.empty_state", "account.status_note", "调度状态"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing empty/status state marker %q", want)
		}
	}
}

func TestDynamicAccountRenderingHasBilingualStatusLabels(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{
		"DUE_REASON_LABELS",
		"UNAVAILABLE_REASON_LABELS",
		"labelDueReason(account.refresh_due_reason)",
		"labelUnavailableReason(account.unavailable_reason)",
		".empty strong,.empty div",
		"Quota data is stale or pending refresh",
		"No Codex accounts were found",
		"The scheduler is sleeping",
		"Retry is due now",
		"Circuit wait",
		"No data",
		"Used up",
		"Scheduler state",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing bilingual dynamic label marker %q", want)
		}
	}
}

func TestStatusPageUsesProtectedLoadForStatusAndKeyForEdit(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	for _, want := range []string{
		"loadStatus",
		"startStatusPolling",
		"await refreshStatus({management:true,fillSettings:true})",
		"function openEdit(authID){if(!hasManagementKey())",
		"logs:STATUS.logs||[]",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing protected load/key-gated edit marker %q", want)
		}
	}
	for _, forbidden := range []string{"PUBLIC_STATUS_BASE", "requestPublicStatus", "await requestPublicStatus()"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("page still contains public status marker %q", forbidden)
		}
	}
}

func renderStatusPageForTest(t *testing.T, store *PluginState) string {
	t.Helper()
	resp := handleStatusRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/status",
	}, time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", resp.StatusCode, resp.Body)
	}
	return string(resp.Body)
}

func TestStatusJSONIncludesCircuitStateAndResetCredits(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	count := 2
	account.Quota.ResetCreditsAvailableCount = &count
	total := 3
	account.Quota.ResetCreditsTotalEarnedCount = &total
	account.Quota.ResetCredits = []ResetCredit{
		{ExpiresAt: now.Add(30 * 24 * time.Hour)},
	}
	account.Circuit = CircuitBreakerState{
		State:        CircuitStateOpen,
		FailureCount: 3,
		OpenedAt:     now.Add(-time.Minute),
		NextProbeAt:  now.Add(9 * time.Minute),
		Reason:       usageLimitReachedReason,
	}
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(body.Accounts))
	}
	accountBody := body.Accounts[0]
	if accountBody.Circuit.State != CircuitStateOpen || accountBody.Circuit.Label != "熔断" || accountBody.Circuit.FailureCount != 3 || accountBody.UnavailableReason != "circuit_open" {
		t.Fatalf("circuit = %#v unavailable=%q", accountBody.Circuit, accountBody.UnavailableReason)
	}
	if accountBody.ResetCreditsAvailableCount == nil || *accountBody.ResetCreditsAvailableCount != 2 || len(accountBody.ResetCredits) != 1 {
		t.Fatalf("reset credits = count %#v list %#v", accountBody.ResetCreditsAvailableCount, accountBody.ResetCredits)
	}
	if accountBody.ResetCreditsTotalEarnedCount == nil || *accountBody.ResetCreditsTotalEarnedCount != 3 {
		t.Fatalf("total earned reset credits = %#v", accountBody.ResetCreditsTotalEarnedCount)
	}
}

func TestSettingsEndpointUpdatesConfigAndPersistsDefaultState(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/plugins/codex-quota-scheduler/settings",
		Body:   []byte(`{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45s","stale_after":"15m","enable_usage_feedback":false,"max_refresh_concurrency":2,"max_log_entries":25,"log_retention":"3h"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	cfg := store.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval != 45*time.Second || cfg.StaleAfter != 15*time.Minute || cfg.EnableUsageFeedback || cfg.MaxRefreshConcurrency != 2 || cfg.MaxLogEntries != 25 || cfg.LogRetention != 3*time.Hour {
		t.Fatalf("config = %#v", cfg)
	}
	disk, _, err := loadUserData(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if disk.Config.MonthlyMode != MonthlyModePriority || disk.Config.HandleEnabled {
		t.Fatalf("persisted config = %#v", disk.Config)
	}
}

func TestSettingsPayloadIncludesAdaptiveRefresh(t *testing.T) {
	cfg := DefaultConfig()
	payload := SettingsFromConfig(cfg)
	if payload.RefreshActiveWindow != "1h0m0s" {
		t.Fatalf("RefreshActiveWindow = %q, want 1h0m0s", payload.RefreshActiveWindow)
	}
	if payload.RefreshAfterResetDelay != "1m0s" {
		t.Fatalf("RefreshAfterResetDelay = %q, want 1m0s", payload.RefreshAfterResetDelay)
	}
	if payload.RefreshRetryDelays != "1m0s,5m0s,15m0s" {
		t.Fatalf("RefreshRetryDelays = %q, want 1m0s,5m0s,15m0s", payload.RefreshRetryDelays)
	}
	if payload.RefreshOnStartup {
		t.Fatal("RefreshOnStartup = true, want false")
	}
}

func TestSettingsPayloadIncludesResetProbeFlag(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	payload := SettingsFromConfig(cfg)
	if !payload.EnableResetProbe {
		t.Fatal("EnableResetProbe = false, want true")
	}

	payload.EnableResetProbe = true
	roundTrip, err := ConfigFromSettings(DefaultConfig(), payload)
	if err != nil {
		t.Fatalf("ConfigFromSettings returned error: %v", err)
	}
	if !roundTrip.EnableResetProbe {
		t.Fatal("roundTrip EnableResetProbe = false, want true")
	}
}

func TestStatusPayloadIncludesRefreshFailureVisibility(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{
		AuthID:        "auth-1",
		AuthIndex:     "idx-1",
		Provider:      "codex",
		Priority:      1,
		LastSuccessAt: now.Add(-6 * time.Hour),
		LastError:     "quota request returned status 403",
		Refresh: AccountRefreshState{
			LastFailureKind: RefreshFailureTransient,
			RetryAttempt:    1,
			NextRetryAt:     now.Add(time.Minute),
			LastFailureAt:   now,
		},
	})
	store.RecordCodexActivity(now)

	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(payload.Accounts))
	}
	account := payload.Accounts[0]
	if account.LastError != "quota request returned status 403" {
		t.Fatalf("LastError = %q", account.LastError)
	}
	if account.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
	if account.NextRetryText != now.Add(time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("NextRetryText = %q, want retry timestamp", account.NextRetryText)
	}
	if account.RefreshDueReason != "retry_wait" {
		t.Fatalf("RefreshDueReason = %q, want retry_wait", account.RefreshDueReason)
	}

	raw, err := json.Marshal(payload.Accounts[0])
	if err != nil {
		t.Fatalf("marshal status account: %v", err)
	}
	if !strings.Contains(string(raw), `"auth_failure":false`) {
		t.Fatalf("status JSON = %s, want explicit auth_failure false", string(raw))
	}
}

func TestStatusPayloadIncludesAuthFailureVisibility(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		Provider:  "codex",
		Priority:  1,
		LastError: "quota request returned status 401",
		Refresh: AccountRefreshState{
			LastFailureKind: RefreshFailureAuth,
			AuthFailure:     true,
			LastFailureAt:   now,
		},
	})

	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(payload.Accounts))
	}
	account := payload.Accounts[0]
	if !account.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if account.RefreshDueReason != "auth_failure" {
		t.Fatalf("RefreshDueReason = %q, want auth_failure", account.RefreshDueReason)
	}
}

func TestStatusPayloadIncludesResetProbeStatus(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.ResetProbes = map[WindowKind]ResetProbeState{
		WindowWeekly: {
			WindowKind:  WindowWeekly,
			Status:      ResetProbeStatusFailed,
			ResetAt:     now.Add(7 * time.Hour),
			NextCheckAt: now.Add(8 * time.Hour),
			LastProbeAt: now.Add(-10 * time.Minute),
			VerifiedAt:  now.Add(-5 * time.Minute),
			Attempts:    2,
			Error:       "redacted upstream error",
		},
		WindowFiveHour: {
			WindowKind:  WindowFiveHour,
			Status:      ResetProbeStatusPending,
			ResetAt:     now.Add(5 * time.Hour),
			NextCheckAt: now.Add(6 * time.Hour),
		},
	}
	store.UpsertQuota(account)

	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal status payload: %v", err)
	}
	var body struct {
		Accounts []struct {
			ResetProbes []struct {
				WindowKind  string `json:"window_kind"`
				Status      string `json:"status"`
				ResetAt     string `json:"reset_at,omitempty"`
				NextCheckAt string `json:"next_check_at,omitempty"`
				LastProbeAt string `json:"last_probe_at,omitempty"`
				VerifiedAt  string `json:"verified_at,omitempty"`
				Attempts    int    `json:"attempts,omitempty"`
				Error       string `json:"error,omitempty"`
			} `json:"reset_probes"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal status payload: %v; body=%s", err, raw)
	}
	if len(body.Accounts) != 1 || len(body.Accounts[0].ResetProbes) != 2 {
		t.Fatalf("reset probes = %#v; body=%s", body.Accounts, raw)
	}
	probes := body.Accounts[0].ResetProbes
	if probes[0].WindowKind != "five_hour" || probes[1].WindowKind != "weekly" {
		t.Fatalf("reset probes not sorted by window kind: %#v", probes)
	}
	if probes[0].Status != "pending" || probes[0].ResetAt != formatTime(now.Add(5*time.Hour)) || probes[0].NextCheckAt != formatTime(now.Add(6*time.Hour)) {
		t.Fatalf("five-hour probe = %#v", probes[0])
	}
	if probes[1].Status != "failed" || probes[1].LastProbeAt != formatTime(now.Add(-10*time.Minute)) || probes[1].VerifiedAt != formatTime(now.Add(-5*time.Minute)) || probes[1].Attempts != 2 || probes[1].Error != "redacted upstream error" {
		t.Fatalf("weekly probe = %#v", probes[1])
	}
}

func TestConfigFromSettingsParsesAdaptiveRefresh(t *testing.T) {
	cfg, err := ConfigFromSettings(DefaultConfig(), SettingsPayload{
		HandleEnabled:                   true,
		MonthlyMode:                     MonthlyModeExpiryOrder,
		QuotaRefreshInterval:            "30m",
		StaleAfter:                      "5h",
		EnableUsageFeedback:             true,
		MaxRefreshConcurrency:           1,
		CircuitFailureThreshold:         5,
		CircuitOpenDuration:             "30m",
		CircuitHalfOpenSuccessThreshold: 2,
		MaxLogEntries:                   200,
		LogRetention:                    "24h",
		RefreshActiveWindow:             "90m",
		RefreshAfterResetDelay:          "2m",
		RefreshRetryDelays:              "2m,6m,18m",
		RefreshOnStartup:                true,
	})
	if err != nil {
		t.Fatalf("ConfigFromSettings returned error: %v", err)
	}
	if cfg.RefreshActiveWindow != 90*time.Minute || cfg.RefreshAfterResetDelay != 2*time.Minute || cfg.RefreshOnStartup != true {
		t.Fatalf("adaptive refresh config = %#v", cfg)
	}
	want := []time.Duration{2 * time.Minute, 6 * time.Minute, 18 * time.Minute}
	if len(cfg.RefreshRetryDelays) != len(want) {
		t.Fatalf("RefreshRetryDelays = %#v, want %#v", cfg.RefreshRetryDelays, want)
	}
	for i := range want {
		if cfg.RefreshRetryDelays[i] != want[i] {
			t.Fatalf("RefreshRetryDelays = %#v, want %#v", cfg.RefreshRetryDelays, want)
		}
	}
}

func TestStatusJSONIncludesQuotaWindowsForProgressBars(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	fiveHourUsed := 100.0
	weeklyUsed := 40.0
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), true)
	account.Quota.FiveHour.UsedPercent = &fiveHourUsed
	account.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(body.Accounts))
	}
	accountBody := body.Accounts[0]
	if accountBody.FiveHour.UsedPercent != 100 || accountBody.FiveHour.RemainingPercent != 0 || accountBody.FiveHour.DisplayText != "已用完" || !accountBody.FiveHour.Exhausted {
		t.Fatalf("five hour window = %#v, want exhausted with zero remaining", accountBody.FiveHour)
	}
	if accountBody.LongWindow.UsedPercent != 40 || accountBody.LongWindow.RemainingPercent != 60 || accountBody.LongWindow.DisplayText != "剩余 60%" || accountBody.LongWindow.Label == "" {
		t.Fatalf("long window = %#v, want weekly remaining label and percent", accountBody.LongWindow)
	}
}

func TestStatusPayloadAllowsMissingFiveHourWithLongWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 0, now.Add(24*time.Hour), false)
	account.Quota.FiveHour = nil
	account.LastSuccessAt = now
	store.UpsertQuota(account)
	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(requestWithCandidates("auth-1"), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)

	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts = %#v", payload.Accounts)
	}
	got := payload.Accounts[0]
	if !got.Available || got.QueueStatus != QueueStatusAvailable {
		t.Fatalf("account = %#v, want available", got)
	}
	if !got.FiveHour.Missing {
		t.Fatalf("five_hour = %#v, want missing", got.FiveHour)
	}
	if got.LongWindow.Missing {
		t.Fatalf("long_window = %#v, want visible", got.LongWindow)
	}
	html := string(RenderStatusHTML(payload))
	if rows := strings.Count(html, `<div class="quota-row">`); rows != 1 {
		t.Fatalf("server-rendered quota rows = %d, want only LongWindow", rows)
	}
	if !strings.Contains(html, "if(account.five_hour&&!account.five_hour.missing)") {
		t.Fatal("dynamic account renderer lost missing FiveHour guard")
	}
}

func TestStatusWindowPrefersRealUsagePercentOverExhaustedFlag(t *testing.T) {
	used := 40.0
	status := statusWindow(&QuotaWindow{
		Kind:        WindowWeekly,
		UsedPercent: &used,
		Exhausted:   true,
	}, "周额度")
	if status.Exhausted {
		t.Fatalf("Exhausted = true, want false when used_percent shows quota remains: %#v", status)
	}
	if status.RemainingPercent != 60 || status.DisplayText != "剩余 60%" {
		t.Fatalf("window = %#v, want remaining quota from real used_percent", status)
	}
}

func TestStatusHTMLShowsRemainingQuotaLocalResetTimesAndCompactMetadata(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "主力号", Notes: "账号备注内容", Tags: []string{"team", "paid"}, GroupID: "ops"},
		},
		Groups: map[string]GroupAnnotation{
			"ops": {Name: "运营组", Notes: "分组备注内容"},
		},
	})
	usedFive := 25.0
	usedLong := 40.0
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	account.Quota.FiveHour.UsedPercent = &usedFive
	account.Quota.FiveHour.ResetAt = now.Add(3 * time.Hour)
	account.Quota.LongWindow.UsedPercent = &usedLong
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	for _, want := range []string{"剩余 75%", "剩余 60%", "5 小时重置", "长额度重置", "localTime", "data-time=\"2026-06-21T12:00:00Z\"", "主力号", "运营组", "账号备注内容", "分组备注内容"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q: %s", want, html)
		}
	}
}

func TestManagementSettingsEndpointUpdatesConfig(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/codex-quota-scheduler/settings",
		Body:   []byte(`{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45m","stale_after":"6h","enable_usage_feedback":false,"max_refresh_concurrency":2,"max_log_entries":30,"log_retention":"4h"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	cfg := store.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval != 45*time.Minute || cfg.StaleAfter != 6*time.Hour || cfg.EnableUsageFeedback || cfg.MaxRefreshConcurrency != 2 || cfg.MaxLogEntries != 30 || cfg.LogRetention != 4*time.Hour {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestResourceStatusQueryActionsDoNotMutateState(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	originalConfig := store.Config()
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "original"},
		},
	})

	refreshes := 0
	refreshOne := ""
	previousRefreshSoon := managementRefreshSoon
	previousRefreshOneSoon := managementRefreshOneSoon
	managementRefreshSoon = func() { refreshes++ }
	managementRefreshOneSoon = func(authID string) { refreshOne = authID }
	t.Cleanup(func() {
		managementRefreshSoon = previousRefreshSoon
		managementRefreshOneSoon = previousRefreshOneSoon
	})

	actions := map[string]string{
		"settings":            `{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45m","stale_after":"6h","enable_usage_feedback":false,"enable_reset_probe":true,"max_refresh_concurrency":2,"max_log_entries":30,"log_retention":"4h"}`,
		"refresh":             `{}`,
		"refresh_account":     `{"auth_id":"auth-1"}`,
		"import":              `{"config":{"monthly_mode":"priority","quota_refresh_interval":2700000000000,"stale_after":21600000000000,"max_refresh_concurrency":2}}`,
		"annotations_replace": `{"accounts":{"auth:auth-1":{"alias":"replaced"}}}`,
		"annotations_account": `{"auth_id":"auth-1","alias":"changed"}`,
		"annotations_group":   `{"id":"group-1","name":"changed"}`,
	}
	for action, payload := range actions {
		resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
			Query: url.Values{
				"action":  []string{action},
				"payload": []string{payload},
			},
		}, time.Now())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s StatusCode = %d, want %d; body=%s", action, resp.StatusCode, http.StatusOK, resp.Body)
		}
	}
	if !reflect.DeepEqual(store.Config(), originalConfig) {
		t.Fatalf("resource action changed config: %#v", store.Config())
	}
	if store.Config().EnableResetProbe {
		t.Fatalf("resource action enabled reset probe: %#v", store.Config())
	}
	if got := store.Annotations().Accounts["auth:auth-1"].Alias; got != "original" {
		t.Fatalf("resource action changed annotation alias = %q, want original", got)
	}
	if refreshes != 0 || refreshOne != "" {
		t.Fatalf("resource action triggered refreshes = %d, refreshOne = %q; want none", refreshes, refreshOne)
	}
}

func TestResourceStatusFormatJSONStillRendersHTMLShell(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if got := resp.Headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	body := strings.TrimSpace(string(resp.Body))
	if strings.HasPrefix(body, "{") || !strings.Contains(body, "<!doctype html>") {
		t.Fatalf("resource format=json returned non-HTML body: %s", body)
	}
}

func TestResourceStatusRendersUsablePluginPage(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "secret alias", Notes: "private note"},
		},
	})
	store.UpsertQuota(weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false))
	store.RecordLog("info", "scheduler.selected", "private log", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	body := string(resp.Body)
	for _, forbidden := range []string{"window.location.replace", "http-equiv=\"refresh\"", "requestPlugin(action,options)", "RESOURCE_ENDPOINT+'?'"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("resource status still depends on management redirect marker %q: %s", forbidden, body)
		}
	}
	for _, forbidden := range []string{"secret alias", "private note", "private log", "auth-1"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("resource status leaked privileged status marker %q: %s", forbidden, body)
		}
	}
	for _, want := range []string{"logList", "managementKey", "loadStatus", "MANAGEMENT_BASE", "/v0/management/plugins/codex-quota-scheduler", "authHeaders()", "let STATUS=", `"shell":true`, "statusLoaded=!STATUS.shell", "notice.statusLoaded", "refreshStatus", "只要调度器启动了，它就会在后台自动运行，无需保持页面开启"} {
		if !strings.Contains(body, want) {
			t.Fatalf("resource status missing usable plugin page marker %q: %s", want, body)
		}
	}
}

func TestResourceStatusDataIsNotPublicWithoutManagementKey(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "公开别名", Notes: "公开备注"},
		},
	})
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastError = "transport failed with access_token Authorization Bearer cookie at " + chatGPTQuotaEndpoint + " and " + codexResetProbeEndpoint
	count := 1
	account.Quota.ResetCreditsAvailableCount = &count
	account.Quota.ResetCredits = []ResetCredit{{ExpiresAt: now.Add(30 * 24 * time.Hour), Status: "available"}}
	account.ResetProbes = map[WindowKind]ResetProbeState{
		WindowFiveHour: {
			WindowKind:  WindowFiveHour,
			Status:      ResetProbeStatusFailed,
			ResetAt:     now.Add(5 * time.Hour),
			NextCheckAt: now.Add(6 * time.Hour),
			LastProbeAt: now.Add(-time.Minute),
			Attempts:    1,
			Error:       "redacted access_token bearer authorization cookie " + chatGPTQuotaEndpoint + " " + codexResetProbeEndpoint,
		},
	}
	store.UpsertQuota(account)
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "auth-1"}, now)

	refreshes := 0
	refreshOne := ""
	previousRefreshSoon := managementRefreshSoon
	previousRefreshOneSoon := managementRefreshOneSoon
	managementRefreshSoon = func() { refreshes++ }
	managementRefreshOneSoon = func(authID string) { refreshOne = authID }
	t.Cleanup(func() {
		managementRefreshSoon = previousRefreshSoon
		managementRefreshOneSoon = previousRefreshOneSoon
	})

	store.RecordLog("warn", "quota.reset_probe_failed", "Codex reset probe failed", map[string]any{"error": "probe failed at " + codexResetProbeEndpoint}, now)
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status-data",
		Query:  url.Values{"action": []string{"refresh"}, "payload": []string{"{}"}},
	}, now)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusNotFound, resp.Body)
	}
	if refreshes != 0 || refreshOne != "" {
		t.Fatalf("public status data triggered refreshes = %d, refreshOne = %q; want none", refreshes, refreshOne)
	}
	bodyText := strings.ToLower(string(resp.Body))
	for _, forbidden := range []string{"auth-1", "public", "private", "scheduler.selected", "reset_probes", "access_token", "refresh_token", "id_token", "bearer ", "authorization", "cookie", chatGPTQuotaEndpoint, codexResetProbeEndpoint} {
		if strings.Contains(bodyText, forbidden) {
			t.Fatalf("public status data leaked sensitive marker %q: %s", forbidden, resp.Body)
		}
	}
}

func TestManagementAccountEndpointUpdatesAnnotation(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"auth_id":"auth-1","alias":"工作账号","group_id":"team-a","tags":["team","paid"],"notes":"常用"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	got := store.Annotations().Accounts["auth:auth-1"]
	if got.Alias != "工作账号" || got.GroupID != "team-a" || len(got.Tags) != 2 || got.Notes != "常用" {
		t.Fatalf("account annotation = %#v", got)
	}
}

func TestManagementAccountEndpointUpdatesAndResetsSchedulerPriority(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 3, time.Now().Add(24*time.Hour), false)
	store.UpsertQuota(account)
	for _, test := range []struct {
		body string
		want int
	}{
		{body: `{"auth_id":"auth-1","scheduler_priority":8}`, want: 8},
		{body: `{"auth_id":"auth-1","scheduler_priority":0}`, want: 0},
	} {
		resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
			Method: http.MethodPatch,
			Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/account",
			Body:   []byte(test.body),
		}, time.Now())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
		}
		if got := store.Annotations().Accounts["auth:auth-1"].SchedulerPriority; got != test.want {
			t.Fatalf("scheduler priority = %d, want %d", got, test.want)
		}
		persisted, _, err := loadUserData(semanticStatePaths(defaultStatePath()).UserData)
		if err != nil {
			t.Fatalf("LoadPluginDiskState returned error: %v", err)
		}
		if got := persisted.Accounts["auth:auth-1"].SchedulerPriority; got != test.want {
			t.Fatalf("persisted scheduler priority = %d, want %d", got, test.want)
		}
		if got := store.Snapshot(time.Now()).Accounts[0].Priority; got != 3 {
			t.Fatalf("CPA priority = %d, want unchanged value 3", got)
		}
	}
	raw, err := os.ReadFile(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if strings.Contains(string(raw), "cpa_priority") {
		t.Fatalf("annotation PATCH persisted CPA priority: %s", raw)
	}
}

func TestManagementAccountEndpointClearsTags(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "A", Tags: []string{"wrong", "old"}},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"auth_id":"auth-1","tags":[]}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if tags := store.Annotations().Accounts["auth:auth-1"].Tags; len(tags) != 0 {
		t.Fatalf("tags = %#v, want cleared", tags)
	}
}

func TestManagementGroupEndpointAllowsClearingFields(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Groups: map[string]GroupAnnotation{
			"1": {Name: "group1", Notes: "keep", Color: "#00f"},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/group",
		Body:   []byte(`{"id":"1","name":"","notes":"","color":""}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	got := store.Annotations().Groups["1"]
	if got.Name != "" || got.Notes != "" || got.Color != "" {
		t.Fatalf("group = %#v, want clearable fields emptied", got)
	}
}

func TestResourceExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	store := NewPluginState(cfg)
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "A", Tags: []string{"team"}, GroupID: "1", SchedulerPriority: 9},
		},
		Groups: map[string]GroupAnnotation{
			"1": {Name: "group1"},
		},
	})

	exportResp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/codex-quota-scheduler/export",
	}, time.Now())
	if exportResp.StatusCode != http.StatusOK {
		t.Fatalf("export StatusCode = %d, want %d; body=%s", exportResp.StatusCode, http.StatusOK, exportResp.Body)
	}

	imported := NewPluginState(DefaultConfig())
	importResp := HandleManagementRequest(imported, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/codex-quota-scheduler/import",
		Body:   exportResp.Body,
	}, time.Now())
	if importResp.StatusCode != http.StatusOK {
		t.Fatalf("import StatusCode = %d, want %d; body=%s", importResp.StatusCode, http.StatusOK, importResp.Body)
	}
	if imported.Config().MonthlyMode != MonthlyModePriority {
		t.Fatalf("imported config = %#v", imported.Config())
	}
	if got := imported.Annotations().Groups["1"].Name; got != "group1" {
		t.Fatalf("imported group name = %q, want group1", got)
	}
	if got := imported.Annotations().Accounts["auth:auth-1"].Alias; got != "A" {
		t.Fatalf("imported account alias = %q, want A", got)
	}
	if got := imported.Annotations().Accounts["auth:auth-1"].SchedulerPriority; got != 9 {
		t.Fatalf("imported scheduler priority = %d, want 9", got)
	}
}

func TestManagementSettingsAndImportRejectNonChatGPTQuotaEndpoint(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	settingsResp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/codex-quota-scheduler/settings",
		Body:   []byte(`{"handle_enabled":true,"monthly_mode":"expiry_order","quota_refresh_interval":"30m","stale_after":"5h","enable_usage_feedback":true,"max_refresh_concurrency":1,"quota_endpoint":"https://example.test/usage","max_log_entries":2000,"log_retention":"24h"}`),
	}, time.Now())
	if settingsResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("settings StatusCode = %d, want %d; body=%s", settingsResp.StatusCode, http.StatusBadRequest, settingsResp.Body)
	}
	if store.Config().QuotaEndpoint != DefaultConfig().QuotaEndpoint {
		t.Fatalf("settings changed quota endpoint to %q", store.Config().QuotaEndpoint)
	}

	importResp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/codex-quota-scheduler/import",
		Body:   []byte(`{"config":{"HandleEnabled":true,"MonthlyMode":"expiry_order","QuotaRefreshInterval":1800000000000,"StaleAfter":18000000000000,"MaxRefreshConcurrency":1,"QuotaEndpoint":"https://example.test/usage","MaxLogEntries":2000,"LogRetention":86400000000000}}`),
	}, time.Now())
	if importResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("import StatusCode = %d, want %d; body=%s", importResp.StatusCode, http.StatusBadRequest, importResp.Body)
	}
	if store.Config().QuotaEndpoint != DefaultConfig().QuotaEndpoint {
		t.Fatalf("import changed quota endpoint to %q", store.Config().QuotaEndpoint)
	}
}

func TestLogsEndpointReturnsSchedulerDecision(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/codex-quota-scheduler/logs",
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body struct {
		Logs []LogEntry `json:"logs"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Logs) != 1 || body.Logs[0].Event != "scheduler.selected" {
		t.Fatalf("logs = %#v", body.Logs)
	}
}

func TestManagementLogsExposeProbeLifecycleWithoutSensitiveValues(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.quotaStatus = http.StatusServiceUnavailable
	host.quota = [][]byte{[]byte(`{"access_token":"access-token-sentinel","refresh_token":"refresh-token-sentinel","account_id":"account-id-sentinel","authorization":"Bearer authorization-header-sentinel","request_body":"request-body-sentinel","response_body":"response-body-sentinel"}`)}
	host.mu.Unlock()
	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce returned nil error")
	}

	resp := HandleManagementRequest(r.state, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/management/plugins/codex-quota-scheduler/status",
		Headers: http.Header{"Authorization": []string{"Bearer management-key"}},
		Query:   url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Logs) != 2 {
		t.Fatalf("logs = %#v, want precheck and terminal failure entries", body.Logs)
	}
	for i, want := range []struct{ event, message string }{
		{"probe.precheck_started", "开始检查疑似未激活的额度周期"},
		{"probe.failed", "额度周期探测失败，已按安全策略处理"},
	} {
		if body.Logs[i].Event != want.event || body.Logs[i].Message != want.message {
			t.Fatalf("log %d = %#v, want visible event/message %#v", i, body.Logs[i], want)
		}
	}
	for _, forbidden := range []string{"access-token-sentinel", "refresh-token-sentinel", "account-id-sentinel", "authorization-header-sentinel", "request-body-sentinel", "response-body-sentinel"} {
		if strings.Contains(string(resp.Body), forbidden) {
			t.Fatalf("management status leaked %q: %s", forbidden, resp.Body)
		}
	}
}

func TestAnnotationsEndpointsNormalizePatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:keep": {Alias: "Keep", Tags: []string{"old"}},
		},
		Groups: map[string]GroupAnnotation{
			"group-1": {Name: "Existing"},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "PATCH",
		Path:   "/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"key":"auth:new","alias":" New ","tags":["alpha","alpha"," "],"group_id":"group-1"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	state := store.Annotations()
	if state.Accounts["auth:keep"].Alias != "Keep" {
		t.Fatalf("unrelated account annotation was not preserved: %#v", state.Accounts)
	}
	got := state.Accounts["auth:new"]
	if got.Alias != " New " || len(got.Tags) != 1 || got.Tags[0] != "alpha" || got.GroupID != "group-1" {
		t.Fatalf("patched account annotation = %#v", got)
	}
	raw, err := os.ReadFile(semanticStatePaths(defaultStatePath()).UserData)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(raw), `"auth:new"`) {
		t.Fatalf("persisted annotations missing patched key: %s", raw)
	}

	resp = HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "PATCH",
		Path:   "/plugins/codex-quota-scheduler/annotations/group",
		Body:   []byte(`{"id":"group-2","annotation":{"name":"Blue","tags":["x","x"],"color":"#00f"}}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	state = store.Annotations()
	if state.Groups["group-1"].Name != "Existing" || state.Groups["group-2"].Name != "Blue" || len(state.Groups["group-2"].Tags) != 1 {
		t.Fatalf("group annotations = %#v", state.Groups)
	}
}

func TestAnnotationsPersistenceFailureDoesNotMutateMemory(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
		check  func(t *testing.T, state AnnotationState)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			path:   "/plugins/codex-quota-scheduler/annotations",
			body:   []byte(`{"accounts":{"auth:new":{"alias":"New"}}}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Accounts["auth:new"]; ok {
					t.Fatalf("failed PUT mutated annotations: %#v", state.Accounts)
				}
			},
		},
		{
			name:   "patch account",
			method: http.MethodPatch,
			path:   "/plugins/codex-quota-scheduler/annotations/account",
			body:   []byte(`{"key":"auth:new","alias":"New"}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Accounts["auth:new"]; ok {
					t.Fatalf("failed account PATCH mutated annotations: %#v", state.Accounts)
				}
			},
		},
		{
			name:   "patch group",
			method: http.MethodPatch,
			path:   "/plugins/codex-quota-scheduler/annotations/group",
			body:   []byte(`{"id":"group-new","name":"New"}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Groups["group-new"]; ok {
					t.Fatalf("failed group PATCH mutated annotations: %#v", state.Groups)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousDefaultStatePath := defaultStatePath
			defaultStatePath = func() string { return filepath.Join(t.TempDir()+string(rune(0)), "state.json") }
			t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

			store := NewPluginState(DefaultConfig())
			store.SetAnnotations(AnnotationState{
				Accounts: map[string]AccountAnnotation{
					"auth:keep": {Alias: "Keep"},
				},
				Groups: map[string]GroupAnnotation{
					"group-keep": {Name: "Keep"},
				},
			})

			resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
				Method: tt.method,
				Path:   tt.path,
				Body:   tt.body,
			}, time.Now())
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusInternalServerError, resp.Body)
			}
			state := store.Annotations()
			if state.Accounts["auth:keep"].Alias != "Keep" || state.Groups["group-keep"].Name != "Keep" {
				t.Fatalf("existing annotations changed after failed persistence: %#v", state)
			}
			tt.check(t, state)
		})
	}
}
