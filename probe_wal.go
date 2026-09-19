package main

import (
	"errors"
	"fmt"
	"time"
)

const (
	OperationProbeSend OperationClass = "probe_send"
)

type ProbeWAL struct {
	store *StateStore
	crash CrashHitter
}

var ErrProbeAttemptChanged = errors.New("probe attempt changed")

func exactProbeAttempt(state *PersistentState, instance AuthInstanceID, attemptID string, phases ...ProbeAttemptPhase) (ProbeAttempt, error) {
	attempt, ok := state.ProbeAttempts[instance]
	if !ok || attemptID == "" || attempt.AttemptID != attemptID {
		return ProbeAttempt{}, fmt.Errorf("%w for instance %d", ErrProbeAttemptChanged, instance)
	}
	for _, phase := range phases {
		if attempt.Phase == phase {
			return attempt, nil
		}
	}
	return ProbeAttempt{}, fmt.Errorf("%w for instance %d: phase %q", ErrProbeAttemptChanged, instance, attempt.Phase)
}

func persistProbeSentUnknown(state *PersistentState, instance AuthInstanceID, attemptID string, suppress time.Time) error {
	attempt, err := exactProbeAttempt(state, instance, attemptID, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown)
	if err != nil {
		return err
	}
	attempt.Phase = ProbeAttemptSentUnknown
	if suppress.After(attempt.SuppressUntil) {
		attempt.SuppressUntil = suppress
	}
	state.ProbeAttempts[instance] = attempt
	return nil
}

func NewProbeWAL(s *StateStore, crash ...CrashHitter) *ProbeWAL {
	w := &ProbeWAL{store: s}
	if len(crash) > 0 {
		w.crash = crash[0]
	}
	return w
}
func (w *ProbeWAL) hit(name string) error {
	if w.crash != nil {
		return w.crash.Hit(name)
	}
	return nil
}
func (w *ProbeWAL) PersistSending(a ProbeAttempt) error {
	a.Phase = ProbeAttemptSending
	if a.SuppressUntil.IsZero() {
		a.SuppressUntil = a.CreatedAt.Add(10 * time.Minute)
	}
	//kpoint:K_PROBE_SENDING_WRITE
	if err := w.hit("K_PROBE_SENDING_WRITE"); err != nil {
		return err
	}
	_, err := w.store.Update(func(s *PersistentState) error {
		if _, err := exactProbeAttempt(s, a.Instance, a.AttemptID, ProbeAttemptPrepared); err != nil {
			return err
		}
		s.ProbeAttempts[a.Instance] = a
		return nil
	})
	//kpoint:K_PROBE_AFTER_SENDING
	if err == nil {
		err = w.hit("K_PROBE_AFTER_SENDING")
	}
	return err
}
func (w *ProbeWAL) ExecuteSend(send func() error) error { //kpoint:K_PROBE_BEFORE_HTTP
	if err := w.hit("K_PROBE_BEFORE_HTTP"); err != nil {
		return err
	}
	err := send()
	if err != nil {
		return err
	} //kpoint:K_PROBE_AFTER_HTTP
	return w.hit("K_PROBE_AFTER_HTTP")
}
func (w *ProbeWAL) PersistSent(i AuthInstanceID, attemptID string, at time.Time) error {
	//kpoint:K_PROBE_SENT_WRITE
	if err := w.hit("K_PROBE_SENT_WRITE"); err != nil {
		return err
	}
	_, err := w.store.Update(func(s *PersistentState) error {
		a, err := exactProbeAttempt(s, i, attemptID, ProbeAttemptSending)
		if err != nil {
			return err
		}
		a.Phase = ProbeAttemptSent
		a.SentAt = &at
		s.ProbeAttempts[i] = a
		return nil
	})
	return err
}
func (w *ProbeWAL) PersistSentUnknown(i AuthInstanceID, attemptID string, suppress time.Time) error {
	_, err := w.store.Update(func(s *PersistentState) error {
		return persistProbeSentUnknown(s, i, attemptID, suppress)
	})
	return err
}
func (w *ProbeWAL) Complete(i AuthInstanceID, attemptID string, phases ...ProbeAttemptPhase) error {
	_, err := w.store.Update(func(s *PersistentState) error {
		if _, err := exactProbeAttempt(s, i, attemptID, phases...); err != nil {
			return err
		}
		delete(s.ProbeAttempts, i)
		return nil
	})
	return err
}
func (w *ProbeWAL) Recover(now time.Time) []Intent {
	out, _ := w.RecoverChecked(now)
	return out
}

// RecoveryDeadline returns the earliest VerifyNotBefore among durable attempts
// that RecoverChecked can recover (Sending, Sent, or SentUnknown). It gives the
// refresh loop a wake source for windows whose in-memory state
// (ProbeSentAwaitingVerify / ProbeSentUnknown) contributes no
// ProbeController.NextDeadline entry, so an ambiguous send cannot sit dormant
// until an unrelated wake (Siriussee fix/prewarm-unused-lazy-windows, adapted).
// A zero returned time means an attempt is already recoverable now.
func (w *ProbeWAL) RecoveryDeadline() (time.Time, bool) {
	st, err := w.store.PersistentSnapshot()
	if err != nil {
		return time.Time{}, false
	}
	found := false
	var out time.Time
	for _, a := range st.ProbeAttempts {
		if a.Phase != ProbeAttemptSending && a.Phase != ProbeAttemptSent && a.Phase != ProbeAttemptSentUnknown {
			continue
		}
		if a.VerifyNotBefore.IsZero() {
			return time.Time{}, true
		}
		if !found || a.VerifyNotBefore.Before(out) {
			out, found = a.VerifyNotBefore, true
		}
	}
	return out, found
}

func (w *ProbeWAL) RecoverChecked(now time.Time) ([]Intent, error) {
	st, err := w.store.PersistentSnapshot()
	if err != nil {
		return nil, err
	}
	var out []Intent
	for i, a := range st.ProbeAttempts {
		if a.Phase != ProbeAttemptSending && a.Phase != ProbeAttemptSent && a.Phase != ProbeAttemptSentUnknown {
			continue
		}
		if now.Before(a.VerifyNotBefore) {
			continue
		}
		if a.Phase == ProbeAttemptSending {
			committed, transitionErr := w.store.Update(func(s *PersistentState) error {
				return persistProbeSentUnknown(s, i, a.AttemptID, a.SuppressUntil)
			})
			if transitionErr != nil {
				if errors.Is(transitionErr, ErrProbeAttemptChanged) {
					continue
				}
				return nil, transitionErr
			}
			a = committed.ProbeAttempts[i]
		}
		out = append(out, Intent{Instance: i, Class: OperationProbeVerify, Source: SourceProbeVerify, StartedAfter: a.SendFenceSeq, AttemptID: a.AttemptID, Payload: a.Windows})
	}
	return out, nil
}
