package main

import (
	"reflect"
	"testing"
	"time"
)

func TestProbeControllerPersistentStateSetAndIllegalNoop(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeIdle})
	before, _ := c.Window(1, ProbeWindowFiveHour)
	if intents := c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: ProbeWindowFiveHour, Now: now}); len(intents) != 0 {
		t.Fatalf("illegal intents=%v", intents)
	}
	after, _ := c.Window(1, ProbeWindowFiveHour)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("illegal mutation: %#v -> %#v", before, after)
	}
}

func TestProbeControllerDualWindowIndependent(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)})
	c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 60, 7*24*time.Hour)})
	ints := c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(4 * time.Hour)), Usage: ptrFloat(0)},
		ProbeWindowLong:     {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(60)},
	}})
	five, _ := c.Window(1, ProbeWindowFiveHour)
	long, _ := c.Window(1, ProbeWindowLong)
	if five.State != ProbeConfirmed || long.State != ProbeSentAwaitingVerify {
		t.Fatalf("five=%s long=%s intents=%v", five.State, long.State, ints)
	}
}

func TestProbeControllerDormantDeadlineStillEmitsProbe(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(2, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset, Deadline: now})
	ints := c.Advance(2, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now, RefreshMode: RefreshModeDormant})
	if len(ints) != 1 || ints[0].Class != OperationProbePrecheck {
		t.Fatalf("intents=%v", ints)
	}
}

func TestKnownResetDeadlineIncludesExternalResetObservation(t *testing.T) {
	now := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	base := ResetProbeBaseline(now.Add(5*time.Hour), 40, 5*time.Hour)
	if got := deadlineFor(base, now, 30*time.Minute); !got.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("known reset observation deadline = %s", got)
	}
	if got := deadlineFor(base, now, 0); !got.Equal(base.ResetAt.Add(probeRefreshAfterResetDelay)) {
		t.Fatalf("disabled observation deadline = %s", got)
	}
	closeReset := ResetProbeBaseline(now.Add(10*time.Minute), 40, 5*time.Hour)
	if got := deadlineFor(closeReset, now, 30*time.Minute); !got.Equal(closeReset.ResetAt.Add(probeRefreshAfterResetDelay)) {
		t.Fatalf("mature reset deadline = %s", got)
	}
}

func TestUnchangedWindowObservationReschedulesWithoutProbe(t *testing.T) {
	now := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	usage := 40.0
	controller := NewProbeController(now)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(reset, usage, 5*time.Hour)})
	intents := controller.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, ObservationInterval: 30 * time.Minute, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: &reset, Usage: &usage},
	}})
	window, _ := controller.Window(1, ProbeWindowFiveHour)
	if len(intents) != 0 || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("unchanged observation scheduled probe: window=%#v intents=%#v", window, intents)
	}
}

func TestMockGroupC(t *testing.T) {
	t.Run("classifier", TestProbeClassifierOrderedRules)
	t.Run("dual", TestProbeControllerDualWindowIndependent)
}

func TestProbeAllStateEventsAndDualWindowProduct(t *testing.T) {
	states := []ProbeWindowState{ProbeIdle, ProbeWaitingReset, ProbePendingCheck, ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeRetryWait, ProbeConfirmed, ProbeAuthBlocked, ProbeAnomalyHold, ProbeWaitingRoster}
	events := []ProbeEventKind{ProbeEventDeadline, ProbeEventPrecheckResult, ProbeEventVerifyResult, ProbeEventAuthFailed, ProbeEventExternalLogin, ProbeEventRosterConfirmed, ProbeEventInstanceRemoved}
	now := time.Unix(9000, 0).UTC()
	for _, left := range states {
		for _, right := range states {
			c := NewProbeController(now)
			base := ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)
			c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: left, Baseline: base, Deadline: now})
			c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: right, Baseline: base, Deadline: now})
			c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}, ProbeWindowLong: {Valid: true, ResetAt: ptrTime(now.Add(time.Hour)), Usage: ptrFloat(0)}}})
			five, _ := c.Window(1, ProbeWindowFiveHour)
			long, _ := c.Window(1, ProbeWindowLong)
			wantFive := left
			if left == ProbePendingCheck {
				wantFive = ProbeSentAwaitingVerify
			}
			wantLong := right
			if right == ProbePendingCheck {
				wantLong = ProbeConfirmed
			}
			if five.State != wantFive || long.State != wantLong {
				t.Fatalf("%s x %s -> %s/%s want %s/%s", left, right, five.State, long.State, wantFive, wantLong)
			}
		}
	}
	for _, s := range states {
		for _, e := range events {
			c := NewProbeController(now)
			c.SetWindow(2, ProbeWindowFiveHour, ProbeWindow{State: s, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 0), Deadline: now})
			intents := c.Advance(2, ProbeEvent{Kind: e, Window: ProbeWindowFiveHour, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}}})
			w, ok := c.Window(2, ProbeWindowFiveHour)
			want, exists, wantIntents := probeTransitionOracle(s, e)
			if ok != exists || (ok && w.State != want) || len(intents) != wantIntents {
				t.Fatalf("state=%s event=%s -> %#v ok=%v intents=%d want state=%s exists=%v intents=%d", s, e, w, ok, len(intents), want, exists, wantIntents)
			}
		}
	}
}
func probeTransitionOracle(s ProbeWindowState, e ProbeEventKind) (ProbeWindowState, bool, int) {
	if e == ProbeEventInstanceRemoved {
		return "", false, 0
	}
	switch e {
	case ProbeEventDeadline:
		if s == ProbeWaitingReset || s == ProbeRetryWait || s == ProbeAnomalyHold {
			return ProbePendingCheck, true, 1
		}
	case ProbeEventPrecheckResult:
		if s == ProbePendingCheck {
			return ProbeSentAwaitingVerify, true, 1
		}
	case ProbeEventVerifyResult:
		if s == ProbeSentAwaitingVerify || s == ProbeSentUnknown {
			return ProbeRetryWait, true, 0
		}
	case ProbeEventAuthFailed:
		if s != ProbeIdle {
			return ProbeAuthBlocked, true, 0
		}
	case ProbeEventExternalLogin:
		if s == ProbeAuthBlocked {
			return ProbePendingCheck, true, 0
		}
	case ProbeEventRosterConfirmed:
		if s == ProbeWaitingRoster {
			return ProbeWaitingReset, true, 0
		}
	}
	return s, true, 0
}
func validProbeState(s ProbeWindowState) bool {
	switch s {
	case ProbeIdle, ProbeWaitingReset, ProbePendingCheck, ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeRetryWait, ProbeConfirmed, ProbeAuthBlocked, ProbeAnomalyHold, ProbeWaitingRoster:
		return true
	}
	return false
}
func TestFreshUnusedLazyResetPrecheckSendsProbe(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	controller := NewProbeController(now)
	reset := now.Add(5 * time.Hour)
	base := ResetProbeBaseline(reset, 0, 5*time.Hour)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: base})
	usage := 0.0
	intents := controller.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: &reset, Usage: &usage},
	}})
	window, _ := controller.Window(1, ProbeWindowFiveHour)
	if window.State != ProbeSentAwaitingVerify || len(intents) != 1 || intents[0].Class != OperationProbeSend {
		t.Fatalf("sliding unused reset was not probed: window=%#v intents=%#v", window, intents)
	}
}

func TestExternalResetObservationSendsProbe(t *testing.T) {
	now := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	oldReset := now.Add(4 * time.Hour)
	newReset := now.Add(5 * time.Hour)
	zero := 0.0
	controller := NewProbeController(now)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(oldReset, 65, 5*time.Hour)})
	intents := controller.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, ObservationInterval: 30 * time.Minute, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: &newReset, Usage: &zero},
	}})
	window, _ := controller.Window(1, ProbeWindowFiveHour)
	if window.State != ProbeSentAwaitingVerify || !window.Baseline.ResetAt.Equal(newReset) || len(intents) != 1 || intents[0].Class != OperationProbeSend {
		t.Fatalf("external lazy reset was not probed: window=%#v intents=%#v", window, intents)
	}
}

func TestUnusedLazyResetShiftVerifyConverges(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	controller := NewProbeController(now)
	base := ResetProbeBaseline(now.Add(-time.Minute), 0, 5*time.Hour)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentAwaitingVerify, Baseline: base})
	reset := now.Add(5 * time.Hour)
	usage := 0.0
	controller.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: &reset, Usage: &usage},
	}})
	window, _ := controller.Window(1, ProbeWindowFiveHour)
	if window.State != ProbeConfirmed {
		t.Fatalf("post-probe reset did not converge: %#v", window)
	}
}

func TestSuiteProbe(t *testing.T) {
	t.Run("state", TestProbeControllerPersistentStateSetAndIllegalNoop)
	t.Run("dormant", TestProbeControllerDormantDeadlineStillEmitsProbe)
}
