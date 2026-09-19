package main

import (
	"fmt"
	"sync"
	"time"
)

type ProbeWindowState string

const (
	ProbeIdle               ProbeWindowState = "Idle"
	ProbeWaitingReset       ProbeWindowState = "WaitingReset"
	ProbePendingCheck       ProbeWindowState = "PendingCheck"
	ProbeSentAwaitingVerify ProbeWindowState = "SentAwaitingVerify"
	ProbeSentUnknown        ProbeWindowState = "SentUnknown"
	ProbeRetryWait          ProbeWindowState = "RetryWait"
	ProbeConfirmed          ProbeWindowState = "Confirmed"
	ProbeAuthBlocked        ProbeWindowState = "AuthBlocked"
	ProbeAnomalyHold        ProbeWindowState = "AnomalyHold"
	ProbeWaitingRoster      ProbeWindowState = "WaitingRoster"
)

type ProbeWindow struct {
	State              ProbeWindowState `json:"state"`
	Baseline           ProbeBaseline    `json:"baseline"`
	Deadline           time.Time        `json:"deadline,omitempty"`
	RetryCount         int              `json:"retry_count,omitempty"`
	AttemptID          string           `json:"attempt_id,omitempty"`
	AuthBlockedAtLogin LoginEpoch       `json:"auth_blocked_at_login,omitempty"`
}
type ProbeEventKind string

const (
	ProbeEventDeadline        ProbeEventKind = "deadline"
	ProbeEventPrecheckResult  ProbeEventKind = "precheck_result"
	ProbeEventVerifyResult    ProbeEventKind = "verify_result"
	ProbeEventAuthFailed      ProbeEventKind = "auth_failed"
	ProbeEventExternalLogin   ProbeEventKind = "external_login"
	ProbeEventRosterConfirmed ProbeEventKind = "roster_confirmed"
	ProbeEventInstanceRemoved ProbeEventKind = "instance_removed"
)

type ProbeEvent struct {
	Kind                ProbeEventKind
	Window              ProbeWindowKind
	Now                 time.Time
	Snapshots           map[ProbeWindowKind]QuotaSnapshot
	RefreshMode         RefreshMode
	ObservationInterval time.Duration
}
type ProbeController struct {
	mu      sync.Mutex
	now     time.Time
	windows map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow
	seq     uint64
}

func (c *ProbeController) Load(all map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows = map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}
	for i, ws := range all {
		c.windows[i] = map[ProbeWindowKind]ProbeWindow{}
		for k, w := range ws {
			if validPersistentProbeState(w.State) {
				c.windows[i][k] = w
			}
		}
	}
}
func (c *ProbeController) Snapshot() map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}
	for i, ws := range c.windows {
		out[i] = map[ProbeWindowKind]ProbeWindow{}
		for k, w := range ws {
			out[i][k] = w
		}
	}
	return out
}
func (c *ProbeController) NextDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out time.Time
	for _, ws := range c.windows {
		for _, w := range ws {
			if w.State != ProbeWaitingReset && w.State != ProbeRetryWait && w.State != ProbeAnomalyHold {
				continue
			}
			if !w.Deadline.IsZero() && (out.IsZero() || w.Deadline.Before(out)) {
				out = w.Deadline
			}
		}
	}
	return out
}
func validPersistentProbeState(s ProbeWindowState) bool {
	switch s {
	case ProbeIdle, ProbeWaitingReset, ProbePendingCheck, ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeRetryWait, ProbeConfirmed, ProbeAuthBlocked, ProbeAnomalyHold, ProbeWaitingRoster:
		return true
	}
	return false
}

func NewProbeController(now time.Time) *ProbeController {
	return &ProbeController{now: now, windows: map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}}
}
func (c *ProbeController) SetWindow(i AuthInstanceID, k ProbeWindowKind, w ProbeWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.windows[i] == nil {
		c.windows[i] = map[ProbeWindowKind]ProbeWindow{}
	}
	c.windows[i][k] = w
}
func (c *ProbeController) ReplaceInstance(i AuthInstanceID, windows map[ProbeWindowKind]ProbeWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(windows) == 0 {
		delete(c.windows, i)
		return
	}
	next := make(map[ProbeWindowKind]ProbeWindow, len(windows))
	for kind, window := range windows {
		next[kind] = window
	}
	c.windows[i] = next
}
func (c *ProbeController) Window(i AuthInstanceID, k ProbeWindowKind) (ProbeWindow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.windows[i][k]
	return w, ok
}
func (c *ProbeController) RemoveWindow(i AuthInstanceID, k ProbeWindowKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.windows[i], k)
	if len(c.windows[i]) == 0 {
		delete(c.windows, i)
	}
}
func (c *ProbeController) Advance(i AuthInstanceID, e ProbeEvent) []Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.windows[i]
	if ws == nil {
		return nil
	}
	if e.Kind == ProbeEventInstanceRemoved {
		delete(c.windows, i)
		return nil
	}
	var out []Intent
	for k, w := range ws {
		if e.Window != "" && e.Window != k {
			continue
		}
		switch e.Kind {
		case ProbeEventDeadline:
			if (w.State == ProbeWaitingReset || w.State == ProbeRetryWait || w.State == ProbeAnomalyHold) && !w.Deadline.After(e.Now) {
				w.State = ProbePendingCheck
				w.Deadline = time.Time{}
				ws[k] = w
				out = append(out, Intent{Instance: i, Class: OperationProbePrecheck, Source: SourceProbePrecheck, Payload: []ProbeWindowKind{k}})
			}
		case ProbeEventPrecheckResult, ProbeEventVerifyResult:
			if (e.Kind == ProbeEventPrecheckResult && w.State != ProbePendingCheck) || (e.Kind == ProbeEventVerifyResult && w.State != ProbeSentAwaitingVerify && w.State != ProbeSentUnknown) {
				continue
			}
			snap, ok := e.Snapshots[k]
			if !ok {
				continue
			}
			baseline := w.Baseline
			shiftedZeroCandidate := false
			strictAuthorized := false
			if e.Kind == ProbeEventPrecheckResult && baseline.Kind == ProbeBaselineReset && snap.Valid && snap.ResetAt != nil && snap.Usage != nil {
				migrated := baseline.SuspectedLazy
				shifted := snap.ResetAt.After(baseline.ResetAt.Add(probeSkewTolerance))
				shiftedZeroCandidate = shifted && *snap.Usage == 0
				if baseline.WindowKind == "" && compatibleLegacyProbeWindowKind(k, baseline, snap) {
					baseline.WindowKind = snap.WindowKind
				}
				kindMatches := baseline.WindowKind != "" && snap.WindowKind == baseline.WindowKind
				strictWindow := QuotaWindow{Kind: snap.WindowKind, UsedPercent: snap.Usage, ResetAt: *snap.ResetAt}
				if (shifted || migrated) && kindMatches && snap.WindowLengthKnown && looksLikeStrictLazyObservation(e.Now, strictWindow, snap.WindowLength) {
					baseline.ResetAt = *snap.ResetAt
					baseline.Usage = *snap.Usage
					baseline.SuspectedLazy = true
					strictAuthorized = true
				}
			}
			if e.Kind == ProbeEventVerifyResult && externalResetRebaseApplies(baseline, snap, e.Now) {
				// A server-side compensating reset minted a fresh never-used
				// window while our probe was in flight. Rebase the baseline to
				// the new window instead of classifying the multi-cycle jump as
				// an anomaly, so the window retries activation on the next
				// precheck instead of looping in AnomalyHold (Siriussee
				// fix/prewarm-unused-lazy-windows, adapted to the strict
				// evidence model: the fresh-window signature below is required).
				baseline.ResetAt = *snap.ResetAt
				baseline.Usage = *snap.Usage
				baseline.SuspectedLazy = true
			}
			cl := ClassifyProbeWindow(baseline, snap, e.Now)
			w.Baseline = cl.Baseline
			unauthorizedLazy := e.Kind == ProbeEventPrecheckResult && (cl.Kind == ProbeStillLazy || cl.Kind == ProbeAmbiguous) && !strictAuthorized
			unauthorizedShiftedZero := e.Kind == ProbeEventPrecheckResult && shiftedZeroCandidate && !strictAuthorized && cl.Kind == ProbeActivatedNew
			if unauthorizedLazy || unauthorizedShiftedZero {
				w.Baseline.SuspectedLazy = false
				w.State = ProbeWaitingReset
				w.AttemptID = ""
				w.Deadline = readOnlyObservationDeadline(w.Baseline, e.Now, e.ObservationInterval)
				ws[k] = w
				continue
			}
			switch cl.Kind {
			case ProbeActivatedNew, ProbeActivatedInferred:
				w.State = ProbeConfirmed
				w.Deadline = time.Time{}
			case ProbeNotDueYet:
				w.State = ProbeWaitingReset
				w.Deadline = deadlineFor(w.Baseline, e.Now, e.ObservationInterval)
			case ProbeAnomaly:
				w.State = ProbeAnomalyHold
				w.Deadline = e.Now.Add(probeUnknownResetRecheck)
			case ProbeStillLazy, ProbeAmbiguous:
				if e.Kind == ProbeEventPrecheckResult {
					c.seq++
					w.State = ProbeSentAwaitingVerify
					w.AttemptID = fmt.Sprintf("probe-%d", c.seq)
					out = append(out, Intent{Instance: i, Class: OperationProbeSend, Source: SourceProbeActivation, Payload: []ProbeWindowKind{k}})
				} else {
					w.State = ProbeRetryWait
					w.RetryCount++
					w.Deadline = e.Now.Add(probeBackoff(w.RetryCount))
				}
			default:
				w.State = ProbeRetryWait
				w.RetryCount++
				w.Deadline = e.Now.Add(probeBackoff(w.RetryCount))
			}
			ws[k] = w
		case ProbeEventAuthFailed:
			if w.State != ProbeIdle {
				w.State = ProbeAuthBlocked
				ws[k] = w
			}
		case ProbeEventExternalLogin:
			if w.State == ProbeAuthBlocked {
				w.State = ProbePendingCheck
				ws[k] = w
			}
		case ProbeEventRosterConfirmed:
			if w.State == ProbeWaitingRoster {
				if w.Baseline.SuspectedLazy {
					w.State = ProbePendingCheck
					w.Deadline = time.Time{}
				} else {
					w.State = ProbeWaitingReset
					w.Deadline = deadlineFor(w.Baseline, e.Now, e.ObservationInterval)
				}
				ws[k] = w
			}
		}
	}
	return out
}

func compatibleLegacyProbeWindowKind(slot ProbeWindowKind, baseline ProbeBaseline, snap QuotaSnapshot) bool {
	if baseline.WindowLength <= 0 || snap.WindowKind == "" || !snap.WindowLengthKnown || snap.WindowLength <= 0 || absDuration(snap.WindowLength-baseline.WindowLength) > probeSkewTolerance {
		return false
	}
	switch slot {
	case ProbeWindowFiveHour:
		return snap.WindowKind == WindowFiveHour
	case ProbeWindowLong:
		return snap.WindowKind == WindowWeekly || snap.WindowKind == WindowMonthly
	default:
		return false
	}
}

func deadlineFor(b ProbeBaseline, now time.Time, observationInterval time.Duration) time.Time {
	if b.Kind == ProbeBaselineUsageOnly {
		return b.NextRecheckAt
	}
	if b.ResetAt.IsZero() {
		return now
	}
	resetMaturity := b.ResetAt.Add(probeRefreshAfterResetDelay)
	if b.WindowLength <= 0 || observationInterval <= 0 {
		return resetMaturity
	}
	observation := now.Add(observationInterval)
	if observation.Before(resetMaturity) {
		return observation
	}
	return resetMaturity
}
func readOnlyObservationDeadline(b ProbeBaseline, now time.Time, observationInterval time.Duration) time.Time {
	deadline := deadlineFor(b, now, observationInterval)
	if deadline.After(now) {
		return deadline
	}
	if observationInterval <= 0 {
		observationInterval = probeUnknownResetRecheck
	}
	return now.Add(observationInterval)
}
func probeBackoff(n int) time.Duration {
	if n <= 1 {
		return time.Minute
	}
	if n == 2 {
		return 5 * time.Minute
	}
	return 15 * time.Minute
}

// externalResetRebaseApplies reports whether a verify snapshot proves that a
// server-side compensating reset replaced the observed window mid-probe: the
// reset jumped forward beyond a full extra window cycle, the snapshot is a
// fresh never-used window (zero usage, known length, reset one window-length
// after the observation), and the window kind matches the baseline. Garbage
// far-future timestamps, backwards jumps, and used windows all fail the
// signature and keep the conservative anomaly classification.
func externalResetRebaseApplies(baseline ProbeBaseline, snap QuotaSnapshot, now time.Time) bool {
	if baseline.Kind != ProbeBaselineReset || !snap.Valid || snap.ResetAt == nil || snap.Usage == nil || *snap.Usage != 0 {
		return false
	}
	forward := snap.ResetAt.After(baseline.ResetAt.Add(probeSkewTolerance))
	if !forward {
		return false
	}
	delta := snap.ResetAt.Sub(baseline.ResetAt)
	if baseline.WindowLength > 0 && delta <= 2*baseline.WindowLength {
		return false
	}
	if baseline.WindowLength == 0 && delta <= probeMaxPlausibleWindow {
		return false
	}
	if baseline.WindowKind != "" && snap.WindowKind != baseline.WindowKind {
		return false
	}
	length := baseline.WindowLength
	if snap.WindowLengthKnown && snap.WindowLength > 0 {
		length = snap.WindowLength
	}
	if length <= 0 {
		return false
	}
	strictWindow := QuotaWindow{Kind: snap.WindowKind, UsedPercent: snap.Usage, ResetAt: *snap.ResetAt}
	return looksLikeStrictLazyObservation(now, strictWindow, length)
}
