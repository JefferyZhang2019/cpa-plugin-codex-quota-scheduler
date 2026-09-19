package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
const resetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const codexTokenEndpoint = "https://auth.openai.com/oauth/token"
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const rosterLifecycleRequestHeader = "X-CPA-Roster-Lifecycle"
const rosterLifecycleDegraded = "degraded"
const rosterLifecycleProvisional = "provisional"

const maxErrorBodySummaryLen = 220

var (
	rawCookiePattern                  = regexp.MustCompile(`(?i)\b(cookie\s*[:=]\s*)[^;\s,}"']+(?:\s*;\s*[^;\s,}"']+)*`)
	rawSecretPattern                  = regexp.MustCompile(`(?i)\b((?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)\s*[:=]\s*)(?:bearer\s+)?[^\s,}"']+`)
	secretFieldPattern                = regexp.MustCompile(`(?i)((?:"?(?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)"?)\s*[:=]\s*)("[^"]*"|[^\s,}\]]+)`)
	bearerPattern                     = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	errCPAAdmissionChanged            = errors.New("CPA admission changed during refresh")
	ErrProvisionalFingerprintMismatch = errors.New("provisional credential fingerprint changed")
	ErrCredentialResolutionScope      = errors.New("credential resolution requires an active roster instance")
	ErrAccountIdentityUnresolved      = errors.New("account identity unresolved")
)

type CredentialResolutionAction string

const (
	CredentialConfirmOwned    CredentialResolutionAction = "confirm_owned_rotation"
	CredentialConfirmExternal CredentialResolutionAction = "confirm_external_login"
	CredentialReread          CredentialResolutionAction = "reread"
)

func (a CredentialResolutionAction) Valid() bool {
	return a == CredentialConfirmOwned || a == CredentialConfirmExternal || a == CredentialReread
}

func rosterContainsAuthID(roster ActiveRoster, authID string) bool {
	if !roster.Confirmed || roster.Health == RosterFailClosed {
		return false
	}
	for _, active := range roster.Instances {
		if active == authID {
			return true
		}
	}
	return false
}

func (r *QuotaRefresher) ResolveCredentialAmbiguity(ctx context.Context, roster ActiveRoster, authID string, action CredentialResolutionAction) error {
	if r == nil || r.bindings == nil || r.credentials == nil || r.runtimeStore == nil || !action.Valid() || !rosterContainsAuthID(roster, authID) {
		return ErrCredentialResolutionScope
	}
	binding, ok := r.bindings.Lookup(authID)
	if !ok || binding.Instance == 0 || binding.AuthID != authID {
		return ErrBindingNotRosterConfirmed
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	chain, ok := persisted.CredentialChains[binding.Instance]
	if !ok || !credentialChainUnresolved(chain) {
		return ErrCredentialResolutionScope
	}
	if action == CredentialReread {
		report, err := r.credentials.ReconcileUnresolved(ctx, binding.Instance)
		if err != nil {
			return err
		}
		if report.Ambiguous {
			return ErrCredentialUnresolved
		}
		return r.reloadCredentialMirrors()
	}
	var oldGenerations map[AuthInstanceID]TierGeneration
	var cancellationTokens map[AuthInstanceID]uint64
	cancelledExternal := action == CredentialConfirmExternal && r.coordinator != nil
	if cancelledExternal {
		oldGenerations = map[AuthInstanceID]TierGeneration{binding.Instance: TierGeneration(binding.Generation)}
		cancellationTokens = r.coordinator.suspendInstancesForRollback([]AuthInstanceID{binding.Instance})
	}
	restoreCoordinator := func() {
		if cancelledExternal {
			r.coordinator.restoreCancelledInstances(oldGenerations, cancellationTokens)
		}
	}
	observed, err := r.credentials.host.GetAuth(ctx, binding.Instance)
	if err != nil {
		restoreCoordinator()
		return err
	}
	if !observed.Fingerprint.ValidHashes() {
		restoreCoordinator()
		return ErrCredentialUnresolved
	}

	r.credentials.mu.Lock()
	r.bindings.mu.Lock()
	current, ok := r.bindings.bindings[authID]
	if !ok || current.Instance != binding.Instance || current.Admission != binding.Admission || current.Generation != binding.Generation || current.Fingerprint != binding.Fingerprint {
		r.bindings.mu.Unlock()
		r.credentials.mu.Unlock()
		restoreCoordinator()
		return ErrCredentialResolutionScope
	}
	committed, err := r.runtimeStore.UpdateMirrored(func(state *PersistentState) error {
		latest, ok := state.Bindings[authID]
		if !ok || latest.Instance != current.Instance || latest.Admission != current.Admission || latest.Generation != current.Generation || latest.Fingerprint != current.Fingerprint {
			return ErrCredentialResolutionScope
		}
		if !credentialChainUnresolved(state.CredentialChains[current.Instance]) {
			return ErrCredentialResolutionScope
		}
		state.CredentialChains[current.Instance] = TransitionChain{Cursor: observed.Fingerprint}
		switch action {
		case CredentialConfirmOwned:
			latest.Token++
			latest.Fingerprint = observed.Fingerprint
			state.Bindings[authID] = latest
		case CredentialConfirmExternal:
			latest.Login++
			latest.Admission++
			latest.Fingerprint = observed.Fingerprint
			latest.AuthBlocked = false
			state.TierGeneration++
			if state.TierGeneration == 0 {
				state.TierGeneration = 1
			}
			if state.AdmissionEpochs == nil {
				state.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
			}
			state.AdmissionEpochs[current.Instance] = latest.Admission
			latest.Generation = AuthBindingEpoch(state.TierGeneration)
			for id, active := range state.Bindings {
				active.Generation = AuthBindingEpoch(state.TierGeneration)
				state.Bindings[id] = active
			}
			state.Bindings[authID] = latest
		}
		return nil
	})
	if err != nil {
		r.bindings.mu.Unlock()
		r.credentials.mu.Unlock()
		restoreCoordinator()
		return err
	}
	r.bindings.bindings = committed.Bindings
	r.credentials.state = committed
	r.bindings.mu.Unlock()
	r.credentials.mu.Unlock()
	if cancelledExternal {
		generations := make(map[AuthInstanceID]TierGeneration, len(committed.Bindings))
		for _, active := range committed.Bindings {
			generations[active.Instance] = TierGeneration(active.Generation)
		}
		r.coordinator.activateInstances(generations)
	}
	return nil
}

func credentialChainUnresolved(chain TransitionChain) bool {
	current := chain.Cursor
	for _, transition := range chain.Transitions {
		if transition.Prev.CompositeHash != current.CompositeHash {
			break
		}
		switch transition.Phase {
		case TransitionApplied:
			current = transition.Next
		case TransitionPlanned, TransitionOutcomeUnknown:
			return true
		default:
			return false
		}
	}
	return false
}

var callHostCallback = func(method string, payload any) (json.RawMessage, error) {
	return nil, fmt.Errorf("host callback %s unavailable", method)
}

type HostClient interface {
	ListAuths() ([]pluginapi.HostAuthFileEntry, error)
	GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error)
	SaveAuth(name string, raw json.RawMessage) error
	Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	Log(level, message string, fields map[string]any)
}

type ABIHostClient struct{}

type QuotaRefresher struct {
	host              HostClient
	state             *PluginState
	now               func() time.Time
	runtimeStore      *StateStore
	bindings          *BindingRegistry
	credentials       *CredentialManager
	roster            HostRosterSnapshot
	rosterMu          sync.RWMutex
	rosterAdmissionMu sync.RWMutex
	lifecycleOwner    *QuotaRefresher
	lifecycleGate     func(time.Time) ActiveRoster

	mu                     sync.Mutex
	running                bool
	startRequested         bool
	refreshing             bool
	stopping               bool
	stop                   chan struct{}
	done                   chan struct{}
	wake                   chan struct{}
	pendingDue             func() error
	pendingDueVersion      uint64
	wg                     sync.WaitGroup
	coordinator            *Coordinator
	legacyTxn              *LegacyRefreshTxn
	refreshController      *RefreshController
	probeController        *ProbeController
	probeWAL               *ProbeWAL
	probeFence             *FenceAllocator
	probeRunMu             sync.Mutex
	probeRunStateMu        sync.Mutex
	probeRerunPending      bool
	probeLaunchActive      bool
	probeRetryAt           time.Time
	probeRetryGeneration   uint64
	probeRecoveryPending   bool
	probeOwnerRetryPending bool
	probeHoldMu            sync.Mutex
	probePersistPending    map[AuthInstanceID]struct{}
	probeHoldPending       bool
	probeHoldErr           error
	provisionalMu          sync.Mutex
	provisionalPermit      bool
	fenceMu                sync.Mutex
	nextFence              uint64
	txnIntent              *Intent
	txnContext             context.Context
	txnLease               *HeldLease
	txnPermit              func() bool
	finalPublicationHook   func()
}

// NewProductionQuotaRefresher wires S2 persistence/identity primitives into
// the production refresher without changing S3 coordinator behavior.
func NewProductionQuotaRefresher(host HostClient, state *PluginState, credentialHost CredentialHost, roster HostRosterSnapshot, legacyStatePath string, now func() time.Time) (*QuotaRefresher, error) {
	r := NewQuotaRefresher(host, state, now)
	store := NewStateStore(semanticStatePaths(legacyStatePath).Runtime, OSFileHooks(), nil)
	bindings, err := NewBindingRegistry(store)
	if err != nil {
		return nil, err
	}
	credentials, err := NewCredentialManager(store, credentialHost, now, nil)
	if err != nil {
		return nil, err
	}
	r.runtimeStore, r.bindings, r.credentials, r.roster = store, bindings, credentials, normalizeHostRosterLifecycle(roster)
	if err := r.initProbeRuntime(); err != nil {
		return nil, err
	}
	if roster.Capability == CapabilityB {
		if provisional := r.ProvisionalRoster(); provisional != nil {
			r.roster = hostRosterSnapshotFromActive(*provisional)
		}
	}
	return r, nil
}

func (r *QuotaRefresher) ProvisionalRoster() *ActiveRoster {
	if r == nil || r.runtimeStore == nil {
		return nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil || state.LastConfirmedRoster == nil {
		return nil
	}
	persisted := state.LastConfirmedRoster
	if persisted.Generation == 0 || !provisionalAgeValid(r.now(), persisted.ConfirmedAt) || len(persisted.Entries) == 0 {
		return nil
	}
	active := &ActiveRoster{Capability: CapabilityB, Provisional: true, Generation: persisted.Generation, ConfirmedAt: persisted.ConfirmedAt, Health: RosterWaiting}
	seen := make(map[string]struct{}, len(persisted.Entries))
	for i, entry := range persisted.Entries {
		if entry.ID == "" || entry.AuthIndex == "" || !entry.Fingerprint.ValidHashes() {
			return nil
		}
		if i > 0 && entry.Priority != persisted.Entries[0].Priority {
			return nil
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return nil
		}
		seen[entry.ID] = struct{}{}
		priority := entry.Priority
		active.Entries = append(active.Entries, RosterEntry{ID: entry.ID, AuthIndex: entry.AuthIndex, Provider: "codex", Priority: &priority})
		active.Instances = append(active.Instances, entry.ID)
		active.HighestPriority = entry.Priority
	}
	sort.Strings(active.Instances)
	return active
}

func (r *QuotaRefresher) VerifyProvisionalRoster(ctx context.Context, roster ActiveRoster) bool {
	if r == nil || r.runtimeStore == nil || r.bindings == nil || r.host == nil || !roster.Provisional || roster.Confirmed || roster.Capability != CapabilityB || !provisionalAgeValid(r.now(), roster.ConfirmedAt) {
		r.clearProvisionalPermit()
		return false
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil || state.LastConfirmedRoster == nil {
		r.clearProvisionalPermit()
		return false
	}
	persisted := state.LastConfirmedRoster
	if persisted.Generation != roster.Generation || !persisted.ConfirmedAt.Equal(roster.ConfirmedAt) || len(persisted.Entries) != len(roster.Entries) {
		r.clearProvisionalPermit()
		return false
	}
	want := make(map[string]RosterEntry, len(roster.Entries))
	for _, entry := range roster.Entries {
		want[entry.ID] = entry
	}
	for _, entry := range persisted.Entries {
		current, ok := want[entry.ID]
		if !ok || current.AuthIndex != entry.AuthIndex || current.Priority == nil || *current.Priority != entry.Priority {
			r.clearProvisionalPermit()
			return false
		}
		binding, bound := r.bindings.Lookup(entry.ID)
		if !bound || binding.AuthID != entry.ID || binding.Instance == 0 || binding.Instance != legacyAuthInstanceID(entry.ID) || uint64(binding.Generation) != roster.Generation || binding.AuthIndex != entry.AuthIndex || binding.Fingerprint != entry.Fingerprint {
			r.clearProvisionalPermit()
			return false
		}
		select {
		case <-ctx.Done():
			r.clearProvisionalPermit()
			return false
		default:
		}
		auth, getErr := r.host.GetAuth(entry.AuthIndex)
		if getErr != nil {
			r.clearProvisionalPermit()
			return false
		}
		credentials, extractErr := ExtractCodexCredentials(auth.JSON)
		if extractErr != nil || NewCredentialFingerprint(credentials.ChatGPTAccountID, credentials.RefreshToken, entry.AuthIndex) != entry.Fingerprint {
			r.clearProvisionalPermit()
			return false
		}
	}
	if !provisionalAgeValid(r.now(), roster.ConfirmedAt) {
		r.clearProvisionalPermit()
		return false
	}
	r.provisionalMu.Lock()
	r.provisionalPermit = true
	r.provisionalMu.Unlock()
	return true
}

func (r *QuotaRefresher) VerifyConfiguredProvisionalRoster(ctx context.Context, roster ActiveRoster) bool {
	if r == nil || r.state == nil || !r.state.Config().ProbeOnProvisionalRoster {
		r.clearProvisionalPermit()
		return false
	}
	if !r.VerifyProvisionalRoster(ctx, roster) {
		return false
	}
	if !r.state.Config().ProbeOnProvisionalRoster || !provisionalAgeValid(r.now(), roster.ConfirmedAt) {
		r.clearProvisionalPermit()
		return false
	}
	return true
}

func (r *QuotaRefresher) clearProvisionalPermit() {
	if r == nil {
		return
	}
	r.provisionalMu.Lock()
	r.provisionalPermit = false
	r.provisionalMu.Unlock()
}

func (r *QuotaRefresher) consumeProvisionalPermit() bool {
	r.provisionalMu.Lock()
	defer r.provisionalMu.Unlock()
	allowed := r.provisionalPermit
	r.provisionalPermit = false
	return allowed
}

func (r *QuotaRefresher) BootstrapBinding(ctx context.Context, authID string) (RuntimeBinding, bool, error) {
	if r == nil || r.bindings == nil || r.credentials == nil {
		return RuntimeBinding{}, false, errors.New("runtime wiring unavailable")
	}
	return r.bindings.ObserveAuthoritative(ctx, r.runtimeRoster(), authID, r.credentials.host)
}

func (r *QuotaRefresher) runtimeRoster() HostRosterSnapshot {
	owner := r.lifecycleRefresher()
	owner.rosterMu.RLock()
	defer owner.rosterMu.RUnlock()
	return owner.roster
}

func (r *QuotaRefresher) lifecycleRefresher() *QuotaRefresher {
	if r != nil && r.lifecycleOwner != nil {
		return r.lifecycleOwner
	}
	return r
}

func (r *QuotaRefresher) SetRosterLifecycleAuthority(gate func(time.Time) ActiveRoster) {
	if r == nil {
		return
	}
	r.lifecycleRefresher().lifecycleGate = gate
}

func (r *QuotaRefresher) lifecycleNow() time.Time {
	owner := r.lifecycleRefresher()
	if owner != nil && owner.now != nil {
		return owner.now()
	}
	return time.Now()
}

func failClosedRosterAt(roster HostRosterSnapshot, now time.Time) (HostRosterSnapshot, bool) {
	roster = normalizeHostRosterLifecycle(roster)
	if roster.Capability != CapabilityA || !roster.Confirmed || roster.Health != RosterDegraded ||
		roster.DegradedSince.IsZero() || now.Before(roster.DegradedSince.Add(rosterDegradedLimit)) {
		return roster, false
	}
	roster.Health = RosterFailClosed
	roster.BackgroundAllowed = false
	roster.LifecycleRevision++
	return roster, true
}

func (r *QuotaRefresher) enforceRosterLifecycle() HostRosterSnapshot {
	if r == nil {
		return HostRosterSnapshot{Capability: CapabilityB}
	}
	owner := r.lifecycleRefresher()
	now := owner.lifecycleNow()
	if owner.lifecycleGate != nil {
		owner.lifecycleGate(now)
	}
	owner.rosterMu.RLock()
	out := owner.roster
	_, expired := failClosedRosterAt(out, now)
	owner.rosterMu.RUnlock()
	if !expired {
		return out
	}
	owner.rosterMu.Lock()
	next, transitioned := failClosedRosterAt(owner.roster, now)
	if transitioned {
		owner.roster = next
	}
	out = owner.roster
	owner.rosterMu.Unlock()
	if transitioned {
		_ = owner.persistProbeRosterHold()
	}
	return out
}

func (r *QuotaRefresher) ObserveRosterLifecycle(active ActiveRoster) {
	if r == nil {
		return
	}
	owner := r.lifecycleRefresher()
	owner.rosterMu.Lock()
	next := hostRosterSnapshotFromActive(active)
	if next.Provisional && (!provisionalAgeValid(owner.now(), next.ConfirmedAt) || owner.state == nil || !owner.state.Config().ProbeOnProvisionalRoster) {
		next.BackgroundAllowed = false
	}
	if next.LifecycleRevision == 0 {
		next.LifecycleRevision = owner.roster.LifecycleRevision + 1
	}
	if next.LifecycleRevision <= owner.roster.LifecycleRevision {
		owner.rosterMu.Unlock()
		return
	}
	owner.roster = next
	owner.rosterMu.Unlock()
	if next.Health == RosterFailClosed {
		_ = owner.persistProbeRosterHold()
	}
	owner.mu.Lock()
	requested := owner.startRequested
	owner.mu.Unlock()
	if requested && (owner.normalBackgroundAllowed() || owner.probeBackgroundAllowed()) {
		owner.Start()
	}
}
func (r *QuotaRefresher) normalBackgroundAllowed() bool {
	if r == nil {
		return false
	}
	roster := normalizeHostRosterLifecycle(r.enforceRosterLifecycle())
	return normalRosterBackgroundAllowed(roster)
}
func (r *QuotaRefresher) resetProbeEnabled() bool {
	return r != nil && r.state != nil && r.state.Config().EnableResetProbe
}
func (r *QuotaRefresher) probeBackgroundAllowed() bool {
	if !r.resetProbeEnabled() {
		return false
	}
	if r.normalBackgroundAllowed() {
		return true
	}
	roster := normalizeHostRosterLifecycle(r.enforceRosterLifecycle())
	return provisionalProbeBackgroundAllowed(roster) && r.state != nil && r.state.Config().ProbeOnProvisionalRoster && provisionalAgeValid(r.now(), roster.ConfirmedAt)
}

func (r *QuotaRefresher) provisionalProbeRiskConfigured() bool {
	if r == nil || r.state == nil || !r.state.Config().ProbeOnProvisionalRoster {
		return false
	}
	roster := normalizeHostRosterLifecycle(r.runtimeRoster())
	return roster.Capability == CapabilityB && roster.Provisional && !roster.Confirmed && provisionalAgeValid(r.now(), roster.ConfirmedAt)
}

func normalRosterBackgroundAllowed(roster HostRosterSnapshot) bool {
	return roster.Capability == CapabilityA && roster.Confirmed && roster.BackgroundAllowed && (roster.Health == RosterHealthy || roster.Health == RosterDegraded)
}

func provisionalProbeBackgroundAllowed(roster HostRosterSnapshot) bool {
	return roster.Capability == CapabilityB && roster.Provisional && roster.BackgroundAllowed && roster.Health == RosterWaiting
}
func (r *QuotaRefresher) PublishAuthoritativeRoster(ctx context.Context, roster HostRosterSnapshot) error {
	if r == nil || r.bindings == nil || r.credentials == nil {
		return errors.New("runtime wiring unavailable")
	}
	roster = normalizeHostRosterLifecycle(roster)
	if err := r.retryProbeRosterHold(); err != nil {
		return err
	}
	priority, ids, ok := HighestCodexTier(roster.Entries)
	if roster.Capability != CapabilityA || !ok {
		r.rosterMu.Lock()
		r.roster = HostRosterSnapshot{Capability: CapabilityB}
		r.rosterMu.Unlock()
		return ErrCapabilityB
	}
	allowed := map[string]struct{}{}
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	filtered := roster
	filtered.Entries = nil
	for _, entry := range roster.Entries {
		if _, yes := allowed[entry.ID]; yes {
			filtered.Entries = append(filtered.Entries, entry)
		}
	}
	bootstrapHost := r.credentials.host
	if adapter, ok := r.credentials.host.(*rosterCredentialHost); ok {
		bootstrapHost = bootstrapCredentialHost{adapter: adapter, roster: filtered}
	}
	previousBindings := make(map[string]RuntimeBinding)
	r.bindings.mu.RLock()
	for authID, binding := range r.bindings.bindings {
		previousBindings[authID] = binding
	}
	r.bindings.mu.RUnlock()
	reconciled, err := r.bindings.ReconcileRoster(ctx, filtered, bootstrapHost)
	if err != nil {
		return err
	}
	freshlyObserved := make(map[string]struct{})
	for authID, binding := range reconciled.Bindings {
		previous, existed := previousBindings[authID]
		if !existed || previous.AuthIndex != binding.AuthIndex {
			freshlyObserved[authID] = struct{}{}
		}
	}
	r.reconcileActiveCredentialTails(ctx, reconciled.Bindings, freshlyObserved)
	publishedState, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	publishedBindings := make(map[string]RuntimeBinding, len(reconciled.Bindings))
	for authID := range reconciled.Bindings {
		binding, ok := publishedState.Bindings[authID]
		if !ok || binding.Instance == 0 {
			return ErrBindingNotRosterConfirmed
		}
		publishedBindings[authID] = binding
	}
	reconciled.Bindings = publishedBindings
	reconciled.Generation = publishedState.TierGeneration
	filtered.Generation = uint64(reconciled.Generation)
	if filtered.ConfirmedAt.IsZero() {
		filtered.ConfirmedAt = r.now()
	}
	persistedRoster := &PersistedConfirmedRoster{Generation: filtered.Generation, ConfirmedAt: filtered.ConfirmedAt}
	for _, entry := range filtered.Entries {
		binding, found := reconciled.Bindings[entry.ID]
		if !found || entry.Priority == nil {
			return ErrBindingNotRosterConfirmed
		}
		persistedRoster.Entries = append(persistedRoster.Entries, PersistedRosterEntry{ID: entry.ID, AuthIndex: entry.AuthIndex, Priority: *entry.Priority, Fingerprint: binding.Fingerprint})
	}
	if _, err = r.runtimeStore.UpdateMirrored(func(s *PersistentState) error {
		s.LastConfirmedRoster = persistedRoster
		return nil
	}); err != nil {
		return err
	}
	activeGenerations := make(map[AuthInstanceID]TierGeneration, len(reconciled.Bindings))
	for _, binding := range reconciled.Bindings {
		activeGenerations[binding.Instance] = TierGeneration(binding.Generation)
	}
	r.coordinator.activateInstances(activeGenerations)
	removedIDs := make([]string, 0, len(reconciled.Removed))
	for _, binding := range reconciled.Removed {
		removedIDs = append(removedIDs, binding.AuthID)
	}
	if err = r.CancelRosterInstances(removedIDs); err != nil {
		return err
	}
	if err = r.recoverProbeFromRoster(reconciled.Bindings); err != nil {
		return err
	}
	owner := r.lifecycleRefresher()
	owner.rosterAdmissionMu.Lock()
	r.state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: priority, AuthIDs: allowed})
	if r.finalPublicationHook != nil {
		r.finalPublicationHook()
	}
	publishSchedulerState(r.state, allowed, r.now())
	r.rosterMu.Lock()
	r.roster = filtered
	r.rosterMu.Unlock()
	if adapter, ok := r.credentials.host.(*rosterCredentialHost); ok {
		adapter.setRoster(filtered)
	}
	owner.rosterAdmissionMu.Unlock()
	r.mu.Lock()
	requested := r.startRequested
	r.mu.Unlock()
	if requested {
		r.Start()
	}
	return nil
}

func (r *QuotaRefresher) reconcileActiveCredentialTails(ctx context.Context, bindings map[string]RuntimeBinding, freshlyObserved map[string]struct{}) {
	if r == nil || r.credentials == nil || len(bindings) == 0 {
		return
	}
	authIDs := make([]string, 0, len(bindings))
	for authID := range bindings {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	for _, authID := range authIDs {
		if _, skip := freshlyObserved[authID]; skip {
			continue
		}
		binding := bindings[authID]
		if binding.Instance == 0 {
			continue
		}
		oldGenerations := map[AuthInstanceID]TierGeneration{binding.Instance: TierGeneration(binding.Generation)}
		var cancellationTokens map[AuthInstanceID]uint64
		suspended := false
		report, err := r.credentials.ReconcileWithHooks(ctx, binding.Instance, CredentialReconcileHooks{
			BeforeCommit: func(report CredentialRecoveryReport) error {
				if report.Observation.Kind == CredentialExternalLogin && r.coordinator != nil {
					cancellationTokens = r.coordinator.suspendInstancesForRollback([]AuthInstanceID{binding.Instance})
					if _, ok := cancellationTokens[binding.Instance]; !ok {
						return ErrCredentialResolutionScope
					}
					suspended = true
				}
				return nil
			},
			AfterCommit: func(report CredentialRecoveryReport, committed PersistentState) error {
				if report.Observation.Kind != CredentialOwnedRotation && report.Observation.Kind != CredentialExternalLogin {
					return nil
				}
				return r.publishCredentialBindingMirror(committed)
			},
		})
		if err != nil && r.state != nil {
			if suspended && !errors.Is(err, ErrCredentialRollbackFailed) {
				r.coordinator.restoreCancelledInstances(oldGenerations, cancellationTokens)
			}
			r.state.RecordLog("warn", "credential.reconcile_failed", "credential reconciliation will retry on a later authoritative roster sync", map[string]any{"auth_id": authID}, r.now())
			continue
		}
		if err != nil {
			if suspended && !errors.Is(err, ErrCredentialRollbackFailed) {
				r.coordinator.restoreCancelledInstances(oldGenerations, cancellationTokens)
			}
			continue
		}
		if report.Observation.Kind == CredentialExternalLogin {
			current, ok := r.bindings.Lookup(authID)
			if ok && r.coordinator != nil {
				r.coordinator.activateInstances(map[AuthInstanceID]TierGeneration{current.Instance: TierGeneration(current.Generation)})
			}
		}
	}
}

func (r *QuotaRefresher) publishCredentialBindingMirror(state PersistentState) error {
	if r == nil || r.bindings == nil {
		return ErrCredentialResolutionScope
	}
	r.bindings.mu.Lock()
	r.bindings.bindings = state.Bindings
	r.bindings.mu.Unlock()
	return nil
}

func (r *QuotaRefresher) reloadCredentialMirrors() error {
	if r == nil || r.runtimeStore == nil || r.credentials == nil || r.bindings == nil {
		return ErrCredentialResolutionScope
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	r.credentials.mu.Lock()
	r.credentials.state = state
	r.credentials.mu.Unlock()
	r.bindings.mu.Lock()
	r.bindings.bindings = state.Bindings
	r.bindings.mu.Unlock()
	return nil
}

func (r *QuotaRefresher) CancelRosterInstances(authIDs []string) error {
	if r == nil || len(authIDs) == 0 {
		return nil
	}
	instances := make([]AuthInstanceID, 0, len(authIDs))
	for _, authID := range authIDs {
		instances = append(instances, legacyAuthInstanceID(authID))
	}
	if r.coordinator != nil {
		r.coordinator.CancelInstances(instances)
	}
	if r.probeController != nil {
		for _, instance := range instances {
			r.probeController.Advance(instance, ProbeEvent{Kind: ProbeEventInstanceRemoved, Now: r.now()})
		}
	}
	if r.runtimeStore == nil {
		return nil
	}
	_, err := r.runtimeStore.Update(func(s *PersistentState) error {
		for _, instance := range instances {
			delete(s.ProbeWindows, instance)
			delete(s.ProbeAttempts, instance)
		}
		return nil
	})
	return err
}

func NewQuotaRefresher(host HostClient, state *PluginState, now func() time.Time) *QuotaRefresher {
	if now == nil {
		now = time.Now
	}
	r := &QuotaRefresher{
		host:              host,
		state:             state,
		now:               now,
		roster:            normalizeHostRosterLifecycle(HostRosterSnapshot{Capability: CapabilityA}),
		refreshController: NewRefreshController(state.Config().QuotaRefreshInterval, state.Config().RefreshActiveWindow),
	}
	r.legacyTxn = &LegacyRefreshTxn{refresher: r}
	r.coordinator = NewCoordinator(CoordinatorOptions{
		MaxHTTPSlots:  state.Config().MaxRefreshConcurrency,
		LeaseDuration: legacyLeaseDuration,
		Now:           now,
		Execute: func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
			if intent.Class != OperationLegacyRefresh {
				return r.runTypedHeld(ctx, intent, held)
			}
			return r.legacyTxn.RunHeld(ctx, intent, held)
		},
		Validate: func(intent Intent, result OperationResult) bool {
			if r.bindings == nil {
				p, _ := intent.Payload.(LegacyRefreshPayload)
				return r.state.CPAAdmissionVersionCurrent(p.AdmissionVersion)
			}
			b, ok := r.bindings.Lookup(intent.AuthID)
			if !ok {
				return false
			}
			return ValidateWriteback(BindingVersion{Instance: b.Instance, Admission: b.Admission, Tier: TierGeneration(b.Generation), Login: b.Login, Fingerprint: b.Fingerprint}, WritebackVersion{Token: result.Token, Login: result.Login, Fingerprint: result.Fingerprint})
		},
		Apply: func(intent Intent, result OperationResult) error {
			if result.Journal == nil {
				return nil
			}
			var applyErr error
			apply := func() {
				for _, account := range result.Journal.Accounts {
					if account.AuthID != intent.AuthID || account.LastSuccessAt.IsZero() || account.LastError != "" {
						continue
					}
					if err := r.reconcileObservedProbeWindows(intent.Instance, account.Quota); err != nil {
						applyErr = err
						return
					}
				}
				r.state.applyLegacyEffectJournal(*result.Journal)
			}
			if r.bindings != nil && !r.bindings.ApplyIfCurrent(intent.AuthID, WritebackVersion{Token: result.Token, Login: result.Login, Fingerprint: result.Fingerprint}, apply) {
				return ErrStaleExecutionToken
			}
			if r.bindings == nil {
				apply()
			}
			if applyErr != nil {
				return applyErr
			}
			if err := r.bootstrapProbeWindows(); err != nil {
				r.state.RecordLog("warn", "probe.bootstrap_failed", "Probe 状态初始化失败，将在下次刷新重试", map[string]any{"auth_id": intent.AuthID, "error": redactSecrets(err.Error())}, r.now())
			} else if r.probeController != nil && hasPendingProbeWindows(r.probeController.Snapshot()) {
				r.launchProbe(false)
			}
			return nil
		},
		DispatchLogs: func(ctx context.Context, intent Intent, result OperationResult, held *HeldLease) error {
			for _, log := range result.Journal.HostLogs {
				if strings.EqualFold(log.Level, "info") {
					continue
				}
				l := log
				if err := held.DoHTTP(ctx, func(context.Context) error { r.host.Log(l.Level, l.Message, l.Fields); return nil }); err != nil {
					return err
				}
			}
			return nil
		},
		InheritSentUnknown: func(intent Intent, suppressUntil time.Time) error {
			if (intent.Class == OperationProbeSend || intent.Class == OperationProbeSequence) && r.probeWAL != nil {
				r.probeHoldMu.Lock()
				defer r.probeHoldMu.Unlock()
				if err := r.probeWAL.PersistSentUnknown(intent.Instance, intent.AttemptID, suppressUntil); err != nil {
					return err
				}
				persisted, err := r.runtimeStore.PersistentSnapshot()
				if err == nil {
					r.syncProbeControllerInstance(intent.Instance, persisted)
				}
				return err
			}
			return r.persistLegacySentUnknown(intent, suppressUntil)
		},
	})
	return r
}

func (r *QuotaRefresher) persistLegacySentUnknown(intent Intent, suppressUntil time.Time) error {
	if r.runtimeStore == nil {
		return nil
	}
	now := r.now()
	sent := now
	attempt := ProbeAttemptSeam{Instance: intent.Instance, AttemptID: fmt.Sprintf("legacy-%d", intent.Token.Fence), Windows: []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: intent.Token.Fence, CreatedAt: now, SentAt: &sent, VerifyNotBefore: now, SuppressUntil: suppressUntil}
	_, err := r.runtimeStore.Update(func(s *PersistentState) error { s.ProbeAttempts[intent.Instance] = attempt; return nil })
	return err
}

func (r *QuotaRefresher) RefreshOnce() error {
	if !r.normalBackgroundAllowed() {
		return ErrCapabilityB
	}
	_, version := r.state.CPAAdmissionVersioned()
	return r.refreshOnce(version)
}

func (r *QuotaRefresher) refreshOnce(version uint64) error {
	admission, currentVersion := r.state.CPAAdmissionVersioned()
	if !admission.Observed || currentVersion != version {
		return nil
	}
	auths, err := r.listAuthsWithAdmissionPermit(version)
	if err != nil {
		if errors.Is(err, errCPAAdmissionChanged) || !r.state.CPAAdmissionVersionCurrent(version) {
			return nil
		}
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := filterAdmittedAuths(auths, admission)
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible, version, SourceSchedulerInitial)
	return nil
}

func (r *QuotaRefresher) RefreshDueOnce() error {
	if !r.normalBackgroundAllowed() {
		return ErrCapabilityB
	}
	_, version := r.state.CPAAdmissionVersioned()
	return r.refreshDueOnce(version)
}

func (r *QuotaRefresher) refreshDueOnce(version uint64) error {
	admission, currentVersion := r.state.CPAAdmissionVersioned()
	if !admission.Observed || currentVersion != version {
		return nil
	}
	now := r.now()
	if !r.state.RefreshActive(now) {
		return nil
	}
	dueAccounts := r.state.DueAccounts(now)
	dueAuthIDs := make(map[string]struct{}, len(dueAccounts))
	for _, account := range dueAccounts {
		if account.AuthID != "" {
			dueAuthIDs[account.AuthID] = struct{}{}
		}
	}
	knownAuthIDs := knownAccountAuthIDs(r.state.Snapshot(now))
	if len(dueAuthIDs) == 0 && len(knownAuthIDs) > 0 {
		return nil
	}

	auths, err := r.listAuthsWithAdmissionPermit(version)
	if err != nil {
		if errors.Is(err, errCPAAdmissionChanged) || !r.state.CPAAdmissionVersionCurrent(version) {
			return nil
		}
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range filterAdmittedAuths(auths, admission) {
		if len(dueAuthIDs) == 0 {
			eligible = append(eligible, auth)
			continue
		}
		if _, due := dueAuthIDs[auth.ID]; due {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible, version, SourceSchedulerInterval)
	return nil
}

func (r *QuotaRefresher) RefreshDueCandidatesOnce(req pluginapi.SchedulerPickRequest) error {
	if !r.normalBackgroundAllowed() {
		return ErrCapabilityB
	}
	_, version := r.state.CPAAdmissionVersioned()
	if r.runtimeStore == nil {
		if admission, ok := HighestPriorityCodexAdmission(req); ok {
			version = r.state.ReplaceCPAAdmission(admission)
		}
	}
	return r.refreshDueCandidatesOnce(version)
}

func (r *QuotaRefresher) refreshDueCandidatesOnce(version uint64) error {
	admission, currentVersion := r.state.CPAAdmissionVersioned()
	if !admission.Observed || currentVersion != version {
		return nil
	}
	now := r.now()
	if !r.state.RefreshActive(now) {
		return nil
	}
	candidateIDs := admission.AuthIDs
	if len(candidateIDs) == 0 {
		return r.RefreshDueOnce()
	}
	dueAccounts := r.state.DueAccounts(now)
	dueAuthIDs := make(map[string]struct{}, len(dueAccounts))
	for _, account := range dueAccounts {
		if account.AuthID != "" {
			dueAuthIDs[account.AuthID] = struct{}{}
		}
	}
	knownAuthIDs := knownAccountAuthIDs(r.state.Snapshot(now))
	if len(dueAuthIDs) == 0 && !hasUnknownCandidate(candidateIDs, knownAuthIDs) {
		return nil
	}
	auths, err := r.listAuthsWithAdmissionPermit(version)
	if err != nil {
		if errors.Is(err, errCPAAdmissionChanged) || !r.state.CPAAdmissionVersionCurrent(version) {
			return nil
		}
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(candidateIDs))
	for _, auth := range filterAdmittedAuths(auths, admission) {
		if _, candidate := candidateIDs[auth.ID]; !candidate {
			continue
		}
		if len(dueAuthIDs) == 0 {
			if _, known := knownAuthIDs[auth.ID]; !known {
				eligible = append(eligible, auth)
			}
			continue
		}
		if _, due := dueAuthIDs[auth.ID]; due {
			eligible = append(eligible, auth)
			continue
		}
		if _, known := knownAuthIDs[auth.ID]; !known {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	r.refreshAuths(eligible, version, SourceSchedulerStaleRecovery)
	return nil
}

func (r *QuotaRefresher) recordAuthScan(auths []pluginapi.HostAuthFileEntry, admission CPAAdmissionState, version uint64) {
	if r.state == nil {
		return
	}
	count := 0
	for _, auth := range auths {
		if _, admitted := admission.AuthIDs[auth.ID]; isRefreshEligible(auth) && admitted {
			count++
		}
	}
	r.state.RecordAuthScanIfAdmissionCurrent(version, count, r.now())
}

func knownAccountAuthIDs(snapshot StateSnapshot) map[string]struct{} {
	ids := make(map[string]struct{}, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID != "" {
			ids[account.AuthID] = struct{}{}
		}
	}
	return ids
}

func hasUnknownCandidate(candidateIDs, knownAuthIDs map[string]struct{}) bool {
	for candidateID := range candidateIDs {
		if _, known := knownAuthIDs[candidateID]; !known {
			return true
		}
	}
	return false
}

func (r *QuotaRefresher) refreshAuths(eligible []pluginapi.HostAuthFileEntry, version uint64, source string) {
	concurrency := r.state.Config().MaxRefreshConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(eligible) {
		concurrency = len(eligible)
	}

	jobs := make(chan pluginapi.HostAuthFileEntry)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for auth := range jobs {
				r.refreshAuthVersionedSource(auth, version, source)
			}
		}()
	}
	for _, auth := range eligible {
		jobs <- auth
	}
	close(jobs)
	wg.Wait()
}

func (r *QuotaRefresher) RefreshOneAuthID(authID string) error {
	if !r.normalBackgroundAllowed() {
		return ErrCapabilityB
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return fmt.Errorf("auth_id is required")
	}
	_, version, admitted := r.state.AdmittedCPAPriorityVersioned(authID)
	if !admitted {
		return fmt.Errorf("auth %s is outside the active CPA priority tier", authID)
	}
	return r.refreshOneAuthIDVersioned(authID, version)
}

func (r *QuotaRefresher) refreshOneAuthIDVersioned(authID string, version uint64) error {
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return errCPAAdmissionChanged
	}
	auths, err := r.host.ListAuths()
	if err != nil {
		if !r.state.CPAAdmissionVersionCurrent(version) {
			return errCPAAdmissionChanged
		}
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return errCPAAdmissionChanged
	}
	for _, auth := range auths {
		if auth.ID != authID {
			continue
		}
		if !isRefreshEligible(auth) {
			return fmt.Errorf("auth %s is not eligible for quota refresh", authID)
		}
		r.refreshAuthVersionedSource(auth, version, SourceManualRefresh)
		return nil
	}
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return errCPAAdmissionChanged
	}
	return fmt.Errorf("auth %s not found", authID)
}

func (r *QuotaRefresher) RefreshOneSoon(authID string) {
	if !r.normalBackgroundAllowed() {
		return
	}
	_, version, _ := r.state.AdmittedCPAPriorityVersioned(strings.TrimSpace(authID))
	r.mu.Lock()
	if r.refreshing || r.stopping {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		if err := r.refreshOneAuthIDVersioned(strings.TrimSpace(authID), version); err != nil {
			if errors.Is(err, errCPAAdmissionChanged) || !r.state.BeginCPAAdmissionCall(strings.TrimSpace(authID), version) {
				return
			}
			r.host.Log("error", "quota refresh failed", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())})
			if r.state != nil {
				r.recordAdmissionLog(authID, version, "warn", "quota.refresh_one_failed", "账号额度刷新失败", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())})
			}
		}
	}()
}

func (r *QuotaRefresher) Start() {
	r.mu.Lock()
	r.startRequested = true
	r.mu.Unlock()
	if !r.normalBackgroundAllowed() && !r.probeBackgroundAllowed() && !(r.resetProbeEnabled() && r.provisionalProbeRiskConfigured()) {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		if r.probeController != nil && r.resetProbeEnabled() && hasPendingProbeWindows(r.probeController.Snapshot()) {
			r.launchProbe(false)
		}
		r.wakeRefreshLoop()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	r.stop = stop
	r.done = done
	r.wake = wake
	r.running = true
	r.stopping = false
	r.mu.Unlock()
	if r.probeController != nil && r.resetProbeEnabled() {
		r.launchProbe(true)
	}

	go func() {
		defer close(done)
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()

		for {
			interval, scheduled := r.nextRefreshLoopDelay()
			var timer *time.Timer
			var timerC <-chan time.Time
			if scheduled {
				timer = time.NewTimer(interval)
				timerC = timer.C
			}
			select {
			case <-timerC:
				r.refreshController.OnDeadline(r.now())
				r.RefreshDueSoon()
				if r.probeController != nil {
					r.launchProbeFromTimer(false)
				}
			case <-wake:
				if timer != nil && !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-stop:
				if timer != nil && !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}
	}()
}

func hasPendingProbeWindows(windows map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow) bool {
	for _, instanceWindows := range windows {
		for _, window := range instanceWindows {
			if window.State == ProbePendingCheck {
				return true
			}
		}
	}
	return false
}

func (r *QuotaRefresher) launchProbe(recoverFirst bool) {
	r.launchProbeWithAdmission(recoverFirst, false)
}

func (r *QuotaRefresher) launchProbeFromTimer(recoverFirst bool) {
	r.launchProbeWithAdmission(recoverFirst, true)
}

func (r *QuotaRefresher) launchProbeWithAdmission(recoverFirst, timerAdmission bool) {
	if !r.resetProbeEnabled() {
		return
	}
	now := r.now()
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	r.probeRunStateMu.Lock()
	if r.probeLaunchActive {
		r.probeRerunPending = true
		if recoverFirst {
			r.probeRecoveryPending = true
		}
		r.probeRunStateMu.Unlock()
		r.mu.Unlock()
		return
	}
	retryDue := !r.probeRetryAt.IsZero() && !now.Before(r.probeRetryAt)
	if !r.probeRetryAt.IsZero() && (!timerAdmission || !retryDue) {
		if recoverFirst {
			r.probeRecoveryPending = true
		}
		r.probeRunStateMu.Unlock()
		r.mu.Unlock()
		return
	}
	if timerAdmission && retryDue {
		r.probeOwnerRetryPending = false
	}
	launchGeneration := r.probeRetryGeneration
	recoverFirst = recoverFirst || r.probeRecoveryPending
	if recoverFirst {
		r.probeRecoveryPending = false
	}
	r.probeLaunchActive = true
	r.probeRunStateMu.Unlock()
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer func() {
			r.wakeRefreshLoop()
			r.wg.Done()
		}()
		for {
			var firstErr error
			var recoveryErr error
			recoveryOwned := true
			var dueOwned bool
			if recoverFirst {
				recoveryOwned, recoveryErr = r.runProbeRecoveryOnceAdmitted(context.Background(), launchGeneration)
				firstErr = recoveryErr
			}
			var dueErr error
			if recoveryErr == nil && recoveryOwned {
				dueOwned, dueErr = r.runProbeDueOnceAdmitted(context.Background(), launchGeneration)
				if firstErr == nil {
					firstErr = dueErr
				}
			}
			recoveryRequired := dueErr != nil
			if firstErr == nil && dueOwned {
				var checkErr error
				recoveryRequired, checkErr = r.hasDurableNonterminalProbeAttempts()
				if checkErr != nil {
					firstErr = checkErr
					recoveryRequired = true
				}
			}
			rerun, recoverNext := r.finishProbeLaunch(firstErr, recoverFirst, recoveryErr, dueOwned, recoveryRequired, launchGeneration)
			if !rerun {
				return
			}
			recoverFirst = recoverNext
		}
	}()
}

func (r *QuotaRefresher) hasDurableNonterminalProbeAttempts() (bool, error) {
	if r.runtimeStore == nil {
		return false, nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return false, err
	}
	for _, attempt := range state.ProbeAttempts {
		if nonterminalProbeAttempt(attempt) {
			return true, nil
		}
	}
	return false, nil
}

func (r *QuotaRefresher) probeLaunchAdmissionCurrentLocked(generation uint64) bool {
	return r.probeRetryGeneration == generation && !r.probeOwnerRetryPending
}

func (r *QuotaRefresher) publishProbeRetryLocked(now time.Time, owner bool) {
	r.probeRetryGeneration++
	r.probeRetryAt = now.Add(probeBackoff(1))
	if owner {
		r.probeOwnerRetryPending = true
	}
}

func (r *QuotaRefresher) finishProbeLaunch(runErr error, recoveryAttempted bool, recoveryErr error, dueOwned bool, recoveryRequired bool, launchGeneration uint64) (bool, bool) {
	now := r.now()
	r.probeRunStateMu.Lock()
	if (recoveryAttempted && recoveryErr != nil) || recoveryRequired {
		r.probeRecoveryPending = true
	}
	newerRetry := r.probeRetryGeneration != launchGeneration
	if runErr != nil || recoveryRequired || r.probeOwnerRetryPending || newerRetry {
		if (runErr != nil || recoveryRequired) && !r.probeOwnerRetryPending && !newerRetry {
			r.publishProbeRetryLocked(now, false)
		}
		r.probeRerunPending = false
		r.probeLaunchActive = false
		r.probeRunStateMu.Unlock()
		return false, false
	} else {
		r.probeRetryAt = time.Time{}
	}
	if !dueOwned && (!r.probeRerunPending || !r.probeRunMu.TryLock()) {
		r.probeLaunchActive = false
		r.probeRunStateMu.Unlock()
		return false, false
	}
	if !dueOwned {
		r.probeRunMu.Unlock()
	}
	if r.probeRerunPending {
		r.probeRerunPending = false
		recoverNext := r.probeRecoveryPending
		r.probeRecoveryPending = false
		r.probeRunStateMu.Unlock()
		return true, recoverNext
	}
	r.probeLaunchActive = false
	r.probeRunStateMu.Unlock()
	return false, false
}

func (r *QuotaRefresher) nextRefreshLoopDelay() (time.Duration, bool) {
	now := r.now()
	deadline := r.refreshController.NextDeadline(now)
	if r.probeController != nil && r.resetProbeEnabled() {
		r.probeRunStateMu.Lock()
		launchActive := r.probeLaunchActive
		probeRetryAt := r.probeRetryAt
		r.probeRunStateMu.Unlock()
		probe := time.Time{}
		if !launchActive && !probeRetryAt.IsZero() {
			probe = probeRetryAt
		} else if !launchActive {
			probe = r.probeController.NextDeadline()
		}
		if !probe.IsZero() && (deadline.IsZero() || probe.Before(deadline)) {
			deadline = probe
		}
	}
	if legacy := r.state.NextRefreshDueAt(now); !legacy.IsZero() && (deadline.IsZero() || legacy.Before(deadline)) {
		deadline = legacy
	}
	if deadline.IsZero() {
		return 0, false
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (r *QuotaRefresher) wakeRefreshLoop() {
	r.mu.Lock()
	wake := r.wake
	running := r.running
	stopping := r.stopping
	r.mu.Unlock()
	if !running || stopping || wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *QuotaRefresher) wakeRefreshLoopIfDueAdvanced(previous time.Time) {
	now := r.now()
	next := r.state.NextRefreshDueAt(now)
	if next.IsZero() || !next.After(now) {
		return
	}
	if previous.IsZero() || !previous.After(now) || next.Before(previous) {
		r.wakeRefreshLoop()
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (r *QuotaRefresher) Stop() {
	r.mu.Lock()
	r.stopping = true
	running := r.running
	stop := r.stop
	done := r.done
	if running {
		r.running = false
	}
	r.mu.Unlock()

	if running {
		close(stop)
	}
	if r.coordinator != nil {
		r.coordinator.DrainLegacy(context.Background())
	}
	if running {
		<-done
	}
	r.wg.Wait()
	if r.coordinator != nil {
		r.coordinator.Close()
	}
}

func (r *QuotaRefresher) RefreshSoon() {
	if !r.normalBackgroundAllowed() {
		return
	}
	r.mu.Lock()
	if r.refreshing || r.stopping {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		_, version := r.state.CPAAdmissionVersioned()
		if err := r.refreshOnce(version); err != nil {
			if r.state.CPAAdmissionVersionCurrent(version) {
				r.host.Log("error", "quota refresh failed", map[string]any{"error": redactSecrets(err.Error())})
			}
		}
	}()
}

func (r *QuotaRefresher) RefreshDueSoon() {
	if !r.normalBackgroundAllowed() {
		return
	}
	_, version := r.state.CPAAdmissionVersioned()
	r.refreshDueSoon(version, func() error {
		return r.refreshDueOnce(version)
	})
}

func (r *QuotaRefresher) RefreshDueCandidatesSoon(req pluginapi.SchedulerPickRequest, version uint64) {
	if !r.normalBackgroundAllowed() {
		return
	}
	_ = req
	r.refreshDueSoon(version, func() error {
		return r.refreshDueCandidatesOnce(version)
	})
}

func (r *QuotaRefresher) OnSchedulerPick(req pluginapi.SchedulerPickRequest, version uint64, now time.Time) {
	if r == nil || r.refreshController == nil || !r.normalBackgroundAllowed() {
		return
	}
	snapshot := r.state.Snapshot(now)
	known := knownAccountAuthIDs(snapshot)
	cache := CacheSnapshot{Accounts: snapshot.Accounts, StaleAfter: snapshot.Config.StaleAfter}
	for authID := range snapshot.CPAAdmission.AuthIDs {
		if _, ok := known[authID]; !ok {
			cache.Initial = true
			break
		}
	}
	for _, account := range snapshot.Accounts {
		if account.LastSuccessAt.IsZero() {
			cache.Initial = true
		}
		if !account.LastSuccessAt.IsZero() && now.Sub(account.LastSuccessAt) > snapshot.Config.StaleAfter {
			cache.StaleRecovery = true
			break
		}
	}
	intents := r.refreshController.OnPick(now, cache)
	r.wakeRefreshLoop()
	if len(intents) != 0 {
		r.RefreshDueCandidatesSoon(req, version)
	}
}

func (r *QuotaRefresher) refreshDueSoon(version uint64, refresh func() error) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	if r.refreshing {
		if r.pendingDue == nil || version >= r.pendingDueVersion {
			r.pendingDue = refresh
			r.pendingDueVersion = version
		}
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		if err := refresh(); err != nil {
			if r.state.CPAAdmissionVersionCurrent(version) {
				r.host.Log("error", "quota due refresh failed", map[string]any{"error": redactSecrets(err.Error())})
			}
		}
	}()
}

func (r *QuotaRefresher) finishRefresh(previousDue time.Time) {
	r.mu.Lock()
	pending := r.pendingDue
	pendingVersion := r.pendingDueVersion
	if r.stopping || pending == nil {
		r.pendingDue = nil
		r.pendingDueVersion = 0
		r.refreshing = false
		r.mu.Unlock()
		r.wg.Done()
		r.wakeRefreshLoopIfDueAdvanced(previousDue)
		return
	}
	r.pendingDue = nil
	r.pendingDueVersion = 0
	r.mu.Unlock()
	r.wakeRefreshLoopIfDueAdvanced(previousDue)
	go func() {
		defer r.finishRefresh(previousDue)
		if err := pending(); err != nil {
			if r.state.CPAAdmissionVersionCurrent(pendingVersion) {
				r.host.Log("error", "quota due refresh failed", map[string]any{"error": redactSecrets(err.Error())})
			}
		}
	}()
}

func (r *QuotaRefresher) refreshAuth(auth pluginapi.HostAuthFileEntry) {
	_, version, admitted := r.state.AdmittedCPAPriorityVersioned(auth.ID)
	if admitted {
		r.refreshAuthVersionedSource(auth, version, SourceManualRefresh)
	}
}

func (r *QuotaRefresher) refreshAuthVersioned(auth pluginapi.HostAuthFileEntry, version uint64) {
	r.refreshAuthVersionedSource(auth, version, SourceManualRefresh)
}
func (r *QuotaRefresher) refreshAuthVersionedSource(auth pluginapi.HostAuthFileEntry, version uint64, source string) {
	if r.coordinator == nil {
		return
	}
	r.fenceMu.Lock()
	r.nextFence++
	fence := r.nextFence
	r.fenceMu.Unlock()
	instance := legacyAuthInstanceID(auth.ID)
	generation := TierGeneration(version)
	var admission InstanceAdmissionEpoch
	var login LoginEpoch
	var fingerprint CredentialFingerprint
	if r.bindings != nil {
		binding, ok := r.bindings.Lookup(auth.ID)
		if !ok {
			return
		}
		instance, admission, generation, login, fingerprint = binding.Instance, binding.Admission, TierGeneration(binding.Generation), binding.Login, binding.Fingerprint
	}
	intent := Intent{
		AuthID: auth.ID, Instance: instance, Generation: generation, Class: OperationLegacyRefresh,
		Source: source, Token: ExecutionToken{Instance: instance, Admission: admission, Tier: generation, Fence: fence}, Login: login, Fingerprint: fingerprint,
		Payload: LegacyRefreshPayload{Auth: auth, AdmissionVersion: version},
	}
	_ = r.coordinator.Submit(intent).Await(context.Background())
}

func (r *QuotaRefresher) refreshAuthVersionedHeld(auth pluginapi.HostAuthFileEntry, version uint64) {
	priority, currentVersion, admitted := r.state.AdmittedCPAPriorityVersioned(auth.ID)
	if !admitted || currentVersion != version {
		return
	}
	account := accountStateFromAuth(auth, r.now())
	account.Priority = priority

	authResp, err := r.getAuthWithAdmissionPermit(account.AuthID, version, auth.AuthIndex)
	if err != nil {
		if errors.Is(err, errCPAAdmissionChanged) {
			return
		}
		r.upsertRefreshFailure(account, version, redactSecrets(fmt.Sprintf("get auth: %v", err)), RefreshFailureTransient)
		return
	}

	credentials, err := ExtractCodexCredentials(authResp.JSON)
	if err != nil {
		r.upsertRefreshFailure(account, version, redactWithCredentials(fmt.Sprintf("extract credentials: %v", err), credentials), RefreshFailureLocal)
		return
	}
	if accessTokenExpired(credentials, r.now()) {
		credentials, err = r.refreshAndSaveCredentials(authResp, credentials, account.AuthID, version)
		if err != nil {
			if errors.Is(err, errCPAAdmissionChanged) {
				return
			}
			r.upsertRefreshFailure(account, version, redactWithCredentials(fmt.Sprintf("refresh access token: %v", err), credentials), refreshTokenFailureKind(err))
			return
		}
	}
	account = r.mergeExistingAccount(account)
	account.ChatGPTAccountID = credentials.ChatGPTAccountID
	if credentials.ChatGPTAccountID == "" {
		account.LastError = ""
		account.Refresh = AccountRefreshState{}
		if r.state.ApplyQuotaRefreshSuccessIfAdmissionCurrent(account, version, r.now()) {
			publishSchedulerState(r.state, highestTierSet(r.runtimeRoster()), r.now())
		}
		return
	}
	quota, err := r.fetchQuota(credentials, account.AuthID, version)
	if err != nil {
		if errors.Is(err, errCPAAdmissionChanged) {
			return
		}
		kind := RefreshFailureTransient
		var statusErr quotaStatusError
		if errors.As(err, &statusErr) {
			kind = refreshFailureKind(statusErr.status)
		}
		r.upsertRefreshFailure(account, version, redactWithCredentials(err.Error(), credentials), kind)
		return
	}
	if resetCredits, err := r.refreshResetCredits(credentials, account.AuthID, version); err != nil {
		if errors.Is(err, errCPAAdmissionChanged) {
			return
		}
		r.recordAdmissionLog(account.AuthID, version, "warn", "quota.reset_credits_failed", "主动重置次数刷新失败", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)})
	} else {
		mergeResetCredits(&quota, resetCredits)
	}
	// S6: Probe deadlines run independently through ProbeController, including
	// while normal refresh is Dormant. Keep legacy data readable, but never
	// execute the old POST/post-read envelope.
	account.Quota = quota
	account.Family = quota.Family
	account.LastError = ""
	account.LastSuccessAt = r.now()
	if r.state.ApplyQuotaRefreshSuccessIfAdmissionCurrent(account, version, r.now()) {
		globalTrials.ObserveEvidence(account.Instance, Evidence{Kind: EvidenceReliableQuotaWriteback, At: r.now()})
		publishSchedulerState(r.state, highestTierSet(r.runtimeRoster()), r.now())
		r.recordAdmissionLog(account.AuthID, version, "info", "quota.refresh_success", "账号额度刷新成功", map[string]any{"auth_id": account.AuthID})
	}
}

func (r *QuotaRefresher) recordAdmissionLog(authID string, version uint64, level, event, message string, fields map[string]any) {
	r.state.RecordLogIfAdmissionCurrent(authID, version, level, event, message, fields, r.now())
}

func (r *QuotaRefresher) listAuthsWithAdmissionPermit(version uint64) ([]pluginapi.HostAuthFileEntry, error) {
	if r.runtimeStore != nil {
		owner := r.lifecycleRefresher()
		owner.rosterAdmissionMu.RLock()
		defer owner.rosterAdmissionMu.RUnlock()
		if !r.state.BeginCPAAdmissionVersionCall(version) {
			return nil, errCPAAdmissionChanged
		}
		roster := r.runtimeRoster()
		if roster.Capability != CapabilityA {
			return nil, ErrCapabilityB
		}
		out := make([]pluginapi.HostAuthFileEntry, 0, len(roster.Entries))
		for _, e := range roster.Entries {
			if e.Priority != nil {
				out = append(out, pluginapi.HostAuthFileEntry{ID: e.ID, AuthIndex: e.AuthIndex, Provider: e.Provider, Priority: *e.Priority})
			}
		}
		return out, nil
	}
	if !r.state.BeginCPAAdmissionVersionCall(version) {
		return nil, errCPAAdmissionChanged
	}
	return r.host.ListAuths()
}

func (r *QuotaRefresher) getAuthWithAdmissionPermit(authID string, version uint64, authIndex string) (pluginapi.HostAuthGetResponse, error) {
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return pluginapi.HostAuthGetResponse{}, errCPAAdmissionChanged
	}
	return r.host.GetAuth(authIndex)
}

func (r *QuotaRefresher) doWithAdmissionPermit(authID string, version uint64, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return pluginapi.HTTPResponse{}, errCPAAdmissionChanged
	}
	return r.doBackgroundHTTPRequest(req, false)
}

func (r *QuotaRefresher) doBackgroundHTTPRequest(req pluginapi.HTTPRequest, probe bool) (pluginapi.HTTPResponse, error) {
	if r == nil {
		return pluginapi.HTTPResponse{}, ErrCapabilityB
	}
	owner := r.lifecycleRefresher()
	if owner.lifecycleGate != nil {
		owner.lifecycleGate(owner.lifecycleNow())
	}
	owner.rosterMu.RLock()
	roster := normalizeHostRosterLifecycle(owner.roster)
	if _, expired := failClosedRosterAt(roster, owner.lifecycleNow()); expired {
		owner.rosterMu.RUnlock()
		owner.rosterMu.Lock()
		if latest, changed := failClosedRosterAt(owner.roster, owner.lifecycleNow()); changed {
			owner.roster = latest
			roster = latest
		} else {
			roster = normalizeHostRosterLifecycle(owner.roster)
		}
		owner.rosterMu.Unlock()
		_ = owner.persistProbeRosterHold()
		return pluginapi.HTTPResponse{}, ErrCapabilityB
	}
	defer owner.rosterMu.RUnlock()
	if owner.state == nil && probe {
		return pluginapi.HTTPResponse{}, ErrCapabilityB
	}
	if owner.state != nil && (probe || roster.Provisional) {
		owner.state.mu.RLock()
		defer owner.state.mu.RUnlock()
		if probe && !owner.state.cfg.EnableResetProbe {
			return pluginapi.HTTPResponse{}, ErrCapabilityB
		}
	}
	allowed := normalRosterBackgroundAllowed(roster)
	if probe {
		allowed = allowed || provisionalProbeBackgroundAllowed(roster)
	}
	if roster.Provisional {
		if owner.state == nil {
			return pluginapi.HTTPResponse{}, ErrCapabilityB
		}
		if !owner.state.cfg.ProbeOnProvisionalRoster || !provisionalAgeValid(owner.now(), roster.ConfirmedAt) {
			allowed = false
		}
	}
	if !allowed {
		return pluginapi.HTTPResponse{}, ErrCapabilityB
	}
	marker := ""
	if roster.Health == RosterDegraded {
		marker = rosterLifecycleDegraded
	} else if probe && roster.Provisional {
		marker = rosterLifecycleProvisional
	}
	if marker != "" {
		if req.Headers == nil {
			req.Headers = make(http.Header)
		}
		req.Headers.Set(rosterLifecycleRequestHeader, marker)
	}
	return r.host.Do(req)
}

func (r *QuotaRefresher) saveAuthWithAdmissionPermit(authID string, version uint64, name string, raw json.RawMessage) error {
	if !r.state.BeginCPAAdmissionCall(authID, version) {
		return errCPAAdmissionChanged
	}
	return r.host.SaveAuth(name, raw)
}

func (r *QuotaRefresher) fetchQuota(credentials CodexCredentials, authID string, version uint64) (ParsedQuota, error) {
	cfg := r.state.Config()
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    cfg.QuotaEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := r.doWithAdmissionPermit(authID, version, req)
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("quota request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, quotaStatusError{status: resp.StatusCode, body: resp.Body}
	}
	quota, err := ParseCodexUsagePayload(resp.Body, r.now())
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("parse quota: %w", err)
	}
	return quota, nil
}

type quotaStatusError struct {
	status int
	body   []byte
}

func (e quotaStatusError) Error() string {
	return fmt.Sprintf("quota request returned status %d: response body: %s", e.status, sanitizedBodySummary(e.body))
}

func (r *QuotaRefresher) refreshAndSaveCredentials(authResp pluginapi.HostAuthGetResponse, current CodexCredentials, authID string, version uint64) (CodexCredentials, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return CodexCredentials{}, errors.New("access_token expired and refresh_token is missing")
	}
	refreshed, err := r.refreshCodexAccessToken(current.RefreshToken, authID, version)
	if err != nil {
		return CodexCredentials{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if refreshed.ChatGPTAccountID == "" {
		refreshed.ChatGPTAccountID = current.ChatGPTAccountID
	}
	if refreshed.IDToken == "" {
		refreshed.IDToken = current.IDToken
	}
	if refreshed.AccessToken == "" {
		return CodexCredentials{}, errors.New("refresh response did not include access_token")
	}

	updated, err := updateCodexAuthJSON(authResp.JSON, refreshed, r.now())
	if err != nil {
		return CodexCredentials{}, err
	}
	name := strings.TrimSpace(authResp.Name)
	if name == "" {
		return CodexCredentials{}, errors.New("auth file name is required to save refreshed token")
	}
	if r.credentials != nil && r.bindings != nil {
		binding, ok := r.bindings.Lookup(authID)
		if !ok {
			return CodexCredentials{}, ErrBindingNotRosterConfirmed
		}
		next := HostAuth{Name: name, Raw: updated, Fingerprint: NewCredentialFingerprint(refreshed.ChatGPTAccountID, refreshed.RefreshToken, binding.AuthIndex)}
		token := binding.ExecutionToken(r.nextFence)
		ctx := context.Background()
		host := r.credentials.host
		if r.txnIntent != nil {
			token = r.txnIntent.Token
			ctx = r.txnContext
			host = heldCredentialHost{CredentialHost: r.credentials.host, ctx: ctx, lease: r.txnLease, permit: r.txnPermit}
		}
		write := WritebackVersion{Token: token, Login: binding.Login, Fingerprint: binding.Fingerprint}
		if !r.bindings.ApplyIfCurrent(authID, write, func() {}) {
			return CodexCredentials{}, ErrStaleExecutionToken
		}
		if _, err := r.credentials.SaveVersionedWithHost(ctx, binding.Instance, next, token, host); err != nil {
			return CodexCredentials{}, err
		}
		if !r.bindings.ApplyIfCurrent(authID, write, func() {}) {
			return CodexCredentials{}, ErrStaleExecutionToken
		}
	} else if err := r.saveAuthWithAdmissionPermit(authID, version, name, updated); err != nil {
		return CodexCredentials{}, err
	}
	r.recordAdmissionLog(authID, version, "info", "quota.token_refreshed", "Codex access token refreshed and saved", map[string]any{"auth_file": name})
	return refreshed, nil
}

func (r *QuotaRefresher) refreshCodexAccessToken(refreshToken, authID string, version uint64) (CodexCredentials, error) {
	form := url.Values{
		"client_id":     {codexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexTokenEndpoint,
		Headers: http.Header{
			"Accept":       []string{"application/json"},
			"Content-Type": []string{"application/x-www-form-urlencoded"},
		},
		Body: []byte(form.Encode()),
	}
	resp, err := r.doWithAdmissionPermit(authID, version, req)
	if err != nil {
		return CodexCredentials{}, fmt.Errorf("token refresh request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CodexCredentials{}, tokenRefreshStatusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("token refresh returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body)),
		}
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		AccountID    string `json:"account_id"`
	}
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil {
		return CodexCredentials{}, fmt.Errorf("parse token refresh response: %w", err)
	}
	credentials := CodexCredentials{
		AccessToken:      strings.TrimSpace(tokenResp.AccessToken),
		RefreshToken:     strings.TrimSpace(tokenResp.RefreshToken),
		IDToken:          strings.TrimSpace(tokenResp.IDToken),
		ChatGPTAccountID: strings.TrimSpace(tokenResp.AccountID),
	}
	if credentials.ChatGPTAccountID == "" && credentials.IDToken != "" {
		if accountID, err := extractChatGPTAccountID(credentials.IDToken); err == nil {
			credentials.ChatGPTAccountID = accountID
		}
	}
	if tokenResp.ExpiresIn > 0 {
		credentials.ExpiresAt = r.now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	return credentials, nil
}

type tokenRefreshStatusError struct {
	status int
	msg    string
}

func (e tokenRefreshStatusError) Error() string { return e.msg }

func refreshTokenFailureKind(err error) RefreshFailureKind {
	var statusErr tokenRefreshStatusError
	if errors.As(err, &statusErr) {
		if statusErr.status == http.StatusBadRequest || statusErr.status == http.StatusUnauthorized || strings.Contains(strings.ToLower(statusErr.msg), "invalid_grant") {
			return RefreshFailureAuth
		}
		return refreshFailureKind(statusErr.status)
	}
	if strings.Contains(err.Error(), "refresh_token is missing") ||
		strings.Contains(err.Error(), "auth file name is required") ||
		strings.Contains(err.Error(), "refresh response did not include access_token") {
		return RefreshFailureLocal
	}
	return RefreshFailureTransient
}

func updateCodexAuthJSON(raw json.RawMessage, credentials CodexCredentials, now time.Time) (json.RawMessage, error) {
	var doc map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	}
	if doc == nil {
		doc = make(map[string]any)
	}
	doc["access_token"] = credentials.AccessToken
	doc["refresh_token"] = credentials.RefreshToken
	doc["id_token"] = credentials.IDToken
	doc["account_id"] = credentials.ChatGPTAccountID
	doc["type"] = "codex"
	if !credentials.ExpiresAt.IsZero() {
		doc["expired"] = credentials.ExpiresAt.Format(time.RFC3339)
	}
	if !now.IsZero() {
		doc["last_refresh"] = now.Format(time.RFC3339)
	}
	return json.Marshal(doc)
}

func (r *QuotaRefresher) refreshResetCredits(credentials CodexCredentials, authID string, version uint64) (ParsedQuota, error) {
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    resetCreditsEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := r.doWithAdmissionPermit(authID, version, req)
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("reset credits request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, fmt.Errorf("reset credits request returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
	}
	quota, err := ParseResetCreditsPayload(resp.Body, r.now())
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("parse reset credits: %w", err)
	}
	return quota, nil
}

func mergeResetCredits(quota *ParsedQuota, resetCredits ParsedQuota) {
	if resetCredits.ResetCreditsAvailableCount != nil {
		count := *resetCredits.ResetCreditsAvailableCount
		quota.ResetCreditsAvailableCount = &count
	}
	if resetCredits.ResetCreditsTotalEarnedCount != nil {
		count := *resetCredits.ResetCreditsTotalEarnedCount
		quota.ResetCreditsTotalEarnedCount = &count
	}
	if len(resetCredits.ResetCredits) > 0 {
		quota.ResetCredits = append([]ResetCredit(nil), resetCredits.ResetCredits...)
	}
}

func refreshFailureKind(status int) RefreshFailureKind {
	if status == http.StatusUnauthorized {
		return RefreshFailureAuth
	}
	return RefreshFailureTransient
}

func (r *QuotaRefresher) upsertRefreshFailure(account AccountState, version uint64, message string, kind RefreshFailureKind) {
	merged := r.mergeExistingAccount(account)
	merged.LastRefreshAt = account.LastRefreshAt
	merged.LastError = message
	if r.state.ApplyQuotaRefreshFailureIfAdmissionCurrent(merged, version, kind, message, r.now()) {
		globalTrials.ObserveRetry(merged.Instance, r.now())
		publishSchedulerState(r.state, highestTierSet(r.runtimeRoster()), r.now())
		r.recordAdmissionLog(merged.AuthID, version, "warn", "quota.refresh_failed", "账号额度刷新失败", map[string]any{"auth_id": merged.AuthID, "error": message})
	}
}

func highestTierSet(roster HostRosterSnapshot) map[string]struct{} {
	_, ids, ok := HighestCodexTier(roster.Entries)
	if !ok {
		return nil
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func (r *QuotaRefresher) mergeExistingAccount(account AccountState) AccountState {
	key := accountStateKey(account)
	if key == "" {
		return account
	}
	for _, existing := range r.state.Snapshot(r.now()).Accounts {
		if accountStateKey(existing) != key {
			continue
		}
		merged := existing
		merged.AuthID = account.AuthID
		merged.AuthIndex = account.AuthIndex
		merged.DisplayName = account.DisplayName
		merged.Email = account.Email
		merged.Provider = account.Provider
		merged.Priority = account.Priority
		if account.ChatGPTAccountID != "" {
			merged.ChatGPTAccountID = account.ChatGPTAccountID
		}
		return merged
	}
	return account
}

func accountStateFromAuth(auth pluginapi.HostAuthFileEntry, now time.Time) AccountState {
	return AccountState{
		AuthID:        auth.ID,
		AuthIndex:     auth.AuthIndex,
		DisplayName:   auth.Label,
		Email:         auth.Email,
		Provider:      auth.Provider,
		Priority:      auth.Priority,
		LastRefreshAt: now,
	}
}

func isRefreshEligible(auth pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(auth.Provider, "codex") &&
		!auth.Disabled &&
		!auth.Unavailable &&
		auth.AuthIndex != ""
}

func filterAdmittedAuths(auths []pluginapi.HostAuthFileEntry, admission CPAAdmissionState) []pluginapi.HostAuthFileEntry {
	filtered := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		if _, admitted := admission.AuthIDs[auth.ID]; isRefreshEligible(auth) && admitted {
			filtered = append(filtered, auth)
		}
	}
	return filtered
}

func redactWithCredentials(message string, credentials CodexCredentials) string {
	message = redactSecrets(message)
	for _, secret := range []string{credentials.AccessToken, credentials.IDToken} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func redactSecrets(message string) string {
	message = rawCookiePattern.ReplaceAllString(message, `${1}[redacted]`)
	message = rawSecretPattern.ReplaceAllString(message, `${1}[redacted]`)
	message = secretFieldPattern.ReplaceAllString(message, `${1}"[redacted]"`)
	message = bearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	return message
}

func containsSensitiveCredentialMaterial(value string) bool {
	return rawCookiePattern.MatchString(value) || rawSecretPattern.MatchString(value) || secretFieldPattern.MatchString(value) || bearerPattern.MatchString(value)
}

func sanitizedBodySummary(body []byte) string {
	if len(body) == 0 {
		return "<empty>"
	}
	summary := redactSecrets(string(body))
	summary = strings.Join(strings.Fields(summary), " ")
	if len(summary) > maxErrorBodySummaryLen {
		summary = summary[:maxErrorBodySummaryLen] + "..."
	}
	return summary
}

func sanitizedProbeResponseSummary(body []byte) string {
	if len(body) == 0 {
		return "<empty>"
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Sprintf("<non-json response body: %d bytes>", len(body))
	}
	safe := probeDiagnosticValue(value)
	if safe == nil {
		return "<json response without diagnostic fields>"
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return "<unavailable>"
	}
	summary := redactSecrets(string(encoded))
	if len(summary) > maxErrorBodySummaryLen {
		summary = summary[:maxErrorBodySummaryLen] + "..."
	}
	return summary
}

func probeDiagnosticValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any)
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "error", "type", "code", "message", "detail", "title", "status", "param":
				if sanitized := probeDiagnosticValue(child); sanitized != nil {
					out[key] = sanitized
				}
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			if sanitized := probeDiagnosticValue(child); sanitized != nil {
				out = append(out, sanitized)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		return redactSecrets(typed)
	case float64, bool, nil:
		return typed
	default:
		return nil
	}
}

func (ABIHostClient) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := callHostCallback(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	return resp.Files, nil
}

func (ABIHostClient) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	result, err := callHostCallback(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", err)
	}
	return resp, nil
}

func (ABIHostClient) SaveAuth(name string, raw json.RawMessage) error {
	_, err := callHostCallback(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw})
	return err
}

func (ABIHostClient) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	result, err := callHostCallback(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do result: %w", err)
	}
	return resp, nil
}

func (ABIHostClient) Log(level, message string, fields map[string]any) {
	_, _ = callHostCallback(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
}
