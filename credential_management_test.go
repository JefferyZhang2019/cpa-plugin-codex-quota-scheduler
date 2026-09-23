package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type failingCredentialReadHost struct{ err error }

func (h failingCredentialReadHost) SaveAuth(context.Context, AuthInstanceID, HostAuth) error {
	return h.err
}

type gatedCredentialReadHost struct {
	started chan struct{}
	release chan struct{}
	result  HostAuth
	err     error
}

func (h *gatedCredentialReadHost) SaveAuth(context.Context, AuthInstanceID, HostAuth) error {
	return h.err
}
func (h *gatedCredentialReadHost) GetAuth(context.Context, AuthInstanceID) (HostAuth, error) {
	close(h.started)
	<-h.release
	return h.result, h.err
}
func (h failingCredentialReadHost) GetAuth(context.Context, AuthInstanceID) (HostAuth, error) {
	return HostAuth{}, h.err
}

func newCredentialManagementFixture(t *testing.T, observed CredentialFingerprint) (*QuotaRefresher, *StateStore, *walHost, ActiveRoster) {
	t.Helper()
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	f0 := fp("subject", "refresh-0", "metadata")
	f1 := fp("subject", "refresh-1", "metadata")
	state := NewPersistentState()
	state.TierGeneration = 5
	state.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx", Instance: 1, Admission: 3, Generation: 5, Login: 7, Token: 11, Fingerprint: f0, AuthBlocked: true}
	state.CredentialChains[1] = TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, SaveSeq: 1, Phase: TransitionOutcomeUnknown, CreatedAt: now}}}
	store := NewStateStore(filepath.Join(t.TempDir(), "runtime.json"), OSFileHooks(), nil)
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	bindings, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	host := &walHost{current: HostAuth{Fingerprint: observed}}
	credentials := mustCredentialManager(t, store, host, func() time.Time { return now }, nil)
	refresher := &QuotaRefresher{runtimeStore: store, bindings: bindings, credentials: credentials}
	return refresher, store, host, ActiveRoster{Confirmed: true, Health: RosterHealthy, Generation: 5, Instances: []string{"active"}}
}

func TestAutomaticExternalLoginUnparksAuthFailure(t *testing.T) {
	now := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	f0 := fp("subject", "refresh-0", "metadata")
	external := fp("subject", "refresh-new", "metadata")
	persistent := NewPersistentState()
	persistent.TierGeneration = 5
	persistent.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx", Instance: 1, Admission: 3, Generation: 5, Login: 7, Token: 11, Fingerprint: f0}
	persistent.CredentialChains[1] = TransitionChain{Cursor: f0}
	store := NewStateStore(filepath.Join(t.TempDir(), "runtime.json"), OSFileHooks(), nil)
	if err := store.WriteThrough(persistent); err != nil {
		t.Fatal(err)
	}
	bindings, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	host := &walHost{current: HostAuth{Fingerprint: external}}
	credentials := mustCredentialManager(t, store, host, func() time.Time { return now }, nil)
	state := NewPluginState(DefaultConfig())
	state.UpsertQuota(AccountState{AuthID: "active", AuthIndex: "idx", Provider: "codex", LastSuccessAt: now.Add(-2 * time.Hour)})
	state.RecordRefreshFailure("active", "idx", RefreshFailureAuth, "token refresh returned status 401", now.Add(-time.Hour))
	refresher := NewQuotaRefresher(nil, state, func() time.Time { return now })
	refresher.runtimeStore, refresher.bindings, refresher.credentials = store, bindings, credentials
	// Park the single-flight guard so the unpark's RefreshOneSoon trigger is a
	// deterministic no-op; the durable half (the flag clear) is under test.
	refresher.mu.Lock()
	refresher.refreshing = true
	refresher.mu.Unlock()

	binding := persistent.Bindings["active"]
	refresher.reconcileActiveCredentialTails(context.Background(), map[string]RuntimeBinding{"active": binding}, nil)

	account := accountByAuthID(t, state.Snapshot(now), "active")
	if account.Refresh.AuthFailure {
		t.Fatalf("AuthFailure still true after external login reconcile: %#v", account.Refresh)
	}
	if !account.Refresh.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt = %s, want zero after unpark", account.Refresh.NextRetryAt)
	}
	if due := state.DueAccounts(now); len(due) != 1 || due[0].AuthID != "active" {
		t.Fatalf("due accounts after unpark = %#v, want active", due)
	}
	logs := state.Snapshot(now).Logs
	if len(logs) == 0 || logs[len(logs)-1].Event != "credential.external_login_unparked" {
		t.Fatalf("logs = %#v, want credential.external_login_unparked entry", logs)
	}
}

func TestCredentialAmbiguityManagementExitsEpochSemantics(t *testing.T) {
	f1 := fp("subject", "refresh-1", "metadata")
	external := fp("external", "refresh-x", "metadata")
	for _, tc := range []struct {
		name     string
		action   CredentialResolutionAction
		observed CredentialFingerprint
		want     RuntimeBinding
		wantG    TierGeneration
	}{
		{name: "confirm-owned", action: CredentialConfirmOwned, observed: f1, want: RuntimeBinding{Admission: 3, Generation: 5, Login: 7, Token: 12, Fingerprint: f1, AuthBlocked: true}, wantG: 5},
		{name: "confirm-external", action: CredentialConfirmExternal, observed: external, want: RuntimeBinding{Admission: 4, Generation: 6, Login: 8, Token: 11, Fingerprint: external, AuthBlocked: false}, wantG: 6},
		{name: "reread", action: CredentialReread, observed: f1, want: RuntimeBinding{Admission: 3, Generation: 5, Login: 7, Token: 12, Fingerprint: f1, AuthBlocked: true}, wantG: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refresher, store, _, roster := newCredentialManagementFixture(t, tc.observed)
			if err := refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", tc.action); err != nil {
				t.Fatal(err)
			}
			state, err := store.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			got := state.Bindings["active"]
			if got.Admission != tc.want.Admission || got.Generation != tc.want.Generation || got.Login != tc.want.Login || got.Token != tc.want.Token || got.Fingerprint != tc.want.Fingerprint || got.AuthBlocked != tc.want.AuthBlocked || state.TierGeneration != tc.wantG {
				t.Fatalf("binding=%#v G=%d want=%#v G=%d", got, state.TierGeneration, tc.want, tc.wantG)
			}
			chain := state.CredentialChains[1]
			if tc.action == CredentialReread {
				if chain.Cursor != tc.observed || len(chain.Transitions) != 0 {
					t.Fatalf("reread chain=%#v", chain)
				}
			} else if chain.Cursor != tc.observed || len(chain.Transitions) != 0 {
				t.Fatalf("resolved chain=%#v", chain)
			}
		})
	}
}

func TestCredentialAmbiguityManagementRouteScopesAndAudits(t *testing.T) {
	f1 := fp("subject", "refresh-1", "metadata")
	refresher, _, _, roster := newCredentialManagementFixture(t, f1)
	store := NewPluginState(DefaultConfig())
	resolver := func(ctx context.Context, authID string, action CredentialResolutionAction) error {
		return refresher.ResolveCredentialAmbiguity(ctx, roster, authID, action)
	}
	body, _ := json.Marshal(map[string]string{"auth_id": "active", "action": string(CredentialConfirmOwned)})
	resp := HandleManagementRequestWithLifecycle(store, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/credentials/resolve", Body: body}, time.Now(), ManagementLifecycleSnapshot{Roster: roster, ResolveCredential: resolver})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	logs := store.Snapshot(time.Now()).Logs
	if len(logs) == 0 || logs[len(logs)-1].Event != "credential.ambiguity_resolved" {
		t.Fatalf("audit logs=%#v", logs)
	}

	for _, authID := range []string{"removed", "lower-tier"} {
		body, _ = json.Marshal(map[string]string{"auth_id": authID, "action": string(CredentialConfirmOwned)})
		resp = HandleManagementRequestWithLifecycle(store, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/credentials/resolve", Body: body}, time.Now(), ManagementLifecycleSnapshot{Roster: roster, ResolveCredential: resolver})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s status=%d body=%s", authID, resp.StatusCode, resp.Body)
		}
	}

	failing := func(context.Context, string, CredentialResolutionAction) error { return errors.New("disk unavailable") }
	resp = HandleManagementRequestWithLifecycle(store, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/credentials/resolve", Body: body}, time.Now(), ManagementLifecycleSnapshot{Roster: roster, ResolveCredential: failing})
	if resp.StatusCode == http.StatusOK {
		t.Fatal("durability failure returned success")
	}
}

func TestCredentialResolutionRejectsNonAmbiguousChainWithoutMutation(t *testing.T) {
	for _, action := range []CredentialResolutionAction{CredentialConfirmOwned, CredentialConfirmExternal, CredentialReread} {
		t.Run(string(action), func(t *testing.T) {
			observed := fp("external", "new", "metadata")
			refresher, store, _, roster := newCredentialManagementFixture(t, observed)
			before, err := store.UpdateMirrored(func(s *PersistentState) error {
				b := s.Bindings["active"]
				s.CredentialChains[b.Instance] = TransitionChain{Cursor: b.Fingerprint}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			refresher.credentials.state = before
			if err = refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", action); !errors.Is(err, ErrCredentialResolutionScope) {
				t.Fatalf("action=%s err=%v, want ErrCredentialResolutionScope", action, err)
			}
			after, err := store.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			beforeRaw, _ := json.Marshal(before)
			afterRaw, _ := json.Marshal(after)
			if string(beforeRaw) != string(afterRaw) {
				t.Fatalf("action=%s mutated non-ambiguous state\nbefore=%s\nafter=%s", action, beforeRaw, afterRaw)
			}
		})
	}
}

func TestCredentialResolutionRouteRejectsNonAmbiguousChain(t *testing.T) {
	refresher, store, _, roster := newCredentialManagementFixture(t, fp("external", "new", "metadata"))
	committed, err := store.UpdateMirrored(func(s *PersistentState) error {
		b := s.Bindings["active"]
		s.CredentialChains[b.Instance] = TransitionChain{Cursor: b.Fingerprint}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	refresher.credentials.state = committed
	pluginState := NewPluginState(DefaultConfig())
	body, _ := json.Marshal(map[string]string{"auth_id": "active", "action": string(CredentialConfirmOwned)})
	resp := HandleManagementRequestWithLifecycle(pluginState, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/credentials/resolve", Body: body}, time.Now(), ManagementLifecycleSnapshot{Roster: roster, ResolveCredential: func(ctx context.Context, authID string, action CredentialResolutionAction) error {
		return refresher.ResolveCredentialAmbiguity(ctx, roster, authID, action)
	}})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestCredentialResolutionRechecksAmbiguityAtCommit(t *testing.T) {
	for _, action := range []CredentialResolutionAction{CredentialConfirmOwned, CredentialConfirmExternal, CredentialReread} {
		t.Run(string(action), func(t *testing.T) {
			observed := fp("subject", "refresh-1", "metadata")
			if action == CredentialConfirmExternal {
				observed = fp("external", "refresh-x", "metadata")
			}
			refresher, store, _, roster := newCredentialManagementFixture(t, observed)
			host := &gatedCredentialReadHost{started: make(chan struct{}), release: make(chan struct{}), result: HostAuth{Fingerprint: observed}}
			refresher.credentials.host = host
			done := make(chan error, 1)
			go func() { done <- refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", action) }()
			<-host.started
			resolved, err := store.UpdateMirrored(func(s *PersistentState) error {
				b := s.Bindings["active"]
				s.CredentialChains[b.Instance] = TransitionChain{Cursor: b.Fingerprint}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			close(host.release)
			if err = <-done; !errors.Is(err, ErrCredentialResolutionScope) {
				t.Fatalf("action=%s err=%v, want scope rejection", action, err)
			}
			after, _ := store.PersistentSnapshot()
			beforeRaw, _ := json.Marshal(resolved)
			afterRaw, _ := json.Marshal(after)
			if string(beforeRaw) != string(afterRaw) {
				t.Fatalf("action=%s mutated after ambiguity cleared\nbefore=%s\nafter=%s", action, beforeRaw, afterRaw)
			}
		})
	}
}

func TestAutomaticExternalLoginFencesBeforeDurableCommit(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	oldFingerprint := fp("subject", "old", "metadata")
	external := fp("external", "new", "metadata")
	commitReached := make(chan struct{})
	releaseCommit := make(chan struct{})
	var armed atomic.Bool
	hooks := OSFileHooks()
	hooks.Observe = func(op string) {
		if op == "primary-dir-fsync" && armed.CompareAndSwap(true, false) {
			close(commitReached)
			<-releaseCommit
		}
	}
	store := NewStateStore(filepath.Join(t.TempDir(), "runtime.json"), hooks, nil)
	state := NewPersistentState()
	state.TierGeneration = 5
	state.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx", Instance: 1, Admission: 3, Generation: 5, Login: 7, Token: 11, Fingerprint: oldFingerprint}
	state.CredentialChains[1] = TransitionChain{Cursor: oldFingerprint}
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	manager := mustCredentialManager(t, store, &walHost{current: HostAuth{Fingerprint: external}}, func() time.Time { return now }, nil)
	pluginState := NewPluginState(DefaultConfig())
	started := make(chan struct{})
	var applied atomic.Bool
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		close(started)
		<-commitReached
		return OperationResult{Token: intent.Token, Login: intent.Login, Fingerprint: intent.Fingerprint}
	}, Validate: func(intent Intent, result OperationResult) bool {
		return registry.ApplyIfCurrent(intent.AuthID, WritebackVersion{Token: result.Token, Login: result.Login, Fingerprint: result.Fingerprint}, func() {
			applied.Store(true)
			pluginState.RecordLog("info", "stale.apply", "stale automatic writeback applied", nil, now)
		})
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	r := &QuotaRefresher{runtimeStore: store, bindings: registry, credentials: manager, coordinator: coordinator, state: pluginState, now: func() time.Time { return now }}
	binding, _ := registry.Lookup("active")
	old := coordinator.Submit(Intent{AuthID: "active", Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: binding.ExecutionToken(0), Login: binding.Login, Fingerprint: binding.Fingerprint})
	<-started
	armed.Store(true)
	syncDone := make(chan struct{})
	go func() {
		r.reconcileActiveCredentialTails(context.Background(), map[string]RuntimeBinding{"active": binding}, nil)
		close(syncDone)
	}()
	<-commitReached
	result := old.Await(context.Background())
	if result.Disposition != ResultCancelled || applied.Load() || len(pluginState.Snapshot(now).Logs) != 0 {
		close(releaseCommit)
		<-syncDone
		t.Fatalf("old result crossed automatic external commit: disposition=%s applied=%v logs=%v", result.Disposition, applied.Load(), pluginState.Snapshot(now).Logs)
	}
	close(releaseCommit)
	<-syncDone
}

func TestAutomaticExternalLoginReloadFailureRollsBack(t *testing.T) {
	external := fp("external", "new", "metadata")
	r, store, _, _ := newCredentialManagementFixture(t, external)
	committed, err := store.UpdateMirrored(func(s *PersistentState) error {
		b := s.Bindings["active"]
		s.CredentialChains[b.Instance] = TransitionChain{Cursor: b.Fingerprint}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.credentials.state = committed
	binding := committed.Bindings["active"]
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{binding.Instance: TierGeneration(binding.Generation)})
	r.coordinator = coordinator
	r.bindings = nil // force mirror reload failure after durable reconciliation
	r.reconcileActiveCredentialTails(context.Background(), map[string]RuntimeBinding{"active": binding}, nil)
	after, _ := store.PersistentSnapshot()
	beforeRaw, _ := json.Marshal(committed)
	afterRaw, _ := json.Marshal(after)
	if string(beforeRaw) != string(afterRaw) {
		t.Fatalf("reload failure left durable external epochs advanced\nbefore=%s\nafter=%s", beforeRaw, afterRaw)
	}
	result := coordinator.Submit(Intent{Instance: binding.Instance, Generation: TierGeneration(binding.Generation), Class: OperationQuotaRead, Source: SourceManualRefresh, Token: binding.ExecutionToken(0)}).Await(context.Background())
	if result.Disposition != ResultApplied {
		t.Fatalf("reload failure did not restore exact old generation: %s", result.Disposition)
	}
}

func TestAutomaticExternalLoginRollbackDoesNotUndoConcurrentCancel(t *testing.T) {
	now := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	oldFingerprint := fp("subject", "old", "metadata")
	external := fp("external", "new", "metadata")
	store := NewStateStore(filepath.Join(t.TempDir(), "runtime.json"), OSFileHooks(), nil)
	state := NewPersistentState()
	state.TierGeneration = 5
	state.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx", Instance: 1, Admission: 3, Generation: 5, Login: 7, Token: 11, Fingerprint: oldFingerprint}
	state.CredentialChains[1] = TransitionChain{Cursor: oldFingerprint}
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	manager := mustCredentialManager(t, store, &walHost{current: HostAuth{Fingerprint: external}}, func() time.Time { return now }, nil)
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	oldGenerations := map[AuthInstanceID]TierGeneration{1: 5}
	var tokens map[AuthInstanceID]uint64
	committed := make(chan struct{})
	releaseReload := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := manager.ReconcileWithHooks(context.Background(), 1, CredentialReconcileHooks{
			BeforeCommit: func(report CredentialRecoveryReport) error {
				if report.Observation.Kind != CredentialExternalLogin {
					return errors.New("expected external observation")
				}
				tokens = coordinator.suspendInstancesForRollback([]AuthInstanceID{1})
				return nil
			},
			AfterCommit: func(CredentialRecoveryReport, PersistentState) error {
				close(committed)
				<-releaseReload
				return errors.New("reload failed")
			},
		})
		done <- err
	}()
	<-committed
	coordinator.CancelInstances([]AuthInstanceID{1})
	close(releaseReload)
	if err := <-done; err == nil {
		t.Fatal("reload failure missing")
	}
	coordinator.restoreCancelledInstances(oldGenerations, tokens)
	result := coordinator.Submit(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Tier: 5}}).Await(context.Background())
	if result.Disposition != ResultCancelled {
		t.Fatalf("automatic rollback undid later authoritative cancellation: %s", result.Disposition)
	}
	after, _ := store.PersistentSnapshot()
	if after.TierGeneration != 5 || after.Bindings["active"].Login != 7 || after.CredentialChains[1].Cursor != oldFingerprint {
		t.Fatalf("reload rollback did not restore durable credential state: %#v", after)
	}
}

func TestCredentialConfirmExternalFencesPriorAdmissionBeforeReuse(t *testing.T) {
	external := fp("external", "refresh-x", "metadata")
	refresher, _, _, roster := newCredentialManagementFixture(t, external)
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		if intent.Generation == 5 {
			close(started)
			<-release
		}
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	refresher.coordinator = coordinator
	old := coordinator.Submit(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}})
	<-started
	if err := refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", CredentialConfirmExternal); err != nil {
		t.Fatal(err)
	}
	if got := old.Await(context.Background()); got.Disposition != ResultCancelled {
		t.Fatalf("old disposition=%q", got.Disposition)
	}
	newResult := coordinator.Submit(Intent{Instance: 1, Generation: 6, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 4, Tier: 6}}).Await(context.Background())
	if newResult.Disposition != ResultApplied {
		t.Fatalf("new disposition=%q", newResult.Disposition)
	}
	close(release)
}

func TestCredentialConfirmExternalReadFailureRestoresOldAdmission(t *testing.T) {
	external := fp("external", "refresh-x", "metadata")
	refresher, _, _, roster := newCredentialManagementFixture(t, external)
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	refresher.coordinator = coordinator
	refresher.credentials.host = failingCredentialReadHost{err: errors.New("get auth failed")}
	if err := refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", CredentialConfirmExternal); err == nil {
		t.Fatal("GetAuth failure returned nil")
	}
	result := coordinator.Submit(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}}).Await(context.Background())
	if result.Disposition != ResultApplied {
		t.Fatalf("restored disposition=%q", result.Disposition)
	}
}

func TestCredentialConfirmExternalRollbackDoesNotUndoConcurrentRemoval(t *testing.T) {
	external := fp("external", "refresh-x", "metadata")
	refresher, _, _, roster := newCredentialManagementFixture(t, external)
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	refresher.coordinator = coordinator
	host := &gatedCredentialReadHost{started: make(chan struct{}), release: make(chan struct{}), err: errors.New("get auth failed")}
	refresher.credentials.host = host
	done := make(chan error, 1)
	go func() {
		done <- refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", CredentialConfirmExternal)
	}()
	<-host.started
	coordinator.CancelInstances([]AuthInstanceID{1}) // authoritative removal after Management's suspension
	close(host.release)
	if err := <-done; err == nil {
		t.Fatal("GetAuth failure returned nil")
	}
	result := coordinator.Submit(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}}).Await(context.Background())
	if result.Disposition != ResultCancelled {
		t.Fatalf("concurrent removal was undone: disposition=%q", result.Disposition)
	}
}

func TestCredentialConfirmExternalRollbackClearsCancelledTypedJoin(t *testing.T) {
	external := fp("external", "refresh-x", "metadata")
	refresher, _, _, roster := newCredentialManagementFixture(t, external)
	legacyStarted := make(chan struct{})
	releaseLegacy := make(chan struct{})
	var quotaStarts atomic.Int64
	coordinator := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		if intent.Class == OperationLegacyRefresh {
			close(legacyStarted)
			<-releaseLegacy
		} else if intent.Class == OperationQuotaRead {
			quotaStarts.Add(1)
		}
		return OperationResult{Token: intent.Token}
	}})
	t.Cleanup(coordinator.Close)
	coordinator.activateInstances(map[AuthInstanceID]TierGeneration{1: 5})
	refresher.coordinator = coordinator
	legacy := coordinator.Submit(Intent{Instance: 1, Generation: 5, Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}})
	<-legacyStarted
	queued := coordinator.SubmitTyped(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}})
	refresher.credentials.host = failingCredentialReadHost{err: errors.New("get auth failed")}
	if err := refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", CredentialConfirmExternal); err == nil {
		t.Fatal("GetAuth failure returned nil")
	}
	if got := queued.Await(context.Background()); got.Disposition != ResultCancelled {
		t.Fatalf("queued disposition=%q", got.Disposition)
	}
	fresh := coordinator.SubmitTyped(Intent{Instance: 1, Generation: 5, Class: OperationQuotaRead, Source: SourceManualRefresh, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5}}).Await(context.Background())
	if fresh.Disposition != ResultApplied || quotaStarts.Load() != 1 {
		t.Fatalf("fresh=%#v quotaStarts=%d", fresh, quotaStarts.Load())
	}
	_ = legacy.Await(context.Background())
	close(releaseLegacy)
}
