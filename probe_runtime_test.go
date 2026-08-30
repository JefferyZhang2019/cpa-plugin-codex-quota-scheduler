package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type sequenceProbeHost struct {
	mu            sync.Mutex
	auth          pluginapi.HostAuthGetResponse
	authByIndex   map[string]pluginapi.HostAuthGetResponse
	authFiles     []pluginapi.HostAuthFileEntry
	authReads     []pluginapi.HostAuthGetResponse
	quota         [][]byte
	urls          []string
	requests      []pluginapi.HTTPRequest
	quotaStatus   int
	probeStatus   int
	probeBody     []byte
	getErr        error
	doErrors      map[string][]error
	gets          int
	gateAuthIndex string
	getStarted    chan struct{}
	releaseGet    chan struct{}
	doStarted     chan struct{}
	releaseDo     chan struct{}
	afterDo       func(pluginapi.HTTPRequest)
	gateDoURL     string
}

type probeHandoffClock struct {
	mu      sync.Mutex
	now     time.Time
	armed   bool
	reached chan struct{}
	release chan struct{}
}

func (c *probeHandoffClock) Now() time.Time {
	c.mu.Lock()
	now := c.now
	if !c.armed {
		c.mu.Unlock()
		return now
	}
	c.armed = false
	reached := c.reached
	release := c.release
	c.mu.Unlock()
	close(reached)
	<-release
	return now
}

func (c *probeHandoffClock) ArmNextCall() (<-chan struct{}, chan<- struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.reached = make(chan struct{})
	c.release = make(chan struct{})
	return c.reached, c.release
}

type probeDeferredClock struct {
	mu      sync.Mutex
	now     time.Time
	armed   bool
	reached chan struct{}
	release chan struct{}
}

func (c *probeDeferredClock) Now() time.Time {
	c.mu.Lock()
	if !c.armed {
		now := c.now
		c.mu.Unlock()
		return now
	}
	c.armed = false
	reached := c.reached
	release := c.release
	c.mu.Unlock()
	close(reached)
	<-release
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	return now
}

func (c *probeDeferredClock) ArmNextCall() (<-chan struct{}, chan<- struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.reached = make(chan struct{})
	c.release = make(chan struct{})
	return c.reached, c.release
}

func (c *probeDeferredClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type probeNthCaptureClock struct {
	mu        sync.Mutex
	now       time.Time
	remaining int
	reached   chan struct{}
	release   chan struct{}
}

func (c *probeNthCaptureClock) Now() time.Time {
	c.mu.Lock()
	now := c.now
	if c.remaining <= 0 {
		c.mu.Unlock()
		return now
	}
	c.remaining--
	if c.remaining > 0 {
		c.mu.Unlock()
		return now
	}
	reached := c.reached
	release := c.release
	c.mu.Unlock()
	close(reached)
	<-release
	return now
}

func (c *probeNthCaptureClock) ArmNthCall(n int) (<-chan struct{}, chan<- struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remaining = n
	c.reached = make(chan struct{})
	c.release = make(chan struct{})
	return c.reached, c.release
}

func (c *probeNthCaptureClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func newProbeFixtureHost() *sequenceProbeHost {
	return &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{
		AuthIndex: "idx",
		Name:      "a.json",
		JSON:      json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`),
	}}
}

func TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	finger := NewCredentialFingerprint("acct", "r0", "idx")
	if _, err := store.Update(func(s *PersistentState) error {
		s.Bindings["a"] = RuntimeBinding{AuthID: "a", Instance: 1, Login: 4, Fingerprint: finger}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.MarkAuthBlocked("a"); err != nil {
		t.Fatal(err)
	}
	if err = registry.ObserveExternalLogin("a", 4, finger); err != nil {
		t.Fatal(err)
	}
	b, _ := registry.Lookup("a")
	if !b.AuthBlocked {
		t.Fatal("same LoginEpoch unlocked AuthBlocked")
	}
	if err = registry.ObserveExternalLogin("a", 5, finger); err != nil {
		t.Fatal(err)
	}
	b, _ = registry.Lookup("a")
	if b.AuthBlocked || b.Login != 5 {
		t.Fatalf("binding=%#v", b)
	}
}

func (h *sequenceProbeHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	if len(h.authFiles) > 0 {
		return append([]pluginapi.HostAuthFileEntry(nil), h.authFiles...), nil
	}
	return []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: 9}}, nil
}
func (h *sequenceProbeHost) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	h.mu.Lock()
	h.gets++
	auth, started, release, getErr := h.auth, h.getStarted, h.releaseGet, h.getErr
	if indexed, ok := h.authByIndex[authIndex]; ok {
		auth = indexed
	}
	if h.gateAuthIndex != "" && h.gateAuthIndex != authIndex {
		started, release = nil, nil
	}
	if len(h.authReads) > 0 {
		auth = h.authReads[0]
		h.authReads = h.authReads[1:]
	}
	h.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		<-release
	}
	return auth, getErr
}
func (h *sequenceProbeHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *sequenceProbeHost) Log(string, string, map[string]any)     {}
func (h *sequenceProbeHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	started, release, after := h.doStarted, h.releaseDo, h.afterDo
	if h.gateDoURL != "" {
		if req.URL != h.gateDoURL {
			started, release = nil, nil
		}
	} else if req.URL != codexResetProbeEndpoint {
		started, release = nil, nil
	}
	h.urls = append(h.urls, req.URL)
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		<-release
	}
	h.mu.Lock()
	var injected error
	if sequence := h.doErrors[req.URL]; len(sequence) > 0 {
		injected = sequence[0]
		h.doErrors[req.URL] = sequence[1:]
	}
	if injected != nil {
		h.mu.Unlock()
		if after != nil {
			after(req)
		}
		return pluginapi.HTTPResponse{}, injected
	}
	var response pluginapi.HTTPResponse
	if req.URL == codexResetProbeEndpoint {
		status := h.probeStatus
		if status == 0 {
			status = http.StatusOK
		}
		body := h.probeBody
		if len(body) == 0 {
			body = []byte(`{"usage":{"total_tokens":1}}`)
		}
		response = pluginapi.HTTPResponse{StatusCode: status, Body: body}
	} else if req.URL == resetCreditsEndpoint {
		response = pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}
	} else {
		body := h.quota[0]
		h.quota = h.quota[1:]
		status := h.quotaStatus
		if status == 0 {
			status = http.StatusOK
		}
		response = pluginapi.HTTPResponse{StatusCode: status, Body: body}
	}
	h.mu.Unlock()
	if after != nil {
		after(req)
	}
	return response, nil
}

func newDueProbeRuntime(t *testing.T, now time.Time, host *sequenceProbeHost) *QuotaRefresher {
	t.Helper()
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	base := ResetProbeBaseline(now.Add(-time.Hour), 0, fiveHourSeconds*time.Second)
	base.SuspectedLazy = true
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now, Baseline: base})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	return r
}

func newProductionLazyRefreshRuntime(t *testing.T, now time.Time, host *sequenceProbeHost) *QuotaRefresher {
	t.Helper()
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err := r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	return r
}

func waitForProbeHTTP(t *testing.T, host *sequenceProbeHost, want int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		host.mu.Lock()
		got := len(host.requests)
		host.mu.Unlock()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProductionStartDoesNotRunDurablePendingProbeWhenOptInDisabled(t *testing.T) {
	now := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}

	enabled := DefaultConfig()
	enabled.EnableResetProbe = true
	firstState := NewPluginState(enabled)
	firstState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	firstHost := newProbeFixtureHost()
	firstAdapter := &rosterCredentialHost{host: firstHost, roster: roster}
	first, err := NewProductionQuotaRefresher(firstHost, firstState, firstAdapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	firstAdapter.bindings = first.bindings
	binding, _, err := first.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	setPendingSuspectedProbe(t, first, binding.Instance, ProbeWindowFiveHour, now.Add(-time.Hour), 5*time.Hour)

	disabled := enabled
	disabled.EnableResetProbe = false
	restartState := NewPluginState(disabled)
	restartState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	restartHost := newProbeFixtureHost()
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	for i := 0; i < 8; i++ {
		restartHost.quota = append(restartHost.quota, lazy, active)
	}
	restartAdapter := &rosterCredentialHost{host: restartHost, roster: roster}
	restart, err := NewProductionQuotaRefresher(restartHost, restartState, restartAdapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	if _, _, err = restart.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	restart.Start()
	t.Cleanup(restart.Stop)
	waitForProbeHTTP(t, restartHost, 1)

	restartHost.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), restartHost.requests...)
	restartHost.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("disabled production Start issued Probe HTTP: %#v", requests)
	}
	persisted, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ProbeAttempts) != 0 {
		t.Fatalf("disabled production Start created attempts: %#v", persisted.ProbeAttempts)
	}
}

func TestProductionDisabledProbeDeadlineDoesNotArmRefreshLoop(t *testing.T) {
	now := time.Date(2026, 8, 1, 15, 15, 0, 0, time.UTC)
	r := NewQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), func() time.Time { return now })
	t.Cleanup(r.coordinator.Close)
	r.probeController = NewProbeController(now)
	r.probeController.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbeWaitingReset,
		Deadline: now.Add(-time.Minute),
	})

	if delay, scheduled := r.nextRefreshLoopDelay(); scheduled {
		t.Fatalf("disabled Probe armed refresh loop with delay %s", delay)
	}
}

func TestProductionProbeDisableAfterPrecheckFencesCompactPOST(t *testing.T) {
	now := time.Date(2026, 8, 1, 16, 10, 0, 0, time.UTC)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := newProbeFixtureHost()
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	var precheckFinished atomic.Bool
	host.afterDo = func(req pluginapi.HTTPRequest) {
		if req.URL == r.state.Config().QuotaEndpoint {
			precheckFinished.Store(true)
		}
	}
	var disabled atomic.Bool
	r.SetRosterLifecycleAuthority(func(time.Time) ActiveRoster {
		if precheckFinished.Load() && disabled.CompareAndSwap(false, true) {
			cfg := r.state.Config()
			cfg.EnableResetProbe = false
			r.state.ReplaceConfig(cfg)
		}
		return ActiveRoster{}
	})

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("config-final-gate denial returned nil")
	}
	posts, urls := probePOSTCount(host)
	if posts != 0 {
		t.Fatalf("disabled Probe sent %d compact POSTs after precheck; urls=%v", posts, urls)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := r.bindings.Lookup("a")
	attempt, ok := persisted.ProbeAttempts[binding.Instance]
	if !ok || attempt.AttemptID == "" || attempt.SendFenceSeq == 0 || (attempt.Phase != ProbeAttemptSending && attempt.Phase != ProbeAttemptSentUnknown) {
		t.Fatalf("denied sent attempt/fence was not retained: %#v, ok=%v", attempt, ok)
	}
}

func TestProductionDisabledRestartPreservesSentAttemptWithoutHTTP(t *testing.T) {
	now := time.Date(2026, 8, 1, 16, 20, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	enabled := DefaultConfig()
	enabled.EnableResetProbe = true
	state := NewPluginState(enabled)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	host := newProbeFixtureHost()
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "sent-before-disable", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 41, CreatedAt: now.Add(-time.Minute), VerifyNotBefore: now, SuppressUntil: now.Add(9 * time.Minute)}
	if _, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ReservedCeiling = 100
		s.ProbeAttempts[binding.Instance] = attempt
		s.ProbeWindows[binding.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeSentUnknown, AttemptID: attempt.AttemptID, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 0, 5*time.Hour)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	disabled := enabled
	disabled.EnableResetProbe = false
	restartState := NewPluginState(disabled)
	restartState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	restartHost := newProbeFixtureHost()
	for i := 0; i < 8; i++ {
		restartHost.quota = append(restartHost.quota, []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`))
	}
	restartAdapter := &rosterCredentialHost{host: restartHost, roster: roster}
	restart, err := NewProductionQuotaRefresher(restartHost, restartState, restartAdapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	if _, _, err = restart.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	restart.Start()
	t.Cleanup(restart.Stop)
	waitForProbeHTTP(t, restartHost, 1)

	restartHost.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), restartHost.requests...)
	restartHost.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("disabled sent-attempt restart issued Probe HTTP: %#v", requests)
	}
	persisted, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeAttempts[binding.Instance]; !reflect.DeepEqual(got, attempt) {
		t.Fatalf("disabled restart changed sent attempt/fence: got %#v want %#v", got, attempt)
	}
}

func TestProbeObservationIntervalHasThirtyMinuteFloor(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "ten minutes floors", configured: 10 * time.Minute, want: 30 * time.Minute},
		{name: "forty five minutes remains", configured: 45 * time.Minute, want: 45 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			cfg := r.state.Config()
			cfg.QuotaRefreshInterval = tt.configured
			r.state.ReplaceConfig(cfg)
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			reset := now.Add(5 * time.Hour)
			r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{
				State:    ProbeWaitingReset,
				Baseline: ResetProbeBaseline(reset, 20, 5*time.Hour),
				Deadline: reset.Add(probeRefreshAfterResetDelay),
			})
			if err := r.persistProbeWindows(); err != nil {
				t.Fatal(err)
			}

			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || !window.Deadline.Equal(now.Add(tt.want)) {
				t.Fatalf("window = %#v, ok=%v; want observation deadline %s", window, ok, now.Add(tt.want))
			}
		})
	}
}

func TestUnchangedWindowObservationReschedulesWithoutProbe(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	host := newProbeFixtureHost()
	host.quota = [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(reset, 20, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("unchanged observation sent %d Probe POSTs; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("window = %#v, ok=%v; want bounded WaitingReset", window, ok)
	}
}

func TestExternalResetObservationSendsOnce(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	oldReset := now.Add(2 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(oldReset, 20, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("external lazy reset sent %d Probe POSTs, want 1; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeConfirmed {
		t.Fatalf("window = %#v, ok=%v; want Confirmed", window, ok)
	}
}

func TestExternalResetObservationMissingEvidenceDoesNotSend(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	oldReset := now.Add(2 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	host := newProbeFixtureHost()
	host.quota = [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(oldReset, 20, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("missing usage evidence sent %d Probe POSTs; urls=%v", posts, urls)
	}
}

func TestProbePrecheckMatureUntrustedObservationDoesNotSend(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 30, 0, 0, time.UTC)
	reset := now.Add(-time.Hour)
	unchanged := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{unchanged, unchanged}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(reset, 80, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("mature non-strict observation sent %d Probe POSTs; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("window = %#v, ok=%v; want read-only bounded WaitingReset", window, ok)
	}
}

func TestProbePrecheckOutOfToleranceZeroDoesNotSend(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 30, 0, 0, time.UTC)
	oldReset := now.Add(2 * time.Hour)
	outsideTolerance := now.Add(4 * time.Hour)
	observation := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, outsideTolerance.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{observation}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(oldReset, 20, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("out-of-tolerance zero sent %d Probe POSTs; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(30*time.Minute)) || window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v, ok=%v; want read-only bounded WaitingReset", window, ok)
	}
}

func TestProductionRefreshLaunchesFirstObservedLazyProbe(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 18, 49, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, lazy, active}
	host.doStarted = make(chan struct{})
	r := newProductionLazyRefreshRuntime(t, now, host)
	t.Cleanup(r.Stop)

	if err := r.RefreshOneAuthID("a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.doStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production refresh did not launch Probe")
	}
	deadline := time.After(5 * time.Second)
	for {
		posts, _ := probePOSTCount(host)
		if posts == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("probe POST count = %d, want 1", posts)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestProbeRefreshRemovesAbsentFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeWaitingReset, Deadline: now.Add(24 * time.Hour)})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}}
	if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		t.Fatal("absent FiveHour Probe window retained")
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowLong); !ok {
		t.Fatal("LongWindow Probe state removed")
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; ok {
		t.Fatal("persisted absent FiveHour Probe window retained")
	}
	if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; !ok {
		t.Fatal("persisted LongWindow Probe state removed")
	}
}

func TestProbeRefreshPreservesAbsentFiveHourDuringNonterminalAttempt(t *testing.T) {
	for _, phase := range []ProbeAttemptPhase{ProbeAttemptPrepared, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
				s.ProbeWindows = r.probeController.Snapshot()
				s.ProbeAttempts[binding.Instance] = ProbeAttempt{Instance: binding.Instance, AttemptID: "active", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: phase}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}}
			if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); !ok {
				t.Fatal("nonterminal attempt lost referenced FiveHour state")
			}
			persisted, err := r.runtimeStore.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; !ok {
				t.Fatal("nonterminal attempt lost persisted FiveHour state")
			}
		})
	}
}

func TestReconcileConfirmedOrdinaryObservationRearmsWaitingReset(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	oldReset := now.Add(2 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbeConfirmed,
		Baseline: ResetProbeBaseline(oldReset, 1, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	used := 25.0
	seconds := int64(5 * time.Hour / time.Second)
	quota := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, LimitWindowSeconds: &seconds, ResetAt: newReset}}

	if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeWaitingReset || window.Baseline.SuspectedLazy || window.Baseline.Usage != used || !window.Baseline.ResetAt.Equal(newReset) || window.Baseline.WindowLength != 5*time.Hour || !window.Deadline.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("re-armed ordinary window = %#v, ok=%v", window, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; !reflect.DeepEqual(got, window) {
		t.Fatalf("persisted window = %#v, want %#v", got, window)
	}
}

func TestReconcileConfirmedStrictLazyObservationRearmsPending(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	oldReset := now.Add(2 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbeConfirmed,
		Baseline: ResetProbeBaseline(oldReset, 1, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	seconds := int64(5 * time.Hour / time.Second)
	quota := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: newReset}}
	r.state.UpsertQuota(AccountState{
		AuthID:        "a",
		AuthIndex:     "idx",
		Provider:      "codex",
		LastSuccessAt: now,
		Quota:         quota,
	})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy || !window.Deadline.IsZero() {
		t.Fatalf("re-armed strict lazy window = %#v, ok=%v", window, ok)
	}
}

func TestReconcileConfirmedInvalidOrMissingEvidenceRemainsConfirmed(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		quota ParsedQuota
	}{
		{
			name: "invalid usage",
			quota: ParsedQuota{FiveHour: &QuotaWindow{
				Kind: WindowFiveHour, LimitWindowSeconds: func() *int64 { value := int64(5 * time.Hour / time.Second); return &value }(), ResetAt: now.Add(5 * time.Hour),
			}},
		},
		{name: "missing window", quota: ParsedQuota{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			confirmed := ProbeWindow{State: ProbeConfirmed, Baseline: ResetProbeBaseline(now.Add(2*time.Hour), 1, 5*time.Hour)}
			r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, confirmed)
			if err := r.persistProbeWindows(); err != nil {
				t.Fatal(err)
			}

			if err := r.reconcileObservedProbeWindows(binding.Instance, tt.quota); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
			if !ok || !reflect.DeepEqual(window, confirmed) {
				t.Fatalf("window = %#v, ok=%v; want unchanged Confirmed %#v", window, ok, confirmed)
			}
		})
	}
}

func TestProbeBootstrapDoesNotResurrectConcurrentReconciliation(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 30, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	reset := now.Add(5 * time.Hour)
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{
		State:    ProbeWaitingReset,
		Baseline: ResetProbeBaseline(reset, 20, 5*time.Hour),
		Deadline: reset.Add(probeRefreshAfterResetDelay),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	originalReplace := r.runtimeStore.hooks.Replace
	reconcileWriteStarted := make(chan struct{})
	releaseReconcileWrite := make(chan struct{})
	var replaceCalls atomic.Int32
	r.runtimeStore.hooks.Replace = func(src, dst string) error {
		if replaceCalls.Add(1) == 1 {
			close(reconcileWriteStarted)
			<-releaseReconcileWrite
		}
		return originalReplace(src, dst)
	}

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- r.reconcileObservedProbeWindows(binding.Instance, ParsedQuota{}) }()
	<-reconcileWriteStarted
	bootstrapDone := make(chan error, 1)
	go func() { bootstrapDone <- r.bootstrapProbeWindows() }()
	// Without the controller/persistence linearization boundary, bootstrap
	// shortens the deadline and captures a stale snapshot during this hold.
	// With it, bootstrap waits until reconciliation also removes the controller
	// entry, then observes no window to persist.
	time.Sleep(10 * time.Millisecond)
	close(releaseReconcileWrite)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if err := <-bootstrapDone; err != nil {
		t.Fatal(err)
	}

	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; exists {
		t.Fatalf("stale bootstrap resurrected reconciled window: %#v", persisted.ProbeWindows[binding.Instance][ProbeWindowLong])
	}
	if _, exists := r.probeController.Window(binding.Instance, ProbeWindowLong); exists {
		t.Fatal("controller retained reconciled window")
	}
}

func TestProbeBootstrapCopiesBindingsBeforeProbeStateLock(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 45, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	used := 20.0
	seconds := int64(5 * time.Hour / time.Second)
	quota := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, LimitWindowSeconds: &seconds, ResetAt: now.Add(5 * time.Hour)}}
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", LastSuccessAt: now, Quota: quota})

	bootstrapHasProbeLock := make(chan struct{})
	releaseBootstrapClock := make(chan struct{})
	var firstClock atomic.Bool
	r.now = func() time.Time {
		if firstClock.CompareAndSwap(false, true) {
			close(bootstrapHasProbeLock)
			<-releaseBootstrapClock
		}
		return now
	}
	bootstrapDone := make(chan error, 1)
	go func() { bootstrapDone <- r.bootstrapProbeWindows() }()
	<-bootstrapHasProbeLock

	readerEntered := make(chan struct{})
	readerDone := make(chan bool, 1)
	write := WritebackVersion{Token: binding.ExecutionToken(0), Login: binding.Login, Fingerprint: binding.Fingerprint}
	go func() {
		readerDone <- r.bindings.ApplyIfCurrent("a", write, func() {
			close(readerEntered)
			_ = r.reconcileObservedProbeWindows(binding.Instance, quota)
		})
	}()
	<-readerEntered

	writerDone := make(chan error, 1)
	go func() { writerDone <- r.bindings.MarkAuthBlocked("a") }()
	select {
	case err := <-writerDone:
		t.Fatalf("binding writer completed while active reader held RLock: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseBootstrapClock)

	deadline := time.After(500 * time.Millisecond)
	for bootstrapDone != nil || readerDone != nil || writerDone != nil {
		select {
		case err := <-bootstrapDone:
			if err != nil {
				t.Fatal(err)
			}
			bootstrapDone = nil
		case applied := <-readerDone:
			if !applied {
				t.Fatal("active binding reader rejected current writeback")
			}
			readerDone = nil
		case err := <-writerDone:
			if err != nil {
				t.Fatal(err)
			}
			writerDone = nil
		case <-deadline:
			t.Fatal("binding reader/writer and Probe bootstrap deadlocked")
		}
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Bindings["a"].AuthBlocked != true {
		t.Fatal("queued binding writer did not persist after bootstrap")
	}
}

func TestProbeBootstrapRecreatesReappearedFiveHour(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	fiveUsed := 20.0
	fiveReset := now.Add(5 * time.Hour)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &fiveUsed, ResetAt: fiveReset},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || !window.Baseline.ResetAt.Equal(fiveReset) {
		t.Fatalf("recreated FiveHour window = %#v, ok=%v", window, ok)
	}
}

func TestProbeBootstrapSchedulesFirstObservationLazyWindowsImmediately(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	fiveSeconds := int64(5 * time.Hour / time.Second)
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)
	monthSeconds := int64(30 * 24 * time.Hour / time.Second)
	zero := 0.0

	tests := []struct {
		name   string
		kind   ProbeWindowKind
		family AccountFamily
		window QuotaWindow
	}{
		{"five-hour", ProbeWindowFiveHour, AccountFamilyWeekly, QuotaWindow{Kind: WindowFiveHour, UsedPercent: &zero, LimitWindowSeconds: &fiveSeconds, ResetAt: now.Add(5 * time.Hour)}},
		{"weekly", ProbeWindowLong, AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly", ProbeWindowLong, AccountFamilyMonthly, QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, LimitWindowSeconds: &monthSeconds, ResetAt: now.Add(30 * 24 * time.Hour)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, tt.kind)
			quota := ParsedQuota{Family: tt.family, LongWindow: &tt.window}
			if tt.kind == ProbeWindowFiveHour {
				quota.FiveHour = &tt.window
				quota.LongWindow = &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}
			}
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: quota.Family, LastSuccessAt: now, Quota: quota})

			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, tt.kind)
			if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy || window.Baseline.WindowLength <= 0 {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}

func TestProbeBootstrapRejectsFirstObservationLazyFalsePositives(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	zero, used := 0.0, 1.0
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)

	tests := []struct {
		name   string
		family AccountFamily
		window QuotaWindow
	}{
		{"non-zero-usage", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"missing-usage", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly-duration-unknown", AccountFamilyMonthly, QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, ResetAt: now.Add(30 * 24 * time.Hour)}},
		{"anchor-outside-tolerance", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7*24*time.Hour + 10*time.Minute)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: tt.family, LastSuccessAt: now, Quota: ParsedQuota{Family: tt.family, LongWindow: &tt.window}})
			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || window.State != ProbeWaitingReset || window.Baseline.SuspectedLazy {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}

func TestProbeBootstrapUsesQuotaObservationTimeForLazyAnchor(t *testing.T) {
	observedAt := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	bootstrapAt := observedAt.Add(10 * time.Minute)
	r := newDueProbeRuntime(t, bootstrapAt, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	zero := 0.0
	seconds := int64(604800)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: observedAt, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: observedAt.Add(7 * 24 * time.Hour)}}})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v, ok=%v", window, ok)
	}
}

func TestSuspectedLazyProbeBaselinePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	base := ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)
	base.SuspectedLazy = true
	if _, err := store.Update(func(state *PersistentState) error {
		state.ProbeWindows[1] = map[ProbeWindowKind]ProbeWindow{
			ProbeWindowLong: {State: ProbePendingCheck, Baseline: base},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStateStore(path, OSFileHooks(), nil)
	persisted, err := restarted.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	window := persisted.ProbeWindows[1][ProbeWindowLong]
	if !window.Baseline.SuspectedLazy || window.State != ProbePendingCheck {
		t.Fatalf("persisted window = %#v", window)
	}
}

func TestProbeBootstrapMigratesV020LazyBaselineImmediately(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	reset := now.Add(7 * 24 * time.Hour)
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{
		State:    ProbeWaitingReset,
		Baseline: ResetProbeBaseline(reset, 0, 0),
		Deadline: reset.Add(probeRefreshAfterResetDelay),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	seconds := int64(7 * 24 * time.Hour / time.Second)
	r.state.UpsertQuota(AccountState{
		AuthID:        "a",
		AuthIndex:     "idx",
		Provider:      "codex",
		Family:        AccountFamilyWeekly,
		LastSuccessAt: now,
		Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{
			Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset,
		}},
	})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.Baseline.WindowLength != 7*24*time.Hour || !window.Baseline.SuspectedLazy || window.State != ProbePendingCheck || !window.Deadline.IsZero() {
		t.Fatalf("migrated window = %#v, ok=%v", window, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; got.Baseline.WindowLength != 7*24*time.Hour || !got.Baseline.SuspectedLazy || got.State != ProbePendingCheck || !got.Deadline.IsZero() {
		t.Fatalf("persisted migrated window = %#v", got)
	}
}

func TestProbeBootstrapBoundsNonStrictV020MigrationImmediatelyAndAcrossRestart(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	seconds := int64(7 * 24 * time.Hour / time.Second)
	nonzero := 25.0
	zero := 0.0
	tests := []struct {
		name   string
		window QuotaWindow
	}{
		{name: "nonzero usage", window: QuotaWindow{Kind: WindowWeekly, UsedPercent: &nonzero, LimitWindowSeconds: &seconds, ResetAt: reset}},
		{name: "missing usage", window: QuotaWindow{Kind: WindowWeekly, LimitWindowSeconds: &seconds, ResetAt: reset}},
		{name: "reset outside strict tolerance", window: QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset.Add(-time.Hour)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			cfg := DefaultConfig()
			cfg.EnableResetProbe = true
			state := NewPluginState(cfg)
			state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
			host := newProbeFixtureHost()
			adapter := &rosterCredentialHost{host: host, roster: roster}
			r, err := NewProductionQuotaRefresher(host, state, adapter, roster, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			adapter.bindings = r.bindings
			binding, _, err := r.BootstrapBinding(context.Background(), "a")
			if err != nil {
				t.Fatal(err)
			}
			r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeWaitingReset, Baseline: ResetProbeBaseline(reset, 0, 0), Deadline: reset.Add(probeRefreshAfterResetDelay)})
			if err = r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
				t.Fatal(err)
			}
			state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &tt.window}})

			if err = r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || window.State != ProbeWaitingReset || window.Baseline.WindowLength != 7*24*time.Hour || !window.Deadline.Equal(now.Add(30*time.Minute)) {
				t.Fatalf("non-strict migrated window = %#v, ok=%v; want immediate bounded +30m", window, ok)
			}

			restartState := NewPluginState(cfg) // deliberately has no quota cache
			restartState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			restartAdapter := &rosterCredentialHost{host: host, roster: roster}
			restart, err := NewProductionQuotaRefresher(host, restartState, restartAdapter, roster, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			restartAdapter.bindings = restart.bindings
			restarted, ok := restart.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || restarted.State != ProbeWaitingReset || restarted.Baseline.WindowLength != 7*24*time.Hour || !restarted.Deadline.Equal(now.Add(30*time.Minute)) {
				t.Fatalf("cacheless restart lost bounded migration: %#v, ok=%v", restarted, ok)
			}
		})
	}
}

func TestProbeBootstrapRejectsUntrustedObservationTimeForLazyEvidence(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	zero := 0.0
	seconds := int64(7 * 24 * time.Hour / time.Second)
	tests := []struct {
		name       string
		observedAt time.Time
		wantLazy   bool
	}{
		{"valid", now, true},
		{"zero", time.Time{}, false},
		{"future", now.Add(time.Second), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset}}

			fresh := newDueProbeRuntime(t, now, newProbeFixtureHost())
			freshBinding, ok := fresh.bindings.Lookup("a")
			if !ok {
				t.Fatal("fresh binding missing")
			}
			fresh.probeController.RemoveWindow(freshBinding.Instance, ProbeWindowFiveHour)
			fresh.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: tt.observedAt, Quota: quota})
			if err := fresh.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			freshWindow, ok := fresh.probeController.Window(freshBinding.Instance, ProbeWindowLong)
			if !ok || freshWindow.Baseline.SuspectedLazy != tt.wantLazy || (tt.wantLazy && freshWindow.State != ProbePendingCheck) || (!tt.wantLazy && freshWindow.State != ProbeWaitingReset) {
				t.Fatalf("fresh window = %#v, ok=%v", freshWindow, ok)
			}

			host := newProbeFixtureHost()
			migrated := newDueProbeRuntime(t, now, host)
			migratedBinding, ok := migrated.bindings.Lookup("a")
			if !ok {
				t.Fatal("migrated binding missing")
			}
			migrated.probeController.RemoveWindow(migratedBinding.Instance, ProbeWindowFiveHour)
			migrated.probeController.SetWindow(migratedBinding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeWaitingReset, Baseline: ResetProbeBaseline(reset, 0, 0), Deadline: reset.Add(probeRefreshAfterResetDelay)})
			migrated.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: tt.observedAt, Quota: quota})
			if err := migrated.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			migratedWindow, ok := migrated.probeController.Window(migratedBinding.Instance, ProbeWindowLong)
			if !ok || migratedWindow.Baseline.SuspectedLazy != tt.wantLazy || (tt.wantLazy && migratedWindow.State != ProbePendingCheck) || (!tt.wantLazy && migratedWindow.State != ProbeWaitingReset) {
				t.Fatalf("migrated window = %#v, ok=%v", migratedWindow, ok)
			}
			if !tt.wantLazy {
				if err := migrated.RunProbeDueOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				posts, urls := probePOSTCount(host)
				if posts != 0 {
					t.Fatalf("untrusted observation launched %d probe POSTs; urls=%v", posts, urls)
				}
			}
		})
	}
}

func TestProbeBootstrapLoadsKnownLengthBaselineWithoutQuotaCache(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	baseline := ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)
	if _, err := store.Update(func(state *PersistentState) error {
		state.Bindings["a"] = RuntimeBinding{AuthID: "a", AuthIndex: "idx", Instance: 1}
		state.ProbeWindows[1] = map[ProbeWindowKind]ProbeWindow{
			ProbeWindowLong: {State: ProbeWaitingReset, Baseline: baseline, Deadline: baseline.ResetAt.Add(probeRefreshAfterResetDelay)},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: newProbeFixtureHost(), roster: roster}
	restarted, err := NewProductionQuotaRefresher(adapter.host, state, adapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = restarted.bindings
	window, ok := restarted.probeController.Window(1, ProbeWindowLong)
	if !ok || window.State != ProbeWaitingReset || window.Baseline.WindowLength != 7*24*time.Hour || !window.Baseline.ResetAt.Equal(baseline.ResetAt) {
		t.Fatalf("restarted window = %#v, ok=%v", window, ok)
	}
}

func newFirstObservedWeeklyLazyRuntime(t *testing.T, now time.Time) (*QuotaRefresher, *sequenceProbeHost, RuntimeBinding) {
	t.Helper()
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	zero := 0.0
	seconds := int64(604800)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset}}})
	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	return r, host, binding
}

func probePOSTCount(host *sequenceProbeHost) (int, []string) {
	host.mu.Lock()
	defer host.mu.Unlock()
	posts := 0
	urls := append([]string(nil), host.urls...)
	for _, url := range urls {
		if url == codexResetProbeEndpoint {
			posts++
		}
	}
	return posts, urls
}

func setPendingSuspectedProbe(t *testing.T, r *QuotaRefresher, instance AuthInstanceID, kind ProbeWindowKind, reset time.Time, length time.Duration) {
	t.Helper()
	base := ResetProbeBaseline(reset, 0, length)
	base.SuspectedLazy = true
	r.probeController.SetWindow(instance, kind, ProbeWindow{State: ProbePendingCheck, Baseline: base})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
}

func TestFirstObservedWeeklyLazyWindowSendsOneActivationAndVerifies(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	posts, urls := probePOSTCount(host)
	if posts != 1 {
		t.Fatalf("probe POST count = %d, want 1; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbeConfirmed || window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v, ok=%v", window, ok)
	}
	logs := r.state.Snapshot(now).Logs
	events := make([]string, 0, len(logs))
	for _, entry := range logs {
		if strings.HasPrefix(entry.Event, "probe.") {
			events = append(events, entry.Event)
		}
	}
	want := []string{"probe.precheck_started", "probe.activation_sent", "probe.verified"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("Probe events = %v, want %v", events, want)
	}
}

func TestProbeFailureLogRedactsSecrets(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.quotaStatus = http.StatusServiceUnavailable
	host.quota = [][]byte{[]byte(`{"access_token":"access-token-sentinel","refresh_token":"refresh-token-sentinel","account_id":"account-id-sentinel","authorization":"Bearer authorization-header-sentinel","request_body":"request-body-sentinel","response_body":"response-body-sentinel"}`)}
	host.mu.Unlock()

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce returned nil error")
	}
	logs := r.state.Snapshot(now).Logs
	var failures []LogEntry
	for _, entry := range logs {
		if entry.Event == "probe.failed" {
			failures = append(failures, entry)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("probe.failed logs = %#v, want one terminal failure", failures)
	}
	if sent, ok := failures[0].Fields["sent"].(bool); !ok || sent {
		t.Fatalf("probe.failed sent = %#v, want false", failures[0].Fields["sent"])
	}
	if windows, ok := failures[0].Fields["windows"].([]ProbeWindowKind); !ok || !reflect.DeepEqual(windows, []ProbeWindowKind{ProbeWindowLong}) {
		t.Fatalf("probe.failed windows = %#v, want [long]", failures[0].Fields["windows"])
	}
	errText, _ := failures[0].Fields["error"].(string)
	for _, forbidden := range []string{"access-token-sentinel", "refresh-token-sentinel", "account-id-sentinel", "authorization-header-sentinel", "request-body-sentinel", "response-body-sentinel"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("probe failure log leaked %q: %#v", forbidden, failures[0])
		}
	}
}

func TestProbeActivationHTTPFailureLogsStageAndSanitizedResponse(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.probeStatus = http.StatusNotFound
	host.probeBody = []byte(`{"detail":"model not found","access_token":"access-token-sentinel"}`)
	host.mu.Unlock()

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce returned nil error")
	}
	logs := r.state.Snapshot(now).Logs
	var failure *LogEntry
	for i := range logs {
		if logs[i].Event == "probe.failed" {
			failure = &logs[i]
			break
		}
	}
	if failure == nil {
		t.Fatalf("logs = %#v, want probe.failed", logs)
	}
	if got := failure.Fields["error"]; got != "activation_http_status" {
		t.Fatalf("error = %#v, want activation_http_status", got)
	}
	if got := failure.Fields["stage"]; got != "activation_post" {
		t.Fatalf("stage = %#v, want activation_post", got)
	}
	if got := failure.Fields["http_status"]; got != http.StatusNotFound {
		t.Fatalf("http_status = %#v, want 404", got)
	}
	if got := failure.Fields["sent"]; got != true {
		t.Fatalf("sent = %#v, want true", got)
	}
	summary, _ := failure.Fields["response_summary"].(string)
	if !strings.Contains(summary, "model not found") || strings.Contains(summary, "access-token-sentinel") {
		t.Fatalf("response_summary = %q, want useful redacted detail", summary)
	}
	for _, entry := range logs {
		if entry.Event == "probe.activation_sent" {
			t.Fatalf("activation_sent logged after rejected POST: %#v", logs)
		}
	}
}

func TestProbeActivationRequestUsesMinimalResponsesChanges(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	var activation *pluginapi.HTTPRequest
	for i := range host.requests {
		if host.requests[i].Method == http.MethodPost && host.requests[i].URL == codexResetProbeEndpoint {
			request := host.requests[i]
			activation = &request
			break
		}
	}
	if activation == nil {
		t.Fatalf("requests = %#v, want activation POST", host.requests)
	}
	want := map[string]string{
		"Authorization":      "Bearer access",
		"Chatgpt-Account-Id": "acct",
		"Content-Type":       "application/json",
	}
	if len(activation.Headers) != len(want) {
		t.Errorf("activation headers = %#v, want only original three headers", activation.Headers)
	}
	for name, value := range want {
		if got := activation.Headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Store  bool   `json:"store"`
	}
	if err := json.Unmarshal(activation.Body, &body); err != nil {
		t.Fatalf("activation body: %v", err)
	}
	if body.Model != codexResetProbeModel || !body.Stream || body.Store {
		t.Fatalf("activation body = %#v, want model=%q stream=true store=false", body, codexResetProbeModel)
	}
	if string(activation.Body) != resetProbePayload {
		t.Fatalf("activation body = %s, want original payload plus stream/store only", activation.Body)
	}
}

func TestProbeLifecycleLogsNeverPersistArbitraryExternalCallbackText(t *testing.T) {
	now := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	for _, stage := range []string{"get_auth", "quota_get", "compact_post", "verify"} {
		t.Run(stage, func(t *testing.T) {
			sentinel := "tenant-private-sentinel-" + stage + "-847263"
			host := newProbeFixtureHost()
			host.quota = [][]byte{lazy}
			r := newDueProbeRuntime(t, now, host)
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			base := ResetProbeBaseline(reset, 0, 5*time.Hour)
			base.WindowKind = WindowFiveHour
			base.SuspectedLazy = true
			r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: base})
			if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
				t.Fatal(err)
			}
			externalErr := errors.New("transport failed; response body: " + sentinel)
			switch stage {
			case "get_auth":
				host.getErr = externalErr
			case "quota_get":
				host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: {externalErr}}
			case "compact_post":
				host.doErrors = map[string][]error{codexResetProbeEndpoint: {externalErr}}
			case "verify":
				host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: {nil, externalErr}}
			}

			if err := r.RunProbeDueOnce(context.Background()); err == nil {
				t.Fatalf("%s injected callback error returned nil", stage)
			}
			for _, path := range []string{
				"/v0/management/plugins/codex-quota-scheduler/status",
				"/v0/management/plugins/codex-quota-scheduler/logs",
				"/v0/management/plugins/codex-quota-scheduler/export",
			} {
				resp := HandleManagementRequest(r.state, pluginapi.ManagementRequest{Method: http.MethodGet, Path: path, Headers: http.Header{"Authorization": []string{"Bearer management-key"}}, Query: url.Values{"format": []string{"json"}}}, now)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, resp.Body)
				}
				if strings.Contains(string(resp.Body), sentinel) {
					t.Fatalf("%s persisted %s callback sentinel %q: %s", path, stage, sentinel, resp.Body)
				}
			}
		})
	}
}

type probeTerminalWriteFailure struct {
	before   PersistentState
	injected bool
}

func failProbeTerminalWrite(t *testing.T, r *QuotaRefresher, instance AuthInstanceID, kind ProbeWindowKind, final ProbeWindowState) *probeTerminalWriteFailure {
	t.Helper()
	result := &probeTerminalWriteFailure{}
	before, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.ProbeAttempts[instance]; ok {
		result.before = before
	}
	previous := r.runtimeStore.hooks.Replace
	r.runtimeStore.hooks.Replace = func(source, target string) error {
		raw, readErr := os.ReadFile(source)
		if readErr == nil {
			var candidate PersistentState
			if json.Unmarshal(raw, &candidate) == nil {
				if !result.injected {
					if _, ok := candidate.ProbeAttempts[instance]; ok {
						result.before = candidate
					}
					window, hasWindow := candidate.ProbeWindows[instance][kind]
					_, hasAttempt := candidate.ProbeAttempts[instance]
					if target == r.runtimeStore.path && hasWindow && window.State == final && !hasAttempt {
						result.injected = true
						return errors.New("terminal Probe persistence failed")
					}
				}
			}
		}
		if previous != nil {
			return previous(source, target)
		}
		return nil
	}
	t.Cleanup(func() { r.runtimeStore.hooks.Replace = previous })
	return result
}

func assertProbeTerminalWriteRolledBack(t *testing.T, r *QuotaRefresher, failure *probeTerminalWriteFailure, instance AuthInstanceID) PersistentState {
	t.Helper()
	if !failure.injected {
		t.Fatal("terminal Probe write failure was not injected")
	}
	restarted := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil)
	persisted, err := restarted.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantAttempt, ok := failure.before.ProbeAttempts[instance]
	if !ok {
		t.Fatalf("failure injector did not observe a pre-terminal attempt: %#v", failure.before.ProbeAttempts)
	}
	got, ok := persisted.ProbeAttempts[instance]
	phaseOK := got.Phase == wantAttempt.Phase || (wantAttempt.Phase == ProbeAttemptSent && got.Phase == ProbeAttemptSentUnknown)
	normalized := got
	normalized.Phase = wantAttempt.Phase
	if !ok || !phaseOK || !reflect.DeepEqual(normalized, wantAttempt) {
		t.Fatalf("attempt after failed terminal write = %#v, ok=%v; want unchanged %#v", got, ok, wantAttempt)
	}
	if got, want := persisted.ProbeWindows[instance], failure.before.ProbeWindows[instance]; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows after failed terminal write = %#v; want unchanged %#v", got, want)
	}
	return persisted
}

func assertProbeTerminalFailure(t *testing.T, r *QuotaRefresher, now time.Time, sent bool) {
	t.Helper()
	logs := r.state.Snapshot(now).Logs
	var failures []LogEntry
	for _, entry := range logs {
		if entry.Event == "probe.failed" {
			failures = append(failures, entry)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("probe.failed logs = %#v, want one terminal failure", failures)
	}
	if got, ok := failures[0].Fields["sent"].(bool); !ok || got != sent {
		t.Fatalf("probe.failed sent = %#v, want %t", failures[0].Fields["sent"], sent)
	}
	if logs[len(logs)-1].Event != "probe.failed" {
		t.Fatalf("last probe lifecycle log = %#v, want probe.failed", logs[len(logs)-1])
	}
}

func TestProbePostCompletionFailuresLogTerminalFailure(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	t.Run("recovery reconciliation", func(t *testing.T) {
		r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
		host.mu.Lock()
		host.quota = [][]byte{append([]byte(nil), host.quota[1]...)}
		host.mu.Unlock()
		if _, err := r.probeFence.Next(); err != nil {
			t.Fatal(err)
		}
		attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "recovery-attempt", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 1, VerifyNotBefore: now, SuppressUntil: now.Add(10 * time.Minute)}
		window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
		if !ok {
			t.Fatal("long window missing")
		}
		window.State = ProbeSentUnknown
		window.AttemptID = attempt.AttemptID
		r.probeController.SetWindow(binding.Instance, ProbeWindowLong, window)
		if err := r.persistProbeWindows(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
			state.ProbeAttempts[binding.Instance] = attempt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		result := r.runTypedHeld(context.Background(), Intent{AuthID: "a", Instance: binding.Instance, Class: OperationProbeSequence, Source: SourceProbeVerify, StartedAfter: attempt.SendFenceSeq, AttemptID: attempt.AttemptID, Payload: probeSequencePayload{Binding: binding, Windows: attempt.Windows, Attempt: attempt, Recovery: true}}, &HeldLease{coordinator: r.coordinator})
		if result.Err == nil {
			t.Fatal("recovery returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, true)
	})

	t.Run("precheck reconciliation", func(t *testing.T) {
		r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
		host.mu.Lock()
		host.quota = [][]byte{append([]byte(nil), host.quota[1]...)}
		host.mu.Unlock()
		binding, ok := r.bindings.Lookup("a")
		if !ok {
			t.Fatal("binding missing")
		}
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		if err := r.RunProbeDueOnce(context.Background()); err == nil {
			t.Fatal("precheck returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, false)
	})

	t.Run("verify reconciliation", func(t *testing.T) {
		r, _, binding := newFirstObservedWeeklyLazyRuntime(t, now)
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		if err := r.RunProbeDueOnce(context.Background()); err == nil {
			t.Fatal("verify returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, true)
	})
}

func TestProbeTerminalCompletionFailureRestartsVerifyFirst(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.quota = append(host.quota, append([]byte(nil), host.quota[1]...))
	host.mu.Unlock()
	failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("terminal persistence failure was not surfaced")
	}
	persisted := assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
	attempt := persisted.ProbeAttempts[binding.Instance]
	if attempt.AttemptID == "" || attempt.SendFenceSeq == 0 || (attempt.Phase != ProbeAttemptSent && attempt.Phase != ProbeAttemptSentUnknown) {
		t.Fatalf("failed completion did not retain recovery attempt: %#v", attempt)
	}
	postsBefore, urlsBefore := probePOSTCount(host)
	if postsBefore != 1 {
		t.Fatalf("probe POST count before restart = %d, want 1; urls=%v", postsBefore, urlsBefore)
	}

	restartNow := now.Add(4 * time.Second)
	roster := r.runtimeRoster()
	adapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, r.state, adapter, roster, r.runtimeStore.path, func() time.Time { return restartNow })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = restart.bindings
	restart.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	postsAfter, urlsAfter := probePOSTCount(host)
	if postsAfter != 1 {
		t.Fatalf("Probe recovery resent activation: posts=%d urls=%v", postsAfter, urlsAfter)
	}
	completed, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := completed.ProbeAttempts[binding.Instance]; ok {
		t.Fatalf("recovery attempt survived successful verification: %#v", completed.ProbeAttempts[binding.Instance])
	}
	if window := completed.ProbeWindows[binding.Instance][ProbeWindowLong]; window.State != ProbeConfirmed {
		t.Fatalf("recovery window = %#v, want Confirmed", window)
	}
}

func TestProbeTerminalCompletionMergesOnlyCompletingInstance(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	attempt := ProbeAttempt{Instance: 1, AttemptID: "a-terminal", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSent, SendFenceSeq: 7, VerifyNotBefore: now}
	bPending := ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)}
	if _, err := store.Update(func(state *PersistentState) error {
		state.ProbeAttempts[1] = attempt
		state.ProbeWindows[1] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeSentAwaitingVerify}}
		state.ProbeWindows[2] = map[ProbeWindowKind]ProbeWindow{ProbeWindowLong: bPending}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller := NewProbeController(now)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeConfirmed})
	r := &QuotaRefresher{runtimeStore: store, probeController: controller}
	quota := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(5 * time.Hour)}}

	if err := r.persistTerminalProbeCompletion(1, attempt.AttemptID, quota, ProbeAttemptSent); err != nil {
		t.Fatal(err)
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := persisted.ProbeWindows[2][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, bPending) {
		t.Fatalf("instance B window overwritten by A completion: got=%#v ok=%v want=%#v", got, ok, bPending)
	}
	if _, ok := persisted.ProbeAttempts[1]; ok {
		t.Fatalf("instance A attempt survived completion: %#v", persisted.ProbeAttempts[1])
	}
}

func TestProbeTerminalCompletionAndRosterHoldShareLinearizationBoundary(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	type fixture struct {
		r       *QuotaRefresher
		attempt ProbeAttempt
		quota   ParsedQuota
		other   ProbeWindow
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
		attempt := ProbeAttempt{Instance: 1, AttemptID: "terminal-vs-hold", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 9, CreatedAt: now.Add(-time.Minute), VerifyNotBefore: now}
		window := ProbeWindow{State: ProbeSentUnknown, AttemptID: attempt.AttemptID, Baseline: ResetProbeBaseline(now.Add(5*time.Hour), 0, 5*time.Hour)}
		other := ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)}
		if _, err := store.Update(func(state *PersistentState) error {
			state.ProbeAttempts[attempt.Instance] = attempt
			state.ProbeWindows[attempt.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: window}
			state.ProbeWindows[2] = map[ProbeWindowKind]ProbeWindow{ProbeWindowLong: other}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		controller := NewProbeController(now)
		controller.SetWindow(attempt.Instance, ProbeWindowFiveHour, window)
		return fixture{
			r:       &QuotaRefresher{runtimeStore: store, probeController: controller, now: func() time.Time { return now }},
			attempt: attempt,
			quota:   ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(5 * time.Hour), UsedPercent: ptrFloat(1)}},
			other:   other,
		}
	}
	markConfirmed := func(f fixture) {
		window, _ := f.r.probeController.Window(f.attempt.Instance, ProbeWindowFiveHour)
		window.State = ProbeConfirmed
		window.Deadline = time.Time{}
		f.r.probeController.SetWindow(f.attempt.Instance, ProbeWindowFiveHour, window)
	}
	assertTerminal := func(t *testing.T, f fixture) {
		t.Helper()
		persisted, err := f.r.runtimeStore.PersistentSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if attempt, ok := persisted.ProbeAttempts[f.attempt.Instance]; ok {
			t.Errorf("terminal/hold ordering retained attempt: %#v", attempt)
		}
		if got := persisted.ProbeWindows[f.attempt.Instance][ProbeWindowFiveHour]; got.State != ProbeConfirmed {
			t.Errorf("durable terminal/hold window=%#v, want Confirmed", got)
		}
		if got, ok := f.r.probeController.Window(f.attempt.Instance, ProbeWindowFiveHour); !ok || got.State != ProbeConfirmed {
			t.Errorf("controller terminal/hold window=%#v ok=%v, want Confirmed", got, ok)
		}
		if got, ok := persisted.ProbeWindows[2][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, f.other) {
			t.Errorf("roster hold replaced unrelated instance: got=%#v ok=%v want=%#v", got, ok, f.other)
		}
	}

	t.Run("terminal persistence blocks behind roster hold", func(t *testing.T) {
		f := newFixture(t)
		markConfirmed(f)
		writeStarted := make(chan struct{})
		var writeOnce sync.Once
		previousObserve := f.r.runtimeStore.hooks.Observe
		f.r.runtimeStore.hooks.Observe = func(op string) {
			if previousObserve != nil {
				previousObserve(op)
			}
			if op == "backup-write" {
				writeOnce.Do(func() { close(writeStarted) })
			}
		}
		f.r.probeHoldMu.Lock()
		done := make(chan error, 1)
		go func() {
			done <- f.r.persistTerminalProbeCompletion(f.attempt.Instance, f.attempt.AttemptID, f.quota, ProbeAttemptSentUnknown)
		}()
		select {
		case <-writeStarted:
			t.Error("terminal completion reached durable write while roster-hold boundary was locked")
		case <-time.After(100 * time.Millisecond):
		}
		f.r.probeHoldMu.Unlock()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("terminal completion did not resume after roster-hold boundary")
		}
	})

	t.Run("terminal commits before roster hold", func(t *testing.T) {
		f := newFixture(t)
		markConfirmed(f)
		if err := f.r.persistTerminalProbeCompletion(f.attempt.Instance, f.attempt.AttemptID, f.quota, ProbeAttemptSentUnknown); err != nil {
			t.Fatal(err)
		}
		if err := f.r.persistProbeRosterHold(); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, f)
	})

	t.Run("roster hold commits before terminal", func(t *testing.T) {
		f := newFixture(t)
		if err := f.r.persistProbeRosterHold(); err != nil {
			t.Fatal(err)
		}
		markConfirmed(f)
		if err := f.r.persistTerminalProbeCompletion(f.attempt.Instance, f.attempt.AttemptID, f.quota, ProbeAttemptSentUnknown); err != nil {
			t.Fatal(err)
		}
		assertTerminal(t, f)
	})
}

type productionVerifyRosterBarrierFixture struct {
	r                  *QuotaRefresher
	host               *sequenceProbeHost
	a                  RuntimeBinding
	b                  RuntimeBinding
	attempt            ProbeAttempt
	bDurableWindows    map[ProbeWindowKind]ProbeWindow
	bControllerWindows map[ProbeWindowKind]ProbeWindow
	bAttempt           ProbeAttempt
	bAttemptPresent    bool
	legacyPath         string
	roster             HostRosterSnapshot
	now                time.Time
}

func cloneProbeWindowInstance(windows map[ProbeWindowKind]ProbeWindow) map[ProbeWindowKind]ProbeWindow {
	out := make(map[ProbeWindowKind]ProbeWindow, len(windows))
	for kind, window := range windows {
		out[kind] = window
	}
	return out
}

func newProductionVerifyRosterBarrierFixture(t *testing.T) productionVerifyRosterBarrierFixture {
	t.Helper()
	now := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{active}
	r, a, b, legacyPath, roster := newTwoAccountProbeRuntime(t, now, host)
	t.Cleanup(r.coordinator.Close)
	sendFence, err := r.probeFence.Next()
	if err != nil {
		t.Fatal(err)
	}
	bSendFence, err := r.probeFence.Next()
	if err != nil {
		t.Fatal(err)
	}
	attempt := ProbeAttempt{Instance: a.Instance, AttemptID: "production-terminal-vs-hold", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: now.Add(-time.Minute), VerifyNotBefore: now, SuppressUntil: now.Add(10 * time.Minute)}
	aBaseline := ResetProbeBaseline(reset, 0, 5*time.Hour)
	aBaseline.WindowKind = WindowFiveHour
	aBaseline.SuspectedLazy = true
	aWindow := ProbeWindow{State: ProbeSentUnknown, AttemptID: attempt.AttemptID, Baseline: aBaseline}
	bFiveBaseline := ResetProbeBaseline(now.Add(5*time.Hour), 30, 5*time.Hour)
	bFiveBaseline.WindowKind = WindowFiveHour
	bLongBaseline := ResetProbeBaseline(now.Add(7*24*time.Hour), 20, 7*24*time.Hour)
	bLongBaseline.WindowKind = WindowWeekly
	bAttempt := ProbeAttempt{Instance: b.Instance, AttemptID: "unrelated-future-verify", Windows: []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: bSendFence, CreatedAt: now, VerifyNotBefore: now.Add(time.Hour), SuppressUntil: now.Add(10 * time.Minute)}
	bWindows := map[ProbeWindowKind]ProbeWindow{
		ProbeWindowFiveHour: {State: ProbeSentUnknown, Baseline: bFiveBaseline, RetryCount: 2, AttemptID: bAttempt.AttemptID},
		ProbeWindowLong:     {State: ProbeSentUnknown, Baseline: bLongBaseline, RetryCount: 3, AttemptID: bAttempt.AttemptID},
	}
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, aWindow)
	for kind, window := range bWindows {
		r.probeController.SetWindow(b.Instance, kind, window)
	}
	if _, err = r.runtimeStore.Update(func(state *PersistentState) error {
		state.ProbeAttempts[a.Instance] = attempt
		state.ProbeAttempts[b.Instance] = bAttempt
		state.ProbeWindows[a.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: aWindow}
		state.ProbeWindows[b.Instance] = cloneProbeWindowInstance(bWindows)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	controllerWindows := r.probeController.Snapshot()
	persistedBAttempt, bAttemptPresent := persisted.ProbeAttempts[b.Instance]
	return productionVerifyRosterBarrierFixture{
		r:                  r,
		host:               host,
		a:                  a,
		b:                  b,
		attempt:            attempt,
		bDurableWindows:    cloneProbeWindowInstance(persisted.ProbeWindows[b.Instance]),
		bControllerWindows: cloneProbeWindowInstance(controllerWindows[b.Instance]),
		bAttempt:           persistedBAttempt,
		bAttemptPresent:    bAttemptPresent,
		legacyPath:         legacyPath,
		roster:             roster,
		now:                now,
	}
}

func (f productionVerifyRosterBarrierFixture) lifecycle(health RosterHealth, revision uint64) ActiveRoster {
	background := health == RosterHealthy || health == RosterDegraded
	return ActiveRoster{
		Capability:        CapabilityA,
		Confirmed:         true,
		BackgroundAllowed: background,
		Health:            health,
		Generation:        f.r.runtimeRoster().Generation,
		LifecycleRevision: revision,
		ConfirmedAt:       f.now,
		HighestPriority:   9,
		Instances:         []string{"a", "b"},
		Entries:           append([]RosterEntry(nil), f.roster.Entries...),
	}
}

func armNextProbeStateWriteBarrier(t *testing.T, r *QuotaRefresher) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	previousObserve := r.runtimeStore.hooks.Observe
	r.runtimeStore.hooks.Observe = func(op string) {
		if previousObserve != nil {
			previousObserve(op)
		}
		if op == "backup-write" {
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)
	return entered, releaseFn
}

func waitForProductionBarrier(t *testing.T, barrier <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func assertUnrelatedProductionProbeState(t *testing.T, f productionVerifyRosterBarrierFixture, persisted PersistentState, controller *ProbeController) {
	t.Helper()
	durableWindows, durablePresent := persisted.ProbeWindows[f.b.Instance]
	if !durablePresent || !reflect.DeepEqual(durableWindows, f.bDurableWindows) {
		t.Fatalf("unrelated durable windows changed: got=%#v present=%v want=%#v", durableWindows, durablePresent, f.bDurableWindows)
	}
	controllerWindows, controllerPresent := controller.Snapshot()[f.b.Instance]
	if !controllerPresent || !reflect.DeepEqual(controllerWindows, f.bControllerWindows) {
		t.Fatalf("unrelated controller windows changed: got=%#v present=%v want=%#v", controllerWindows, controllerPresent, f.bControllerWindows)
	}
	durableAttempt, attemptPresent := persisted.ProbeAttempts[f.b.Instance]
	if attemptPresent != f.bAttemptPresent || !reflect.DeepEqual(durableAttempt, f.bAttempt) {
		t.Fatalf("unrelated attempt changed: got=%#v present=%v want=%#v present=%v", durableAttempt, attemptPresent, f.bAttempt, f.bAttemptPresent)
	}
}

func assertProductionVerifyRosterBarrierResult(t *testing.T, f productionVerifyRosterBarrierFixture) {
	t.Helper()
	persisted, err := f.r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	durableA, ok := persisted.ProbeWindows[f.a.Instance][ProbeWindowFiveHour]
	if !ok || durableA.State != ProbeConfirmed {
		t.Fatalf("durable target window=%#v ok=%v, want Confirmed", durableA, ok)
	}
	controllerA, ok := f.r.probeController.Window(f.a.Instance, ProbeWindowFiveHour)
	if !ok || !reflect.DeepEqual(controllerA, durableA) {
		t.Fatalf("target controller/durable mismatch: controller=%#v ok=%v durable=%#v", controllerA, ok, durableA)
	}
	if attempt, ok := persisted.ProbeAttempts[f.a.Instance]; ok {
		t.Fatalf("terminal target retained attempt: %#v", attempt)
	}
	assertUnrelatedProductionProbeState(t, f, persisted, f.r.probeController)
	f.host.mu.Lock()
	requestsBeforeRestart := len(f.host.requests)
	f.host.mu.Unlock()
	if requestsBeforeRestart != 1 {
		t.Fatalf("production verify requests=%d, want one GET", requestsBeforeRestart)
	}

	restartAdapter := &rosterCredentialHost{host: f.host, roster: f.roster}
	restart, err := NewProductionQuotaRefresher(f.host, f.r.state, restartAdapter, f.roster, f.legacyPath, func() time.Time { return f.now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restart.coordinator.Close)
	restartAdapter.bindings = restart.bindings
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery failed or looped: %v", err)
	}
	restarted, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if attempt, ok := restarted.ProbeAttempts[f.a.Instance]; ok {
		t.Fatalf("restart retained target attempt: %#v", attempt)
	}
	assertUnrelatedProductionProbeState(t, f, restarted, restart.probeController)
	f.host.mu.Lock()
	requestsAfterRestart := len(f.host.requests)
	f.host.mu.Unlock()
	if requestsAfterRestart != requestsBeforeRestart {
		t.Fatalf("restart recovery issued requests: before=%d after=%d", requestsBeforeRestart, requestsAfterRestart)
	}
}

func TestProductionVerifyTerminalAndFailClosedLifecycleLinearizeBothOrders(t *testing.T) {
	var missingVerifyBoundary atomic.Bool
	t.Run("verify terminal owns boundary before FailClosed", func(t *testing.T) {
		f := newProductionVerifyRosterBarrierFixture(t)
		seeded, err := f.r.runtimeStore.PersistentSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if got := seeded.ProbeAttempts[f.a.Instance]; got.AttemptID != f.attempt.AttemptID || got.SendFenceSeq != f.attempt.SendFenceSeq || got.Phase != ProbeAttemptSentUnknown {
			t.Fatalf("seeded attempt lost exact identity/fence: %#v", got)
		}
		writeEntered, releaseWrite := armNextProbeStateWriteBarrier(t, f.r)
		recoveryDone := make(chan error, 1)
		go func() { recoveryDone <- f.r.RunProbeRecoveryOnce(context.Background()) }()
		waitForProductionBarrier(t, writeEntered, "production terminal did not reach durable barrier")
		window, ok := f.r.probeController.Window(f.a.Instance, ProbeWindowFiveHour)
		if !ok || window.State != ProbeConfirmed {
			t.Fatalf("real verify classification=%#v ok=%v, want Confirmed", window, ok)
		}
		if f.r.probeHoldMu.TryLock() {
			missingVerifyBoundary.Store(true)
			f.r.probeHoldMu.Unlock()
		}

		baseRevision := f.r.runtimeRoster().LifecycleRevision
		failClosedDone := make(chan struct{})
		go func() {
			f.r.ObserveRosterLifecycle(f.lifecycle(RosterFailClosed, baseRevision+1))
			close(failClosedDone)
		}()
		deadline := time.Now().Add(time.Second)
		for f.r.runtimeRoster().Health != RosterFailClosed {
			if time.Now().After(deadline) {
				t.Fatal("actual FailClosed lifecycle was not published")
			}
			time.Sleep(time.Millisecond)
		}
		select {
		case <-failClosedDone:
			t.Fatal("FailClosed hold bypassed in-flight terminal boundary")
		default:
		}
		releaseWrite()
		if err = <-recoveryDone; err != nil {
			t.Fatal(err)
		}
		waitForProductionBarrier(t, failClosedDone, "FailClosed lifecycle did not finish after terminal commit")
		assertProductionVerifyRosterBarrierResult(t, f)
	})

	t.Run("FailClosed owns boundary before verify terminal", func(t *testing.T) {
		f := newProductionVerifyRosterBarrierFixture(t)
		clock := &probeNthCaptureClock{now: f.now}
		f.r.now = clock.Now
		seeded, err := f.r.runtimeStore.PersistentSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if got := seeded.ProbeAttempts[f.a.Instance]; got.AttemptID != f.attempt.AttemptID || got.SendFenceSeq != f.attempt.SendFenceSeq || got.Phase != ProbeAttemptSentUnknown {
			t.Fatalf("seeded attempt lost exact identity/fence: %#v", got)
		}
		f.host.gateAuthIndex = f.a.AuthIndex
		f.host.getStarted = make(chan struct{})
		f.host.releaseGet = make(chan struct{})
		quotaReturned := make(chan struct{})
		releaseQuota := make(chan struct{})
		var quotaOnce sync.Once
		f.host.afterDo = func(req pluginapi.HTTPRequest) {
			if req.URL == f.r.state.Config().QuotaEndpoint {
				quotaOnce.Do(func() {
					close(quotaReturned)
					<-releaseQuota
				})
			}
		}
		var releaseQuotaOnce sync.Once
		releaseQuotaFn := func() { releaseQuotaOnce.Do(func() { close(releaseQuota) }) }
		t.Cleanup(releaseQuotaFn)
		var releaseGetOnce sync.Once
		releaseGet := func() { releaseGetOnce.Do(func() { close(f.host.releaseGet) }) }
		t.Cleanup(releaseGet)
		writeEntered, releaseWrite := armNextProbeStateWriteBarrier(t, f.r)
		recoveryDone := make(chan error, 1)
		go func() { recoveryDone <- f.r.RunProbeRecoveryOnce(context.Background()) }()
		waitForProductionBarrier(t, f.host.getStarted, "production recovery did not reach GetAuth barrier")

		baseRevision := f.r.runtimeRoster().LifecycleRevision
		failClosedDone := make(chan struct{})
		go func() {
			f.r.ObserveRosterLifecycle(f.lifecycle(RosterFailClosed, baseRevision+1))
			close(failClosedDone)
		}()
		waitForProductionBarrier(t, writeEntered, "actual FailClosed hold did not reach durable barrier")
		if f.r.runtimeRoster().Health != RosterFailClosed {
			t.Fatal("FailClosed hold reached storage before lifecycle publication")
		}
		// Let the already-admitted recovery cross the production HTTP gate while
		// the earlier FailClosed hold still owns the Probe linearization boundary.
		f.r.ObserveRosterLifecycle(f.lifecycle(RosterHealthy, baseRevision+2))
		releaseGet()
		waitForProductionBarrier(t, quotaReturned, "admitted production verify did not finish its quota GET")
		if missingVerifyBoundary.Load() {
			classificationReached, releaseClassification := clock.ArmNthCall(2)
			var releaseClassificationOnce sync.Once
			releaseClassificationFn := func() { releaseClassificationOnce.Do(func() { close(releaseClassification) }) }
			t.Cleanup(releaseClassificationFn)
			releaseQuotaFn()
			waitForProductionBarrier(t, classificationReached, "missing-lock verify did not reach the classification boundary")
			releaseClassificationFn()
			deadline := time.Now().Add(2 * time.Second)
			for {
				window, ok := f.r.probeController.Window(f.a.Instance, ProbeWindowFiveHour)
				if !ok || window.AttemptID != f.attempt.AttemptID {
					t.Fatalf("blocked target lost exact attempt: %#v ok=%v", window, ok)
				}
				if window.State == ProbeConfirmed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("missing-lock verify did not publish stale classification before FailClosed release")
				}
				time.Sleep(time.Millisecond)
			}
		} else {
			releaseQuotaFn()
		}
		releaseWrite()
		waitForProductionBarrier(t, failClosedDone, "FailClosed hold did not commit after release")
		if err = <-recoveryDone; err != nil {
			t.Fatal(err)
		}
		assertProductionVerifyRosterBarrierResult(t, f)
	})
}

func newTwoAccountProbeRuntime(t *testing.T, now time.Time, host *sequenceProbeHost) (*QuotaRefresher, RuntimeBinding, RuntimeBinding, string, HostRosterSnapshot) {
	t.Helper()
	host.authByIndex = map[string]pluginapi.HostAuthGetResponse{
		"idx-a": {AuthIndex: "idx-a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access-a","refresh_token":"refresh-a","account_id":"acct-a"}`)},
		"idx-b": {AuthIndex: "idx-b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"access-b","refresh_token":"refresh-b","account_id":"acct-b"}`)},
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)},
	}}
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	a, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	return r, a, b, legacyPath, roster
}

func durableUnmirroredBWindow(t *testing.T, r *QuotaRefresher, b RuntimeBinding, now time.Time) ProbeWindow {
	t.Helper()
	window := ProbeWindow{State: ProbeWaitingReset, Baseline: ResetProbeBaseline(now.Add(7*24*time.Hour), 25, 7*24*time.Hour), Deadline: now.Add(30 * time.Minute)}
	window.Baseline.WindowKind = WindowWeekly
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowLong: window}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, mirrored := r.probeController.Window(b.Instance, ProbeWindowLong); mirrored {
		t.Fatal("B barrier setup unexpectedly mirrored controller")
	}
	return window
}

func TestProbeClaimMergesOnlyClaimingInstanceAcrossDurableMirrorBarrier(t *testing.T) {
	now := time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{active}
	r, a, b, _, _ := newTwoAccountProbeRuntime(t, now, host)
	base := ResetProbeBaseline(reset, 0, 5*time.Hour)
	base.WindowKind = WindowFiveHour
	base.SuspectedLazy = true
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: base})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{a.Instance: {}}); err != nil {
		t.Fatal(err)
	}
	wantB := durableUnmirroredBWindow(t, r, b, now)
	host.gateAuthIndex = "idx-a"
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-host.getStarted
	persisted, err := r.runtimeStore.PersistentSnapshot()
	close(host.releaseGet)
	if runErr := <-done; runErr != nil {
		t.Fatal(runErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := persisted.ProbeWindows[b.Instance][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, wantB) {
		t.Fatalf("A claim overwrote durable unmirrored B: got=%#v ok=%v want=%#v", got, ok, wantB)
	}
}

func TestPreparedProbeRecoveryMergesOnlyFailedInstanceAcrossDurableMirrorBarrier(t *testing.T) {
	now := time.Date(2026, 8, 1, 19, 10, 0, 0, time.UTC)
	host := newProbeFixtureHost()
	r, a, b, _, _ := newTwoAccountProbeRuntime(t, now, host)
	aWindow := ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(5*time.Hour), 0, 5*time.Hour)}
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, aWindow)
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{a.Instance: {}}); err != nil {
		t.Fatal(err)
	}
	wantB := durableUnmirroredBWindow(t, r, b, now)
	prepared := ProbeAttempt{Instance: a.Instance, AttemptID: "prepared-before-send", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, CreatedAt: now}
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeAttempts[a.Instance] = prepared
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := persisted.ProbeWindows[b.Instance][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, wantB) {
		t.Fatalf("A prepared recovery overwrote durable unmirrored B: got=%#v ok=%v want=%#v", got, ok, wantB)
	}
	if _, ok := persisted.ProbeAttempts[a.Instance]; ok {
		t.Fatalf("prepared A attempt survived pre-send recovery: %#v", persisted.ProbeAttempts[a.Instance])
	}
	if got := persisted.ProbeWindows[a.Instance][ProbeWindowFiveHour]; got.State != ProbeRetryWait || !got.Deadline.Equal(now) {
		t.Fatalf("prepared A recovery = %#v, want RetryWait at now", got)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("prepared recovery sent %d compact POSTs; urls=%v", posts, urls)
	}
}

func TestSentProbeFailureMergesOnlyFailedInstanceAndRestartDoesNotResend(t *testing.T) {
	now := time.Date(2026, 8, 1, 19, 20, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy}
	r, a, b, legacyPath, roster := newTwoAccountProbeRuntime(t, now, host)
	base := ResetProbeBaseline(reset, 0, 5*time.Hour)
	base.WindowKind = WindowFiveHour
	base.SuspectedLazy = true
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: base})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{a.Instance: {}}); err != nil {
		t.Fatal(err)
	}
	propagationStarted := make(chan struct{})
	releasePropagation := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		close(propagationStarted)
		<-releasePropagation
		return errors.New("verify propagation failed")
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-propagationStarted
	wantB := durableUnmirroredBWindow(t, r, b, now)
	beforeFailure, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	aAttempt := beforeFailure.ProbeAttempts[a.Instance]
	close(releasePropagation)
	if runErr := <-done; runErr == nil {
		t.Fatal("sent failure returned nil")
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := persisted.ProbeWindows[b.Instance][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, wantB) {
		t.Fatalf("A sent failure overwrote durable unmirrored B: got=%#v ok=%v want=%#v", got, ok, wantB)
	}
	gotAttempt, ok := persisted.ProbeAttempts[a.Instance]
	if !ok || gotAttempt.AttemptID != aAttempt.AttemptID || gotAttempt.SendFenceSeq != aAttempt.SendFenceSeq || gotAttempt.SuppressUntil != aAttempt.SuppressUntil {
		t.Fatalf("A sent failure changed attempt/fence: got=%#v ok=%v want=%#v", gotAttempt, ok, aAttempt)
	}

	disabled := DefaultConfig()
	disabled.EnableResetProbe = false
	restartState := NewPluginState(disabled)
	restartState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	restartAdapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, restartState, restartAdapter, roster, legacyPath, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	restart.Start()
	t.Cleanup(restart.Stop)
	waitForProbeHTTP(t, host, 3)
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("disabled restart resent sent attempt: posts=%d urls=%v", posts, urls)
	}
	restarted, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.ProbeAttempts[a.Instance]; got.AttemptID != aAttempt.AttemptID || got.SendFenceSeq != aAttempt.SendFenceSeq {
		t.Fatalf("disabled restart changed A attempt/fence: got=%#v want=%#v", got, aAttempt)
	}
}

func probePOSTCountsByAccount(host *sequenceProbeHost) map[string]int {
	host.mu.Lock()
	defer host.mu.Unlock()
	counts := map[string]int{}
	for _, req := range host.requests {
		if req.URL == codexResetProbeEndpoint {
			counts[req.Headers.Get("Chatgpt-Account-Id")]++
		}
	}
	return counts
}

func TestProbeRefreshDuringActiveRunCoalescesRerunAndPersistsSecondInstance(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 30, 0, 0, time.UTC)
	aLazyReset := now.Add(5 * time.Hour)
	aActiveReset := aLazyReset
	bReset := now.Add(7 * 24 * time.Hour)
	aLazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, aLazyReset.Format(time.RFC3339)))
	aActive := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, aActiveReset.Format(time.RFC3339)))
	bLazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, bReset.Format(time.RFC3339)))
	bActive := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, bReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.authFiles = []pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: 9},
	}
	host.authByIndex = map[string]pluginapi.HostAuthGetResponse{
		"idx-a": {AuthIndex: "idx-a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access-a","refresh_token":"refresh-a","account_id":"acct-a"}`)},
		"idx-b": {AuthIndex: "idx-b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"access-b","refresh_token":"refresh-b","account_id":"acct-b"}`)},
	}
	host.quota = [][]byte{aLazy, bLazy, aActive, bLazy, bActive}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	path := filepath.Join(t.TempDir(), "state.json")
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	adapter.bindings = r.bindings
	aBinding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	bBinding, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	aBaseline := ResetProbeBaseline(aLazyReset, 0, 5*time.Hour)
	aBaseline.SuspectedLazy = true
	r.probeController.SetWindow(aBinding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: aBaseline})
	r.probeController.SetWindow(bBinding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeConfirmed, Baseline: ResetProbeBaseline(now.Add(6*24*time.Hour), 1, 7*24*time.Hour)})
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previousRosterController := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previousRosterController; refresherMu.Unlock() })

	propagationStarted := make(chan struct{})
	releasePropagation := make(chan struct{})
	var releasePropagationOnce sync.Once
	releaseA := func() { releasePropagationOnce.Do(func() { close(releasePropagation) }) }
	t.Cleanup(releaseA)
	var propagationCalls atomic.Int32
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		if propagationCalls.Add(1) == 1 {
			close(propagationStarted)
			<-releasePropagation
		}
		return nil
	}
	aDone := make(chan error, 1)
	go func() { aDone <- r.RunProbeDueOnce(context.Background()) }()
	<-propagationStarted
	beforeRearm, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	aAttempt, ok := beforeRearm.ProbeAttempts[aBinding.Instance]
	if !ok || aAttempt.AttemptID == "" || aAttempt.SendFenceSeq == 0 || (aAttempt.Phase != ProbeAttemptSending && aAttempt.Phase != ProbeAttemptSent) {
		t.Fatalf("A durable attempt before B re-arm = %#v", aAttempt)
	}
	if err = r.RefreshOneAuthID("b"); err != nil {
		t.Fatal(err)
	}
	bPending, ok := r.probeController.Window(bBinding.Instance, ProbeWindowLong)
	if !ok || bPending.State != ProbePendingCheck || !bPending.Deadline.IsZero() {
		t.Fatalf("refresh did not bootstrap B PendingCheck: window=%#v ok=%v", bPending, ok)
	}
	afterRearm, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := afterRearm.ProbeAttempts[aBinding.Instance]; !reflect.DeepEqual(got, aAttempt) {
		t.Fatalf("B re-arm changed A attempt/fence: got=%#v want=%#v", got, aAttempt)
	}
	r.wg.Wait() // B's production launch must observe A active before A is released.
	releaseA()
	if err = <-aDone; err != nil {
		t.Fatal(err)
	}

	counts := probePOSTCountsByAccount(host)
	if counts["acct-a"] != 1 || counts["acct-b"] != 1 {
		t.Fatalf("Probe POST counts = %#v, want one A and one B", counts)
	}
	if window, ok := r.probeController.Window(bBinding.Instance, ProbeWindowLong); !ok || window.State != ProbeConfirmed {
		t.Fatalf("in-memory B window = %#v, ok=%v; want Confirmed", window, ok)
	}
	runtimePersisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if window, ok := persisted.ProbeWindows[bBinding.Instance][ProbeWindowLong]; !ok || window.State != ProbeConfirmed {
		t.Fatalf("persisted B window = %#v, ok=%v; want Confirmed; runtime_windows=%#v file_windows=%#v attempts=%#v", window, ok, runtimePersisted.ProbeWindows, persisted.ProbeWindows, persisted.ProbeAttempts)
	}
	if len(persisted.ProbeAttempts) != 0 || persisted.ProbeAttemptSeq != 2 {
		t.Fatalf("terminal attempts/sequence = %#v/%d, want none/2", persisted.ProbeAttempts, persisted.ProbeAttemptSeq)
	}

	restartAdapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, restartAdapter, roster, path, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	if window, ok := restart.probeController.Window(bBinding.Instance, ProbeWindowLong); !ok || window.State != ProbeConfirmed {
		t.Fatalf("restarted B window = %#v, ok=%v; want Confirmed", window, ok)
	}
	if got := probePOSTCountsByAccount(host); got["acct-a"] != 1 || got["acct-b"] != 1 {
		t.Fatalf("restart changed Probe POST counts: %#v", got)
	}
}

func TestFirstObservedWeeklyLazyWindowConcurrentTriggersSingleFlight(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	entered := make(chan struct{})
	release := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-entered
	for i := 0; i < 4; i++ {
		if err := r.RunProbeDueOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	posts, urls := probePOSTCount(host)
	if posts != 1 {
		t.Fatalf("probe POST count during propagation = %d, want 1; urls=%v", posts, urls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if attempt, ok := persisted.ProbeAttempts[binding.Instance]; ok && nonterminalProbeAttempt(attempt) {
		t.Fatalf("nonterminal attempt survived: %#v", attempt)
	}
}

func TestProbeSnapshotsPreservesMissingUsage(t *testing.T) {
	reset := time.Date(2026, 7, 25, 22, 59, 55, 0, time.UTC)
	snapshot := probeSnapshots(ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: reset}})[ProbeWindowLong]
	if snapshot.Usage != nil {
		t.Fatalf("missing usage became %#v", *snapshot.Usage)
	}
}

func TestFirstObservedWeeklyLazyWindowMissingPrecheckUsageDoesNotSend(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	reset := now.Add(7 * 24 * time.Hour)
	missing := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host.mu.Lock()
	host.quota = [][]byte{missing, missing}
	host.mu.Unlock()

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	posts, urls := probePOSTCount(host)
	if posts != 0 {
		t.Fatalf("missing usage sent %d probe POSTs, want 0; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbeRetryWait {
		t.Fatalf("window = %#v, ok=%v; want RetryWait", window, ok)
	}
}

func TestSuccessfulQuotaRefreshRemovesAbsentFiveHourProbeState(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := newProbeFixtureHost()
	host.quota = [][]byte{
		[]byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		[]byte(`{}`),
	}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}

	if err := r.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		persisted, err := r.runtimeStore.PersistentSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("successful secondary-only refresh retained FiveHour Probe state: persisted=%#v attempts=%#v requests=%#v remaining_quota=%d snapshot=%#v", persisted.ProbeWindows[binding.Instance], persisted.ProbeAttempts[binding.Instance], host.requests, len(host.quota), r.state.Snapshot(now).Accounts)
	}
	snapshot := r.state.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	status, available, reason, _ := accountQueueState(snapshot.Accounts[0], now)
	if status != QueueStatusAvailable || !available || reason != "" {
		t.Fatalf("queue state = %s %v %q", status, available, reason)
	}
}

func TestProbeSequenceCleansMissingFiveHourAfterAttemptCompletes(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := newProbeFixtureHost()
	host.quota = [][]byte{
		[]byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		t.Fatal("completed Probe precheck retained absent FiveHour state")
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeAttempts[binding.Instance]; ok {
		t.Fatal("completed Probe attempt retained")
	}
}

func TestProductionProbeFinalStartDeniedEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{
		auth:  pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)},
		quota: [][]byte{[]byte(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000}}}`)},
	}
	r := newDueProbeRuntime(t, now, host)
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	getStarted, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	entries := []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterDegraded, BackgroundAllowed: true, Entries: entries})
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-getStarted
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Entries: entries})
	close(releaseGet)
	if err := <-done; !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("Probe HTTP started after FailClosed publication: %#v", requests)
	}
}

func TestFailClosedHoldsAndRecoveryRecomputesProbe(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "prepared", Phase: ProbeAttemptPrepared, Windows: []ProbeWindowKind{ProbeWindowFiveHour}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	failClosed := ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, BackgroundAllowed: false, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	r.ObserveRosterLifecycle(failClosed)
	if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("FailClosed Probe err=%v", err)
	}
	if len(host.urls) != 0 {
		t.Fatalf("FailClosed started requests=%v", host.urls)
	}
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || w.Deadline != (time.Time{}) {
		t.Fatalf("held window=%#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok = persisted.ProbeAttempts[b.Instance]; ok {
		t.Fatalf("prepared attempt retained in roster hold: %#v", persisted.ProbeAttempts[b.Instance])
	}

	recovered := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 1, LifecycleRevision: 3, Entries: failClosed.Entries}
	if err = r.PublishAuthoritativeRoster(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	w, ok = r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbePendingCheck || !w.Deadline.IsZero() || !w.Baseline.SuspectedLazy {
		t.Fatalf("recomputed window=%#v ok=%v", w, ok)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := host.urls; len(got) != 3 || got[0] != r.state.Config().QuotaEndpoint || got[1] != codexResetProbeEndpoint || got[2] != r.state.Config().QuotaEndpoint {
		t.Fatalf("recovery requests=%v", got)
	}
}

func TestProductionRosterRecoveryClearsPendingDeadlineAndRunsAutomatically(t *testing.T) {
	now := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)
	reset := now.Add(-time.Hour)
	unchanged := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{unchanged}
	r := newDueProbeRuntime(t, now, host)
	binding, _ := r.bindings.Lookup("a")
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingRoster, Baseline: ResetProbeBaseline(reset, 20, 5*time.Hour)})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	failClosed := ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, BackgroundAllowed: false, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	r.ObserveRosterLifecycle(failClosed)
	r.Start() // records startRequested while fail-closed without launching work

	r.probeRunMu.Lock()
	recovered := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 1, LifecycleRevision: 3, Entries: failClosed.Entries}
	err := r.PublishAuthoritativeRoster(context.Background(), recovered)
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	badDeadline := !ok || window.State != ProbePendingCheck || !window.Deadline.IsZero()
	r.probeRunMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if badDeadline {
		t.Fatalf("roster recovery window = %#v, ok=%v; want PendingCheck with zero deadline", window, ok)
	}
	t.Cleanup(r.Stop)
	waitForProbeHTTP(t, host, 1)
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].URL != r.state.Config().QuotaEndpoint {
		t.Fatalf("roster recovery did not run automatically as one read-only GET: %#v", requests)
	}
}

func TestProductionAuthRecoveryClearsPendingDeadlineAndRunsAutomatically(t *testing.T) {
	now := time.Date(2026, 8, 1, 18, 10, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	unchanged := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{unchanged}
	r := newDueProbeRuntime(t, now, host)
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	binding, _ := r.bindings.Lookup("a")
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeAuthBlocked, AuthBlockedAtLogin: binding.Login, Baseline: ResetProbeBaseline(reset, 20, 5*time.Hour)})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	if err := r.bindings.ObserveExternalLogin("a", binding.Login+1, binding.Fingerprint); err != nil {
		t.Fatal(err)
	}
	releaseOnce := sync.Once{}
	release := func() { releaseOnce.Do(func() { close(host.releaseGet) }) }
	t.Cleanup(release)
	r.Start()
	t.Cleanup(r.Stop)
	select {
	case <-host.getStarted:
	case <-time.After(time.Second):
		t.Fatal("auth recovery did not run automatically")
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	badDeadline := !ok || window.State != ProbePendingCheck || !window.Deadline.IsZero()
	release()
	if badDeadline {
		t.Fatalf("auth recovery window = %#v, ok=%v; want in-flight PendingCheck with zero deadline", window, ok)
	}
}

func TestFailClosedHoldPreservesSentAttemptOnlyForItsWindows(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 40, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	r.probeController.SetWindow(b.Instance, ProbeWindowLong, ProbeWindow{State: ProbeRetryWait, Deadline: now.Add(time.Minute), Baseline: UsageOnlyProbeBaseline(55, now.Add(time.Minute))})
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows = r.probeController.Snapshot()
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "sent", Phase: ProbeAttemptSent, Windows: []ProbeWindowKind{ProbeWindowFiveHour}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	five, _ := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	long, _ := r.probeController.Window(b.Instance, ProbeWindowLong)
	if five.State != ProbeSentUnknown || long.State != ProbeWaitingRoster {
		t.Fatalf("five=%s long=%s", five.State, long.State)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProbeAttempts[b.Instance].AttemptID != "sent" {
		t.Fatalf("sent attempt not retained: %#v", persisted.ProbeAttempts[b.Instance])
	}
}

func TestActiveProbeLifecycleDenialPreservesRosterHold(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 42, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	close(releaseGet)
	if err := <-done; err == nil {
		t.Fatal("active Probe unexpectedly succeeded after FailClosed")
	}
	time.Sleep(20 * time.Millisecond)
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || !w.Deadline.IsZero() {
		t.Fatalf("lifecycle-denied Probe escaped roster hold: %#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("persisted lifecycle-denied state=%#v", got)
	}
}

func TestActiveProbeParseFailureAfterFailClosedPreservesRosterHold(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 43, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	host.mu.Lock()
	host.auth.JSON = json.RawMessage(`{"access_token":`)
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	close(releaseGet)
	if err := <-done; err == nil {
		t.Fatal("parse failure unexpectedly succeeded")
	}
	time.Sleep(20 * time.Millisecond)
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || !w.Deadline.IsZero() {
		t.Fatalf("parse failure escaped roster hold: %#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("persisted parse failure state=%#v", got)
	}
}

func TestFailClosedRetriesProbeHoldPersistence(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 44, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	fail := true
	r.runtimeStore.hooks.Fail = func(op string) error {
		if fail && op == "backup-write" {
			return errors.New("hold persistence failed")
		}
		return nil
	}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour].State == ProbeWaitingRoster {
		t.Fatal("injected failure unexpectedly persisted roster hold")
	}
	fail = false
	if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("retry path err=%v", err)
	}
	persisted, err = r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("retried hold not durable: %#v", got)
	}
}

func TestRemovedProbeLateGetDoesNotRecreateState(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 45, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "auth.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	entries := []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)},
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 1, Entries: entries}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	r, err := NewProductionQuotaRefresher(host, NewPluginState(cfg), adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	b, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(b.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, fiveHourSeconds*time.Second)})
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.gateAuthIndex = "b"
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	replacement := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 2, Entries: entries[:1]}
	if err = r.PublishAuthoritativeRoster(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	close(releaseGet)
	<-done
	time.Sleep(20 * time.Millisecond)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeWindows[b.Instance]; ok {
		t.Fatalf("late removed Probe recreated windows=%v", persisted.ProbeWindows[b.Instance])
	}
	if _, ok := persisted.ProbeAttempts[b.Instance]; ok {
		t.Fatalf("late removed Probe recreated attempt=%v", persisted.ProbeAttempts[b.Instance])
	}
	if len(host.urls) != 0 {
		t.Fatalf("removed Probe started OpenAI requests=%v", host.urls)
	}
}

func TestProductionProvisionalProbeMarkerEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	if provisional == nil || !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests=%#v", requests)
	}
	for i, req := range requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q request=%#v", i, got, req)
		}
	}
}

func TestProductionProvisionalVerificationRejectsMismatchWithoutOpenAI(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"original","id_token":"` + idToken + `"}`)}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	confirmed := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Generation: 3, ConfirmedAt: now, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	if err = r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.auth.JSON = json.RawMessage(`{"access_token":"access","refresh_token":"changed","id_token":"` + idToken + `"}`)
	host.mu.Unlock()
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional snapshot")
	}
	if r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatal("fingerprint mismatch verified")
	}
	r.ObserveRosterLifecycle(*provisional)
	if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 0 || len(host.urls) != 0 {
		t.Fatalf("mismatch made OpenAI calls requests=%#v urls=%v", host.requests, host.urls)
	}
}

func TestProductionProvisionalVerificationRechecksActualPrecheckFingerprint(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	original := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"original","id_token":"` + idToken + `"}`)}
	changed := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"changed-access","refresh_token":"changed","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: original}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	host.mu.Lock()
	host.authReads = []pluginapi.HostAuthGetResponse{original, changed}
	host.mu.Unlock()
	if !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatal("initial verification failed")
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrProvisionalFingerprintMismatch) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("rotated precheck made OpenAI calls: %#v", requests)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := r.bindings.Lookup("a")
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster {
		t.Fatalf("mismatch window=%#v", got)
	}
}

func TestProductionProvisionalRequestExpiryDuringPrecheckReturnsWaitingRoster(t *testing.T) {
	confirmedAt := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	initialNow := confirmedAt.Add(provisionalMaxAge - time.Nanosecond)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	auth := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: auth, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, confirmedAt.Add(-time.Hour).Format(time.RFC3339)))}}
	r := newDueProbeRuntime(t, initialNow, host)
	var clock atomic.Int64
	clock.Store(initialNow.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = confirmedAt
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	provisional := r.ProvisionalRoster()
	if provisional == nil || !r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, release := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	clock.Store(confirmedAt.Add(provisionalMaxAge).UnixNano())
	close(release)
	if err := <-done; !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("expired provisional made requests=%#v", requests)
	}
	binding, _ := r.bindings.Lookup("a")
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("expired provisional window=%#v", got)
	}
}

func TestProductionProvisionalVerificationExpiryDuringGetAuthIssuesNoPermit(t *testing.T) {
	confirmedAt := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	initialNow := confirmedAt.Add(provisionalMaxAge - time.Nanosecond)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	auth := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: auth}
	r := newDueProbeRuntime(t, initialNow, host)
	var clock atomic.Int64
	clock.Store(initialNow.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = confirmedAt
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, release := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) }()
	<-started
	clock.Store(confirmedAt.Add(provisionalMaxAge).UnixNano())
	close(release)
	if <-done {
		t.Fatal("expired GetAuth verification succeeded")
	}
	r.provisionalMu.Lock()
	permit := r.provisionalPermit
	r.provisionalMu.Unlock()
	if permit {
		t.Fatal("expired verification issued permit")
	}
}

func TestProductionProvisionalConfigDisableLinearizesWithHostStart(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &sequenceProbeHost{doStarted: make(chan struct{}), releaseDo: make(chan struct{})}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	cfg.ProbeOnProvisionalRoster = true
	r := NewQuotaRefresher(host, NewPluginState(cfg), func() time.Time { return now })
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true, ConfirmedAt: now})
	requestDone := make(chan error, 1)
	go func() {
		_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint}, true)
		requestDone <- err
	}()
	<-host.doStarted
	disabled := make(chan struct{})
	go func() {
		next := r.state.Config()
		next.ProbeOnProvisionalRoster = false
		r.state.ReplaceConfig(next)
		close(disabled)
	}()
	select {
	case <-disabled:
		t.Fatal("config disable crossed in-flight host.Do linearization point")
	case <-time.After(50 * time.Millisecond):
	}
	close(host.releaseDo)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("config disable remained blocked after host.Do")
	}
	if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint}, true); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("post-disable request err=%v", err)
	}
}

func TestProductionProvisionalRequestMarkerEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	confirmed.Generation = 5
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	if provisional == nil || !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests=%#v", requests)
	}
	for i, req := range requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q", i, got)
		}
	}
}

func TestProductionProvisionalRecoveryRiskStartRunsVerifiedProbe(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	r.ObserveRosterLifecycle(*provisional)
	controller := NewRosterController(RosterControllerOptions{
		Now:                func() time.Time { return now },
		Provisional:        provisional,
		ProbeOnProvisional: true,
		VerifyProvisional:  r.VerifyConfiguredProvisionalRoster,
		Observe:            r.ObserveRosterLifecycle,
	})
	refresherMu.Lock()
	globalRosterController = controller
	refresherMu.Unlock()
	r.Start()
	t.Cleanup(r.Stop)
	deadline := time.Now().Add(2 * time.Second)
	for {
		host.mu.Lock()
		count := len(host.requests)
		host.mu.Unlock()
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified risk start did not run Probe, requests=%d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for i, req := range host.requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q", i, got)
		}
	}
}

func TestProductionProbeDetectsExternalResetWhileNormalRefreshDormant(t *testing.T) {
	now := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	oldReset := now.Add(2 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{
		State:    ProbePendingCheck,
		Baseline: ResetProbeBaseline(oldReset, 20, 5*time.Hour),
	})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	if r.refreshController.Mode(now) != RefreshModeDormant {
		t.Fatal("normal refresh not dormant")
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("external reset sent %d Probe POSTs, want 1; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || window.State != ProbeConfirmed {
		t.Fatalf("window = %#v, ok=%v; want Confirmed", window, ok)
	}
	if r.refreshController.Mode(now) != RefreshModeDormant {
		t.Fatal("Probe changed normal refresh out of Dormant")
	}
}

func TestProductionProbeStrictAuthorizationUsesCurrentWindowEvidence(t *testing.T) {
	now := time.Date(2026, 8, 1, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		kind           ProbeWindowKind
		baselineKind   WindowKind
		baselineLength time.Duration
		reset          time.Time
		payload        string
	}{
		{
			name:           "changed explicit duration",
			kind:           ProbeWindowFiveHour,
			baselineKind:   WindowFiveHour,
			baselineLength: 5 * time.Hour,
			reset:          now.Add(5 * time.Hour),
			payload:        fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":21600,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)),
		},
		{
			name:           "current monthly duration missing",
			kind:           ProbeWindowLong,
			baselineKind:   WindowMonthly,
			baselineLength: 30 * 24 * time.Hour,
			reset:          now.Add(30 * 24 * time.Hour),
			payload:        fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339), now.Add(30*24*time.Hour).Format(time.RFC3339)),
		},
		{
			name:           "changed long-window kind",
			kind:           ProbeWindowLong,
			baselineKind:   WindowWeekly,
			baselineLength: 7 * 24 * time.Hour,
			reset:          now.Add(17 * 24 * time.Hour),
			payload:        fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":2592000,"reset_at":%q}}}`, now.Add(30*24*time.Hour).Format(time.RFC3339)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := newProbeFixtureHost()
			host.quota = [][]byte{[]byte(tt.payload), []byte(tt.payload)}
			r := newDueProbeRuntime(t, now, host)
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
			base := ResetProbeBaseline(tt.reset, 0, tt.baselineLength)
			base.WindowKind = tt.baselineKind
			base.SuspectedLazy = true
			r.probeController.SetWindow(binding.Instance, tt.kind, ProbeWindow{State: ProbePendingCheck, Baseline: base})
			if err := r.persistProbeWindows(); err != nil {
				t.Fatal(err)
			}

			if err := r.RunProbeDueOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts, urls := probePOSTCount(host); posts != 0 {
				t.Fatalf("stale baseline authorized %d compact POSTs; urls=%v", posts, urls)
			}
			window, ok := r.probeController.Window(binding.Instance, tt.kind)
			if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(30*time.Minute)) {
				t.Fatalf("strict rejection did not bound read-only reschedule: %#v, ok=%v", window, ok)
			}
		})
	}
}

func TestProductionLegacyEmptyKindBaselineMigratesOnlyFromCompatibleCurrentEvidence(t *testing.T) {
	now := time.Date(2026, 8, 1, 17, 20, 0, 0, time.UTC)
	tests := []struct {
		name     string
		payloads [][]byte
		wantKind WindowKind
		wantPOST int
	}{
		{
			name: "weekly baseline rejects strict monthly payload",
			payloads: [][]byte{
				[]byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":2592000,"reset_at":%q}}}`, now.Add(30*24*time.Hour).Format(time.RFC3339))),
				[]byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":2592000,"reset_at":%q}}}`, now.Add(30*24*time.Hour).Format(time.RFC3339))),
			},
			wantPOST: 0,
		},
		{
			name: "weekly baseline migrates from strict weekly payload",
			payloads: [][]byte{
				[]byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, now.Add(7*24*time.Hour).Format(time.RFC3339))),
				[]byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, now.Add(7*24*time.Hour).Format(time.RFC3339))),
			},
			wantKind: WindowWeekly,
			wantPOST: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			cfg := DefaultConfig()
			cfg.EnableResetProbe = true
			roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}

			seedState := NewPluginState(cfg)
			seedState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			seedHost := newProbeFixtureHost()
			seedAdapter := &rosterCredentialHost{host: seedHost, roster: roster}
			seed, err := NewProductionQuotaRefresher(seedHost, seedState, seedAdapter, roster, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			seedAdapter.bindings = seed.bindings
			binding, _, err := seed.BootstrapBinding(context.Background(), "a")
			if err != nil {
				t.Fatal(err)
			}
			legacy := ResetProbeBaseline(now.Add(-7*24*time.Hour), 0, 7*24*time.Hour)
			legacy.SuspectedLazy = true
			seed.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: legacy})
			if err = seed.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
				t.Fatal(err)
			}
			if persisted, err := NewStateStore(seed.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot(); err != nil {
				t.Fatal(err)
			} else if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong].Baseline; got.WindowKind != "" || got.WindowLength != 7*24*time.Hour || !got.SuspectedLazy {
				t.Fatalf("serialized legacy baseline = %#v", got)
			}

			restartState := NewPluginState(cfg) // deliberately has no quota cache
			restartState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			restartHost := newProbeFixtureHost()
			restartHost.quota = tt.payloads
			restartAdapter := &rosterCredentialHost{host: restartHost, roster: roster}
			restart, err := NewProductionQuotaRefresher(restartHost, restartState, restartAdapter, roster, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restart.Stop)
			restartAdapter.bindings = restart.bindings
			restart.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }

			if err = restart.RunProbeDueOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts, urls := probePOSTCount(restartHost); posts != tt.wantPOST {
				t.Fatalf("legacy empty-kind baseline sent %d compact POSTs, want %d; urls=%v", posts, tt.wantPOST, urls)
			}
			window, ok := restart.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || window.Deadline.IsZero() && tt.wantPOST == 0 {
				t.Fatalf("legacy rejection did not retain a bounded schedule: %#v, ok=%v", window, ok)
			}
			if tt.wantKind != "" && window.Baseline.WindowKind != tt.wantKind {
				t.Fatalf("migrated baseline kind = %q, want %q", window.Baseline.WindowKind, tt.wantKind)
			}
		})
	}
}

func TestProductionProbeTimerWakesDormantLoopForRearmAndRepeatedObservation(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)
	realStart := time.Now()
	clockNow := func() time.Time { return baseNow.Add(time.Since(realStart)) }
	newReset := baseNow.Add(5 * time.Hour)
	unchanged := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{unchanged, unchanged, unchanged}
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clockNow
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	confirmed := ResetProbeBaseline(baseNow.Add(4*time.Hour), 10, 5*time.Hour)
	confirmed.WindowKind = WindowFiveHour
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeConfirmed, Baseline: confirmed})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	r.Start()
	t.Cleanup(r.Stop)
	r.wg.Wait()
	if mode := r.refreshController.Mode(r.now()); mode != RefreshModeDormant {
		t.Fatalf("normal refresh mode = %q, want Dormant", mode)
	}

	host.gateDoURL = r.state.Config().QuotaEndpoint
	host.doStarted = make(chan struct{})
	host.releaseDo = make(chan struct{})
	seconds := int64(5 * time.Hour / time.Second)
	used := 20.0
	observation := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, LimitWindowSeconds: &seconds, ResetAt: newReset}}

	r.mu.Lock()
	rearmBefore := r.now()
	rearmDone := make(chan error, 1)
	go func() { rearmDone <- r.reconcileObservedProbeWindows(binding.Instance, observation) }()
	var first ProbeWindow
	deadline := time.Now().Add(time.Second)
	for {
		first, ok = r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
		if ok && first.State == ProbeWaitingReset && !first.Deadline.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			r.mu.Unlock()
			t.Fatal("ordinary re-arm did not publish WaitingReset")
		}
		time.Sleep(time.Millisecond)
	}
	rearmAfter := r.now()
	firstDeadlineCorrect := !first.Deadline.Before(rearmBefore.Add(30*time.Minute)) && !first.Deadline.After(rearmAfter.Add(30*time.Minute))
	first.Deadline = r.now().Add(40 * time.Millisecond) // compress only wall-clock waiting; the authoritative +30m was asserted above
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, first)
	r.mu.Unlock()
	if err := <-rearmDone; err != nil {
		t.Fatal(err)
	}
	if !firstDeadlineCorrect {
		t.Fatalf("ordinary re-arm deadline = %s, want observation time +30m", first.Deadline)
	}

	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("dormant production timer did not run re-armed Probe GET")
	}
	r.mu.Lock() // hold Probe completion's recomputation until its new +30m deadline is observed and compressed
	close(host.releaseDo)
	var second ProbeWindow
	deadline = time.Now().Add(time.Second)
	for {
		second, ok = r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
		if ok && second.State == ProbeWaitingReset && second.Deadline.Sub(r.now()) > 29*time.Minute {
			break
		}
		if time.Now().After(deadline) {
			r.mu.Unlock()
			t.Fatalf("first unchanged observation did not schedule another +30m deadline: %#v, ok=%v", second, ok)
		}
		time.Sleep(time.Millisecond)
	}
	second.Deadline = r.now().Add(40 * time.Millisecond)
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, second)
	r.mu.Unlock()

	waitForProbeHTTP(t, host, 2)
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodGet {
		t.Fatalf("production timer requests = %#v, want GET at +30m and +60m", requests)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("unchanged timer observations sent %d compact POSTs; urls=%v", posts, urls)
	}
	if mode := r.refreshController.Mode(r.now()); mode != RefreshModeDormant {
		t.Fatalf("Probe timer changed normal refresh mode to %q", mode)
	}
}

func TestProductionProbeEarlyErrorsRetryOnlyAtBoundedDeadline(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)
	for _, boundary := range []string{"roster_hold", "bootstrap", "state_read"} {
		t.Run(boundary, func(t *testing.T) {
			clock := &s7TestClock{now: baseNow}
			host := newProbeFixtureHost()
			host.quota = [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, baseNow.Add(-time.Hour).Format(time.RFC3339)))}
			r := newProductionLazyRefreshRuntime(t, baseNow, host)
			r.now = clock.Now
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(r.Stop) }
			t.Cleanup(stop)
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
			baseline := ResetProbeBaseline(baseNow.Add(-time.Hour), 0, 5*time.Hour)
			baseline.WindowKind = WindowFiveHour
			if boundary == "bootstrap" {
				baseline = ResetProbeBaseline(baseNow.Add(5*time.Hour), 20, 0)
				used := 20.0
				seconds := int64(5 * time.Hour / time.Second)
				r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", LastSuccessAt: baseNow, Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, LimitWindowSeconds: &seconds, ResetAt: baseline.ResetAt}}})
			}
			seedWindow := ProbeWindow{State: ProbeWaitingReset, Baseline: baseline, Deadline: baseNow.Add(-time.Minute)}
			r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, seedWindow)
			if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
				t.Fatal(err)
			}
			var recoveryEvidence *ProbeAttempt
			var recoveryCeiling uint64
			if boundary == "state_read" {
				host.quota = append(host.quota, append([]byte(nil), host.quota[0]...))
				sendFence, fenceErr := r.probeFence.Next()
				if fenceErr != nil {
					t.Fatal(fenceErr)
				}
				fenceState, snapshotErr := r.runtimeStore.PersistentSnapshot()
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				recoveryCeiling = fenceState.ReservedCeiling
				attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "durable-sent-before-read-error", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: baseNow.Add(-time.Minute), VerifyNotBefore: baseNow, SuppressUntil: baseNow.Add(10 * time.Minute)}
				recoveryEvidence = &attempt
				longBaseline := ResetProbeBaseline(baseNow.Add(7*24*time.Hour), 0, 7*24*time.Hour)
				longBaseline.WindowKind = WindowWeekly
				r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeSentUnknown, Baseline: longBaseline, AttemptID: attempt.AttemptID})
				if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
					state.ProbeAttempts[binding.Instance] = attempt
					state.ProbeWindows[binding.Instance] = r.probeController.Snapshot()[binding.Instance]
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			var boundaryCalls atomic.Int32
			var failBoundary atomic.Bool
			failBoundary.Store(true)
			expectedInitialCalls := int32(1)
			switch boundary {
			case "roster_hold":
				r.probeHoldMu.Lock()
				r.probeHoldPending = true
				r.probeHoldErr = errors.New("injected roster hold failure")
				r.probeHoldMu.Unlock()
				r.runtimeStore.hooks.Fail = func(op string) error {
					if op == "backup-write" {
						boundaryCalls.Add(1)
						if failBoundary.Load() {
							return errors.New("injected roster hold persistence failure")
						}
					}
					return nil
				}
			case "bootstrap":
				r.runtimeStore.hooks.Fail = func(op string) error {
					if op == "backup-write" {
						boundaryCalls.Add(1)
						if failBoundary.Load() {
							return errors.New("injected bootstrap persistence failure")
						}
					}
					return nil
				}
			case "state_read":
				originalRead := r.runtimeStore.hooks.ReadFile
				r.runtimeStore.loaded = false
				r.runtimeStore.hooks.ReadFile = func(name string) ([]byte, error) {
					if name == r.runtimeStore.path {
						boundaryCalls.Add(1)
						if failBoundary.Load() {
							return nil, os.ErrPermission
						}
					}
					return originalRead(name)
				}
			}

			r.Start()
			deadline := time.Now().Add(2 * time.Second)
			for boundaryCalls.Load() < expectedInitialCalls && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := boundaryCalls.Load(); got != expectedInitialCalls {
				t.Fatalf("initial %s boundary calls = %d, want %d", boundary, got, expectedInitialCalls)
			}
			time.Sleep(40 * time.Millisecond)
			if got := boundaryCalls.Load(); got != expectedInitialCalls {
				t.Fatalf("%s self-excited before retry deadline: boundary calls=%d, want %d", boundary, got, expectedInitialCalls)
			}
			if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
				t.Fatalf("%s retry schedule = (%s, %v), want exactly +1m", boundary, delay, scheduled)
			}
			persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; !reflect.DeepEqual(got, seedWindow) {
				t.Fatalf("%s early error changed durable window before retry: got %#v want %#v", boundary, got, seedWindow)
			}
			if recoveryEvidence != nil {
				if got := persisted.ProbeAttempts[binding.Instance]; !reflect.DeepEqual(got, *recoveryEvidence) || persisted.ReservedCeiling != recoveryCeiling {
					t.Fatalf("state-read error changed durable attempt/fence: attempt=%#v ceiling=%d", got, persisted.ReservedCeiling)
				}
			}

			failBoundary.Store(false)
			clock.Set(baseNow.Add(time.Minute))
			r.wakeRefreshLoop()
			deadline = time.Now().Add(2 * time.Second)
			for boundaryCalls.Load() == expectedInitialCalls && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := boundaryCalls.Load(); got <= expectedInitialCalls {
				t.Fatalf("%s did not retry at bounded deadline; boundary calls=%d", boundary, got)
			}
			if recoveryEvidence != nil {
				deadline = time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					state, snapshotErr := r.runtimeStore.PersistentSnapshot()
					if snapshotErr == nil {
						if _, retained := state.ProbeAttempts[binding.Instance]; !retained {
							break
						}
					}
					time.Sleep(time.Millisecond)
				}
				state, snapshotErr := r.runtimeStore.PersistentSnapshot()
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				if _, retained := state.ProbeAttempts[binding.Instance]; retained {
					t.Fatalf("bounded retry did not consume durable recovery attempt: %#v", state.ProbeAttempts[binding.Instance])
				}
				if state.ReservedCeiling < recoveryCeiling {
					t.Fatalf("bounded retry regressed durable fence ceiling to %d", state.ReservedCeiling)
				}
			}
			if posts, urls := probePOSTCount(host); posts != 0 {
				t.Fatalf("%s bounded retry sent %d compact POSTs; urls=%v", boundary, posts, urls)
			}

			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("production Stop found accumulated Probe goroutines")
			}
		})
	}
}

func TestProductionProbeEarlyErrorConcurrentStartHonorsRetryAdmission(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 10, 0, 0, time.UTC)
	for _, boundary := range []string{"roster_hold", "bootstrap", "state_read"} {
		t.Run(boundary, func(t *testing.T) {
			clock := &s7TestClock{now: baseNow}
			host := newProbeFixtureHost()
			r := newProductionLazyRefreshRuntime(t, baseNow, host)
			r.now = clock.Now
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(r.Stop) }
			t.Cleanup(stop)
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
			baseline := ResetProbeBaseline(baseNow.Add(5*time.Hour), 0, 5*time.Hour)
			baseline.WindowKind = WindowFiveHour
			baseline.SuspectedLazy = true
			r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: baseline})
			if boundary == "bootstrap" {
				baseline = ResetProbeBaseline(baseNow.Add(5*time.Hour), 20, 0)
				r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset, Baseline: baseline, Deadline: baseNow.Add(-time.Minute)})
				longBaseline := ResetProbeBaseline(baseNow.Add(7*24*time.Hour), 0, 7*24*time.Hour)
				longBaseline.WindowKind = WindowWeekly
				longBaseline.SuspectedLazy = true
				r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: longBaseline})
				used := 20.0
				seconds := int64(5 * time.Hour / time.Second)
				r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", LastSuccessAt: baseNow, Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, LimitWindowSeconds: &seconds, ResetAt: baseline.ResetAt}}})
			}
			if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
				t.Fatal(err)
			}

			entered := make(chan struct{})
			release := make(chan struct{})
			var boundaryCalls atomic.Int32
			expectedCalls := int32(1)
			switch boundary {
			case "roster_hold", "state_read":
				if boundary == "roster_hold" {
					r.probeHoldMu.Lock()
					r.probeHoldPending = true
					r.probeHoldErr = errors.New("injected roster hold failure")
					r.probeHoldMu.Unlock()
				}
				originalRead := r.runtimeStore.hooks.ReadFile
				r.runtimeStore.loaded = false
				r.runtimeStore.hooks.ReadFile = func(name string) ([]byte, error) {
					if name == r.runtimeStore.path {
						if boundaryCalls.Add(1) == 1 {
							close(entered)
							<-release
						}
						return nil, os.ErrPermission
					}
					return originalRead(name)
				}
			case "bootstrap":
				r.runtimeStore.hooks.Fail = func(op string) error {
					if op == "backup-write" {
						call := boundaryCalls.Add(1)
						if call == 1 {
							close(entered)
							<-release
						}
						return errors.New("injected bootstrap persistence failure")
					}
					return nil
				}
			}

			r.Start()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("production launch did not reach blocked early-error boundary")
			}
			r.Start() // concurrent running Start must fold into the bounded retry
			close(release)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				r.probeRunStateMu.Lock()
				active := r.probeLaunchActive
				r.probeRunStateMu.Unlock()
				if !active && boundaryCalls.Load() >= expectedCalls {
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(40 * time.Millisecond)
			if got := boundaryCalls.Load(); got != expectedCalls {
				t.Fatalf("%s concurrent Start bypassed active-error backoff: calls=%d want=%d", boundary, got, expectedCalls)
			}
			if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
				t.Fatalf("%s concurrent-error retry schedule = (%s, %v), want exactly +1m", boundary, delay, scheduled)
			}

			r.Start() // inactive running Start must still respect the future retryAt
			time.Sleep(40 * time.Millisecond)
			if got := boundaryCalls.Load(); got != expectedCalls {
				t.Fatalf("%s running Start bypassed future retry admission: calls=%d want=%d", boundary, got, expectedCalls)
			}

			clock.Set(baseNow.Add(time.Minute))
			r.wakeRefreshLoop()
			deadline = time.Now().Add(2 * time.Second)
			for boundaryCalls.Load() < 2*expectedCalls && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(40 * time.Millisecond)
			if got := boundaryCalls.Load(); got != 2*expectedCalls {
				t.Fatalf("%s bounded timer retry calls=%d want=%d", boundary, got, 2*expectedCalls)
			}
			if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
				t.Fatalf("%s second bounded retry schedule = (%s, %v), want exactly +1m", boundary, delay, scheduled)
			}

			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("concurrent early-error retry accumulated Probe goroutines")
			}
		})
	}
}

func TestProductionProbeAdmittedRecoveryRevalidatesOwnerRetryBeforeBoundary(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 15, 0, 0, time.UTC)
	clock := &s7TestClock{now: baseNow}
	reset := baseNow.Add(7 * 24 * time.Hour)
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{active}
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	sendFence, err := r.probeFence.Next()
	if err != nil {
		t.Fatal(err)
	}
	attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "admitted-wrapper-owner-retry", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: baseNow.Add(-time.Minute), VerifyNotBefore: baseNow, SuppressUntil: baseNow.Add(10 * time.Minute)}
	baseline := ResetProbeBaseline(reset, 0, 7*24*time.Hour)
	baseline.WindowKind = WindowWeekly
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeSentUnknown, Baseline: baseline, AttemptID: attempt.AttemptID})
	if _, err = r.runtimeStore.Update(func(state *PersistentState) error {
		state.ProbeAttempts[binding.Instance] = attempt
		state.ProbeWindows[binding.Instance] = r.probeController.Snapshot()[binding.Instance]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	originalRead := r.runtimeStore.hooks.ReadFile
	r.runtimeStore.loaded = false
	entered := make(chan struct{})
	release := make(chan struct{})
	var boundaryCalls atomic.Int32
	var failBoundary atomic.Bool
	failBoundary.Store(true)
	r.runtimeStore.hooks.ReadFile = func(name string) ([]byte, error) {
		if name == r.runtimeStore.path && !r.runtimeStore.loaded {
			if boundaryCalls.Add(1) == 1 {
				close(entered)
				<-release
			}
			if failBoundary.Load() {
				return nil, os.ErrPermission
			}
		}
		return originalRead(name)
	}

	directDone := make(chan error, 1)
	go func() { directDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("direct recovery did not reach blocked state-read boundary")
	}
	r.Start() // admits a recover-first production wrapper while direct owner holds probeRunMu
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.probeRunStateMu.Lock()
		launchActive := r.probeLaunchActive
		r.probeRunStateMu.Unlock()
		if launchActive {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.probeRunStateMu.Lock()
	launchActive := r.probeLaunchActive
	r.probeRunStateMu.Unlock()
	if !launchActive {
		close(release)
		t.Fatal("production recover-first wrapper was not admitted")
	}
	close(release)
	select {
	case err = <-directDone:
		if err == nil {
			t.Fatal("direct recovery state-read error was not surfaced")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("direct recovery did not publish bounded retry")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.probeRunStateMu.Lock()
		launchActive = r.probeLaunchActive
		r.probeRunStateMu.Unlock()
		if !launchActive {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
	if got := boundaryCalls.Load(); got != 1 {
		t.Fatalf("already-admitted wrapper crossed owner backoff: boundary calls=%d want=1", got)
	}
	if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
		t.Fatalf("owner retry schedule = (%s, %v), want exactly +1m", delay, scheduled)
	}

	failBoundary.Store(false)
	clock.Set(baseNow.Add(time.Minute))
	r.wakeRefreshLoop()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, snapshotErr := r.runtimeStore.PersistentSnapshot()
		if snapshotErr == nil {
			if _, retained := state.ProbeAttempts[binding.Instance]; !retained && boundaryCalls.Load() >= 2 {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if got := boundaryCalls.Load(); got != 2 {
		t.Fatalf("bounded timer recovery boundary calls=%d want=2", got)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if retained := persisted.ProbeAttempts[binding.Instance]; retained.AttemptID != "" {
		t.Fatalf("bounded timer recovery retained attempt: %#v", retained)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; got.State == ProbeSentUnknown {
		t.Fatalf("bounded timer recovery left nonterminal window: %#v", got)
	}
	if posts, urls := probePOSTCount(host); posts != 0 {
		t.Fatalf("bounded timer recovery resent compact: posts=%d urls=%v", posts, urls)
	}
}

func TestProductionProbeWrapperErrorPreservesNewerOwnerRetryDeadline(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 17, 0, 0, time.UTC)
	clock := &probeNthCaptureClock{now: baseNow}
	reset := baseNow.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	baseline := ResetProbeBaseline(reset, 0, 5*time.Hour)
	baseline.WindowKind = WindowFiveHour
	baseline.SuspectedLazy = true
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: baseline})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
		t.Fatal(err)
	}

	originalRead := r.runtimeStore.hooks.ReadFile
	r.runtimeStore.loaded = false
	wrapperEntered := make(chan struct{})
	releaseWrapper := make(chan struct{})
	var boundaryCalls atomic.Int32
	var failBoundary atomic.Bool
	failBoundary.Store(true)
	r.runtimeStore.hooks.ReadFile = func(name string) ([]byte, error) {
		if name == r.runtimeStore.path && !r.runtimeStore.loaded {
			if boundaryCalls.Add(1) == 1 {
				close(wrapperEntered)
				<-releaseWrapper
			}
			if failBoundary.Load() {
				return nil, os.ErrPermission
			}
		}
		return originalRead(name)
	}

	r.launchProbe(false)
	select {
	case <-wrapperEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("production wrapper did not reach state-read error boundary")
	}
	finishReached, releaseFinish := clock.ArmNthCall(2) // owner-error clock, then finish clock
	close(releaseWrapper)
	select {
	case <-finishReached:
	case <-time.After(2 * time.Second):
		t.Fatal("production wrapper did not capture finish clock")
	}

	clock.Set(baseNow.Add(30 * time.Second))
	directDone := make(chan error, 1)
	go func() { directDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	select {
	case err := <-directDone:
		if err == nil {
			close(releaseFinish)
			t.Fatal("newer direct-owner state-read error was not surfaced")
		}
	case <-time.After(2 * time.Second):
		close(releaseFinish)
		t.Fatal("newer direct owner did not publish retry")
	}
	wantRetryAt := baseNow.Add(90 * time.Second)
	r.probeRunStateMu.Lock()
	newerRetryAt := r.probeRetryAt
	ownerPending := r.probeOwnerRetryPending
	r.probeRunStateMu.Unlock()
	if !newerRetryAt.Equal(wantRetryAt) || !ownerPending {
		close(releaseFinish)
		t.Fatalf("newer owner publication = retryAt %s ownerPending=%v, want %s/true", newerRetryAt, ownerPending, wantRetryAt)
	}

	close(releaseFinish)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.probeRunStateMu.Lock()
		launchActive := r.probeLaunchActive
		r.probeRunStateMu.Unlock()
		if !launchActive {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.probeRunStateMu.Lock()
	gotRetryAt := r.probeRetryAt
	r.probeRunStateMu.Unlock()
	if !gotRetryAt.Equal(wantRetryAt) {
		t.Fatalf("older wrapper regressed newer owner retry: got %s want %s", gotRetryAt, wantRetryAt)
	}
	if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
		t.Fatalf("newer owner retry schedule = (%s, %v), want exactly +1m", delay, scheduled)
	}

	failBoundary.Store(false)
	r.Start()
	time.Sleep(40 * time.Millisecond)
	if got := boundaryCalls.Load(); got != 2 {
		t.Fatalf("ordinary Start bypassed newer owner deadline: boundary calls=%d want=2", got)
	}
	clock.Set(wantRetryAt)
	r.wakeRefreshLoop()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		window, exists := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
		if exists && window.State == ProbeConfirmed && boundaryCalls.Load() >= 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := boundaryCalls.Load(); got != 3 {
		t.Fatalf("newer owner timer boundary calls=%d want=3", got)
	}
	window, exists := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !exists || window.State != ProbeConfirmed {
		t.Fatalf("newer owner timer window = %#v, ok=%v, want Confirmed", window, exists)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("newer owner timer sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
	}
}

func TestProductionProbeDueSentFailureRetriesRecoveryFirstAtBoundedDeadline(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 20, 0, 0, time.UTC)
	clock := &s7TestClock{now: baseNow}
	reset := baseNow.Add(5 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	baseline := ResetProbeBaseline(reset, 0, 5*time.Hour)
	baseline.WindowKind = WindowFiveHour
	baseline.SuspectedLazy = true
	firstDue := baseNow.Add(time.Minute)
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset, Baseline: baseline, Deadline: firstDue})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.quota = [][]byte{lazy, active}
	host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: {nil, errors.New("injected verify read failure")}}
	host.mu.Unlock()

	r.Start()
	r.wg.Wait()
	clock.Set(firstDue)
	r.wakeRefreshLoop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if posts, _ := probePOSTCount(host); posts == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("production due timer sent %d compact POSTs, want 1; urls=%v", posts, urls)
	}
	r.wg.Wait()
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	failedAttempt, ok := persisted.ProbeAttempts[binding.Instance]
	if !ok || failedAttempt.AttemptID == "" || failedAttempt.SendFenceSeq == 0 || failedAttempt.Phase != ProbeAttemptSentUnknown {
		t.Fatalf("verify failure did not preserve durable sent attempt/fence: %#v, ok=%v", failedAttempt, ok)
	}
	window, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]
	if !ok || window.State != ProbeSentUnknown || window.AttemptID != failedAttempt.AttemptID {
		t.Fatalf("verify failure durable window = %#v, ok=%v; attempt=%#v", window, ok, failedAttempt)
	}
	if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
		t.Fatalf("sent failure retry schedule = (%s, %v), want exactly +1m", delay, scheduled)
	}
	host.mu.Lock()
	requestsBeforeRetry := len(host.requests)
	host.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	host.mu.Lock()
	requestsStill := len(host.requests)
	host.mu.Unlock()
	if requestsStill != requestsBeforeRetry {
		t.Fatalf("sent failure retried before bounded deadline: requests=%d want=%d", requestsStill, requestsBeforeRetry)
	}

	clock.Set(firstDue.Add(time.Minute))
	r.wakeRefreshLoop()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		requests := len(host.requests)
		host.mu.Unlock()
		if requests > requestsBeforeRetry {
			break
		}
		time.Sleep(time.Millisecond)
	}
	host.mu.Lock()
	requestsAfterRetry := len(host.requests)
	host.mu.Unlock()
	if requestsAfterRetry != requestsBeforeRetry+1 {
		t.Fatalf("bounded retry issued %d new requests, want one recovery GET", requestsAfterRetry-requestsBeforeRetry)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, snapshotErr := r.runtimeStore.PersistentSnapshot()
		if snapshotErr == nil {
			if _, retained := state.ProbeAttempts[binding.Instance]; !retained {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	persisted, err = r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if retained := persisted.ProbeAttempts[binding.Instance]; retained.AttemptID != "" {
		t.Fatalf("bounded recovery retained sent attempt: %#v", retained)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeConfirmed {
		t.Fatalf("bounded recovery terminal window = %#v, want Confirmed", got)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("bounded recovery resent compact POST: posts=%d urls=%v", posts, urls)
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("sent failure retry accumulated Probe goroutines")
	}
}

func TestProductionProbeLaunchHandoffConsumesExactBoundaryRearm(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 18, 30, 0, 0, time.UTC)
	clock := &probeHandoffClock{now: baseNow}
	oldReset := baseNow.Add(-time.Hour)
	newReset := baseNow.Add(5 * time.Hour)
	firstActive := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, oldReset.Format(time.RFC3339)))
	rearmedLazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	rearmedActive := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, newReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{firstActive, rearmedLazy, rearmedActive}
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	baseline := ResetProbeBaseline(oldReset, 0, 5*time.Hour)
	baseline.WindowKind = WindowFiveHour
	baseline.SuspectedLazy = true
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: baseline})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
		t.Fatal(err)
	}

	host.gateDoURL = r.state.Config().QuotaEndpoint
	host.doStarted = make(chan struct{})
	host.releaseDo = make(chan struct{})
	r.Start()
	select {
	case <-host.doStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial production precheck did not start")
	}
	r.probeRunStateMu.Lock()
	close(host.releaseDo)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		window, exists := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
		persisted, err := r.runtimeStore.PersistentSnapshot()
		if err == nil && exists && window.State == ProbeConfirmed {
			if _, hasAttempt := persisted.ProbeAttempts[binding.Instance]; !hasAttempt {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	window, exists := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		r.probeRunStateMu.Unlock()
		t.Fatal(err)
	}
	if !exists || window.State != ProbeConfirmed {
		r.probeRunStateMu.Unlock()
		t.Fatalf("initial pass did not reach durable Confirmed before handoff: %#v, ok=%v", window, exists)
	}
	if _, hasAttempt := persisted.ProbeAttempts[binding.Instance]; hasAttempt {
		r.probeRunStateMu.Unlock()
		t.Fatalf("initial pass retained attempt before handoff: %#v", persisted.ProbeAttempts[binding.Instance])
	}
	handoffReached, releaseHandoff := clock.ArmNextCall()
	r.probeRunStateMu.Unlock()
	select {
	case <-handoffReached:
	case <-time.After(2 * time.Second):
		t.Fatal("launch did not reach post-RunProbeDueOnce handoff barrier")
	}

	zero := 0.0
	seconds := int64(5 * time.Hour / time.Second)
	observation := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: newReset}}
	if err = r.reconcileObservedProbeWindows(binding.Instance, observation); err != nil {
		close(releaseHandoff)
		t.Fatal(err)
	}
	r.Start() // running production Start routes the zero-deadline re-arm through launchProbe
	r.probeRunStateMu.Lock()
	active, pending := r.probeLaunchActive, r.probeRerunPending
	r.probeRunStateMu.Unlock()
	if !active || !pending {
		close(releaseHandoff)
		t.Fatalf("exact-boundary re-arm did not coalesce onto active launch: active=%v pending=%v", active, pending)
	}
	close(releaseHandoff)
	r.wg.Wait()
	time.Sleep(40 * time.Millisecond)

	window, exists = r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !exists || window.State != ProbeConfirmed {
		t.Fatalf("exact-boundary PendingCheck was not consumed: %#v, ok=%v", window, exists)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("exact-boundary re-arm sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
	}
	r.probeRunStateMu.Lock()
	active, pending = r.probeLaunchActive, r.probeRerunPending
	r.probeRunStateMu.Unlock()
	if active || pending {
		t.Fatalf("launch handoff did not become idle: active=%v pending=%v", active, pending)
	}
}

func TestProductionProbeDirectRecoveryOwnerConsumesDelegatedRerun(t *testing.T) {
	now := time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	oldLongReset := now.Add(6 * 24 * time.Hour)
	newLongReset := now.Add(7 * 24 * time.Hour)
	recoveryQuota := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, fiveReset.Format(time.RFC3339), newLongReset.Format(time.RFC3339)))
	lazyLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
	activeLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	r := newProductionLazyRefreshRuntime(t, now, host)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	r.Start()
	r.wg.Wait()
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}

	sendFence, err := r.probeFence.Next()
	if err != nil {
		t.Fatal(err)
	}
	attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "direct-recovery-owner", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: now.Add(-time.Minute), VerifyNotBefore: now, SuppressUntil: now.Add(10 * time.Minute)}
	fiveBaseline := ResetProbeBaseline(fiveReset, 0, 5*time.Hour)
	fiveBaseline.WindowKind = WindowFiveHour
	longBaseline := ResetProbeBaseline(oldLongReset, 1, 7*24*time.Hour)
	longBaseline.WindowKind = WindowWeekly
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: fiveBaseline, AttemptID: attempt.AttemptID})
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeConfirmed, Baseline: longBaseline})
	if _, err = r.runtimeStore.Update(func(state *PersistentState) error {
		state.ProbeAttempts[binding.Instance] = attempt
		state.ProbeWindows[binding.Instance] = r.probeController.Snapshot()[binding.Instance]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	host.quota = [][]byte{recoveryQuota, lazyLong, activeLong}
	host.gateDoURL = r.state.Config().QuotaEndpoint
	host.doStarted = make(chan struct{})
	host.releaseDo = make(chan struct{})
	host.mu.Unlock()
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	select {
	case <-host.doStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("direct recovery did not acquire runner and start verify GET")
	}

	zero := 0.0
	seconds := int64(7 * 24 * time.Hour / time.Second)
	rearm := ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: newLongReset}}
	if err = r.reconcileObservedProbeWindows(binding.Instance, rearm); err != nil {
		close(host.releaseDo)
		t.Fatal(err)
	}
	r.Start()
	r.wg.Wait() // production due launch delegates while direct recovery owns probeRunMu
	r.probeRunStateMu.Lock()
	active, pending := r.probeLaunchActive, r.probeRerunPending
	r.probeRunStateMu.Unlock()
	if active || !pending {
		close(host.releaseDo)
		t.Fatalf("production launch did not delegate to direct recovery owner: active=%v pending=%v", active, pending)
	}
	close(host.releaseDo)
	if err = <-recoveryDone; err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbeConfirmed {
		t.Fatalf("direct recovery owner lost delegated PendingCheck: %#v, ok=%v", window, ok)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("delegated rerun sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ProbeAttempts) != 0 {
		t.Fatalf("direct recovery handoff retained attempts: %#v", persisted.ProbeAttempts)
	}
	r.probeRunStateMu.Lock()
	active, pending = r.probeLaunchActive, r.probeRerunPending
	r.probeRunStateMu.Unlock()
	if active || pending {
		t.Fatalf("direct recovery handoff did not become idle: active=%v pending=%v", active, pending)
	}
}

func TestProductionProbeDirectRecoveryOwnerErrorSchedulesDelegatedRetry(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 19, 5, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name     string
		doErrors []error
	}{
		{name: "recovery_error", doErrors: []error{errors.New("injected direct recovery read failure")}},
		{name: "delegated_due_error", doErrors: []error{nil, nil, errors.New("injected delegated verify read failure")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			clock := &s7TestClock{now: baseNow}
			fiveReset := baseNow.Add(5 * time.Hour)
			oldLongReset := baseNow.Add(6 * 24 * time.Hour)
			newLongReset := baseNow.Add(7 * 24 * time.Hour)
			recoveryQuota := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, fiveReset.Format(time.RFC3339), newLongReset.Format(time.RFC3339)))
			lazyLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
			activeLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
			host := newProbeFixtureHost()
			r := newProductionLazyRefreshRuntime(t, baseNow, host)
			r.now = clock.Now
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(r.Stop) }
			t.Cleanup(stop)
			r.Start()
			r.wg.Wait()
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}

			sendFence, err := r.probeFence.Next()
			if err != nil {
				t.Fatal(err)
			}
			attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "direct-recovery-owner-error", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: baseNow.Add(-time.Minute), VerifyNotBefore: baseNow, SuppressUntil: baseNow.Add(10 * time.Minute)}
			fiveBaseline := ResetProbeBaseline(fiveReset, 0, 5*time.Hour)
			fiveBaseline.WindowKind = WindowFiveHour
			longBaseline := ResetProbeBaseline(oldLongReset, 1, 7*24*time.Hour)
			longBaseline.WindowKind = WindowWeekly
			r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: fiveBaseline, AttemptID: attempt.AttemptID})
			r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeConfirmed, Baseline: longBaseline})
			if _, err = r.runtimeStore.Update(func(state *PersistentState) error {
				state.ProbeAttempts[binding.Instance] = attempt
				state.ProbeWindows[binding.Instance] = r.probeController.Snapshot()[binding.Instance]
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			host.mu.Lock()
			host.quota = [][]byte{recoveryQuota, lazyLong, activeLong}
			host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: append([]error(nil), scenario.doErrors...)}
			host.gateDoURL = r.state.Config().QuotaEndpoint
			host.doStarted = make(chan struct{})
			host.releaseDo = make(chan struct{})
			releaseDo := host.releaseDo
			host.mu.Unlock()
			recoveryDone := make(chan error, 1)
			go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
			select {
			case <-host.doStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("direct recovery did not acquire runner and start verify GET")
			}

			zero := 0.0
			seconds := int64(7 * 24 * time.Hour / time.Second)
			rearm := ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: newLongReset}}
			if err = r.reconcileObservedProbeWindows(binding.Instance, rearm); err != nil {
				close(releaseDo)
				t.Fatal(err)
			}
			r.Start()
			r.wg.Wait()
			r.probeRunStateMu.Lock()
			active, pending := r.probeLaunchActive, r.probeRerunPending
			r.probeRunStateMu.Unlock()
			if active || !pending {
				close(releaseDo)
				t.Fatalf("production launch did not delegate before owner error: active=%v pending=%v", active, pending)
			}
			close(releaseDo)
			select {
			case err = <-recoveryDone:
				if err == nil {
					t.Fatal("direct owner error was not surfaced")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("direct recovery owner did not return after injected error")
			}
			failedState, err := r.runtimeStore.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			failedAttempt, retained := failedState.ProbeAttempts[binding.Instance]
			switch scenario.name {
			case "recovery_error":
				if !retained || failedAttempt.AttemptID != attempt.AttemptID || failedAttempt.Phase != ProbeAttemptSentUnknown || failedAttempt.SendFenceSeq != sendFence {
					t.Fatalf("direct recovery error changed durable attempt/fence: %#v, retained=%v", failedAttempt, retained)
				}
			case "delegated_due_error":
				if !retained || failedAttempt.AttemptID == "" || failedAttempt.AttemptID == attempt.AttemptID || failedAttempt.Phase != ProbeAttemptSentUnknown || failedAttempt.SendFenceSeq == 0 {
					t.Fatalf("delegated due error did not preserve exact durable sent attempt/fence: %#v, retained=%v", failedAttempt, retained)
				}
				failedWindow := failedState.ProbeWindows[binding.Instance][ProbeWindowLong]
				if failedWindow.State != ProbeSentUnknown || failedWindow.AttemptID != failedAttempt.AttemptID {
					t.Fatalf("delegated due error durable window = %#v, attempt=%#v", failedWindow, failedAttempt)
				}
			}

			host.mu.Lock()
			requestsAfterFailure := len(host.requests)
			host.mu.Unlock()
			if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
				t.Fatalf("direct owner error retry schedule = (%s, %v), want exactly +1m", delay, scheduled)
			}
			r.Start()
			time.Sleep(40 * time.Millisecond)
			host.mu.Lock()
			requestsBeforeDeadline := len(host.requests)
			host.mu.Unlock()
			if requestsBeforeDeadline != requestsAfterFailure {
				t.Fatalf("ordinary Start bypassed direct-owner backoff: requests=%d want=%d", requestsBeforeDeadline, requestsAfterFailure)
			}

			clock.Set(baseNow.Add(time.Minute))
			r.wakeRefreshLoop()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				state, snapshotErr := r.runtimeStore.PersistentSnapshot()
				if snapshotErr == nil {
					window := state.ProbeWindows[binding.Instance][ProbeWindowLong]
					if len(state.ProbeAttempts) == 0 && window.State == ProbeConfirmed {
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
			persisted, err := r.runtimeStore.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if len(persisted.ProbeAttempts) != 0 {
				t.Fatalf("bounded direct-owner recovery retained attempts: %#v", persisted.ProbeAttempts)
			}
			if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; got.State != ProbeConfirmed {
				t.Fatalf("bounded direct-owner retry window = %#v, want Confirmed", got)
			}
			if posts, urls := probePOSTCount(host); posts != 1 {
				t.Fatalf("direct-owner retry sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
			}
			deadline = time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				r.probeRunStateMu.Lock()
				active, pending = r.probeLaunchActive, r.probeRerunPending
				r.probeRunStateMu.Unlock()
				if !active && !pending {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if active || pending {
				t.Fatalf("bounded direct-owner retry did not become idle: active=%v pending=%v", active, pending)
			}
		})
	}
}

func TestProductionProbeElapsedExternalOwnerRetrySurvivesLaunchFinish(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 19, 10, 0, 0, time.UTC)
	clock := &probeDeferredClock{now: baseNow}
	fiveReset := baseNow.Add(5 * time.Hour)
	oldLongReset := baseNow.Add(6 * 24 * time.Hour)
	newLongReset := baseNow.Add(7 * 24 * time.Hour)
	recoveryQuota := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, fiveReset.Format(time.RFC3339), newLongReset.Format(time.RFC3339)))
	lazyLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
	activeLong := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, newLongReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	sendFence, err := r.probeFence.Next()
	if err != nil {
		t.Fatal(err)
	}
	attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "elapsed-external-owner-retry", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: sendFence, CreatedAt: baseNow.Add(-time.Minute), VerifyNotBefore: baseNow, SuppressUntil: baseNow.Add(10 * time.Minute)}
	fiveBaseline := ResetProbeBaseline(fiveReset, 0, 5*time.Hour)
	fiveBaseline.WindowKind = WindowFiveHour
	longBaseline := ResetProbeBaseline(oldLongReset, 1, 7*24*time.Hour)
	longBaseline.WindowKind = WindowWeekly
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: fiveBaseline, AttemptID: attempt.AttemptID})
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeConfirmed, Baseline: longBaseline})
	if _, err = r.runtimeStore.Update(func(state *PersistentState) error {
		state.ProbeAttempts[binding.Instance] = attempt
		state.ProbeWindows[binding.Instance] = r.probeController.Snapshot()[binding.Instance]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.quota = [][]byte{recoveryQuota, lazyLong, activeLong}
	host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: {errors.New("injected elapsed retry recovery failure")}}
	host.gateDoURL = r.state.Config().QuotaEndpoint
	host.doStarted = make(chan struct{})
	host.releaseDo = make(chan struct{})
	releaseDo := host.releaseDo
	host.mu.Unlock()
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	select {
	case <-host.doStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("direct recovery did not acquire runner and start verify GET")
	}

	zero := 0.0
	seconds := int64(7 * 24 * time.Hour / time.Second)
	rearm := ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: newLongReset}}
	if err = r.reconcileObservedProbeWindows(binding.Instance, rearm); err != nil {
		close(releaseDo)
		t.Fatal(err)
	}
	r.probeRunStateMu.Lock()
	admissionReached, releaseAdmission := clock.ArmNextCall()
	launchReturned := make(chan struct{})
	go func() {
		r.launchProbe(false)
		close(launchReturned)
	}()
	select {
	case <-admissionReached:
	case <-time.After(2 * time.Second):
		r.probeRunStateMu.Unlock()
		close(releaseDo)
		t.Fatal("production launch did not reach admission clock")
	}
	close(releaseAdmission)
	finishReached, releaseFinish := clock.ArmNextCall()
	r.probeRunStateMu.Unlock()
	select {
	case <-finishReached:
	case <-time.After(2 * time.Second):
		close(releaseDo)
		t.Fatal("production wrapper did not reach completion clock")
	}

	close(releaseDo)
	select {
	case err = <-recoveryDone:
		if err == nil {
			close(releaseFinish)
			t.Fatal("direct recovery error was not surfaced")
		}
	case <-time.After(2 * time.Second):
		close(releaseFinish)
		t.Fatal("direct recovery did not publish retry while wrapper was paused")
	}
	r.probeRunStateMu.Lock()
	retryAt := r.probeRetryAt
	active, recoveryPending := r.probeLaunchActive, r.probeRecoveryPending
	r.probeRunStateMu.Unlock()
	if !retryAt.Equal(baseNow.Add(time.Minute)) || !active || !recoveryPending {
		close(releaseFinish)
		t.Fatalf("external owner publication = retryAt %s active=%v recovery=%v", retryAt, active, recoveryPending)
	}

	clock.Set(baseNow.Add(2 * time.Minute))
	close(releaseFinish)
	select {
	case <-launchReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("production launch call did not return")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.probeRunStateMu.Lock()
		active = r.probeLaunchActive
		r.probeRunStateMu.Unlock()
		if !active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != 0 {
		t.Fatalf("elapsed external-owner retry schedule = (%s, %v), want immediate timer work", delay, scheduled)
	}

	r.Start()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, snapshotErr := r.runtimeStore.PersistentSnapshot()
		if snapshotErr == nil {
			window := state.ProbeWindows[binding.Instance][ProbeWindowLong]
			if len(state.ProbeAttempts) == 0 && window.State == ProbeConfirmed {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ProbeAttempts) != 0 {
		t.Fatalf("elapsed external-owner retry retained attempts: %#v", persisted.ProbeAttempts)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; got.State != ProbeConfirmed {
		t.Fatalf("elapsed external-owner retry window = %#v, want Confirmed", got)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("elapsed external-owner retry sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
	}
}

func TestProductionProbeDirectDueOwnerErrorSchedulesDelegatedRetry(t *testing.T) {
	baseNow := time.Date(2026, 8, 1, 19, 15, 0, 0, time.UTC)
	clock := &s7TestClock{now: baseNow}
	fiveReset := baseNow.Add(5 * time.Hour)
	oldLongReset := baseNow.Add(6 * 24 * time.Hour)
	longReset := baseNow.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, fiveReset.Format(time.RFC3339), longReset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q},"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, fiveReset.Format(time.RFC3339), longReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	r := newProductionLazyRefreshRuntime(t, baseNow, host)
	r.now = clock.Now
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(r.Stop) }
	t.Cleanup(stop)
	r.Start()
	r.wg.Wait()
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	fiveBaseline := ResetProbeBaseline(fiveReset, 0, 5*time.Hour)
	fiveBaseline.WindowKind = WindowFiveHour
	fiveBaseline.SuspectedLazy = true
	longBaseline := ResetProbeBaseline(oldLongReset, 1, 7*24*time.Hour)
	longBaseline.WindowKind = WindowWeekly
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: fiveBaseline})
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeConfirmed, Baseline: longBaseline})
	if err := r.persistProbeInstances(map[AuthInstanceID]struct{}{binding.Instance: {}}); err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	host.quota = [][]byte{lazy, active}
	host.doErrors = map[string][]error{r.state.Config().QuotaEndpoint: {errors.New("injected direct due read failure")}}
	host.gateDoURL = r.state.Config().QuotaEndpoint
	host.doStarted = make(chan struct{})
	host.releaseDo = make(chan struct{})
	releaseDo := host.releaseDo
	host.mu.Unlock()
	dueDone := make(chan error, 1)
	go func() { dueDone <- r.RunProbeDueOnce(context.Background()) }()
	select {
	case <-host.doStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("direct due did not acquire runner and start quota GET")
	}

	zero := 0.0
	seconds := int64(7 * 24 * time.Hour / time.Second)
	rearm := ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: longReset}}
	if err := r.reconcileObservedProbeWindows(binding.Instance, rearm); err != nil {
		close(releaseDo)
		t.Fatal(err)
	}
	r.Start()
	r.wg.Wait()
	r.probeRunStateMu.Lock()
	launchActive, pending := r.probeLaunchActive, r.probeRerunPending
	r.probeRunStateMu.Unlock()
	if launchActive || !pending {
		close(releaseDo)
		t.Fatalf("production launch did not delegate to direct due owner: active=%v pending=%v", launchActive, pending)
	}
	close(releaseDo)
	select {
	case err := <-dueDone:
		if err == nil {
			t.Fatal("direct due owner error was not surfaced")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("direct due owner did not return after injected error")
	}

	host.mu.Lock()
	requestsAfterFailure := len(host.requests)
	host.mu.Unlock()
	if delay, scheduled := r.nextRefreshLoopDelay(); !scheduled || delay != time.Minute {
		t.Fatalf("direct due owner retry schedule = (%s, %v), want exactly +1m", delay, scheduled)
	}
	r.Start()
	time.Sleep(40 * time.Millisecond)
	host.mu.Lock()
	requestsBeforeDeadline := len(host.requests)
	host.mu.Unlock()
	if requestsBeforeDeadline != requestsAfterFailure {
		t.Fatalf("ordinary Start bypassed direct-due backoff: requests=%d want=%d", requestsBeforeDeadline, requestsAfterFailure)
	}

	clock.Set(baseNow.Add(time.Minute))
	r.wakeRefreshLoop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, snapshotErr := r.runtimeStore.PersistentSnapshot()
		if snapshotErr == nil {
			five := state.ProbeWindows[binding.Instance][ProbeWindowFiveHour]
			long := state.ProbeWindows[binding.Instance][ProbeWindowLong]
			if len(state.ProbeAttempts) == 0 && five.State == ProbeConfirmed && long.State == ProbeConfirmed {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ProbeAttempts) != 0 {
		t.Fatalf("bounded direct-due retry retained attempts: %#v", persisted.ProbeAttempts)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeConfirmed {
		t.Fatalf("bounded direct-due retry five-hour window = %#v, want Confirmed", got)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; got.State != ProbeConfirmed {
		t.Fatalf("bounded direct-due retry long window = %#v, want Confirmed", got)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Fatalf("direct-due retry sent %d compact POSTs, want exactly 1; urls=%v", posts, urls)
	}
}

func TestProductionProbeRunsWhileNormalRefreshDormant(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339))), []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, TemporaryExhausted: true, TemporaryResetAt: now.Add(time.Hour), Circuit: CircuitBreakerState{State: CircuitStateOpen, FailureCount: 7}, Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80), Exhausted: true}}})
	previousTrials := globalTrials
	globalTrials = NewTrialRegistry()
	globalTrials.TryBegin(instance, now)
	t.Cleanup(func() { globalTrials = previousTrials })
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, now.Add(5*time.Hour), 5*time.Hour)
	if r.refreshController.Mode(now) != RefreshModeDormant {
		t.Fatal("normal refresh not dormant")
	}
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	urls := append([]string(nil), host.urls...)
	host.mu.Unlock()
	want := []string{cfg.QuotaEndpoint, codexResetProbeEndpoint, cfg.QuotaEndpoint}
	if len(urls) != len(want) {
		t.Fatalf("urls=%v", urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("urls=%v", urls)
		}
	}
	w, ok := r.probeController.Window(legacyAuthInstanceID("a"), ProbeWindowFiveHour)
	if !ok || w.State != ProbeConfirmed {
		t.Fatalf("window=%#v ok=%v", w, ok)
	}
	a := accountByAuthID(t, state.Snapshot(now), "a")
	if a.Circuit.FailureCount != 7 || a.Circuit.State != CircuitStateOpen || globalTrials.State(instance, now) != TrialActive {
		t.Fatalf("probe mutated business state circuit=%#v trial=%v", a.Circuit, globalTrials.State(instance, now))
	}
}

func TestProductionProbeKPointCrashRestartVerifyFirst(t *testing.T) {
	points := []string{"K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP", "K_PROBE_SENT_WRITE"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			clock := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
			idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
			host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, clock.Add(5*time.Hour).Format(time.RFC3339))), []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, clock.Add(5*time.Hour).Format(time.RFC3339)))}}
			cfg := DefaultConfig()
			cfg.EnableResetProbe = true
			pluginState := NewPluginState(cfg)
			pluginState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			pluginState.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: clock.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
			roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
			path := filepath.Join(t.TempDir(), "state.json")
			adapter := &rosterCredentialHost{host: host, roster: roster}
			r, err := NewProductionQuotaRefresher(host, pluginState, adapter, roster, path, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			adapter.bindings = r.bindings
			binding, _, err := r.BootstrapBinding(context.Background(), "a")
			if err != nil {
				t.Fatal(err)
			}
			setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, clock.Add(5*time.Hour), 5*time.Hour)
			r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
			registry := testsupport.NewKPointRegistry(points...)
			r.probeWAL.crash = testsupport.NewCrashController(registry, point)
			if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("err=%v", err)
			}
			clock = clock.Add(4 * time.Second)
			adapter2 := &rosterCredentialHost{host: host, roster: roster}
			restart, err := NewProductionQuotaRefresher(host, pluginState, adapter2, roster, path, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			adapter2.bindings = restart.bindings
			restart.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
			if point != "K_PROBE_SENDING_WRITE" {
				if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			host.mu.Lock()
			posts := 0
			for _, u := range host.urls {
				if u == codexResetProbeEndpoint {
					posts++
				}
			}
			host.mu.Unlock()
			if posts > 1 {
				t.Fatalf("probe resent after crash: urls=%v", host.urls)
			}
		})
	}
}

func TestProductionProbeExpiredCallbackCannotResurrectCompletedAttempt(t *testing.T) {
	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	clock := newCoordinatorTestClock(now)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	postReturned := make(chan struct{})
	var returnOnce sync.Once
	host := &sequenceProbeHost{
		auth:      pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)},
		quota:     [][]byte{lazy, active},
		doStarted: postStarted,
		releaseDo: releasePost,
	}
	host.afterDo = func(req pluginapi.HTTPRequest) {
		if req.Method == http.MethodPost && req.URL == codexResetProbeEndpoint {
			returnOnce.Do(func() { close(postReturned) })
		}
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePost) }) }
	t.Cleanup(release)

	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	cfg.MaxRefreshConcurrency = 2
	pluginState := NewPluginState(cfg)
	pluginState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, pluginState, adapter, roster, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, now.Add(5*time.Hour), 5*time.Hour)
	r.coordinator.opts.AfterFunc = clock.AfterFunc

	dueDone := make(chan error, 1)
	go func() { dueDone <- r.RunProbeDueOnce(context.Background()) }()
	select {
	case <-postStarted:
	case <-time.After(time.Second):
		t.Fatal("compact POST did not start")
	}
	clock.Advance(legacyLeaseDuration)
	if err = <-dueDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired Probe err=%v, want deadline exceeded", err)
	}
	expired, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	attempt, ok := expired.ProbeAttempts[binding.Instance]
	if !ok || attempt.AttemptID == "" || attempt.Phase != ProbeAttemptSentUnknown {
		t.Fatalf("expired Probe attempt=%#v ok=%v, want exact SentUnknown", attempt, ok)
	}

	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	terminalDeadline := time.Now().Add(time.Second)
	for {
		recovered, snapshotErr := r.runtimeStore.PersistentSnapshot()
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		_, hasAttempt := recovered.ProbeAttempts[binding.Instance]
		window := recovered.ProbeWindows[binding.Instance][ProbeWindowFiveHour]
		if !hasAttempt && window.State == ProbeConfirmed {
			break
		}
		if time.Now().After(terminalDeadline) {
			t.Fatalf("second-slot recovery did not reach terminal commit: attempt=%#v window=%#v", recovered.ProbeAttempts[binding.Instance], window)
		}
		time.Sleep(time.Millisecond)
	}

	release()
	select {
	case <-postReturned:
	case <-time.After(time.Second):
		t.Fatal("expired compact POST did not return")
	}
	if err = <-recoveryDone; err != nil {
		t.Fatalf("second-slot recovery failed after terminal commit: %v", err)
	}
	lateDeadline := time.Now().Add(time.Second)
	for {
		found := false
		for _, entry := range pluginState.Snapshot(clock.Now()).Logs {
			if entry.Event == "probe.failed" {
				found = true
				break
			}
		}
		if found {
			break
		}
		if time.Now().After(lateDeadline) {
			t.Fatal("late expired callback did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	late, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if resurrected, ok := late.ProbeAttempts[binding.Instance]; ok {
		t.Errorf("late callback resurrected completed attempt: %#v", resurrected)
	}
	if got := late.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeConfirmed {
		t.Errorf("late callback replaced terminal window: %#v", got)
	}

	restartAdapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, pluginState, restartAdapter, roster, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Errorf("restart recovery entered a missing-fence loop: %v", err)
	}
	if posts, urls := probePOSTCount(host); posts != 1 {
		t.Errorf("compact POST count=%d, want exactly 1; urls=%v", posts, urls)
	}
}

func TestProbeAttemptIDsMonotonicWithFrozenClock(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active, lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, now.Add(5*time.Hour), 5*time.Hour)
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	first := snap.ProbeAttemptSeq
	w, _ := r.probeController.Window(legacyAuthInstanceID("a"), ProbeWindowFiveHour)
	w.State = ProbePendingCheck
	w.Deadline = now
	w.Baseline.SuspectedLazy = true
	r.probeController.SetWindow(legacyAuthInstanceID("a"), ProbeWindowFiveHour, w)
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err = r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProbeAttemptSeq <= first {
		t.Fatalf("attempt sequence did not advance: first=%d second=%d", first, snap.ProbeAttemptSeq)
	}
}

func TestProbeDueSingleFlightDuringPropagation(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, now.Add(5*time.Hour), 5*time.Hour)
	entered := make(chan struct{})
	release := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-entered
	if next := r.probeController.NextDeadline(); !next.IsZero() {
		t.Fatalf("expired deadline visible during propagation: %v", next)
	}
	for i := 0; i < 4; i++ {
		if err := r.RunProbeDueOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	host.mu.Lock()
	calls := len(host.urls)
	host.mu.Unlock()
	if calls != 2 {
		t.Fatalf("timer spin started extra work before propagation completed: calls=%d", calls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProbeRecoveryExcludesConcurrentDueClaim(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	activeQuota := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{activeQuota, activeQuota, activeQuota, activeQuota}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	a, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeRetryWait, Deadline: now})
	r.probeController.SetWindow(b.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now})
	committed, err := r.runtimeStore.Update(func(s *PersistentState) error {
		blocked := s.Bindings["a"]
		blocked.AuthBlocked = true
		s.Bindings["a"] = blocked
		s.ProbeWindows = r.probeController.Snapshot()
		s.ProbeAttempts[a.Instance] = ProbeAttempt{Instance: a.Instance, AttemptID: "old-prepared", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, CreatedAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.bindings.mu.Lock()
	r.bindings.bindings = committed.Bindings
	r.bindings.mu.Unlock()

	originalExecute := r.coordinator.opts.Execute
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	r.coordinator.opts.Execute = func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
		if intent.Class == OperationLegacyRefresh && intent.AttemptID == "block-b" {
			close(blockerStarted)
			<-releaseBlocker
			return OperationResult{Token: intent.Token}
		}
		return originalExecute(ctx, intent, held)
	}
	r.coordinator.activateInstances(map[AuthInstanceID]TierGeneration{a.Instance: TierGeneration(a.Generation), b.Instance: TierGeneration(b.Generation)})
	blocker := r.coordinator.Submit(Intent{Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, AttemptID: "block-b", Token: b.ExecutionToken(0)})
	<-blockerStarted

	recoverySnapshot := make(chan struct{})
	releaseRecovery := make(chan struct{})
	var firstNow atomic.Bool
	r.now = func() time.Time {
		if firstNow.CompareAndSwap(false, true) {
			close(recoverySnapshot)
			<-releaseRecovery
		}
		return now
	}
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	<-recoverySnapshot
	dueDone := make(chan error, 1)
	go func() { dueDone <- r.RunProbeDueOnce(context.Background()) }()
	select {
	case err := <-dueDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		snapshot, snapErr := r.runtimeStore.PersistentSnapshot()
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		if attempt, ok := snapshot.ProbeAttempts[b.Instance]; ok && attempt.Phase == ProbeAttemptPrepared {
			close(releaseRecovery)
			close(releaseBlocker)
			_ = <-recoveryDone
			_ = blocker.Await(context.Background())
			_ = <-dueDone
			t.Fatalf("due claimed %s while recovery owned a stale snapshot", attempt.AttemptID)
		}
		t.Fatal("concurrent due neither returned nor exposed its claim")
	}
	close(releaseRecovery)
	close(releaseBlocker)
	_ = blocker.Await(context.Background())
	select {
	case err = <-recoveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery owner did not consume delegated due after blocker release")
	}
}

func TestProbeVerifyRejectsReadAtOrBeforeSendFence(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	state := NewPersistentState()
	attempt := ProbeAttempt{Instance: 1, AttemptID: "verify-fence", Phase: ProbeAttemptSentUnknown, SendFenceSeq: 10, VerifyNotBefore: now}
	state.ProbeAttempts[1] = attempt
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	r := &QuotaRefresher{runtimeStore: store, probeWAL: NewProbeWAL(store), probeFence: NewFenceAllocator(store, state, nil), probeController: NewProbeController(now), now: func() time.Time { return now }, roster: HostRosterSnapshot{Capability: CapabilityA}}
	result := r.runTypedHeld(context.Background(), Intent{Instance: 1, Class: OperationProbeSequence, Source: SourceProbeVerify, StartedAfter: attempt.SendFenceSeq, AttemptID: attempt.AttemptID, Payload: probeSequencePayload{Attempt: attempt, Recovery: true}}, &HeldLease{})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "verify read did not start after send fence") {
		t.Fatalf("verify accepted read_start_seq <= send_fence_seq: result=%#v", result)
	}
	if result.ReadStartSeq != 0 {
		t.Fatalf("forbidden verify published read_start_seq=%d", result.ReadStartSeq)
	}
}

func TestProbeRestartAndDemotionDeleteOrphanState(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	b, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows[999] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeRetryWait, Deadline: now}}
		s.ProbeAttempts[999] = ProbeAttempt{Instance: 999, AttemptID: "orphan", Phase: ProbeAttemptSent, SendFenceSeq: 2, VerifyNotBefore: now}
		s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeRetryWait, Deadline: now.Add(time.Hour)}}
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "demoted", Phase: ProbeAttemptSent, SendFenceSeq: 3, VerifyNotBefore: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter2 := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, adapter2, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter2.bindings = restart.bindings
	restart.rosterMu.Lock()
	restart.roster = HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "b", AuthIndex: "bidx", Provider: "codex", Priority: intPtr(10)}}}
	restart.rosterMu.Unlock()
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ProbeWindows) != 0 || len(snap.ProbeAttempts) != 0 {
		t.Fatalf("orphan state retained after restart/demotion: windows=%v attempts=%v", snap.ProbeWindows, snap.ProbeAttempts)
	}
	host.mu.Lock()
	calls := len(host.urls)
	host.mu.Unlock()
	if calls != 0 {
		t.Fatalf("orphan cleanup issued requests: %v", host.urls)
	}
}

func TestProbeDueFailureContinuesOtherConfirmedInstance(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(5*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(`bad`), lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	for _, id := range []string{"a", "b"} {
		state.UpsertQuota(AccountState{AuthID: id, AuthIndex: "idx-" + id, Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	for _, id := range []string{"a", "b"} {
		binding, _, bootstrapErr := r.BootstrapBinding(context.Background(), id)
		if bootstrapErr != nil {
			t.Fatal(bootstrapErr)
		}
		setPendingSuspectedProbe(t, r, binding.Instance, ProbeWindowFiveHour, now.Add(-time.Hour), 5*time.Hour)
	}
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if err = r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("first instance failure was not surfaced")
	}
	host.mu.Lock()
	posts := 0
	for _, u := range host.urls {
		if u == codexResetProbeEndpoint {
			posts++
		}
	}
	host.mu.Unlock()
	if posts != 1 {
		t.Fatalf("other confirmed instance was starved: urls=%v", host.urls)
	}
}

func TestProbeRecoveryFailureContinuesOtherConfirmedInstance(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(`bad`), active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	var bs []RuntimeBinding
	for _, id := range []string{"a", "b"} {
		b, _, e := r.BootstrapBinding(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		bs = append(bs, b)
	}
	_, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ReservedCeiling = 100
		for n, b := range bs {
			s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeSentUnknown, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)}}
			s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: fmt.Sprintf("r%d", n), Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: uint64(n + 1), CreatedAt: now.Add(-time.Hour), VerifyNotBefore: now}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter2 := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, adapter2, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter2.bindings = restart.bindings
	if err = restart.RunProbeRecoveryOnce(context.Background()); err == nil {
		t.Fatal("recovery failure was not surfaced")
	}
	snap, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ProbeAttempts) != 1 {
		t.Fatalf("other recovery was starved: attempts=%v urls=%v windows=%v", snap.ProbeAttempts, host.urls, snap.ProbeWindows)
	}
}

func TestProductionProbeHasNoSplitSendExecutor(t *testing.T) {
	raw, err := os.ReadFile("probe_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if strings.Contains(source, "case OperationProbeSend:") || strings.Contains(source, "type probeSendPayload struct") {
		t.Fatal("superseded split probe-send production executor remains")
	}
}
