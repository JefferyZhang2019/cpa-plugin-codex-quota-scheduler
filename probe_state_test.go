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
	longBase := ResetProbeBaseline(now.Add(-time.Hour), 0, 7*24*time.Hour)
	longBase.WindowKind = WindowWeekly
	longBase.SuspectedLazy = true
	c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: longBase})
	ints := c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(4 * time.Hour)), Usage: ptrFloat(10)},
		ProbeWindowLong:     {Valid: true, ResetAt: ptrTime(now.Add(7 * 24 * time.Hour)), Usage: ptrFloat(0), WindowKind: WindowWeekly, WindowLength: 7 * 24 * time.Hour, WindowLengthKnown: true},
	}})
	five, _ := c.Window(1, ProbeWindowFiveHour)
	long, _ := c.Window(1, ProbeWindowLong)
	if five.State != ProbeConfirmed || long.State != ProbeSentAwaitingVerify {
		t.Fatalf("five=%s long=%s intents=%v", five.State, long.State, ints)
	}
}

func TestKnownResetDeadlineIncludesBoundedObservation(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		reset time.Time
		want  time.Time
	}{
		{name: "observation wins", reset: now.Add(5 * time.Hour), want: now.Add(30 * time.Minute)},
		{name: "reset maturity wins", reset: now.Add(10 * time.Minute), want: now.Add(11 * time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewProbeController(now)
			c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{
				State:    ProbePendingCheck,
				Baseline: ResetProbeBaseline(tt.reset, 20, 5*time.Hour),
			})
			c.Advance(1, ProbeEvent{
				Kind:                ProbeEventPrecheckResult,
				Window:              ProbeWindowFiveHour,
				Now:                 now,
				ObservationInterval: 30 * time.Minute,
				Snapshots: map[ProbeWindowKind]QuotaSnapshot{
					ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(tt.reset), Usage: ptrFloat(20)},
				},
			})
			window, ok := c.Window(1, ProbeWindowFiveHour)
			if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(tt.want) {
				t.Fatalf("window = %#v, ok=%v; want WaitingReset deadline %s", window, ok, tt.want)
			}
		})
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

func TestProbeControllerUsageOnlyZeroWithoutResetDoesNotSend(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(2, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: UsageOnlyProbeBaseline(0, now)})

	intents := c.Advance(2, ProbeEvent{Kind: ProbeEventPrecheckResult, Window: ProbeWindowLong, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowLong: {Valid: true, Usage: ptrFloat(0)},
	}})
	for _, intent := range intents {
		if intent.Class == OperationProbeSend {
			t.Fatalf("zero usage without reset emitted ProbeSend: %v", intents)
		}
	}
	window, ok := c.Window(2, ProbeWindowLong)
	if !ok || window.State != ProbeWaitingReset || !window.Deadline.Equal(now.Add(probeUnknownResetRecheck)) {
		t.Fatalf("window = %#v, ok=%v; want safely rescheduled WaitingReset", window, ok)
	}
}

func TestProbeRosterConfirmedRestoresSuspectedLazyToPendingCheck(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	base := ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)
	base.SuspectedLazy = true
	c := NewProbeController(now)
	c.SetWindow(3, ProbeWindowLong, ProbeWindow{State: ProbeWaitingRoster, Baseline: base})

	c.Advance(3, ProbeEvent{Kind: ProbeEventRosterConfirmed, Window: ProbeWindowLong, Now: now})

	window, ok := c.Window(3, ProbeWindowLong)
	if !ok || window.State != ProbePendingCheck || !window.Deadline.IsZero() {
		t.Fatalf("window = %#v, ok=%v; want PendingCheck with no deadline", window, ok)
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
			longBase := ResetProbeBaseline(now.Add(-time.Hour), 0, 7*24*time.Hour)
			longBase.WindowKind = WindowWeekly
			longBase.SuspectedLazy = true
			c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: left, Baseline: base, Deadline: now})
			c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: right, Baseline: longBase, Deadline: now})
			c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}, ProbeWindowLong: {Valid: true, ResetAt: ptrTime(now.Add(7 * 24 * time.Hour)), Usage: ptrFloat(0), WindowKind: WindowWeekly, WindowLength: 7 * 24 * time.Hour, WindowLengthKnown: true}}})
			five, _ := c.Window(1, ProbeWindowFiveHour)
			long, _ := c.Window(1, ProbeWindowLong)
			wantFive := left
			if left == ProbePendingCheck {
				wantFive = ProbeWaitingReset
			}
			wantLong := right
			if right == ProbePendingCheck {
				wantLong = ProbeSentAwaitingVerify
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
			return ProbeWaitingReset, true, 0
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
func TestSuiteProbe(t *testing.T) {
	t.Run("state", TestProbeControllerPersistentStateSetAndIllegalNoop)
	t.Run("dormant", TestProbeControllerDormantDeadlineStillEmitsProbe)
}

func TestVerifyExternalResetRebasesInsteadOfAnomalyHold(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	c := NewProbeController(now)
	// Baseline reset deadline was 2 days ago; the server minted a fresh
	// never-used five-hour window while our probe was in flight (reset now+5h,
	// zero usage, known length) — a multi-cycle forward jump.
	base := ResetProbeBaseline(now.Add(-48*time.Hour), 30, 5*time.Hour)
	base.WindowKind = WindowFiveHour
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: base, AttemptID: "probe-1"})
	freshReset := now.Add(5 * time.Hour)
	length := 5 * time.Hour
	c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: ProbeWindowFiveHour, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, WindowKind: WindowFiveHour, ResetAt: &freshReset, Usage: ptrFloat(0), WindowLength: length, WindowLengthKnown: true},
	}})
	window, _ := c.Window(1, ProbeWindowFiveHour)
	if window.State == ProbeAnomalyHold {
		t.Fatalf("external compensating reset landed in AnomalyHold: %#v", window)
	}
	if window.State != ProbeRetryWait {
		t.Fatalf("State = %s, want ProbeRetryWait to retry activation on the next precheck", window.State)
	}
	if !window.Baseline.ResetAt.Equal(freshReset) {
		t.Fatalf("baseline ResetAt = %s, want rebased to %s", window.Baseline.ResetAt, freshReset)
	}
	if !window.Baseline.SuspectedLazy {
		t.Fatal("rebased baseline should be SuspectedLazy so the next precheck re-authorizes")
	}
}

func TestVerifyKeepsAnomalyForGarbageForwardJump(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	c := NewProbeController(now)
	base := ResetProbeBaseline(now.Add(-time.Hour), 30, 5*time.Hour)
	base.WindowKind = WindowFiveHour
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: base, AttemptID: "probe-1"})
	// 15-day forward reset with zero usage: not a fresh-window signature.
	garbageReset := now.Add(15 * 24 * time.Hour)
	length := 5 * time.Hour
	c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: ProbeWindowFiveHour, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, WindowKind: WindowFiveHour, ResetAt: &garbageReset, Usage: ptrFloat(0), WindowLength: length, WindowLengthKnown: true},
	}})
	window, _ := c.Window(1, ProbeWindowFiveHour)
	if window.State != ProbeAnomalyHold {
		t.Fatalf("State = %s, want ProbeAnomalyHold for a garbage far-future reset", window.State)
	}
}

func TestVerifyKeepsAnomalyForUsedWindowForwardJump(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	c := NewProbeController(now)
	base := ResetProbeBaseline(now.Add(-2*5*time.Hour), 30, 5*time.Hour)
	base.WindowKind = WindowFiveHour
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown, Baseline: base, AttemptID: "probe-1"})
	// Multi-cycle forward jump but the window is used: not a fresh window.
	usedReset := now.Add(5 * time.Hour)
	length := 5 * time.Hour
	c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: ProbeWindowFiveHour, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, WindowKind: WindowFiveHour, ResetAt: &usedReset, Usage: ptrFloat(40), WindowLength: length, WindowLengthKnown: true},
	}})
	window, _ := c.Window(1, ProbeWindowFiveHour)
	if window.State != ProbeAnomalyHold {
		t.Fatalf("State = %s, want ProbeAnomalyHold for a used-window multi-cycle jump", window.State)
	}
}
