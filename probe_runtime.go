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
type probeActivationStatusError struct {
	status int
	body   []byte
}

func (e probeActivationStatusError) Error() string {
	return fmt.Sprintf("probe activation request returned status %d: response body: %s", e.status, sanitizedBodySummary(e.body))
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

func (r *QuotaRefresher) bootstrapProbeWindows() error {
	if r.probeController == nil || !r.state.Config().EnableResetProbe {
		return nil
	}
	bindings := map[string]RuntimeBinding{}
	if r.bindings != nil {
		r.bindings.mu.RLock()
		for authID, binding := range r.bindings.bindings {
			bindings[authID] = binding
		}
		r.bindings.mu.RUnlock()
	}
	// The roster-hold mutex is also the Probe controller/persistence
	// linearization boundary. Keep snapshot-and-merge ordered with authoritative
	// reconciliation so a stale bootstrap snapshot cannot resurrect a window.
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	now := r.now()
	observationInterval := r.probeObservationInterval()
	touched := map[AuthInstanceID]struct{}{}
	for instance := range r.probePersistPending {
		touched[instance] = struct{}{}
	}
	for instance, windows := range r.probeController.Snapshot() {
		for kind, window := range windows {
			if window.State != ProbeWaitingReset || window.Baseline.Kind != ProbeBaselineReset || window.Baseline.WindowLength <= 0 {
				continue
			}
			deadline := deadlineFor(window.Baseline, now, observationInterval)
			if window.Deadline.IsZero() || deadline.Before(window.Deadline) {
				window.Deadline = deadline
				r.probeController.SetWindow(instance, kind, window)
				touched[instance] = struct{}{}
			}
		}
	}
	for _, a := range r.state.Snapshot(now).Accounts {
		observedAt := a.LastSuccessAt
		strictObservation := !observedAt.IsZero() && !observedAt.After(now)
		if !strictObservation {
			observedAt = now
		}
		b, ok := bindings[a.AuthID]
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
			if existing, ok := r.probeController.Window(b.Instance, pair.k); ok {
				rearmObservedAt := observedAt
				if !strictObservation {
					rearmObservedAt = time.Time{}
				}
				if rearmed, changed := rearmConfirmedProbeWindow(existing, rearmObservedAt, pair.w, now, observationInterval); changed {
					if r.runtimeRoster().Capability != CapabilityA {
						rearmed.State = ProbeWaitingRoster
						rearmed.Deadline = time.Time{}
					}
					r.probeController.SetWindow(b.Instance, pair.k, rearmed)
					touched[b.Instance] = struct{}{}
					continue
				}
				if existing.Baseline.Kind == ProbeBaselineReset && existing.Baseline.WindowLength == 0 {
					if duration, durationKnown := probeWindowDuration(*pair.w); durationKnown {
						existing.Baseline.WindowLength = duration
						if existing.Baseline.WindowKind == "" {
							existing.Baseline.WindowKind = pair.w.Kind
						}
						if strictObservation && looksLikeStrictLazyObservation(observedAt, *pair.w, duration) {
							existing.Baseline.ResetAt = pair.w.ResetAt
							existing.Baseline.Usage = *pair.w.UsedPercent
							existing.Baseline.SuspectedLazy = true
							existing.State = ProbePendingCheck
							existing.Deadline = time.Time{}
						} else if existing.State == ProbeWaitingReset {
							existing.Deadline = deadlineFor(existing.Baseline, now, observationInterval)
							if !existing.Deadline.After(now) {
								existing.State = ProbePendingCheck
								existing.Deadline = time.Time{}
							}
						}
						if r.runtimeRoster().Capability != CapabilityA {
							existing.State = ProbeWaitingRoster
							existing.Deadline = time.Time{}
						}
						r.probeController.SetWindow(b.Instance, pair.k, existing)
						touched[b.Instance] = struct{}{}
					}
				}
				continue
			}
			usage := 0.0
			if pair.w.UsedPercent != nil {
				usage = *pair.w.UsedPercent
			}
			duration, durationKnown := probeWindowDuration(*pair.w)
			base := ResetProbeBaseline(pair.w.ResetAt, usage, 0)
			base.WindowKind = pair.w.Kind
			if durationKnown {
				base.WindowLength = duration
			}
			state := ProbeWaitingReset
			deadline := deadlineFor(base, now, observationInterval)
			lazy := false
			if strictObservation {
				_, lazy = firstObservationLazyWindow(observedAt, *pair.w)
			}
			if lazy {
				base.SuspectedLazy = true
				state = ProbePendingCheck
				deadline = time.Time{}
			} else if !deadline.After(now) {
				state = ProbePendingCheck
				deadline = time.Time{}
			}
			if r.runtimeRoster().Capability != CapabilityA {
				state = ProbeWaitingRoster
				deadline = time.Time{}
			}
			r.probeController.SetWindow(b.Instance, pair.k, ProbeWindow{State: state, Baseline: base, Deadline: deadline})
			touched[b.Instance] = struct{}{}
		}
	}
	err := r.persistProbeInstances(touched)
	if err != nil {
		if r.probePersistPending == nil {
			r.probePersistPending = map[AuthInstanceID]struct{}{}
		}
		for instance := range touched {
			r.probePersistPending[instance] = struct{}{}
		}
	} else {
		for instance := range touched {
			delete(r.probePersistPending, instance)
		}
	}
	if err == nil && len(touched) > 0 {
		r.wakeRefreshLoop()
	}
	return err
}

func (r *QuotaRefresher) probeObservationInterval() time.Duration {
	interval := NormalizeConfig(r.state.Config()).QuotaRefreshInterval
	if interval < probeUnknownResetRecheck {
		return probeUnknownResetRecheck
	}
	return interval
}

func (r *QuotaRefresher) persistProbeInstances(instances map[AuthInstanceID]struct{}) error {
	if r.runtimeStore == nil || r.probeController == nil || len(instances) == 0 {
		return nil
	}
	snapshot := r.probeController.Snapshot()
	_, err := r.runtimeStore.Update(func(state *PersistentState) error {
		for instance := range instances {
			if windows, ok := snapshot[instance]; ok {
				state.ProbeWindows[instance] = windows
			} else {
				delete(state.ProbeWindows, instance)
			}
		}
		return nil
	})
	return err
}
func (r *QuotaRefresher) persistProbeWindows() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	snapshot := r.probeController.Snapshot()
	_, err := r.runtimeStore.Update(func(s *PersistentState) error {
		for instance, windows := range snapshot {
			s.ProbeWindows[instance] = windows
		}
		return nil
	})
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
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	observed := map[ProbeWindowKind]*QuotaWindow{
		ProbeWindowFiveHour: quota.FiveHour,
		ProbeWindowLong:     quota.LongWindow,
	}
	now := r.now()
	observationInterval := r.probeObservationInterval()
	updated, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		attempt := persisted.ProbeAttempts[instance]
		for kind, observation := range observed {
			window, exists := persisted.ProbeWindows[instance][kind]
			if observation != nil {
				if exists {
					if rearmed, changed := rearmConfirmedProbeWindow(window, now, observation, now, observationInterval); changed {
						persisted.ProbeWindows[instance][kind] = rearmed
					}
				}
				continue
			}
			if window.State == ProbeConfirmed || (nonterminalProbeAttempt(attempt) && attemptReferencesWindow(attempt, kind)) {
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
	for kind := range observed {
		if window, retained := updated.ProbeWindows[instance][kind]; retained {
			r.probeController.SetWindow(instance, kind, window)
		} else {
			r.probeController.RemoveWindow(instance, kind)
		}
	}
	r.wakeRefreshLoop()
	return nil
}

func rearmConfirmedProbeWindow(window ProbeWindow, observedAt time.Time, observation *QuotaWindow, now time.Time, observationInterval time.Duration) (ProbeWindow, bool) {
	if window.State != ProbeConfirmed || observation == nil || observation.UsedPercent == nil || observation.ResetAt.IsZero() || observedAt.IsZero() || observedAt.After(now) {
		return window, false
	}
	duration, durationKnown := probeWindowDuration(*observation)
	if !durationKnown || duration <= 0 {
		return window, false
	}
	if window.Baseline.Kind == ProbeBaselineReset && !window.Baseline.ResetAt.IsZero() && !observation.ResetAt.After(window.Baseline.ResetAt.Add(probeSkewTolerance)) {
		return window, false
	}
	base := ResetProbeBaseline(observation.ResetAt, *observation.UsedPercent, duration)
	base.WindowKind = observation.Kind
	window.Baseline = base
	window.AttemptID = ""
	window.RetryCount = 0
	if looksLikeStrictLazyObservation(observedAt, *observation, duration) {
		window.Baseline.SuspectedLazy = true
		window.State = ProbePendingCheck
		window.Deadline = time.Time{}
	} else {
		window.State = ProbeWaitingReset
		window.Deadline = deadlineFor(window.Baseline, now, observationInterval)
	}
	return window, true
}

func (r *QuotaRefresher) syncProbeControllerInstance(instance AuthInstanceID, state PersistentState) {
	if r.probeController == nil {
		return
	}
	r.probeController.ReplaceInstance(instance, state.ProbeWindows[instance])
}

func (r *QuotaRefresher) syncProbeControllerInstanceFromStore(instance AuthInstanceID) {
	if r.runtimeStore == nil {
		return
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err == nil {
		r.syncProbeControllerInstance(instance, state)
	}
}

func (r *QuotaRefresher) persistTerminalProbeCompletion(instance AuthInstanceID, attemptID string, quota ParsedQuota, phases ...ProbeAttemptPhase) error {
	if r.runtimeStore == nil || r.probeController == nil || instance == 0 {
		return nil
	}
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	return r.persistTerminalProbeCompletionLocked(instance, attemptID, quota, phases...)
}

func (r *QuotaRefresher) persistVerifiedProbeCompletion(instance AuthInstanceID, attemptID string, quota ParsedQuota, phases ...ProbeAttemptPhase) error {
	if r.runtimeStore == nil || r.probeController == nil || instance == 0 {
		return nil
	}
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	r.probeController.Advance(instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(quota)})
	return r.persistTerminalProbeCompletionLocked(instance, attemptID, quota, phases...)
}

func (r *QuotaRefresher) persistTerminalProbeCompletionLocked(instance AuthInstanceID, attemptID string, quota ParsedQuota, phases ...ProbeAttemptPhase) error {
	observed := map[ProbeWindowKind]bool{
		ProbeWindowFiveHour: quota.FiveHour != nil,
		ProbeWindowLong:     quota.LongWindow != nil,
	}
	controllerWindows := r.probeController.Snapshot()[instance]
	windows := make(map[ProbeWindowKind]ProbeWindow, len(controllerWindows))
	for kind, window := range controllerWindows {
		windows[kind] = window
	}
	for kind, present := range observed {
		if present {
			continue
		}
		delete(windows, kind)
	}
	_, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		if _, err := exactProbeAttempt(persisted, instance, attemptID, phases...); err != nil {
			return err
		}
		if len(windows) > 0 {
			persisted.ProbeWindows[instance] = windows
		} else {
			delete(persisted.ProbeWindows, instance)
		}
		delete(persisted.ProbeAttempts, instance)
		return nil
	})
	if err != nil {
		r.syncProbeControllerInstanceFromStore(instance)
		return err
	}
	for kind, present := range observed {
		if !present {
			r.probeController.RemoveWindow(instance, kind)
		}
	}
	return nil
}

func (r *QuotaRefresher) persistProbeFailure(instance AuthInstanceID, attemptID string, windows []ProbeWindowKind, sent, authBlocked bool, login LoginEpoch, suppressUntil time.Time) error {
	if r.runtimeStore == nil || r.probeController == nil || instance == 0 {
		return nil
	}
	r.probeHoldMu.Lock()
	defer r.probeHoldMu.Unlock()
	controllerWindows := r.probeController.Snapshot()[instance]
	now := r.now()
	committed, err := r.runtimeStore.Update(func(state *PersistentState) error {
		var attempt ProbeAttempt
		var attemptErr error
		if sent {
			attempt, attemptErr = exactProbeAttempt(state, instance, attemptID, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown)
		} else {
			attempt, attemptErr = exactProbeAttempt(state, instance, attemptID, ProbeAttemptPrepared, ProbeAttemptSending)
		}
		if attemptErr != nil {
			return attemptErr
		}
		if sent {
			attempt.Phase = ProbeAttemptSentUnknown
			if suppressUntil.After(attempt.SuppressUntil) {
				attempt.SuppressUntil = suppressUntil
			}
			state.ProbeAttempts[instance] = attempt
		} else if attempt.Phase == ProbeAttemptPrepared {
			delete(state.ProbeAttempts, instance)
		}

		next := make(map[ProbeWindowKind]ProbeWindow, len(state.ProbeWindows[instance]))
		for kind, window := range state.ProbeWindows[instance] {
			next[kind] = window
		}
		for _, kind := range windows {
			window, ok := controllerWindows[kind]
			if !ok {
				window, ok = next[kind]
			}
			if !ok || window.State == ProbeWaitingRoster {
				continue
			}
			if authBlocked {
				window.State = ProbeAuthBlocked
				window.AuthBlockedAtLogin = login
				window.Deadline = time.Time{}
			} else {
				if sent {
					window.State = ProbeSentUnknown
					window.AttemptID = attemptID
				} else {
					window.State = ProbeRetryWait
					window.AttemptID = ""
				}
				window.RetryCount++
				window.Deadline = now.Add(probeBackoff(window.RetryCount))
			}
			next[kind] = window
		}
		if len(next) > 0 {
			state.ProbeWindows[instance] = next
		} else {
			delete(state.ProbeWindows, instance)
		}
		return nil
	})
	if err != nil {
		r.syncProbeControllerInstanceFromStore(instance)
		return err
	}
	r.syncProbeControllerInstance(instance, committed)
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
	snapshot := r.probeController.Snapshot()
	committed, err := r.runtimeStore.Update(func(s *PersistentState) error {
		for instance, windows := range snapshot {
			attempt, hasAttempt := s.ProbeAttempts[instance]
			sent := hasAttempt && (attempt.Phase == ProbeAttemptSending || attempt.Phase == ProbeAttemptSent || attempt.Phase == ProbeAttemptSentUnknown)
			sentWindows := make(map[ProbeWindowKind]struct{}, len(attempt.Windows))
			if sent {
				for _, kind := range attempt.Windows {
					sentWindows[kind] = struct{}{}
				}
			} else if hasAttempt {
				delete(s.ProbeAttempts, instance)
			}
			next := make(map[ProbeWindowKind]ProbeWindow, len(windows))
			for kind, window := range windows {
				window.Deadline = time.Time{}
				if _, belongsToSentAttempt := sentWindows[kind]; belongsToSentAttempt {
					window.State = ProbeSentUnknown
					window.AttemptID = attempt.AttemptID
				} else if window.State != ProbeConfirmed {
					window.State = ProbeWaitingRoster
					window.AttemptID = ""
				}
				next[kind] = window
			}
			if len(next) > 0 {
				s.ProbeWindows[instance] = next
			} else {
				delete(s.ProbeWindows, instance)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for instance := range snapshot {
		r.syncProbeControllerInstance(instance, committed)
	}
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
	touched := make(map[AuthInstanceID]struct{}, len(bindings))
	for _, binding := range bindings {
		touched[binding.Instance] = struct{}{}
		r.probeController.Advance(binding.Instance, ProbeEvent{Kind: ProbeEventRosterConfirmed, Now: now, ObservationInterval: r.probeObservationInterval()})
		for _, kind := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
			if window, ok := r.probeController.Window(binding.Instance, kind); ok && window.State == ProbeAuthBlocked && binding.Login > window.AuthBlockedAtLogin {
				window.State = ProbePendingCheck
				window.Deadline = time.Time{}
				r.probeController.SetWindow(binding.Instance, kind, window)
			}
			window, ok := r.probeController.Window(binding.Instance, kind)
			if ok && window.State == ProbeWaitingReset && !window.Deadline.After(now) {
				window.State = ProbePendingCheck
				window.Deadline = time.Time{}
				r.probeController.SetWindow(binding.Instance, kind, window)
			}
		}
	}
	return r.persistProbeInstances(touched)
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
	_, err := r.runProbeDueOnce(ctx)
	return err
}

func (r *QuotaRefresher) runProbeDueOnce(ctx context.Context) (bool, error) {
	return r.runProbeDueOnceWithAdmission(ctx, nil)
}

func (r *QuotaRefresher) runProbeDueOnceAdmitted(ctx context.Context, generation uint64) (bool, error) {
	return r.runProbeDueOnceWithAdmission(ctx, &generation)
}

func (r *QuotaRefresher) runProbeDueOnceWithAdmission(ctx context.Context, generation *uint64) (bool, error) {
	if !r.resetProbeEnabled() {
		return false, nil
	}
	r.probeRunStateMu.Lock()
	if !r.probeRunMu.TryLock() {
		r.probeRerunPending = true
		r.probeRunStateMu.Unlock()
		return false, nil
	}
	if generation != nil && !r.probeLaunchAdmissionCurrentLocked(*generation) {
		r.probeRunMu.Unlock()
		r.probeRunStateMu.Unlock()
		return false, nil
	}
	r.probeRerunPending = false
	r.probeRunStateMu.Unlock()
	var firstErr error
	for {
		err := r.runProbeDuePass(ctx)
		if firstErr == nil {
			firstErr = err
		}
		if err != nil {
			r.finishProbeOwnerError(true)
			return true, firstErr
		}
		r.probeRunStateMu.Lock()
		if r.probeRerunPending {
			r.probeRerunPending = false
			r.probeRunStateMu.Unlock()
			continue
		}
		r.probeRunMu.Unlock()
		r.probeRunStateMu.Unlock()
		return true, firstErr
	}
}

func (r *QuotaRefresher) runProbeDuePass(ctx context.Context) error {
	if !r.resetProbeEnabled() {
		return nil
	}
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
				w.Deadline = time.Time{}
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
		claimWindows := r.probeController.Snapshot()[b.Instance]
		claimed, claimErr := r.runtimeStore.Update(func(s *PersistentState) error {
			if existing, ok := s.ProbeAttempts[b.Instance]; ok && existing.AttemptID != "" {
				attempt = existing
				return nil
			}
			s.ProbeAttemptSeq++
			attempt = ProbeAttempt{Instance: b.Instance, AttemptID: fmt.Sprintf("probe-%d-%d", b.Instance, s.ProbeAttemptSeq), Windows: append([]ProbeWindowKind(nil), due...), Phase: ProbeAttemptPrepared, CreatedAt: now, VerifyNotBefore: now}
			s.ProbeAttempts[b.Instance] = attempt
			if claimWindows != nil {
				s.ProbeWindows[b.Instance] = claimWindows
			} else {
				delete(s.ProbeWindows, b.Instance)
			}
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
	if !r.resetProbeEnabled() {
		return nil
	}
	r.probeRunMu.Lock()
	return r.runProbeRecoveryOwned(ctx)
}

func (r *QuotaRefresher) runProbeRecoveryOnceAdmitted(ctx context.Context, generation uint64) (bool, error) {
	if !r.resetProbeEnabled() {
		return false, nil
	}
	r.probeRunMu.Lock()
	r.probeRunStateMu.Lock()
	if !r.probeLaunchAdmissionCurrentLocked(generation) {
		r.probeRunMu.Unlock()
		r.probeRunStateMu.Unlock()
		return false, nil
	}
	r.probeRunStateMu.Unlock()
	return true, r.runProbeRecoveryOwned(ctx)
}

func (r *QuotaRefresher) runProbeRecoveryOwned(ctx context.Context) (runErr error) {
	defer func() {
		if runErr != nil {
			r.finishProbeOwnerError(true)
			return
		}
		runErr = r.finishProbeRecoveryOwner(ctx)
	}()
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
	touched := map[AuthInstanceID]struct{}{}
	if err = r.reconcileProbeOrphans(persisted, activeInstances); err != nil {
		firstErr = err
	}
	for _, intent := range intents {
		touched[intent.Instance] = struct{}{}
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
		attempt := st.ProbeAttempts[intent.Instance]
		for _, kind := range attempt.Windows {
			if window, ok := r.probeController.Window(intent.Instance, kind); ok {
				window.State = ProbeSentUnknown
				window.AttemptID = attempt.AttemptID
				window.Deadline = time.Time{}
				r.probeController.SetWindow(intent.Instance, kind, window)
			}
		}
		intent.Payload = probeSequencePayload{Binding: binding, Windows: windows, Attempt: attempt, Recovery: true}
		result := r.coordinator.SubmitTyped(intent).Await(ctx)
		if result.Err != nil {
			if firstErr == nil {
				firstErr = result.Err
			}
			continue
		}
	}
	if err := r.persistProbeInstances(touched); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *QuotaRefresher) finishProbeRecoveryOwner(ctx context.Context) error {
	for {
		r.probeRunStateMu.Lock()
		if !r.probeRerunPending {
			r.probeRunMu.Unlock()
			r.probeRunStateMu.Unlock()
			return nil
		}
		r.probeRerunPending = false
		r.probeRunStateMu.Unlock()
		if err := r.runProbeDuePass(ctx); err != nil {
			r.finishProbeOwnerError(true)
			return err
		}
	}
}

func (r *QuotaRefresher) finishProbeOwnerError(recoverFirst bool) {
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	r.probeRunStateMu.Lock()
	if recoverFirst {
		r.probeRecoveryPending = true
	}
	r.publishProbeRetryLocked(now, true)
	r.probeRerunPending = false
	r.probeRunMu.Unlock()
	r.probeRunStateMu.Unlock()
	r.wakeRefreshLoop()
}

func (r *QuotaRefresher) recoverPreparedProbeAttempts() error {
	if r.runtimeStore == nil || r.probeController == nil {
		return nil
	}
	state, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	prepared := map[AuthInstanceID]ProbeAttempt{}
	now := r.now()
	for instance, attempt := range state.ProbeAttempts {
		if attempt.Phase != ProbeAttemptPrepared {
			continue
		}
		prepared[instance] = attempt
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
	}
	if len(prepared) == 0 {
		return nil
	}
	windows := r.probeController.Snapshot()
	_, err = r.runtimeStore.Update(func(persisted *PersistentState) error {
		for instance, attempt := range prepared {
			if instanceWindows, ok := windows[instance]; ok {
				persisted.ProbeWindows[instance] = instanceWindows
			} else {
				delete(persisted.ProbeWindows, instance)
			}
			if current, ok := persisted.ProbeAttempts[instance]; ok && current.AttemptID == attempt.AttemptID && current.Phase == ProbeAttemptPrepared {
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
		stage := "probe_start"
		fields := func(windows []ProbeWindowKind) map[string]any {
			return map[string]any{"auth_id": intent.AuthID, "attempt_id": attempt.AttemptID, "windows": append([]ProbeWindowKind(nil), windows...)}
		}
		recordLog := func(level, event, message string, fields map[string]any) {
			if r.state != nil {
				r.state.RecordLog(level, event, message, fields, r.now())
			}
		}
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
		recordFailure := func(err error, sent bool, windows []ProbeWindowKind) {
			failureFields := fields(windows)
			failureFields["error"] = "probe_operation_failed"
			failureFields["error_category"] = "external_callback"
			failureFields["stage"] = stage
			var activationStatus probeActivationStatusError
			var status quotaStatusError
			switch {
			case errors.As(err, &activationStatus):
				failureFields["error"] = "activation_http_status"
				failureFields["error_category"] = "upstream_http"
				failureFields["http_status"] = activationStatus.status
				failureFields["response_summary"] = sanitizedProbeResponseSummary(activationStatus.body)
			case errors.As(err, &status):
				failureFields["error"] = "quota_http_status"
				failureFields["error_category"] = "upstream_http"
				failureFields["http_status"] = status.status
				failureFields["response_summary"] = sanitizedProbeResponseSummary(status.body)
			case errors.Is(err, ErrCapabilityB):
				failureFields["error"] = "background_not_authorized"
				failureFields["error_category"] = "authorization"
			case errors.Is(err, ErrAccountIdentityUnresolved):
				failureFields["error"] = "account_identity_unresolved"
				failureFields["error_category"] = "authorization"
			case errors.Is(err, ErrProvisionalFingerprintMismatch):
				failureFields["error"] = "credential_fingerprint_mismatch"
				failureFields["error_category"] = "authorization"
			case errors.Is(err, context.Canceled):
				failureFields["error"] = "operation_canceled"
				failureFields["error_category"] = "internal"
			case errors.Is(err, context.DeadlineExceeded):
				failureFields["error"] = "operation_deadline_exceeded"
				failureFields["error_category"] = "internal"
			}
			failureFields["sent"] = sent
			recordLog("warn", "probe.failed", "额度周期探测失败，已按安全策略处理", failureFields)
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
				recordFailure(err, sent, p.Windows)
				return OperationResult{Token: intent.Token, Err: err}
			}
			if r.runtimeRoster().Health == RosterFailClosed {
				recordFailure(err, sent, p.Windows)
				return OperationResult{Token: intent.Token, Err: err}
			}
			status, authBlocked := err.(quotaStatusError)
			authBlocked = authBlocked && status.status == http.StatusUnauthorized
			persistErr := r.persistProbeFailure(intent.Instance, attempt.AttemptID, p.Windows, sent, authBlocked, p.Binding.Login, attempt.SuppressUntil)
			if persistErr != nil {
				if !errors.Is(persistErr, ErrProbeAttemptChanged) {
					err = persistErr
				}
			} else if authBlocked && r.bindings != nil {
				if blockErr := r.bindings.MarkAuthBlocked(intent.AuthID); blockErr != nil {
					err = blockErr
				}
			}
			recordFailure(err, sent, p.Windows)
			return OperationResult{Token: intent.Token, Err: err}
		}
		if p.Recovery {
			stage = "recovery_verify_quota"
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
			stage = "persist_recovery_verification"
			if err = r.persistVerifiedProbeCompletion(intent.Instance, attempt.AttemptID, vr.Quota, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown); err != nil {
				recordFailure(err, true, p.Windows)
				return OperationResult{Token: intent.Token, Err: err}
			}
			recordLog("info", "probe.verified", "额度周期激活验证完成", fields(p.Windows))
			res.ReadStartSeq = seq
			res.Value = vr
			return res
		}
		recordLog("info", "probe.precheck_started", "开始检查疑似未激活的额度周期", fields(p.Windows))
		stage = "precheck_quota"
		_, err := r.probeFence.Next() // actual-start precheck sequence, distinct from completedFence
		if err != nil {
			return fail(err, false)
		}
		pre, err := read()
		if err != nil {
			return fail(err, false)
		}
		r.probeHoldMu.Lock()
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
			err = r.persistTerminalProbeCompletionLocked(intent.Instance, attempt.AttemptID, pre.Quota, ProbeAttemptPrepared)
			r.probeHoldMu.Unlock()
			if err != nil {
				recordFailure(err, false, p.Windows)
				return OperationResult{Token: intent.Token, Err: err}
			}
			return res
		}
		r.probeHoldMu.Unlock()
		fence, err := r.probeFence.Next()
		if err != nil {
			return fail(err, false)
		}
		attempt.Windows = lazy
		attempt.SendFenceSeq = fence
		attempt.CreatedAt = r.now()
		attempt.VerifyNotBefore = attempt.CreatedAt.Add(3 * time.Second)
		attempt.SuppressUntil = attempt.CreatedAt.Add(10 * time.Minute)
		stage = "persist_activation_prepared"
		if err = r.probeWAL.PersistSending(attempt); err != nil {
			return fail(err, false)
		}
		held.MarkProbeSent(attempt.SuppressUntil)
		stage = "activation_post"
		err = held.DoHTTP(ctx, func(context.Context) error {
			return r.probeWAL.ExecuteSend(func() error {
				resp, e := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint, Headers: http.Header{"Authorization": []string{"Bearer " + pre.Credentials.AccessToken}, "Chatgpt-Account-Id": []string{pre.Credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}}, Body: resetProbePayloadBytes()}, true)
				if e != nil {
					return e
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return probeActivationStatusError{status: resp.StatusCode, body: resp.Body}
				}
				return nil
			})
		})
		if err != nil {
			return fail(err, true)
		}
		stage = "persist_activation_sent"
		if err = r.probeWAL.PersistSent(intent.Instance, attempt.AttemptID, r.now()); err != nil {
			return fail(err, true)
		}
		recordLog("info", "probe.activation_sent", "已发送极小请求以激活额度周期", fields(lazy))
		stage = "propagation_wait"
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
		stage = "verify_quota"
		vr, err := read()
		if err != nil {
			return fail(err, true)
		}
		r.probeHoldMu.Lock()
		r.probeController.Advance(intent.Instance, ProbeEvent{Kind: ProbeEventVerifyResult, Now: r.now(), Snapshots: probeSnapshots(vr.Quota)})
		for _, k := range lazy {
			if w, ok := r.probeController.Window(intent.Instance, k); ok && w.State == ProbeRetryWait && w.Deadline.Before(attempt.SuppressUntil) {
				w.Deadline = attempt.SuppressUntil
				r.probeController.SetWindow(intent.Instance, k, w)
			}
		}
		stage = "persist_verification"
		err = r.persistTerminalProbeCompletionLocked(intent.Instance, attempt.AttemptID, vr.Quota, ProbeAttemptSent)
		r.probeHoldMu.Unlock()
		if err != nil {
			recordFailure(err, true, lazy)
			return OperationResult{Token: intent.Token, Err: err}
		}
		recordLog("info", "probe.verified", "额度周期激活验证完成", fields(lazy))
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
		var usage *float64
		if p.w.UsedPercent != nil {
			value := *p.w.UsedPercent
			usage = &value
		}
		reset := p.w.ResetAt
		length, lengthKnown := probeWindowDuration(*p.w)
		out[p.k] = QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: usage, WindowKind: p.w.Kind, WindowLength: length, WindowLengthKnown: lengthKnown}
	}
	return out
}
