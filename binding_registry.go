package main

import (
	"context"
	"errors"
	"sort"
	"sync"
)

var ErrCapabilityB = errors.New("authoritative host roster unavailable")
var ErrBindingNotRosterConfirmed = errors.New("binding is not roster-confirmed")

type RuntimeBinding struct {
	AuthID      string                 `json:"auth_id"`
	AuthIndex   string                 `json:"auth_index"`
	AuthName    string                 `json:"auth_name"`
	Instance    AuthInstanceID         `json:"instance"`
	Admission   InstanceAdmissionEpoch `json:"admission_epoch"`
	Generation  AuthBindingEpoch       `json:"binding_generation"`
	Login       LoginEpoch             `json:"login_epoch"`
	Token       TokenEpoch             `json:"token_epoch"`
	Fingerprint CredentialFingerprint  `json:"fingerprint"`
	AuthBlocked bool                   `json:"auth_blocked,omitempty"`
}

func (b RuntimeBinding) ExecutionToken(fence uint64) ExecutionToken {
	return ExecutionToken{Instance: b.Instance, Admission: b.Admission, Tier: TierGeneration(b.Generation), Fence: fence}
}

type BindingRegistry struct {
	mu       sync.RWMutex
	store    *StateStore
	bindings map[string]RuntimeBinding
}

type RosterReconcileResult struct {
	Bindings   map[string]RuntimeBinding
	Removed    []RuntimeBinding
	Generation TierGeneration
}

func NewBindingRegistry(store *StateStore) (*BindingRegistry, error) {
	state, _, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &BindingRegistry{store: store, bindings: state.Bindings}, nil
}

func (r *BindingRegistry) Lookup(authID string) (RuntimeBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bindings[authID]
	return b, ok
}

func (r *BindingRegistry) ApplyIfCurrent(authID string, write WritebackVersion, apply func()) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bindings[authID]
	if !ok || !ValidateWriteback(BindingVersion{Instance: b.Instance, Admission: b.Admission, Tier: TierGeneration(b.Generation), Login: b.Login, Fingerprint: b.Fingerprint}, write) {
		return false
	}
	apply()
	return true
}

func (r *BindingRegistry) MarkAuthBlocked(authID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.bindings[authID]
	b.AuthID = authID
	b.AuthBlocked = true
	committed, err := r.store.UpdateMirrored(func(s *PersistentState) error { s.Bindings[authID] = b; return nil })
	if err != nil {
		return err
	}
	r.bindings = committed.Bindings
	return nil
}

func (r *BindingRegistry) ObserveExternalLogin(authID string, login LoginEpoch, fingerprint CredentialFingerprint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bindings[authID]
	if !ok {
		return ErrBindingNotRosterConfirmed
	}
	if login <= b.Login {
		return nil
	}
	b.Login = login
	b.Fingerprint = fingerprint
	b.AuthBlocked = false
	committed, err := r.store.UpdateMirrored(func(s *PersistentState) error { s.Bindings[authID] = b; return nil })
	if err != nil {
		return err
	}
	r.bindings = committed.Bindings
	return nil
}

func (r *BindingRegistry) ReconcileRoster(ctx context.Context, roster HostRosterSnapshot, host CredentialHost) (RosterReconcileResult, error) {
	return r.reconcileRosterEntries(ctx, roster, host, false)
}

// ReconcileRosterTiers reconciles every prioritized codex entry of the given
// roster. Production callers pass an admission-filtered roster, so with
// schedule_across_priorities enabled the union of tiers reconciles; the
// single-tier ReconcileRoster keeps its conservative highest-tier-only
// semantics for direct callers.
func (r *BindingRegistry) ReconcileRosterTiers(ctx context.Context, roster HostRosterSnapshot, host CredentialHost) (RosterReconcileResult, error) {
	return r.reconcileRosterEntries(ctx, roster, host, true)
}

func (r *BindingRegistry) reconcileRosterEntries(ctx context.Context, roster HostRosterSnapshot, host CredentialHost, allTiers bool) (RosterReconcileResult, error) {
	if roster.Capability != CapabilityA {
		return RosterReconcileResult{}, ErrCapabilityB
	}
	var ids []string
	if allTiers {
		seen := make(map[string]struct{}, len(roster.Entries))
		for _, entry := range roster.Entries {
			if entry.ID == "" || entry.Provider != "codex" || entry.Priority == nil {
				continue
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				continue
			}
			seen[entry.ID] = struct{}{}
			ids = append(ids, entry.ID)
		}
		if len(ids) == 0 {
			return RosterReconcileResult{}, ErrBindingNotRosterConfirmed
		}
		sort.Strings(ids)
	} else {
		_, tierIDs, ok := HighestCodexTier(roster.Entries)
		if !ok {
			return RosterReconcileResult{}, ErrBindingNotRosterConfirmed
		}
		ids = tierIDs
	}
	entries := make(map[string]RosterEntry, len(ids))
	for _, entry := range roster.Entries {
		for _, id := range ids {
			if entry.ID == id {
				entries[id] = entry
				break
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	durable, err := r.store.PersistentSnapshot()
	if err != nil {
		return RosterReconcileResult{}, err
	}
	if durable.AdmissionEpochs == nil {
		durable.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
	}
	for _, binding := range durable.Bindings {
		if binding.Admission > durable.AdmissionEpochs[binding.Instance] {
			durable.AdmissionEpochs[binding.Instance] = binding.Admission
		}
		if TierGeneration(binding.Generation) > durable.TierGeneration {
			durable.TierGeneration = TierGeneration(binding.Generation)
		}
	}
	rosterChanged := len(r.bindings) != len(ids)
	if !rosterChanged {
		for _, id := range ids {
			existing, found := r.bindings[id]
			if !found || existing.AuthIndex != entries[id].AuthIndex {
				rosterChanged = true
				break
			}
		}
	}
	incomingGeneration := TierGeneration(roster.Generation)
	generation := durable.TierGeneration
	if generation == 0 {
		generation = incomingGeneration
		if generation == 0 {
			generation = 1
		}
	} else if rosterChanged {
		if incomingGeneration > generation {
			generation = incomingGeneration
		} else {
			generation++
		}
	} else if incomingGeneration > generation {
		generation = incomingGeneration
	}
	next := make(map[string]RuntimeBinding, len(ids))
	chains := map[AuthInstanceID]TransitionChain{}
	for _, id := range ids {
		entry := entries[id]
		if existing, found := r.bindings[id]; found && existing.Instance != 0 {
			if existing.AuthIndex != entry.AuthIndex {
				observed, err := host.GetAuth(ctx, existing.Instance)
				if err != nil {
					return RosterReconcileResult{}, err
				}
				admission := durable.AdmissionEpochs[existing.Instance]
				if admission < existing.Admission {
					admission = existing.Admission
				}
				admission++
				existing.Admission = admission
				existing.AuthName = observed.Name
				existing.Fingerprint = observed.Fingerprint
				durable.AdmissionEpochs[existing.Instance] = admission
				chains[existing.Instance] = TransitionChain{Cursor: observed.Fingerprint}
			}
			existing.AuthIndex = entry.AuthIndex
			existing.Generation = AuthBindingEpoch(generation)
			next[id] = existing
			continue
		}
		instance := legacyAuthInstanceID(id)
		observed, err := host.GetAuth(ctx, instance)
		if err != nil {
			return RosterReconcileResult{}, err
		}
		blocked := r.bindings[id].AuthBlocked
		admission := durable.AdmissionEpochs[instance]
		if admission == 0 {
			if _, seen := durable.CredentialChains[instance]; seen {
				admission = 2
			} else {
				admission = 1
			}
		} else {
			admission++
		}
		next[id] = RuntimeBinding{AuthID: id, AuthIndex: entry.AuthIndex, AuthName: observed.Name, Instance: instance, Admission: admission, Generation: AuthBindingEpoch(generation), Login: 1, Token: 1, Fingerprint: observed.Fingerprint, AuthBlocked: blocked}
		durable.AdmissionEpochs[instance] = admission
		chains[instance] = TransitionChain{Cursor: observed.Fingerprint}
	}
	removed := make([]RuntimeBinding, 0)
	for id, binding := range r.bindings {
		if _, keep := next[id]; !keep && binding.Instance != 0 {
			removed = append(removed, binding)
		}
	}
	committed, err := r.store.UpdateMirrored(func(s *PersistentState) error {
		s.TierGeneration = generation
		if s.AdmissionEpochs == nil {
			s.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
		}
		for instance, admission := range durable.AdmissionEpochs {
			if admission > s.AdmissionEpochs[instance] {
				s.AdmissionEpochs[instance] = admission
			}
		}
		for id := range s.Bindings {
			delete(s.Bindings, id)
		}
		for id, binding := range next {
			s.Bindings[id] = binding
		}
		for instance, chain := range chains {
			s.CredentialChains[instance] = chain
		}
		return nil
	})
	if err != nil {
		return RosterReconcileResult{}, err
	}
	r.bindings = committed.Bindings
	out := make(map[string]RuntimeBinding, len(r.bindings))
	for id, binding := range r.bindings {
		out[id] = binding
	}
	return RosterReconcileResult{Bindings: out, Removed: removed, Generation: generation}, nil
}

// ObserveAuthoritative performs the INV-33 read-only genesis bootstrap. It
// accepts bindings only from the S1 authoritative roster snapshot.
func (r *BindingRegistry) ObserveAuthoritative(ctx context.Context, roster HostRosterSnapshot, authID string, host CredentialHost) (RuntimeBinding, bool, error) {
	if roster.Capability != CapabilityA {
		return RuntimeBinding{}, false, ErrCapabilityB
	}
	var entry *RosterEntry
	for i := range roster.Entries {
		if roster.Entries[i].ID == authID {
			entry = &roster.Entries[i]
			break
		}
	}
	if entry == nil || entry.AuthIndex == "" {
		return RuntimeBinding{}, false, ErrBindingNotRosterConfirmed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.bindings[authID]; ok && existing.Instance != 0 {
		return existing, false, nil
	}
	instance := legacyAuthInstanceID(authID)
	observed, err := host.GetAuth(ctx, instance)
	if err != nil {
		return RuntimeBinding{}, false, err
	}
	blocked := r.bindings[authID].AuthBlocked
	b := RuntimeBinding{AuthID: authID, AuthIndex: entry.AuthIndex, AuthName: observed.Name, Instance: instance, Admission: 1, Generation: 1, Login: 1, Token: 1, Fingerprint: observed.Fingerprint, AuthBlocked: blocked}
	committed, err := r.store.Update(func(s *PersistentState) error {
		s.Bindings[authID] = b
		if _, ok := s.CredentialChains[instance]; !ok {
			s.CredentialChains[instance] = TransitionChain{Cursor: observed.Fingerprint}
		}
		return nil
	})
	if err != nil {
		return RuntimeBinding{}, false, err
	}
	r.bindings = committed.Bindings
	return b, true, nil
}
