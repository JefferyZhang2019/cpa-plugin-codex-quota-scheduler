package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeHostClient struct {
	authList []pluginapi.HostAuthFileEntry
	authJSON map[string]json.RawMessage
	httpBody []byte
	saved    map[string]json.RawMessage
	listErr  error

	expectedAuthByAccount map[string]string
	responseByAccount     map[string]pluginapi.HTTPResponse
	responseByURL         map[string]pluginapi.HTTPResponse
	httpStatus            int
	doStarted             chan struct{}
	releaseDo             chan struct{}
	listStarted           chan struct{}
	releaseList           chan struct{}
	startedByURL          chan string
	releaseByURL          map[string]chan struct{}
	saveStarted           chan struct{}
	releaseSave           chan struct{}

	mu           sync.Mutex
	listCalls    int
	getCalls     int
	logCalls     int
	httpCalls    int
	activeHTTP   int
	maxActive    int
	headerErrors []string
	urls         []string
}

func (f *fakeHostClient) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	if f.listStarted != nil {
		select {
		case f.listStarted <- struct{}{}:
		default:
		}
	}
	if f.releaseList != nil {
		<-f.releaseList
	}
	return f.authList, f.listErr
}

func (f *fakeHostClient) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	f.mu.Lock()
	f.getCalls++
	f.mu.Unlock()
	name := authIndex + ".json"
	for _, auth := range f.authList {
		if auth.AuthIndex == authIndex && auth.Name != "" {
			name = auth.Name
			break
		}
	}
	return pluginapi.HostAuthGetResponse{AuthIndex: authIndex, Name: name, JSON: f.authJSON[authIndex]}, nil
}

func (f *fakeHostClient) SaveAuth(name string, raw json.RawMessage) error {
	if f.saveStarted != nil {
		select {
		case f.saveStarted <- struct{}{}:
		default:
		}
	}
	if f.releaseSave != nil {
		<-f.releaseSave
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved == nil {
		f.saved = make(map[string]json.RawMessage)
	}
	f.saved[name] = append(json.RawMessage(nil), raw...)
	return nil
}

func (f *fakeHostClient) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.httpCalls++
	f.activeHTTP++
	f.urls = append(f.urls, req.URL)
	if f.activeHTTP > f.maxActive {
		f.maxActive = f.activeHTTP
	}
	f.mu.Unlock()
	if f.startedByURL != nil {
		select {
		case f.startedByURL <- req.URL:
		default:
		}
	}
	specificRelease := f.releaseByURL[req.URL]
	blocks := specificRelease == nil && f.releaseDo != nil && req.URL != resetCreditsEndpoint && req.URL != codexTokenEndpoint
	if f.doStarted != nil && blocks {
		select {
		case f.doStarted <- struct{}{}:
		default:
		}
	}
	if blocks {
		<-f.releaseDo
	}
	if specificRelease != nil {
		<-specificRelease
	}
	defer func() {
		f.mu.Lock()
		f.activeHTTP--
		f.mu.Unlock()
	}()

	if req.URL == codexTokenEndpoint {
		if req.Method != http.MethodPost {
			f.recordHeaderError("token refresh method = " + req.Method)
		}
		if req.Headers.Get("Content-Type") != "application/x-www-form-urlencoded" {
			f.recordHeaderError("token refresh Content-Type = " + req.Headers.Get("Content-Type"))
		}
		if !strings.Contains(string(req.Body), "grant_type=refresh_token") {
			f.recordHeaderError("token refresh body missing grant_type")
		}
		if resp, ok := f.responseByURL[req.URL]; ok {
			return resp, nil
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{}, Body: []byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)}, nil
	}

	accountID := req.Headers.Get("Chatgpt-Account-Id")
	if accountID == "" {
		f.recordHeaderError("missing Chatgpt-Account-Id header")
	} else if accountID == "acct-1" && f.expectedAuthByAccount == nil && req.Headers.Get("Authorization") != "Bearer access-1" {
		f.recordHeaderError("Authorization header = " + req.Headers.Get("Authorization"))
	}
	if expected := f.expectedAuthByAccount[accountID]; expected != "" && req.Headers.Get("Authorization") != expected {
		f.recordHeaderError("Authorization header = " + req.Headers.Get("Authorization") + ", want " + expected)
	}
	if req.Headers.Get("Content-Type") != "application/json" {
		f.recordHeaderError("Content-Type header = " + req.Headers.Get("Content-Type"))
	}
	if req.Headers.Get("User-Agent") != quotaUserAgent {
		f.recordHeaderError("User-Agent header = " + req.Headers.Get("User-Agent"))
	}

	if resp, ok := f.responseByAccount[accountID]; ok {
		return resp, nil
	}
	if resp, ok := f.responseByURL[req.URL]; ok {
		return resp, nil
	}
	status := f.httpStatus
	if status == 0 {
		status = http.StatusOK
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: f.httpBody}, nil
}

func (f *fakeHostClient) Log(level, message string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls++
}

func (f *fakeHostClient) recordHeaderError(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headerErrors = append(f.headerErrors, message)
}

func (f *fakeHostClient) assertNoHeaderErrors(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headerErrors) > 0 {
		t.Fatalf("host request header errors: %v", f.headerErrors)
	}
}

func (f *fakeHostClient) maxActiveHTTP() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeHostClient) httpCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.httpCalls
}

func (f *fakeHostClient) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeHostClient) getCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeHostClient) logCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logCalls
}

func (f *fakeHostClient) requestedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

func (f *fakeHostClient) activeHTTPCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeHTTP
}

func newAdmittedQuotaRefresherForTest(host *fakeHostClient, store *PluginState, now func() time.Time) *QuotaRefresher {
	authIDs := make(map[string]struct{})
	priority := 0
	hasPriority := false
	for _, auth := range host.authList {
		if auth.ID == "" || !strings.EqualFold(auth.Provider, "codex") {
			continue
		}
		authIDs[auth.ID] = struct{}{}
		if !hasPriority || auth.Priority > priority {
			priority = auth.Priority
			hasPriority = true
		}
	}
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: priority, AuthIDs: authIDs})
	return NewQuotaRefresher(host, store, now)
}

func waitForStartedURL(t *testing.T, started <-chan string, want string) {
	t.Helper()
	for {
		select {
		case got := <-started:
			if got == want {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for URL %s", want)
		}
	}
}

func replaceAdmissionAsync(store *PluginState, admission CPAAdmissionState) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		store.ReplaceCPAAdmission(admission)
		close(done)
	}()
	return done
}

func accountByAuthID(t *testing.T, snapshot StateSnapshot, authID string) AccountState {
	t.Helper()
	for _, account := range snapshot.Accounts {
		if account.AuthID == authID {
			return account
		}
	}
	t.Fatalf("missing account with auth ID %q in %#v", authID, snapshot.Accounts)
	return AccountState{}
}

func TestRefreshDueOnceSkipsFreshAccountsAndRefreshesOnlyDueAccounts(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	staleToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-stale"})
	freshToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-fresh"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "stale", AuthIndex: "idx-stale", Provider: "codex"},
			{ID: "fresh", AuthIndex: "idx-fresh", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-stale": json.RawMessage(`{"access_token":"access-stale","id_token":"` + staleToken + `"}`),
			"idx-fresh": json.RawMessage(`{"access_token":"access-fresh","id_token":"` + freshToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:        "stale",
		AuthIndex:     "idx-stale",
		Provider:      "codex",
		LastSuccessAt: now.Add(-6 * time.Hour),
	})
	store.UpsertQuota(AccountState{
		AuthID:        "fresh",
		AuthIndex:     "idx-fresh",
		Provider:      "codex",
		LastSuccessAt: now.Add(-10 * time.Minute),
	})

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}

	host.assertNoHeaderErrors(t)
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("HTTP calls = %d, want 2 quota/reset calls for stale account only", got)
	}
	stale := accountByAuthID(t, store.Snapshot(now), "stale")
	if stale.LastSuccessAt.IsZero() {
		t.Fatal("stale account LastSuccessAt is zero, want refreshed")
	}
	fresh := accountByAuthID(t, store.Snapshot(now), "fresh")
	if !fresh.LastSuccessAt.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("fresh LastSuccessAt = %s, want unchanged", fresh.LastSuccessAt)
	}
}

func TestRefreshDueOnceSkipsAllFreshKnownAccounts(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:        "fresh",
		AuthIndex:     "idx-fresh",
		Provider:      "codex",
		LastSuccessAt: now.Add(-10 * time.Minute),
	})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "fresh", AuthIndex: "idx-fresh", Provider: "codex"}},
	}

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}

	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want 0 when known accounts are fresh", got)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 when known accounts are fresh", got)
	}
}

func TestRefreshDueOnceRefreshesAccountPastQuotaRefreshInterval(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	cfg.StaleAfter = 5 * time.Hour
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:        "auth-1",
		AuthIndex:     "idx-1",
		Provider:      "codex",
		LastSuccessAt: now.Add(-31 * time.Minute),
	})

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}

	host.assertNoHeaderErrors(t)
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("HTTP calls = %d, want quota/reset calls for interval-due account", got)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if !account.LastSuccessAt.Equal(now) {
		t.Fatalf("LastSuccessAt = %s, want refreshed at %s", account.LastSuccessAt, now)
	}
}

func TestRefreshDueOnceDoesNotRepeatSuccessfulRefreshForObservedPastReset(t *testing.T) {
	resetAt := time.Date(2026, 7, 10, 11, 50, 0, 0, time.UTC)
	currentNow := resetAt.Add(10 * time.Minute)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":%q}}}`, resetAt.Format(time.RFC3339))),
	}
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	store := NewPluginState(cfg)
	store.RecordCodexActivity(currentNow)
	store.UpsertQuota(AccountState{
		AuthID:        "auth-1",
		AuthIndex:     "idx-1",
		Provider:      "codex",
		Quota:         ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: resetAt}},
		LastSuccessAt: resetAt,
	})

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return currentNow })
	account := accountByAuthID(t, store.Snapshot(currentNow), "auth-1")
	if due, reason := accountRefreshDue(account, cfg, currentNow); !due || reason != "five_hour_reset_due" {
		t.Fatalf("pre-refresh due=%t reason=%q, want true five_hour_reset_due", due, reason)
	}
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("first RefreshDueOnce returned error: %v", err)
	}
	firstCalls := host.httpCallCount()
	if firstCalls == 0 {
		t.Fatal("first RefreshDueOnce made no HTTP calls")
	}

	currentNow = currentNow.Add(2 * time.Second)
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("second RefreshDueOnce returned error: %v", err)
	}
	if got := host.httpCallCount(); got != firstCalls {
		t.Fatalf("HTTP calls after second refresh = %d, want unchanged %d", got, firstCalls)
	}
}

func TestRefreshDueOnceDiscoversAuthsWhenStateIsEmpty(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}

	host.assertNoHeaderErrors(t)
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("HTTP calls = %d, want quota/reset discovery calls", got)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastSuccessAt.IsZero() {
		t.Fatal("discovered account LastSuccessAt is zero")
	}
}

func TestRefreshDueCandidatesOnceUsesActivePriorityCandidates(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	lowToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-low"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`),
			"idx-low":  json.RawMessage(`{"access_token":"access-low","id_token":"` + lowToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	legacy := DefaultConfig()
	legacy.ScheduleAcrossPriorities = false
	store := NewPluginState(legacy)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{AuthID: "high", AuthIndex: "idx-high", Provider: "codex", LastSuccessAt: now.Add(-6 * time.Hour), Priority: 10})
	store.UpsertQuota(AccountState{AuthID: "low", AuthIndex: "idx-low", Provider: "codex", LastSuccessAt: now.Add(-6 * time.Hour), Priority: 1})
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "low", Provider: "codex", Priority: 1},
	}}
	if err := refresher.RefreshDueCandidatesOnce(req); err != nil {
		t.Fatalf("RefreshDueCandidatesOnce returned error: %v", err)
	}

	high := accountByAuthID(t, store.Snapshot(now), "high")
	if high.LastSuccessAt.IsZero() || !high.LastSuccessAt.Equal(now) {
		t.Fatalf("high LastSuccessAt = %s, want refreshed now", high.LastSuccessAt)
	}
	for _, account := range store.Snapshot(now).Accounts {
		if account.AuthID == "low" {
			t.Fatalf("low-priority account remained in state: %#v", account)
		}
	}
}

func TestRefreshDueCandidatesOnceSkipsFreshKnownCandidates(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{AuthID: "high", AuthIndex: "idx-high", Provider: "codex", LastSuccessAt: now.Add(-10 * time.Minute), Priority: 10})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "high", AuthIndex: "idx-high", Provider: "codex"}},
	}
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
	}}
	if err := refresher.RefreshDueCandidatesOnce(req); err != nil {
		t.Fatalf("RefreshDueCandidatesOnce returned error: %v", err)
	}

	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want 0 when candidate is already fresh", got)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 when candidate is already fresh", got)
	}
}

func TestRefreshDueOnceDoesNothingOutsideActiveWindow(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{
		AuthID:        "stale",
		AuthIndex:     "idx-stale",
		Provider:      "codex",
		LastSuccessAt: now.Add(-6 * time.Hour),
	})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "stale", AuthIndex: "idx-stale", Provider: "codex"}},
	}

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want 0 outside active window", got)
	}
}

func TestRefreshOnceRecordsCodexAuthScanCount(t *testing.T) {
	now := time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "openai-1", AuthIndex: "idx-openai", Provider: "openai"},
		},
	}
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if snapshot.CodexAuthCount != 0 || !snapshot.LastAuthScanAt.Equal(now) {
		t.Fatalf("auth scan = count %d at %s, want 0 at %s", snapshot.CodexAuthCount, snapshot.LastAuthScanAt, now)
	}
}

func TestRefreshOnceDoesNothingBeforeAdmission(t *testing.T) {
	host := &fakeHostClient{authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", Provider: "codex"}}}
	refresher := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if host.listCallCount() != 0 {
		t.Fatalf("ListAuths calls = %d", host.listCallCount())
	}
}

func TestRefreshOnceFiltersMixedTierAndCountsOnlyAdmitted(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	lowToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-low"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`),
			"idx-low":  json.RawMessage(`{"access_token":"access-low","id_token":"` + lowToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"high": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("HTTP calls = %d, want quota/reset calls for admitted high only", got)
	}
	snapshot := store.Snapshot(now)
	if snapshot.CodexAuthCount != 1 {
		t.Fatalf("CodexAuthCount = %d, want 1", snapshot.CodexAuthCount)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "high" || snapshot.Accounts[0].Priority != 9 {
		t.Fatalf("accounts = %#v, want admitted high at CPA priority 9", snapshot.Accounts)
	}
}

func TestFilterAdmittedAuthsMixedTier(t *testing.T) {
	auths := []pluginapi.HostAuthFileEntry{
		{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
		{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
	}
	admission := CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"high": {}}}
	filtered := filterAdmittedAuths(auths, admission)
	if len(filtered) != 1 || filtered[0].ID != "high" {
		t.Fatalf("filtered = %#v, want only admitted high", filtered)
	}
}

func TestRefreshOnceDoesNotCrossAdmissionGenerationDuringList(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	bToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-b"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "a", AuthIndex: "idx-a", Provider: "codex"},
			{ID: "b", AuthIndex: "idx-b", Provider: "codex"},
		},
		authJSON:    map[string]json.RawMessage{"idx-b": json.RawMessage(`{"access_token":"access-b","id_token":"` + bToken + `"}`)},
		httpBody:    []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		listStarted: make(chan struct{}, 1),
		releaseList: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	done := make(chan error, 1)
	go func() { done <- refresher.RefreshOnce() }()
	select {
	case <-host.listStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale ListAuths call")
	}
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	close(host.releaseList)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 after admission generation changed", got)
	}
	snapshot := store.Snapshot(now)
	if !snapshot.LastAuthScanAt.IsZero() {
		t.Fatalf("LastAuthScanAt = %s, want no stale scan mutation", snapshot.LastAuthScanAt)
	}
}

func TestRefreshDueOnceDoesNotCrossAdmissionGenerationDuringList(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	bToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-b"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "a", AuthIndex: "idx-a", Provider: "codex"},
			{ID: "b", AuthIndex: "idx-b", Provider: "codex"},
		},
		authJSON:    map[string]json.RawMessage{"idx-b": json.RawMessage(`{"access_token":"access-b","id_token":"` + bToken + `"}`)},
		httpBody:    []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		listStarted: make(chan struct{}, 1),
		releaseList: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	store.RecordCodexActivity(now)
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	done := make(chan error, 1)
	go func() { done <- refresher.RefreshDueOnce() }()
	select {
	case <-host.listStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active refresh ListAuths call")
	}
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	close(host.releaseList)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 after admission generation changed", got)
	}
}

func TestAdmissionChangeDuringListFailureDoesNotLogStaleRefresh(t *testing.T) {
	host := &fakeHostClient{
		listErr:     errors.New("list failed"),
		listStarted: make(chan struct{}, 1),
		releaseList: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, time.Now)
	released := false
	t.Cleanup(func() {
		if !released {
			close(host.releaseList)
		}
	})

	refresher.RefreshSoon()
	select {
	case <-host.listStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale ListAuths call")
	}
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	close(host.releaseList)
	released = true
	refresher.wg.Wait()

	if got := host.logCallCount(); got != 0 {
		t.Fatalf("host log calls = %d, want no stale refresh log", got)
	}
}

func TestRefreshDueOnceFiltersMixedTier(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	lowToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-low"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`),
			"idx-low":  json.RawMessage(`{"access_token":"access-low","id_token":"` + lowToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 7, AuthIDs: map[string]struct{}{"high": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })

	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatal(err)
	}
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("HTTP calls = %d, want quota/reset calls for admitted high only", got)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "high" || snapshot.Accounts[0].Priority != 7 {
		t.Fatalf("accounts = %#v, want only high at CPA priority 7", snapshot.Accounts)
	}
}

func TestRefreshAuthDirectlyRejectsExcludedAuth(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	lowToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-low"})
	auth := pluginapi.HostAuthFileEntry{ID: "low", AuthIndex: "idx-low", Provider: "codex"}
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{auth},
		authJSON: map[string]json.RawMessage{"idx-low": json.RawMessage(`{"access_token":"access-low","id_token":"` + lowToken + `"}`)},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"high": {}}})
	NewQuotaRefresher(host, store, func() time.Time { return now }).refreshAuth(auth)

	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 for direct excluded refresh", got)
	}
	if accounts := store.Snapshot(now).Accounts; len(accounts) != 0 {
		t.Fatalf("accounts = %#v, want no excluded account", accounts)
	}
}

func TestRefreshAuthDiscardsInFlightSuccessAfterPriorityGenerationChange(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	host := &fakeHostClient{
		authList:  []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 1}},
		authJSON:  map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","id_token":"` + token + `"}`)},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 1),
		releaseDo: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	done := make(chan error, 1)
	go func() { done <- refresher.RefreshOnce() }()
	<-host.doStarted
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	<-replaceDone
	close(host.releaseDo)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if accounts := store.Snapshot(now).Accounts; len(accounts) != 0 {
		t.Fatalf("stale generation committed accounts: %#v", accounts)
	}
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "a")
	if account.Priority != 9 {
		t.Fatalf("Priority = %d, want current CPA priority 9", account.Priority)
	}
}

func TestRefreshAuthDiscardsInFlightFailureAfterExclusion(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	host := &fakeHostClient{
		authList:   []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex"}},
		authJSON:   map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","id_token":"` + token + `"}`)},
		httpStatus: http.StatusForbidden,
		httpBody:   []byte(`{"error":"forbidden"}`),
		doStarted:  make(chan struct{}, 1),
		releaseDo:  make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	done := make(chan error, 1)
	go func() { done <- refresher.RefreshOnce() }()
	<-host.doStarted
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	<-replaceDone
	close(host.releaseDo)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 0 {
		t.Fatalf("stale failure reinserted account: %#v", snapshot.Accounts)
	}
	for _, entry := range snapshot.Logs {
		if entry.Event == "quota.refresh_failed" {
			t.Fatalf("stale failure log committed: %#v", entry)
		}
	}
}

func TestAdmissionChangeAfterQuotaPreventsResetCreditsRequest(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	quotaRelease := make(chan struct{})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","id_token":"` + token + `"}`)},
		responseByURL: map[string]pluginapi.HTTPResponse{
			chatGPTQuotaEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`)},
			resetCreditsEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"available_count":0}`)},
		},
		startedByURL: make(chan string, 4),
		releaseByURL: map[string]chan struct{}{chatGPTQuotaEndpoint: quotaRelease},
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- refresher.RefreshOnce() }()
	waitForStartedURL(t, host.startedByURL, chatGPTQuotaEndpoint)
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	<-replaceDone
	close(quotaRelease)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if urls := strings.Join(host.requestedURLs(), "\n"); strings.Contains(urls, resetCreditsEndpoint) {
		t.Fatalf("stale refresh started reset credits request:\n%s", urls)
	}
}

func TestAdmissionChangeAfterResetCreditsPreventsProbeRequest(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(10 * time.Minute)
	seconds := int64(fiveHourSeconds)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	resetRelease := make(chan struct{})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","id_token":"` + token + `"}`)},
		responseByURL: map[string]pluginapi.HTTPResponse{
			chatGPTQuotaEndpoint:    {StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)},
			resetCreditsEndpoint:    {StatusCode: http.StatusOK, Body: []byte(`{"available_count":0}`)},
			codexResetProbeEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)},
		},
		startedByURL: make(chan string, 8),
		releaseByURL: map[string]chan struct{}{resetCreditsEndpoint: resetRelease},
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{
		AuthID: "a", AuthIndex: "idx-a", Provider: "codex", LastSuccessAt: resetAt.Add(-time.Hour),
		Quota:       ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt}},
		ResetProbes: map[WindowKind]ResetProbeState{WindowFiveHour: {WindowKind: WindowFiveHour, WindowSeconds: fiveHourSeconds, ResetAt: resetAt, NextCheckAt: now, Status: ResetProbeStatusPending}},
	})
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- refresher.RefreshOnce() }()
	waitForStartedURL(t, host.startedByURL, resetCreditsEndpoint)
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	<-replaceDone
	close(resetRelease)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if urls := strings.Join(host.requestedURLs(), "\n"); strings.Contains(urls, codexResetProbeEndpoint) {
		t.Fatalf("stale refresh started reset probe request:\n%s", urls)
	}
}

func TestAdmissionChangeDuringTokenRefreshPreventsSaveAuth(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	tokenRelease := make(chan struct{})
	host := &fakeHostClient{
		authList:      []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Name: "a.json", Provider: "codex"}},
		authJSON:      map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"old","refresh_token":"refresh","id_token":"` + idToken + `","account_id":"acct-a","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`)},
		responseByURL: map[string]pluginapi.HTTPResponse{codexTokenEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"access_token":"new","refresh_token":"new-refresh","id_token":"` + idToken + `","expires_in":3600}`)}},
		startedByURL:  make(chan string, 4),
		releaseByURL:  map[string]chan struct{}{codexTokenEndpoint: tokenRelease},
		saveStarted:   make(chan struct{}, 1),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- refresher.RefreshOnce() }()
	waitForStartedURL(t, host.startedByURL, codexTokenEndpoint)
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	<-replaceDone
	close(tokenRelease)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.saveStarted:
		t.Fatal("SaveAuth started after token refresh lost admission")
	default:
	}
	if len(host.saved) != 0 {
		t.Fatalf("saved auths = %#v, want none", host.saved)
	}
}

func TestSaveAuthInProgressDoesNotBlockAdmissionReplacement(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	host := &fakeHostClient{
		authList:      []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Name: "a.json", Provider: "codex"}},
		authJSON:      map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"old","refresh_token":"refresh","id_token":"` + idToken + `","account_id":"acct-a","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`)},
		responseByURL: map[string]pluginapi.HTTPResponse{codexTokenEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"access_token":"new","refresh_token":"new-refresh","id_token":"` + idToken + `","expires_in":3600}`)}},
		saveStarted:   make(chan struct{}, 1),
		releaseSave:   make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"a": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- refresher.RefreshOnce() }()
	<-host.saveStarted
	replaceDone := replaceAdmissionAsync(store, CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"b": {}}})
	select {
	case <-replaceDone:
	case <-time.After(time.Second):
		close(host.releaseSave)
		<-refreshDone
		t.Fatal("admission replacement blocked behind SaveAuth")
	}
	close(host.releaseSave)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	<-replaceDone
}

func TestRefreshOneRejectsOutsideAdmission(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"high": {}}})
	err := NewQuotaRefresher(&fakeHostClient{}, store, time.Now).RefreshOneAuthID("low")
	if err == nil || !strings.Contains(err.Error(), "outside the active CPA priority tier") {
		t.Fatalf("error = %v", err)
	}
}

func TestStartDoesNotPollAllAccountsWhileIdle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 20 * time.Millisecond
	store := NewPluginState(cfg)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
	}
	refresher := NewQuotaRefresher(host, store, time.Now)

	refresher.Start()
	defer refresher.Stop()
	time.Sleep(80 * time.Millisecond)

	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want 0 while idle", got)
	}
}

func TestStartWakesAtRetryDueBeforeRefreshInterval(t *testing.T) {
	now := time.Now()
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = time.Hour
	cfg.RefreshRetryDelays = []time.Duration{20 * time.Millisecond}
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "temporary failure", now)

	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	refresher := newAdmittedQuotaRefresherForTest(host, store, time.Now)

	refresher.Start()
	defer refresher.Stop()

	if !waitUntil(500*time.Millisecond, func() bool { return host.listCallCount() > 0 }) {
		t.Fatalf("ListAuths calls = %d, want retry wake before quota_refresh_interval", host.listCallCount())
	}
}

func TestStartRecomputesDelayAfterAsyncRefreshSchedulesRetry(t *testing.T) {
	now := time.Now()
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = time.Hour
	cfg.RefreshRetryDelays = []time.Duration{20 * time.Millisecond}
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)

	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusForbidden,
		httpBody:   []byte(`{"error":"temporary"}`),
	}
	refresher := newAdmittedQuotaRefresherForTest(host, store, time.Now)
	refresher.Start()
	defer refresher.Stop()

	time.Sleep(30 * time.Millisecond)
	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want loop sleeping on quota_refresh_interval before async refresh", got)
	}

	req := pluginapi.SchedulerPickRequest{
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "auth-1", Provider: "codex", Priority: 10}},
	}
	admission, ok := HighestPriorityCodexAdmission(req)
	if !ok {
		t.Fatal("candidate admission missing")
	}
	version := store.ReplaceCPAAdmission(admission)
	refresher.RefreshDueCandidatesSoon(req, version)
	if !waitUntil(700*time.Millisecond, func() bool { return host.listCallCount() >= 2 }) {
		t.Fatalf("ListAuths calls = %d, want async refresh to wake loop for scheduled retry", host.listCallCount())
	}
}

func TestStartDormantDoesNotRefreshOrBusyLoop(t *testing.T) {
	now := time.Now()
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = time.Hour
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now.Add(-6 * time.Hour)
	store.UpsertQuota(account)

	host := &fakeHostClient{}
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 5, AuthIDs: map[string]struct{}{"auth-1": {}}})
	refresher := NewQuotaRefresher(host, store, time.Now)
	refresher.Start()
	defer refresher.Stop()

	time.Sleep(150 * time.Millisecond)
	if got := host.listCallCount(); got != 0 {
		t.Fatalf("ListAuths calls = %d, want dormant normal refresh to remain stopped", got)
	}
}

func TestRefreshDueSuccessCompletesCircuitProbeWithoutRequeue(t *testing.T) {
	now := time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.CircuitHalfOpenSuccessThreshold = 2
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.AuthIndex = "idx-1"
	account.Circuit = CircuitBreakerState{
		State:        CircuitStateOpen,
		FailureCount: 1,
		NextProbeAt:  now.Add(-time.Minute),
	}
	store.UpsertQuota(account)

	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want one account", snapshot.Accounts)
	}
	updated := snapshot.Accounts[0]
	if updated.Circuit.State != CircuitStateHalfOpen || !updated.Circuit.NextProbeAt.IsZero() {
		t.Fatalf("circuit = %#v, want half-open with cleared probe time", updated.Circuit)
	}
	if due, reason := accountRefreshDue(updated, cfg, now); due || reason == "circuit_probe_due" {
		t.Fatalf("accountRefreshDue = %t, %q; want no repeated circuit probe", due, reason)
	}
}

func TestRefreshOnceLoadsCodexAuthAndQuota(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Email: "a@example.com", Priority: 7,
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].ChatGPTAccountID != "acct-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
	if snapshot.Accounts[0].LastError != "" {
		t.Fatalf("LastError = %q, want empty", snapshot.Accounts[0].LastError)
	}
	if snapshot.Accounts[0].LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt is zero, want successful refresh")
	}
	if snapshot.Accounts[0].Quota.FiveHour == nil {
		t.Fatalf("FiveHour quota is nil, want parsed quota")
	}
}

func TestRefreshOnceRefreshesExpiredAccessTokenAndSavesAuth(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	oldIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	newIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	quotaURL := "https://chatgpt.com/backend-api/wham/usage"
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Name: "codex-auth-1.json", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"old-access","refresh_token":"old-refresh","id_token":"` + oldIDToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		},
		expectedAuthByAccount: map[string]string{
			"acct-1": "Bearer new-access",
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint: {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"access_token":"new-access","refresh_token":"new-refresh","id_token":"` + newIDToken + `","expires_in":3600}`),
			},
			quotaURL: {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
			},
			resetCreditsEndpoint: {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"available_count":0,"credits":[]}`),
			},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	host.assertNoHeaderErrors(t)
	saved := host.saved["codex-auth-1.json"]
	if len(saved) == 0 {
		t.Fatalf("saved auth is empty, want refreshed credential saved")
	}
	if !strings.Contains(string(saved), "new-access") || !strings.Contains(string(saved), "new-refresh") || strings.Contains(string(saved), "old-access") {
		t.Fatalf("saved auth JSON = %s, want new tokens without old access token", saved)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError != "" || account.LastSuccessAt.IsZero() {
		t.Fatalf("account refresh state = %#v, want success", account)
	}
}

func TestRefreshTokenFailure401MarksAuthFailure(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	oldIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"old-access","refresh_token":"old-refresh","id_token":"` + oldIDToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint: {StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":"invalid_grant"}`)},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if !account.Refresh.AuthFailure {
		t.Fatalf("AuthFailure = false, want true: %#v", account.Refresh)
	}
	if want := now.Add(DefaultConfig().AuthFailureRetryInterval); !account.Refresh.NextRetryAt.Equal(want) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, want)
	}
}

func TestRefreshTokenFailure400InvalidGrantMarksAuthFailure(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	oldIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"old-access","refresh_token":"old-refresh","id_token":"` + oldIDToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint: {StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid_grant"}`)},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if !account.Refresh.AuthFailure {
		t.Fatalf("AuthFailure = false, want true: %#v", account.Refresh)
	}
	if want := now.Add(DefaultConfig().AuthFailureRetryInterval); !account.Refresh.NextRetryAt.Equal(want) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, want)
	}
}

func TestRefreshTokenFailure403SchedulesRetry(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	oldIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"old-access","refresh_token":"old-refresh","id_token":"` + oldIDToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint: {StatusCode: http.StatusForbidden, Body: []byte(`{"error":"forbidden"}`)},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.Refresh.AuthFailure {
		t.Fatalf("AuthFailure = true, want false: %#v", account.Refresh)
	}
	if !account.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, now.Add(time.Minute))
	}
}

func TestAuthFailureAccountRetriesAfterBackoffIntervalAndRecovers(t *testing.T) {
	start := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	oldIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	newIDToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	clock := start
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"old-access","refresh_token":"old-refresh","id_token":"` + oldIDToken + `","account_id":"acct-1","expired":"` + start.Add(-time.Hour).Format(time.RFC3339) + `"}`),
		},
		expectedAuthByAccount: map[string]string{"acct-1": "Bearer new-access"},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint: {StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":"invalid_grant"}`)},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return clock })
	store.RecordCodexActivity(start)

	// The 401 token refresh parks the account with the low-frequency backoff.
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(start), "auth-1")
	if !account.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false after 401, want true")
	}

	// The operator re-logs in: the auth file now carries fresh credentials.
	host.authJSON["idx-1"] = json.RawMessage(`{"access_token":"new-access","refresh_token":"new-refresh","id_token":"` + newIDToken + `","account_id":"acct-1"}`)
	host.responseByURL = nil
	host.httpBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)

	// Before the backoff deadline the parked account is not refreshed.
	clock = start.Add(29 * time.Minute)
	store.RecordCodexActivity(clock)
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce before backoff returned error: %v", err)
	}
	if account := accountByAuthID(t, store.Snapshot(clock), "auth-1"); !account.Refresh.AuthFailure {
		t.Fatal("AuthFailure cleared before backoff deadline, want still parked")
	}

	// Past the deadline the retry runs; the successful refresh recovers.
	clock = start.Add(31 * time.Minute)
	store.RecordCodexActivity(clock)
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce after backoff returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)
	account = accountByAuthID(t, store.Snapshot(clock), "auth-1")
	if account.Refresh.AuthFailure {
		t.Fatalf("AuthFailure still true after successful retry: %#v", account.Refresh)
	}
	if !account.LastSuccessAt.Equal(clock) {
		t.Fatalf("LastSuccessAt = %s, want %s", account.LastSuccessAt, clock)
	}
}

func TestRefreshOnceLoadsResetCreditsFromDedicatedEndpoint(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	quotaURL := "https://chatgpt.com/backend-api/wham/usage"
	resetURL := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			quotaURL: {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
			},
			resetURL: {
				StatusCode: http.StatusOK,
				Body:       []byte(`{"available_count":2,"total_earned_count":3,"credits":[{"id":"credit-1","status":"available","granted_at":"2026-06-01T00:00:00Z","expires_at":"2026-07-01T00:00:00Z"}]}`),
			},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	urls := strings.Join(host.requestedURLs(), ",")
	if !strings.Contains(urls, quotaURL) || !strings.Contains(urls, resetURL) {
		t.Fatalf("requested URLs = %s", urls)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.Quota.ResetCreditsAvailableCount == nil || *account.Quota.ResetCreditsAvailableCount != 2 {
		t.Fatalf("available reset credits = %#v", account.Quota.ResetCreditsAvailableCount)
	}
	if account.Quota.ResetCreditsTotalEarnedCount == nil || *account.Quota.ResetCreditsTotalEarnedCount != 3 {
		t.Fatalf("total reset credits = %#v", account.Quota.ResetCreditsTotalEarnedCount)
	}
	if len(account.Quota.ResetCredits) != 1 || account.Quota.ResetCredits[0].ID != "credit-1" {
		t.Fatalf("reset credits = %#v", account.Quota.ResetCredits)
	}
}

func TestRefreshOneAuthIDOnlyRefreshesRequestedAccount(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken1 := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	idToken2 := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-2"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"},
			{ID: "auth-2", AuthIndex: "idx-2", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken1 + `"}`),
			"idx-2": json.RawMessage(`{"access_token":"access-2","id_token":"` + idToken2 + `"}`),
		},
		expectedAuthByAccount: map[string]string{
			"acct-2": "Bearer access-2",
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 0, AuthIDs: map[string]struct{}{"auth-2": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })

	if err := refresher.RefreshOneAuthID("auth-2"); err != nil {
		t.Fatalf("RefreshOneAuthID returned error: %v", err)
	}

	host.assertNoHeaderErrors(t)
	if calls := host.httpCallCount(); calls != 2 {
		t.Fatalf("http calls = %d, want 2", calls)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "auth-2" {
		t.Fatalf("accounts = %#v, want only auth-2", snapshot.Accounts)
	}
}

func TestRefreshOnceSkipsIneligibleAuthEntries(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "disabled", AuthIndex: "idx-disabled", Provider: "codex", Disabled: true},
			{ID: "unavailable", AuthIndex: "idx-unavailable", Provider: "codex", Unavailable: true},
			{ID: "other", AuthIndex: "idx-other", Provider: "openai"},
			{ID: "missing-index", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	if got := host.httpCalls; got != 0 {
		t.Fatalf("http calls = %d, want 0", got)
	}
}

func TestRefreshOnceRecordsRedactedErrorWithoutTokenLeak(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"secret-access-1","id_token":"bad-token"}`),
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].LastError == "" {
		t.Fatalf("LastError empty, want redacted error")
	}
	if strings.Contains(snapshot.Accounts[0].LastError, "secret-access-1") {
		t.Fatalf("LastError leaked token: %q", snapshot.Accounts[0].LastError)
	}
}

func TestRefreshOnceMissingAccessTokenRecordsErrorWithoutQuotaSuccess(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1", "nonce": "token-ish-claim"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError == "" {
		t.Fatalf("LastError empty, want missing access_token error")
	}
	if strings.Contains(account.LastError, idToken) || strings.Contains(account.LastError, "token-ish-claim") {
		t.Fatalf("LastError leaked token-ish content: %q", account.LastError)
	}
	if !account.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt = %v, want zero", account.LastSuccessAt)
	}
	if account.Quota.FiveHour != nil {
		t.Fatalf("FiveHour quota = %#v, want nil", account.Quota.FiveHour)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("http calls = %d, want 0", got)
	}
}

func TestRefreshOnceMissingChatGPTAccountIDRecordsErrorWithoutQuotaSuccess(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"sub": "user-1", "nonce": "token-ish-claim"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"secret-access-missing-account","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError != "" {
		t.Fatalf("unresolved identity was treated as refresh failure: %q", account.LastError)
	}
	if !account.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt = %v, want zero", account.LastSuccessAt)
	}
	if account.Quota.FiveHour != nil {
		t.Fatalf("FiveHour quota = %#v, want nil", account.Quota.FiveHour)
	}
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("http calls = %d, want 0", got)
	}
}

func TestRefreshOnceHTTPNon2xxRecordsRedactedErrorWithoutQuotaSuccess(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusUnauthorized,
		httpBody:   []byte(`upstream rejected access-1`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError == "" {
		t.Fatalf("LastError empty, want HTTP status error")
	}
	if strings.Contains(account.LastError, "access-1") {
		t.Fatalf("LastError leaked token: %q", account.LastError)
	}
	if !account.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt = %v, want zero", account.LastSuccessAt)
	}
	if account.Quota.FiveHour != nil {
		t.Fatalf("FiveHour quota = %#v, want nil", account.Quota.FiveHour)
	}
}

func TestRefreshFailure401MarksAuthFailureWithoutRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusUnauthorized,
		httpBody:   []byte(`{"error":"expired"}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.Refresh.LastFailureKind != RefreshFailureAuth {
		t.Fatalf("LastFailureKind = %q, want auth", account.Refresh.LastFailureKind)
	}
	if !account.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if want := now.Add(DefaultConfig().AuthFailureRetryInterval); !account.Refresh.NextRetryAt.Equal(want) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, want)
	}
}

func TestRefreshFailure403SchedulesTransientRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusForbidden,
		httpBody:   []byte(`{"error":"forbidden"}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.Refresh.LastFailureKind != RefreshFailureTransient {
		t.Fatalf("LastFailureKind = %q, want transient", account.Refresh.LastFailureKind)
	}
	if account.Refresh.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
	if !account.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, now.Add(time.Minute))
	}
}

func TestRefreshOnceFailurePreservesPriorSuccessfulQuota(t *testing.T) {
	firstNow := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	secondNow := firstNow.Add(2 * time.Minute)
	currentNow := firstNow
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return currentNow })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("first RefreshOnce returned error: %v", err)
	}
	before := accountByAuthID(t, store.Snapshot(firstNow), "auth-1")
	if before.Quota.FiveHour == nil || before.LastSuccessAt.IsZero() || before.Family != AccountFamilyWeekly {
		t.Fatalf("initial refresh did not populate quota: %#v", before)
	}

	currentNow = secondNow
	host.httpStatus = http.StatusUnauthorized
	host.httpBody = []byte(`{"Authorization":"Bearer leaked-token","Cookie":"session=leaked-cookie","access_token" : "leaked-access"}`)
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("second RefreshOnce returned error: %v", err)
	}

	after := accountByAuthID(t, store.Snapshot(secondNow), "auth-1")
	if after.LastError == "" {
		t.Fatalf("LastError empty, want failed refresh metadata")
	}
	for _, leaked := range []string{"leaked-token", "leaked-cookie", "leaked-access"} {
		if strings.Contains(after.LastError, leaked) {
			t.Fatalf("LastError leaked %q: %q", leaked, after.LastError)
		}
	}
	if after.LastSuccessAt != before.LastSuccessAt {
		t.Fatalf("LastSuccessAt = %v, want preserved %v", after.LastSuccessAt, before.LastSuccessAt)
	}
	if after.Family != before.Family {
		t.Fatalf("Family = %q, want preserved %q", after.Family, before.Family)
	}
	if after.Quota.FiveHour == nil || before.Quota.FiveHour == nil || *after.Quota.FiveHour.UsedPercent != *before.Quota.FiveHour.UsedPercent {
		t.Fatalf("quota not preserved after failure: before=%#v after=%#v", before.Quota.FiveHour, after.Quota.FiveHour)
	}
}

func TestRefreshOnceNon2xxStoresBoundedSanitizedSummary(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	longBody := strings.Repeat("x", 400)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusForbidden,
		httpBody: []byte(`{
			"Authorization": "Bearer json-secret",
			"Cookie": "session=json-cookie",
			"ACCESS_TOKEN" : "json-access",
			"id_token": "json-id-token",
			"raw": "Authorization: Bearer raw-secret Cookie: session=raw-cookie access_token = raw-access Cookie: session=secret-session; csrf=secret-csrf",
			"detail": "` + longBody + `"
		}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError == "" {
		t.Fatal("LastError empty, want non-2xx summary")
	}
	for _, leaked := range []string{"json-secret", "json-cookie", "json-access", "json-id-token", "raw-secret", "raw-cookie", "raw-access", "secret-session", "secret-csrf"} {
		if strings.Contains(account.LastError, leaked) {
			t.Fatalf("LastError leaked %q: %q", leaked, account.LastError)
		}
	}
	if strings.Contains(account.LastError, longBody) {
		t.Fatalf("LastError contains raw long body: %q", account.LastError)
	}
	if len(account.LastError) > 360 {
		t.Fatalf("LastError length = %d, want bounded summary: %q", len(account.LastError), account.LastError)
	}
}

func TestRedactSecretsRedactsFullRawCookieHeader(t *testing.T) {
	redacted := redactSecrets("Cookie: session=secret-session; csrf=secret-csrf")
	for _, leaked := range []string{"secret-session", "secret-csrf"} {
		if strings.Contains(redacted, leaked) {
			t.Fatalf("redactSecrets leaked %q: %q", leaked, redacted)
		}
	}
}

func TestRefreshOnceContinuesAfterOneAccountFails(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken2 := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-2"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "auth-fail", AuthIndex: "idx-fail", Provider: "codex"},
			{ID: "auth-ok", AuthIndex: "idx-ok", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-fail": json.RawMessage(`{"access_token":"secret-access-fail","id_token":"bad-token"}`),
			"idx-ok":   json.RawMessage(`{"access_token":"access-2","id_token":"` + idToken2 + `"}`),
		},
		expectedAuthByAccount: map[string]string{
			"acct-2": "Bearer access-2",
		},
		responseByAccount: map[string]pluginapi.HTTPResponse{
			"acct-2": {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
			},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)

	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(snapshot.Accounts))
	}
	failed := accountByAuthID(t, snapshot, "auth-fail")
	if failed.LastError == "" {
		t.Fatalf("failed account LastError empty")
	}
	if strings.Contains(failed.LastError, "secret-access-fail") {
		t.Fatalf("failed account LastError leaked token: %q", failed.LastError)
	}
	success := accountByAuthID(t, snapshot, "auth-ok")
	if success.LastError != "" {
		t.Fatalf("success account LastError = %q, want empty", success.LastError)
	}
	if success.LastSuccessAt.IsZero() || success.Quota.FiveHour == nil {
		t.Fatalf("success account was not refreshed: %#v", success)
	}
}

func TestRefreshOnceHonorsMaxRefreshConcurrency(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	auths := []pluginapi.HostAuthFileEntry{}
	authJSON := map[string]json.RawMessage{}
	expected := map[string]string{}
	responses := map[string]pluginapi.HTTPResponse{}
	for i := 1; i <= 5; i++ {
		authID := fmt.Sprintf("auth-%d", i)
		authIndex := fmt.Sprintf("idx-%d", i)
		accountID := fmt.Sprintf("acct-%d", i)
		accessToken := fmt.Sprintf("access-%d", i)
		idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": accountID})
		auths = append(auths, pluginapi.HostAuthFileEntry{
			ID: authID, AuthIndex: authIndex, Provider: "codex",
		})
		authJSON[authIndex] = json.RawMessage(`{"access_token":"` + accessToken + `","id_token":"` + idToken + `"}`)
		expected[accountID] = "Bearer " + accessToken
		responses[accountID] = pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{},
			Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		}
	}
	host := &fakeHostClient{
		authList:              auths,
		authJSON:              authJSON,
		expectedAuthByAccount: expected,
		responseByAccount:     responses,
		doStarted:             make(chan struct{}, len(auths)),
		releaseDo:             make(chan struct{}),
	}
	cfg := DefaultConfig()
	cfg.MaxRefreshConcurrency = 2
	store := NewPluginState(cfg)
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	done := make(chan error, 1)
	go func() {
		done <- refresher.RefreshOnce()
	}()

	for i := 0; i < cfg.MaxRefreshConcurrency; i++ {
		select {
		case <-host.doStarted:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for HTTP call %d to start", i+1)
		}
	}
	if got := host.activeHTTPCount(); got != cfg.MaxRefreshConcurrency {
		t.Fatalf("active HTTP = %d, want %d concurrent calls", got, cfg.MaxRefreshConcurrency)
	}
	select {
	case <-host.doStarted:
		t.Fatalf("third HTTP call started before a concurrency slot was released")
	case <-time.After(50 * time.Millisecond):
	}

	for i := 0; i < len(auths); i++ {
		host.releaseDo <- struct{}{}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RefreshOnce returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RefreshOnce to finish")
	}

	if got := host.maxActiveHTTP(); got != cfg.MaxRefreshConcurrency {
		t.Fatalf("max active HTTP = %d, want %d", got, cfg.MaxRefreshConcurrency)
	}
	if got := host.httpCallCount(); got != len(auths)*2 {
		t.Fatalf("http calls = %d, want %d", got, len(auths)*2)
	}
	host.assertNoHeaderErrors(t)
}

func TestRefreshSoonDoesNotOverlapRefreshes(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody:   []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted:  make(chan struct{}, 1),
		releaseDo:  make(chan struct{}),
		httpStatus: http.StatusOK,
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	refresher.RefreshSoon()
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first refresh to start")
	}
	for i := 0; i < 10; i++ {
		refresher.RefreshSoon()
	}
	if !waitUntil(time.Second, func() bool { return host.httpCallCount() == 1 }) {
		t.Fatalf("http calls = %d, want 1 while refresh is active", host.httpCallCount())
	}
	host.releaseDo <- struct{}{}
	if !waitUntil(time.Second, func() bool { return host.activeHTTPCount() == 0 }) {
		t.Fatalf("active HTTP = %d, want 0 after releasing request", host.activeHTTPCount())
	}
	if host.maxActiveHTTP() != 1 {
		t.Fatalf("max active HTTP = %d, want 1", host.maxActiveHTTP())
	}
	host.assertNoHeaderErrors(t)
}

func TestRefreshDueCandidatesSoonKeepsNewestAdmissionWhenOlderCallbackArrivesLast(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	bToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-b"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "b", AuthIndex: "idx-b", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-b": json.RawMessage(`{"access_token":"access-b","id_token":"` + bToken + `"}`),
		},
		httpBody:    []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		listStarted: make(chan struct{}, 1),
		releaseList: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	requestA := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex", Priority: 1}}}
	requestB := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex", Priority: 2}}}
	admissionA, _ := HighestPriorityCodexAdmission(requestA)
	admissionB, _ := HighestPriorityCodexAdmission(requestB)
	versionA := store.ReplaceCPAAdmission(admissionA)
	store.RecordCodexActivity(now)
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	released := false
	t.Cleanup(func() {
		if !released {
			close(host.releaseList)
		}
	})

	refresher.RefreshSoon()
	select {
	case <-host.listStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active refresh ListAuths call")
	}
	versionB := store.ReplaceCPAAdmission(admissionB)
	refresher.RefreshDueCandidatesSoon(requestB, versionB)
	refresher.RefreshDueCandidatesSoon(requestA, versionA)
	close(host.releaseList)
	released = true
	refresher.wg.Wait()

	if got := host.listCallCount(); got != 2 {
		t.Fatalf("ListAuths calls = %d, want newest admission callback retained", got)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "b" {
		t.Fatalf("accounts = %#v, want newest admission B refreshed", snapshot.Accounts)
	}
}

func TestRefreshOneSoonDoesNotOverlapRefreshes(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody:   []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted:  make(chan struct{}, 1),
		releaseDo:  make(chan struct{}),
		httpStatus: http.StatusOK,
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 0, AuthIDs: map[string]struct{}{"auth-1": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })

	refresher.RefreshOneSoon("auth-1")
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first refresh to start")
	}
	released := false
	defer func() {
		if !released {
			close(host.releaseDo)
		}
	}()
	for i := 0; i < 10; i++ {
		refresher.RefreshOneSoon("auth-1")
	}
	time.Sleep(50 * time.Millisecond)
	if got := host.httpCallCount(); got != 1 {
		t.Fatalf("http calls = %d, want 1 while refresh is active", got)
	}
	if got := host.maxActiveHTTP(); got != 1 {
		t.Fatalf("max active HTTP = %d, want 1 while refresh is active", got)
	}
	close(host.releaseDo)
	released = true
	if !waitUntil(time.Second, func() bool { return host.activeHTTPCount() == 0 }) {
		t.Fatalf("active HTTP = %d, want 0 after releasing request", host.activeHTTPCount())
	}
	host.assertNoHeaderErrors(t)
}

func TestStopWaitsForActiveRefreshSoon(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 1),
		releaseDo: make(chan struct{}),
	}
	store := NewPluginState(DefaultConfig())
	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })

	refresher.RefreshSoon()
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh to start")
	}

	stopped := make(chan struct{})
	go func() {
		refresher.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while refresh was still blocked")
	case <-time.After(50 * time.Millisecond):
	}
	host.releaseDo <- struct{}{}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after refresh completed")
	}
	host.assertNoHeaderErrors(t)
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}

func TestRefreshOneOverridesHostUnavailableAndRecoversTemporaryExhausted(t *testing.T) {
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-team"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "team", AuthIndex: "idx-team", Provider: "codex", Unavailable: true},
		},
		authJSON: map[string]json.RawMessage{
			"idx-team": json.RawMessage(`{"access_token":"access-team","id_token":"` + token + `"}`),
		},
		// Same-window full reading (reset stays at the marker's deadline, now+3h):
		// the automatic window-identity reconciliation deliberately declines, and
		// only the operator-requested refresh clears the marker.
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":10800},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:    "team",
		AuthIndex: "idx-team",
		Provider:  "codex",
		Quota: ParsedQuota{
			Family: AccountFamilyWeekly,
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: float64Ptr(100), Exhausted: true,
				ResetAt: now.Add(3 * time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(100), Exhausted: true,
				ResetAt: now.Add(72 * time.Hour)},
		},
		LastSuccessAt: now.Add(-2 * time.Hour),
	})
	store.MarkAccountTemporaryExhausted("team", now.Add(3*time.Hour), usageLimitReachedReason)

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOneAuthID("team"); err != nil {
		t.Fatalf("RefreshOneAuthID returned error: %v", err)
	}
	if host.httpCallCount() == 0 {
		t.Fatal("manual refresh made no upstream calls, want quota fetch despite Unavailable flag")
	}
	account := accountByAuthID(t, store.Snapshot(now), "team")
	if account.TemporaryExhausted {
		t.Fatalf("TemporaryExhausted still set after verified manual refresh: %#v", account)
	}
	if !account.TemporaryResetAt.IsZero() {
		t.Fatalf("TemporaryResetAt = %s, want zero", account.TemporaryResetAt)
	}
	logs := store.Snapshot(now).Logs
	var override, recovered bool
	for _, entry := range logs {
		if entry.Event == "quota.manual_refresh_override" {
			override = true
		}
		if entry.Event == "quota.temporary_recovered" {
			recovered = true
		}
	}
	if !override {
		t.Fatal("missing quota.manual_refresh_override log entry")
	}
	if !recovered {
		t.Fatal("missing quota.temporary_recovered log entry")
	}
}

func TestRefreshOneKeepsTemporaryExhaustedWhenFreshQuotaStillLimited(t *testing.T) {
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	token := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-k12"})
	// K12 negative control from issue #11: the generic quota endpoint reports a
	// full window while the account is still limited upstream.
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "k12", AuthIndex: "idx-k12", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-k12": json.RawMessage(`{"access_token":"access-k12","id_token":"` + token + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_reached":true,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:    "k12",
		AuthIndex: "idx-k12",
		Provider:  "codex",
		Quota: ParsedQuota{
			Family: AccountFamilyWeekly,
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: float64Ptr(100), Exhausted: true,
				ResetAt: now.Add(3 * time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: float64Ptr(50),
				ResetAt: now.Add(48 * time.Hour)},
		},
		LastSuccessAt: now.Add(-2 * time.Hour),
	})
	store.MarkAccountTemporaryExhausted("k12", now.Add(3*time.Hour), usageLimitReachedReason)

	refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
	if err := refresher.RefreshOneAuthID("k12"); err != nil {
		t.Fatalf("RefreshOneAuthID returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "k12")
	if !account.TemporaryExhausted {
		t.Fatal("TemporaryExhausted cleared although the fresh window still reports limit_reached")
	}
	for _, entry := range store.Snapshot(now).Logs {
		if entry.Event == "quota.temporary_recovered" {
			t.Fatal("unexpected quota.temporary_recovered log entry")
		}
	}
}

func TestRefreshOneStillRejectsStructurallyIneligibleAuth(t *testing.T) {
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "other", AuthIndex: "idx-other", Provider: "gemini"},
		},
	}
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 0, AuthIDs: map[string]struct{}{"other": {}}})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	err := refresher.RefreshOneAuthID("other")
	if err == nil || !strings.Contains(err.Error(), "not eligible for quota refresh") {
		t.Fatalf("err = %v, want not-eligible rejection for non-codex auth", err)
	}
}

func float64Ptr(v float64) *float64 { return &v }
