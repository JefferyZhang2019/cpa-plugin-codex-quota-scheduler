package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type probeReadPayload struct {
	Binding RuntimeBinding
	Windows []ProbeWindowKind
}
type probeReadResult struct {
	Quota       ParsedQuota
	Credentials CodexCredentials
}
type probeSequencePayload struct {
	Binding  RuntimeBinding
	Windows  []ProbeWindowKind
	Attempt  ProbeAttempt
	Recovery bool
}

func (r *QuotaRefresher) initProbeRuntime() error {
	if r.runtimeStore == nil {
		return nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	r.probeController = NewProbeController(r.now())
	r.probeController.Load(state.ProbeWindows)
	r.probeWAL = NewProbeWAL(r.runtimeStore)
	r.probeFence = NewFenceAllocator(r.runtimeStore, state, nil)
	r.coordinator.opts.AllocateReadSeq = r.probeFence.Next
	return r.bootstrapProbeWindows()
}

func probeWindowUsage(window QuotaWindow) float64 {
	if window.UsedPercent == nil {
		return 0
	}
	return *window.UsedPercent
}

func (r *QuotaRefresher) probeObservationInterval() time.Duration {
	if r == nil || r.state == nil {
		return 0
	}
	interval := NormalizeConfig(r.state.Config()).QuotaRefreshInterval
	if interval < probeUnknownResetRecheck {
		return probeUnknownResetRecheck
	}
	return interval
}

func resetProbeWindowFromObservation(window QuotaWindow, usage float64, length, observationInterval time.Duration, now time.Time) ProbeWindow {
	baseline := ResetProbeBaseline(window.ResetAt, usage, length)
	deadline := deadlineFor(baseline, now, observationInterval)
	if !deadline.After(now) {
		deadline = now
	}
	return ProbeWindow{State: ProbeWaitingReset, Baseline: baseline, Deadline: deadline}
}

func looksLikeUnusedLazyObservation(observedAt time.Time, window QuotaWindow, usage float64, length time.Duration) bool {
	if window.UsedPercent == nil || usage != 0 || observedAt.IsZero() || length <= 0 || window.ResetAt.IsZero() {
		return false
	}
	return absDuration(window.ResetAt.Sub(observedAt.Add(length))) <= resetProbeCloseThreshold
}

func looksLikeExternalLazyReset(base ProbeBaseline, observedAt time.Time, window QuotaWindow, usage float64, length time.Duration) bool {
	if base.Kind != ProbeBaselineReset || base.ResetAt.IsZero() || !looksLikeUnusedLazyObservation(observedAt, window, usage, length) {
		return false
	}
	return usageActivated(base.Usage, usage) || absDuration(window.ResetAt.Sub(base.ResetAt)) > probeSkewTolerance
}

func (r *QuotaRefresher) schedulePersistedProbeObservations(now time.Time) {
	observationInterval := r.probeObservationInterval()
	for instance, windows := range r.probeController.Snapshot() {
		for kind, window := range windows {
			if window.State != ProbeWaitingReset || window.Baseline.Kind != ProbeBaselineReset || window.Baseline.WindowLength <= 0 {
				continue
			}
			nextObservation := deadlineFor(window.Baseline, now, observationInterval)
			if window.Deadline.IsZero() || nextObservation.Before(window.Deadline) {
				window.Deadline = nextObservation
				r.probeController.SetWindow(instance, kind, window)
			}
		}
	}
}

func (r *QuotaRefresher) bootstrapProbeWindows() error {
	if r.probeController == nil || !r.state.Config().EnableResetProbe {
		return nil
	}
	now := r.now()
	observationInterval := r.probeObservationInterval()
	// Quota snapshots are intentionally in-memory only. Schedule observations
	// directly from durable known-length baselines so restart does not restore
	// the old far-future deadline while waiting for a normal quota refresh.
	r.schedulePersistedProbeObservations(now)
	for _, a := range r.state.Snapshot(now).Accounts {
		b, ok := r.bindings.Lookup(a.AuthID)
		if !ok {
			continue
		}
		for _, pair := range []struct {
			k ProbeWindowKind
			w *QuotaWindow
		}{{ProbeWindowFiveHour, a.Quota.FiveHour}, {ProbeWindowLong, a.Quota.LongWindow}} {
			if pair.w == nil {
				continue
			}
			window := *pair.w
			usage := probeWindowUsage(window)
			length, _ := probeWindowDuration(window)
			if existing, found := r.probeController.Window(b.Instance, pair.k); found {
				if existing.State == ProbeConfirmed {
					r.probeController.SetWindow(b.Instance, pair.k, resetProbeWindowFromObservation(window, usage, length, observationInterval, now))
					continue
				}
				changed := false
				unknownLength := existing.Baseline.Kind == ProbeBaselineReset && existing.Baseline.WindowLength <= 0
				if unknownLength && length > 0 {
					existing.Baseline.WindowLength = length
					changed = true
				}
				if existing.State == ProbeWaitingReset {
					lazyMigration := unknownLength && length > 0 && looksLikeUnusedLazyObservation(a.LastSuccessAt, window, usage, length)
					if lazyMigration || looksLikeExternalLazyReset(existing.Baseline, a.LastSuccessAt, window, usage, length) {
						// A newly observed server-side reset has the same lazy
						// signature as first install. Rebase before probing so a
						// large compensation jump cannot become an anomaly on verify.
						existing.Baseline = ResetProbeBaseline(window.ResetAt, usage, length)
						existing.Deadline = now
						changed = true
					} else if existing.Baseline.Kind == ProbeBaselineReset && existing.Baseline.WindowLength > 0 {
						nextObservation := deadlineFor(existing.Baseline, now, observationInterval)
						if existing.Deadline.IsZero() || nextObservation.Before(existing.Deadline) {
							existing.Deadline = nextObservation
							changed = true
						}
					}
				}
				if changed {
					r.probeController.SetWindow(b.Instance, pair.k, existing)
				}
				continue
			}
			next := resetProbeWindowFromObservation(window, usage, length, observationInterval, now)
			if looksLikeUnusedLazyObservation(a.LastSuccessAt, window, usage, length) {
				next.Deadline = now
			}
			if r.runtimeRoster().Capability != CapabilityA {
				next.State = ProbeWaitingRoster
				next.Deadline = time.Time{}
			}
			r.probeController.SetWindow(b.Instance, pair.k, next)
		}
	}
	return r.persistProbeWindows()
}
func (r *QuotaRefresher) persistProbeWindows() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	snapshot := r.probeController.Snapshot()
	_, err := r.runtimeStore.Update(func(s *PersistentState) error { s.ProbeWindows = snapshot; return nil })
	return err
}

func nonterminalProbeAttempt(attempt ProbeAttempt) bool {
	switch attempt.Phase {
	case ProbeAttemptPrepared, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown:
		return true
	default:
		return false
	}
}

func attemptReferencesWindow(attempt ProbeAttempt, kind ProbeWindowKind) bool {
	for _, candidate := range attempt.Windows {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (r *QuotaRefresher) reconcileObservedProbeWindows(instance AuthInstanceID, quota ParsedQuota) error {
	if r.runtimeStore == nil || r.probeController == nil || instance == 0 {
		return nil
	}
	observed := map[ProbeWindowKind]*QuotaWindow{
		ProbeWindowFiveHour: quota.FiveHour,
		ProbeWindowLong:     quota.LongWindow,
	}
	updated, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		attempt := persisted.ProbeAttempts[instance]
		for kind, window := range observed {
			present := window != nil
			if present || (nonterminalProbeAttempt(attempt) && attemptReferencesWindow(attempt, kind)) {
				continue
			}
			delete(persisted.ProbeWindows[instance], kind)
			if len(persisted.ProbeWindows[instance]) == 0 {
				delete(persisted.ProbeWindows, instance)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for kind, window := range observed {
		if window != nil {
			if current, ok := r.probeController.Window(instance, kind); ok && current.State == ProbeConfirmed {
				usage := probeWindowUsage(*window)
				length, _ := probeWindowDuration(*window)
				r.probeController.SetWindow(instance, kind, resetProbeWindowFromObservation(*window, usage, length, r.probeObservationInterval(), r.now()))
			}
			continue
		}
		if _, retained := updated.ProbeWindows[instance][kind]; !retained {
			r.probeController.RemoveWindow(instance, kind)
		}
	}
	return nil
}

func (r *QuotaRefresher) reconcileProbeOrphans(persisted PersistentState, active map[AuthInstanceID]struct{}) error {
	keys := map[AuthInstanceID]struct{}{}
	for i := range persisted.ProbeWindows {
		keys[i] = struct{}{}
	}
	for i := range persisted.ProbeAttempts {
		keys[i] = struct{}{}
	}
	var firstErr error
	for i := range keys {
		if _, ok := active[i]; ok {
			continue
		}
		r.probeController.Advance(i, ProbeEvent{Kind: ProbeEventInstanceRemoved, Now: r.now()})
		_, err := r.runtimeStore.Update(func(s *PersistentState) error { delete(s.ProbeWindows, i); delete(s.ProbeAttempts, i); return nil })
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *QuotaRefresher) holdProbeForRoster() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	for instance, windows := range r.probeController.Snapshot() {
		attempt, hasAttempt := persisted.ProbeAttempts[instance]
		sent := hasAttempt && (attempt.Phase == ProbeAttemptSending || attempt.Phase == ProbeAttemptSent || attempt.Phase == ProbeAttemptSentUnknown)
		sentWindows := make(map[ProbeWindowKind]struct{}, len(attempt.Windows))
		if sent {
			for _, kind := range attempt.Windows {
				sentWindows[kind] = struct{}{}
			}
		}
		for kind, window := range windows {
			window.Deadline = time.Time{}
			if _, belongsToSentAttempt := sentWindows[kind]; belongsToSentAttempt {
				window.State = ProbeSentUnknown
			} else {
				window.State = ProbeWaitingRoster
				window.AttemptID = ""
			}
			r.probeController.SetWindow(instance, kind, window)
		}
	}
	snapshot := r.probeController.Snapshot()
	_, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows = snapshot
		for instance, attempt := range s.ProbeAttempts {
			if attempt.Phase != ProbeAttemptSending && attempt.Phase != ProbeAttemptSent && attempt.Phase != ProbeAttemptSentUnknown {
				delete(s.ProbeAttempts, instance)
			}
		}
		return nil
	})
	return err
}

func (r *QuotaRefresher) persistProbeRosterHold() error {
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	err := r.holdProbeForRoster()
	r.probeHoldPending = err != nil
	r.probeHoldErr = err
	return err
}

func (r *QuotaRefresher) retryProbeRosterHold() error {
	r.probeHoldMu.Lock()
	pending := r.probeHoldPending
	r.probeHoldMu.Unlock()
	if !pending {
		return nil
	}
	return r.persistProbeRosterHold()
}

func (r *QuotaRefresher) recoverProbeFromRoster(bindings map[string]RuntimeBinding) error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	now := r.now()
	for _, binding := range bindings {
		r.probeController.Advance(binding.Instance, ProbeEvent{Kind: ProbeEventRosterConfirmed, Now: now, ObservationInterval: r.probeObservationInterval()})
		for _, kind := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			window, ok := r.probeController.Window(binding.Instance, kind)
			if ok && window.State == ProbeWaitingReset && !window.Deadline.After(now) {
				window.State = ProbePendingCheck
				window.Deadline = now
				r.probeController.SetWindow(binding.Instance, kind, window)
			}
		}
	}
	return r.persistProbeWindows()
}

func activeProbeBindings(roster HostRosterSnapshot, bindings map[string]RuntimeBinding) (map[string]RuntimeBinding, map[AuthInstanceID]struct{}) {
	tier := highestTierSet(roster)
	byID := map[string]RuntimeBinding{}
	instances := map[AuthInstanceID]struct{}{}
	for id, b := range bindings {
		if _, ok := tier[id]; ok && b.Instance != 0 {
			byID[id] = b
			instances[b.Instance] = struct{}{}
		}
	}
	return byID, instances
}

func (r *QuotaRefresher) RunProbeDueOnce(ctx context.Context) error {
	if !r.probeRunMu.TryLock() {
		return nil
	}
	defer r.probeRunMu.Unlock()
	if err := r.retryProbeRosterHold(); err != nil {
		return err
	}
	refresherMu.Lock()
	rosterController := globalRosterController
	refresherMu.Unlock()
	if rosterController != nil {
		_, _ = rosterController.WakeForProbe(ctx)
	}
	if !r.probeBackgroundAllowed() {
		return ErrCapabilityB
	}
	if r.runtimeRoster().Provisional {
		if !r.provisionalProbeRiskConfigured() || !r.consumeProvisionalPermit() {
			return ErrCapabilityB
		}
	}
	if r.probeController == nil {
		return errors.New("probe runtime unavailable")
	}
	if err := r.bootstrapProbeWindows(); err != nil {
		return err
	}
	now := r.now()
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	r.bindings.mu.RLock()
	bindings := make(map[string]RuntimeBinding, len(r.bindings.bindings))
	for id, b := range r.bindings.bindings {
		bindings[id] = b
	}
	r.bindings.mu.RUnlock()
	bindings, activeInstances := activeProbeBindings(r.runtimeRoster(), bindings)
	var firstErr error
	if err := r.reconcileProbeOrphans(persisted, activeInstances); err != nil {
		firstErr = err
	}
	for authID, b := range bindings {
		if attempt, ok := persisted.ProbeAttempts[b.Instance]; ok && (attempt.Phase == ProbeAttemptSending || attempt.Phase == ProbeAttemptSent || attempt.Phase == ProbeAttemptSentUnknown) {
			continue // recovery owns every nonterminal send, suppression expiry never authorizes resend
		}
		if b.AuthBlocked {
			for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
				if w, ok := r.probeController.Window(b.Instance, k); ok {
					w.State = ProbeAuthBlocked
					w.AuthBlockedAtLogin = b.Login
					r.probeController.SetWindow(b.Instance, k, w)
				}
			}
			continue
		}
		for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			if w, ok := r.probeController.Window(b.Instance, k); ok && w.State == ProbeAuthBlocked && b.Login > w.AuthBlockedAtLogin {
				w.State = ProbePendingCheck
				w.Deadline = now
				r.probeController.SetWindow(b.Instance, k, w)
			}
		}
		var due []ProbeWindowKind
		for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			if w, ok := r.probeController.Window(b.Instance, k); ok && !w.Deadline.IsZero() && !w.Deadline.After(now) && w.State != ProbePendingCheck {
				r.probeController.Advance(b.Instance, ProbeEvent{Kind: ProbeEventDeadline, Window: k, Now: now})
			}
			if w, ok := r.probeController.Window(b.Instance, k); ok && w.State == ProbePendingCheck {
				due = append(due, k)
			}
		}
		if len(due) == 0 {
			continue
		}
		var attempt ProbeAttempt
		claimed, claimErr := r.runtimeStore.Update(func(s *PersistentState) error {
			if existing, ok := s.ProbeAttempts[b.Instance]; ok && existing.AttemptID != "" {
				attempt = existing
				return nil
			}
			s.ProbeAttemptSeq++
			attempt = ProbeAttempt{Instance: b.Instance, AttemptID: fmt.Sprintf("probe-%d-%d", b.Instance, s.ProbeAttemptSeq), Windows: append([]ProbeWindowKind(nil), due...), Phase: ProbeAttemptPrepared, CreatedAt: now, VerifyNotBefore: now}
			s.ProbeAttempts[b.Instance] = attempt
			s.ProbeWindows = r.probeController.Snapshot()
			return nil
		})
		if claimErr != nil {
			if firstErr == nil {
				firstErr = claimErr
			}
			continue
		}
		if claimed.ProbeAttempts[b.Instance].AttemptID != attempt.AttemptID || attempt.Phase != ProbeAttemptPrepared {
			continue
		}
		result := r.coordinator.SubmitTyped(Intent{AuthID: authID, Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationProbeSequence, Source: SourceProbeActivation, AttemptID: attempt.AttemptID, Token: b.ExecutionToken(0), Login: b.Login, Fingerprint: b.Fingerprint, Payload: probeSequencePayload{Binding: b, Windows: due, Attempt: attempt}}).Await(ctx)
		if result.Err != nil {
			if firstErr == nil {
				firstErr = result.Err
			}
			continue
		}
	}
	return firstErr
}

func (r *QuotaRefresher) RunProbeRecoveryOnce(ctx context.Context) error {
	r.probeRunMu.Lock()
	defer r.probeRunMu.Unlock()
	if err := r.retryProbeRosterHold(); err != nil {
		return err
	}
	refresherMu.Lock()
	rosterController := globalRosterController
	refresherMu.Unlock()
	if rosterController != nil {
		_, _ = rosterController.WakeForProbe(ctx)
	}
	if !r.probeBackgroundAllowed() {
		return ErrCapabilityB
	}
	if r.runtimeRoster().Provisional {
		if !r.provisionalProbeRiskConfigured() || !r.consumeProvisionalPermit() {
			return ErrCapabilityB
		}
	}
	if r.probeWAL == nil || r.probeController == nil {
		return nil
	}
	if err := r.recoverPreparedProbeAttempts(); err != nil {
		return err
	}
	intents, err := r.probeWAL.RecoverChecked(r.now())
	if err != nil {
		return err
	}
	r.bindings.mu.RLock()
	allBindings := make(map[string]RuntimeBinding, len(r.bindings.bindings))
	for id, b := range r.bindings.bindings {
		allBindings[id] = b
	}
	r.bindings.mu.RUnlock()
	bindings, activeInstances := activeProbeBindings(r.runtimeRoster(), allBindings)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	var firstErr error
	if err = r.reconcileProbeOrphans(persisted, activeInstances); err != nil {
		firstErr = err
	}
	for _, intent := range intents {
		var authID string
		var binding RuntimeBinding
		for id, b := range bindings {
			if b.Instance == intent.Instance {
				authID, binding = id, b
				break
			}
		}
		if authID == "" || binding.AuthBlocked {
			continue
		}
		windows, _ := intent.Payload.([]ProbeWindowKind)
		intent.AuthID = authID
		intent.Generation = TierGeneration(binding.Generation)
		intent.Class = OperationProbeSequence
		intent.Source = SourceProbeVerify
		intent.Token = binding.ExecutionToken(intent.StartedAfter)
		intent.Login = binding.Login
		intent.Fingerprint = binding.Fingerprint
		st, snapErr := r.runtimeStore.PersistentSnapshot()
		if snapErr != nil {
			if firstErr == nil {
				firstErr = snapErr
			}
			continue
		}
		intent.Payload = probeSequencePayload{Binding: binding, Windows: windows, Attempt: st.ProbeAttempts[intent.Instance], Recovery: true}
		result := r.coordinator.SubmitTyped(intent).Await(ctx)
		if result.Err != nil {
			if firstErr == nil {
				firstErr = result.Err
			}
			continue
		}
	}
	if err := r.persistProbeWindows(); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *QuotaRefresher) recoverPreparedProbeAttempts() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	changed := false
	now := r.now()
	for instance, attempt := range state.ProbeAttempts {
		if attempt.Phase != ProbeAttemptPrepared {
			continue
		}
		for _, kind := range attempt.Windows {
			window, ok := r.probeController.Window(instance, kind)
			if !ok {
				continue
			}
			window.State = ProbeRetryWait
			window.AttemptID = ""
			window.RetryCount++
			window.Deadline = now
			r.probeController.SetWindow(instance, kind, window)
		}
		delete(state.ProbeAttempts, instance)
		changed = true
	}
	if !changed {
		return nil
	}
	windows := r.probeController.Snapshot()
	_, err = r.runtimeStore.Update(func(persisted *PersistentState) error {
		persisted.ProbeWindows = windows
		for instance, attempt := range persisted.ProbeAttempts {
			if attempt.Phase == ProbeAttemptPrepared {
				delete(persisted.ProbeAttempts, instance)
			}
		}
		return nil
	})
	return err
}

func (r *QuotaRefresher) runTypedHeld(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
	res := OperationResult{Token: intent.Token, Login: intent.Login, Fingerprint: intent.Fingerprint}
	switch intent.Class {
	case OperationProbeSequence:
		p := intent.Payload.(probeSequencePayload)
		attempt := p.Attempt
		read := func() (probeReadResult, error) {
			var auth pluginapi.HostAuthGetResponse
			if err := held.DoHTTP(ctx, func(context.Context) error { var e error; auth, e = r.host.GetAuth(p.Binding.AuthIndex); return e }); err != nil {
				return probeReadResult{}, err
			}
			credentials, err := ExtractCodexCredentials(auth.JSON)
			if err != nil {
				return probeReadResult{}, err
			}
			if credentials.ChatGPTAccountID == "" {
				return probeReadResult{}, ErrAccountIdentityUnresolved
			}
			if r.runtimeRoster().Provisional && NewCredentialFingerprint(credentials.ChatGPTAccountID, credentials.RefreshToken, p.Binding.AuthIndex) != p.Binding.Fingerprint {
				return probeReadResult{}, ErrProvisionalFingerprintMismatch
			}
			quota, err := r.typedFetchQuota(ctx, held, credentials)
			return probeReadResult{Quota: quota, Credentials: credentials}, err
		}
		fail := func(err error, sent bool) OperationResult {
			if r.runtimeRoster().Provisional && (errors.Is(err, ErrProvisionalFingerprintMismatch) || errors.Is(err, ErrCapabilityB)) {
				r.clearProvisionalPermit()
				r.rosterMu.Lock()
				if r.roster.Provisional {
					r.roster.BackgroundAllowed = false
					r.roster.Health = RosterWaiting
				}
				r.rosterMu.Unlock()
				_ = r.persistProbeRosterHold()
				return OperationResult{Token: intent.Token, Err: err}
			}
			if r.runtimeRoster().Health == RosterFailClosed {
				return OperationResult{Token: intent.Token, Err: err}
			}
			if status, ok := err.(quotaStatusError); ok && status.status == http.StatusUnauthorized {
				if persistErr := r.bindings.MarkAuthBlocked(intent.AuthID); persistErr != nil {
					err = persistErr
				}
				for _, k := range p.Windows {
					if w, ok := r.probeController.Window(intent.Instance, k); ok {
						w.State = ProbeAuthBlocked
						w.AuthBlockedAtLogin = p.Binding.Login
						w.Deadline = time.Time{}
						r.probeController.SetWindow(intent.Instance, k, w)
					}
				}
			} else {
				for _, k := range p.Windows {
					if w, ok := r.probeController.Window(intent.Instance, k); ok {
						if sent {
							w.State = ProbeSentUnknown
						} else {
							w.State = ProbeRetryWait
						}
						w.RetryCount++
						w.Deadline = r.now().Add(probeBackoff(w.RetryCount))
						r.probeController.SetWindow(intent.Instance, k, w)
					}
				}
			}
			if sent {
				_ = r.probeWAL.PersistSentUnknown(intent.Instance, attempt.SuppressUntil)
			}
			_ = r.persistProbeWindows()
			return OperationResult{Token: intent.Token, Err: err}
		}
		if p.Recovery {
			if attempt.SendFenceSeq == 0 {
				attempt.SendFenceSeq = intent.StartedAfter
			}
			if attempt.SendFenceSeq == 0 {
				return fail(errors.New("probe recovery missing send fence"), true)
			}
			seq, err := r.probeFence.Next()
			if err != nil {
				return fail(err, true)
			}
			if seq <= attempt.SendFenceSeq {
				return fail(errors.New("verify read did not start after send fence"), true)
			}
			vr, err := read()
			if err != nil {
				return fail(err, true)
			}
			r.probeController.Advance(intent.Instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(vr.Quota), ObservationInterval: r.probeObservationInterval()})
			if err = r.probeWAL.Complete(intent.Instance); err != nil {
				return fail(err, true)
			}
			if err = r.reconcileObservedProbeWindows(intent.Instance, vr.Quota); err != nil {
				return OperationResult{Token: intent.Token, Err: err}
			}
			if err = r.persistProbeWindows(); err != nil {
				return OperationResult{Token: intent.Token, Err: err}
			}
			res.ReadStartSeq = seq
			res.Value = vr
			return res
		}
		_, err := r.probeFence.Next() // actual-start precheck sequence, distinct from completedFence
		if err != nil {
			return fail(err, false)
		}
		pre, err := read()
		if err != nil {
			_ = r.probeWAL.Complete(intent.Instance)
			return fail(err, false)
		}
		r.probeController.Advance(intent.Instance, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: r.now(), Snapshots: probeSnapshots(pre.Quota), ObservationInterval: r.probeObservationInterval()})
		var lazy []ProbeWindowKind
		for _, k := range p.Windows {
			if w, ok := r.probeController.Window(intent.Instance, k); ok && w.State == ProbeSentAwaitingVerify {
				w.AttemptID = attempt.AttemptID
				r.probeController.SetWindow(intent.Instance, k, w)
				lazy = append(lazy, k)
			}
		}
		if len(lazy) == 0 {
			if err = r.probeWAL.Complete(intent.Instance); err != nil {
				return fail(err, false)
			}
			if err = r.reconcileObservedProbeWindows(intent.Instance, pre.Quota); err != nil {
				return OperationResult{Token: intent.Token, Err: err}
			}
			if err = r.persistProbeWindows(); err != nil {
				return OperationResult{Token: intent.Token, Err: err}
			}
			return res
		}
		fence, err := r.probeFence.Next()
		if err != nil {
			return fail(err, false)
		}
		attempt.Windows = lazy
		attempt.SendFenceSeq = fence
		attempt.CreatedAt = r.now()
		attempt.VerifyNotBefore = attempt.CreatedAt.Add(3 * time.Second)
		attempt.SuppressUntil = attempt.CreatedAt.Add(10 * time.Minute)
		if err = r.probeWAL.PersistSending(attempt); err != nil {
			return fail(err, false)
		}
		held.MarkProbeSent(attempt.SuppressUntil)
		err = held.DoHTTP(ctx, func(context.Context) error {
			return r.probeWAL.ExecuteSend(func() error {
				resp, e := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint, Headers: http.Header{"Authorization": []string{"Bearer " + pre.Credentials.AccessToken}, "Chatgpt-Account-Id": []string{pre.Credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}}, Body: resetProbePayloadBytes()}, true)
				if e != nil {
					return e
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return quotaStatusError{status: resp.StatusCode, body: resp.Body}
				}
				return nil
			})
		})
		if err != nil {
			return fail(err, true)
		}
		if err = r.probeWAL.PersistSent(intent.Instance, r.now()); err != nil {
			return fail(err, true)
		}
		if err = held.WaitPropagation(ctx, 3*time.Second); err != nil {
			return fail(err, true)
		}
		seq, err := r.probeFence.Next()
		if err != nil {
			return fail(err, true)
		}
		if seq <= fence {
			return fail(errors.New("verify read did not start after send fence"), true)
		}
		vr, err := read()
		if err != nil {
			return fail(err, true)
		}
		r.probeController.Advance(intent.Instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(vr.Quota), ObservationInterval: r.probeObservationInterval()})
		for _, k := range lazy {
			if w, ok := r.probeController.Window(intent.Instance, k); ok && w.State == ProbeRetryWait && w.Deadline.Before(attempt.SuppressUntil) {
				w.Deadline = attempt.SuppressUntil
				r.probeController.SetWindow(intent.Instance, k, w)
			}
		}
		if err = r.probeWAL.Complete(intent.Instance); err != nil {
			return fail(err, true)
		}
		if err = r.reconcileObservedProbeWindows(intent.Instance, vr.Quota); err != nil {
			return OperationResult{Token: intent.Token, Err: err}
		}
		if err = r.persistProbeWindows(); err != nil {
			return OperationResult{Token: intent.Token, Err: err}
		}
		res.ReadStartSeq = seq
		res.Value = vr
		return res
	case OperationQuotaRead, OperationProbePrecheck, OperationProbeVerify:
		p := intent.Payload.(probeReadPayload)
		var auth pluginapi.HostAuthGetResponse
		err := held.DoHTTP(ctx, func(context.Context) error { var e error; auth, e = r.host.GetAuth(p.Binding.AuthIndex); return e })
		if err != nil {
			res.Err = err
			return res
		}
		credentials, err := ExtractCodexCredentials(auth.JSON)
		if err != nil {
			res.Err = err
			return res
		}
		if credentials.ChatGPTAccountID == "" {
			res.Err = ErrAccountIdentityUnresolved
			return res
		}
		quota, err := r.typedFetchQuota(ctx, held, credentials)
		if err != nil {
			if status, ok := err.(quotaStatusError); ok && status.status == http.StatusUnauthorized && r.bindings != nil {
				_ = r.bindings.MarkAuthBlocked(intent.AuthID)
			}
			res.Err = err
			return res
		}
		res.Value = probeReadResult{Quota: quota, Credentials: credentials}
	}
	return res
}

func (r *QuotaRefresher) typedFetchQuota(ctx context.Context, held *HeldLease, credentials CodexCredentials) (ParsedQuota, error) {
	if credentials.ChatGPTAccountID == "" {
		return ParsedQuota{}, ErrAccountIdentityUnresolved
	}
	var resp pluginapi.HTTPResponse
	err := held.DoHTTP(ctx, func(context.Context) error {
		var err error
		resp, err = r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: r.state.Config().QuotaEndpoint, Headers: http.Header{
			"Authorization": []string{"Bearer " + credentials.AccessToken}, "Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}, "User-Agent": []string{quotaUserAgent},
		}}, true)
		return err
	})
	if err != nil {
		return ParsedQuota{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, quotaStatusError{status: resp.StatusCode, body: resp.Body}
	}
	return ParseCodexUsagePayload(resp.Body, r.now())
}
func probeSnapshots(q ParsedQuota) map[ProbeWindowKind]QuotaSnapshot {
	out := map[ProbeWindowKind]QuotaSnapshot{}
	for _, p := range []struct {
		k ProbeWindowKind
		w *QuotaWindow
	}{{ProbeWindowFiveHour, q.FiveHour}, {ProbeWindowLong, q.LongWindow}} {
		if p.w == nil {
			continue
		}
		usage := 0.0
		if p.w.UsedPercent != nil {
			usage = *p.w.UsedPercent
		}
		reset := p.w.ResetAt
		out[p.k] = QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: &usage}
	}
	return out
}
