package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type s7TestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *s7TestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *s7TestClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type s7RosterHost struct {
	mu      sync.Mutex
	entries []RosterEntry
	err     error
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (h *s7RosterHost) ListHostAuths(ctx context.Context) ([]RosterEntry, error) {
	h.mu.Lock()
	h.calls++
	entries := append([]RosterEntry(nil), h.entries...)
	err := h.err
	entered, release := h.entered, h.release
	h.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return entries, err
}

func (h *s7RosterHost) result(entries []RosterEntry, err error) {
	h.mu.Lock()
	h.entries, h.err = append([]RosterEntry(nil), entries...), err
	h.mu.Unlock()
}

func (h *s7RosterHost) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func TestAutonomousDegradedExpiryAtRequestBoundaries(t *testing.T) {
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	clock := &s7TestClock{now: base}
	priority := 9
	entry := RosterEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: &priority}
	rosterHost := &s7RosterHost{entries: []RosterEntry{entry}}
	httpHost := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	state := NewPluginState(DefaultConfig())
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: priority, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx-a", Instance: legacyAuthInstanceID("a"), Provider: "codex", Priority: priority, LastSuccessAt: base})
	adapter := &rosterCredentialHost{host: httpHost, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(httpHost, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	c := NewRosterController(RosterControllerOptions{
		Host:    rosterHost,
		Now:     clock.Now,
		Observe: r.ObserveRosterLifecycle,
	})
	r.SetRosterLifecycleAuthority(c.EnforceLifecycle)

	if got, err := c.Startup(context.Background()); err != nil || got.Health != RosterHealthy {
		t.Fatalf("startup roster=%#v err=%v", got, err)
	}
	firstFailure := base.Add(time.Minute)
	clock.Set(firstFailure)
	rosterHost.result(nil, errors.New("host roster unavailable"))
	if got, err := c.WakeForManagement(context.Background()); err == nil || got.Health != RosterDegraded || !got.DegradedSince.Equal(firstFailure) {
		t.Fatalf("degraded roster=%#v err=%v", got, err)
	}

	r.probeController.SetWindow(legacyAuthInstanceID("a"), ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: firstFailure})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		at      time.Time
		allowed bool
	}{
		{"epsilon-before", firstFailure.Add(rosterDegradedLimit - time.Nanosecond), true},
		{"exact-boundary", firstFailure.Add(rosterDegradedLimit), false},
		{"epsilon-after", firstFailure.Add(rosterDegradedLimit + time.Nanosecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.Set(tc.at)
			if got := r.normalBackgroundAllowed(); got != tc.allowed {
				t.Fatalf("normal background allowed=%v want=%v", got, tc.allowed)
			}
		})
	}

	clock.Set(firstFailure.Add(rosterDegradedLimit + time.Nanosecond))
	for _, probe := range []bool{false, true} {
		if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/background"}, probe); !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("probe=%v err=%v", probe, err)
		}
	}
	if httpHost.http != 0 {
		t.Fatalf("expired degraded lifecycle started HTTP=%d", httpHost.http)
	}
	if got := c.Snapshot(); got.Health != RosterFailClosed || got.BackgroundAllowed || got.LifecycleRevision != 3 {
		t.Fatalf("controller did not durably fail closed once: %#v", got)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[legacyAuthInstanceID("a")][ProbeWindowFiveHour].State; got != ProbeWaitingRoster {
		t.Fatalf("persisted probe state=%s want=%s", got, ProbeWaitingRoster)
	}

	publishSchedulerState(state, map[string]struct{}{"a": {}}, clock.Now())
	decision := schedulerPickPublished(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}}, clock.Now())
	if decision.AuthID != "a" {
		t.Fatalf("real pick lost usable roster snapshot: %#v", decision)
	}

	clock.Set(firstFailure.Add(rosterDegradedLimit + time.Minute))
	rosterHost.result([]RosterEntry{entry}, nil)
	if got, err := c.WakeForManagement(context.Background()); err != nil || got.Health != RosterHealthy {
		t.Fatalf("recovery roster=%#v err=%v", got, err)
	}
	if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/recovered"}, false); err != nil {
		t.Fatal(err)
	}
	if httpHost.http != 1 {
		t.Fatalf("recovery HTTP calls=%d want=1", httpHost.http)
	}
}

func TestProductionSchedulerPickActivityIsAsyncBoundedAndWakesDormantRefresh(t *testing.T) {
	base := time.Now()
	clock := &s7TestClock{now: base}
	priority := 8
	entry := RosterEntry{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: &priority}
	rosterHost := &s7RosterHost{entries: []RosterEntry{entry}}
	state := NewPluginState(DefaultConfig())
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: priority, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx-a", Instance: legacyAuthInstanceID("a"), Provider: "codex", Priority: priority, LastSuccessAt: base})
	httpHost := &countingProductionHost{}
	r := NewQuotaRefresher(httpHost, state, clock.Now)
	c := NewRosterController(RosterControllerOptions{Host: rosterHost, Now: clock.Now, Observe: r.ObserveRosterLifecycle})
	if _, err := c.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Start()
	clock.Set(base.Add(rosterActiveTTL + time.Second))
	rosterHost.entered, rosterHost.release = make(chan struct{}), make(chan struct{})

	refresherMu.Lock()
	previousState, previousRefresher, previousController := globalState, globalRefresher, globalRosterController
	globalState, globalRefresher, globalRosterController = state, r, c
	refresherMu.Unlock()
	replaceGlobalPickActivityPump(c, r)
	publishSchedulerState(state, map[string]struct{}{"a": {}}, clock.Now())
	t.Cleanup(func() {
		cliproxyPluginShutdown()
		refresherMu.Lock()
		globalState, globalRefresher, globalRosterController = previousState, previousRefresher, previousController
		refresherMu.Unlock()
	})

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}})
	done := make(chan error, 1)
	go func() { _, err := handleSchedulerPick(raw); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("scheduler pick blocked on host roster synchronization")
	}
	select {
	case <-rosterHost.entered:
	case <-time.After(time.Second):
		t.Fatal("async pick activity never invoked roster WakeForActivity")
	}
	if got := r.refreshController.Mode(clock.Now()); got != RefreshModeDormant {
		t.Fatalf("refresh mode mutated before gated roster activity completed: %s", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = handleSchedulerPick(raw)
		}()
	}
	wg.Wait()
	close(rosterHost.release)
	if !waitForCondition(t, time.Second, func() bool { return r.refreshController.Mode(clock.Now()) == RefreshModeActive }) {
		r.refreshController.mu.Lock()
		lastActivity, mode := r.refreshController.lastActivity, r.refreshController.mode
		r.refreshController.mu.Unlock()
		pump := globalPickActivityPump.Load()
		var pending bool
		if pump != nil {
			pending = pump.latest.Load() != nil
		}
		t.Fatalf("async pick activity did not activate dormant refresh controller: roster=%#v allowed=%v calls=%d mode=%s last=%v pending=%v", r.runtimeRoster(), r.normalBackgroundAllowed(), rosterHost.callCount(), mode, lastActivity, pending)
	}
	if got := rosterHost.callCount(); got != 2 {
		t.Fatalf("concurrent picks were not bounded/coalesced: roster calls=%d want=2 (startup + one TTL sync)", got)
	}
	if !waitForCondition(t, time.Second, func() bool {
		snapshot := state.Snapshot(clock.Now())
		return !snapshot.LastCodexActivityAt.IsZero() && snapshot.LastSelected == "a"
	}) {
		t.Fatal("async pick observation did not update management activity state")
	}

	clock.Set(clock.Now().Add(rosterActiveTTL + time.Second))
	rosterHost.entered, rosterHost.release = make(chan struct{}), make(chan struct{})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rosterHost.entered:
	case <-time.After(time.Second):
		t.Fatal("shutdown fixture did not enter gated activity sync")
	}
	shutdownDone := make(chan struct{})
	go func() {
		cliproxyPluginShutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked on activity consumer")
	}
	for i := 0; i < 64; i++ {
		if _, err := handleSchedulerPick(raw); err != nil {
			t.Fatal(err)
		}
	}
}
