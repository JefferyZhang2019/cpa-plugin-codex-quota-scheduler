package main

import (
	"errors"
	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"
)

func TestProbeWALSendingRecoversVerifyFirstAndSuppresses(t *testing.T) {
	now := time.Unix(3000, 0).UTC()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)
	a := ProbeAttempt{Instance: 1, AttemptID: "a", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, SendFenceSeq: 7, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second)}
	if _, err := store.Update(func(state *PersistentState) error { state.ProbeAttempts[a.Instance] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := wal.PersistSending(a); err != nil {
		t.Fatal(err)
	}
	got := wal.Recover(now.Add(4 * time.Second))
	if len(got) != 1 || got[0].Class != OperationProbeVerify || got[0].StartedAfter != 7 {
		t.Fatalf("recovery=%v", got)
	}
	state, _ := store.PersistentSnapshot()
	if state.ProbeAttempts[1].Phase != ProbeAttemptSentUnknown || !state.ProbeAttempts[1].SuppressUntil.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("attempt=%#v", state.ProbeAttempts[1])
	}
}

func TestProbeWALTransitionsRequireExactAttemptAndPhase(t *testing.T) {
	now := time.Unix(3500, 0).UTC()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)
	attempt := ProbeAttempt{Instance: 1, AttemptID: "current", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, CreatedAt: now}
	if _, err := store.Update(func(state *PersistentState) error { state.ProbeAttempts[attempt.Instance] = attempt; return nil }); err != nil {
		t.Fatal(err)
	}

	stale := attempt
	stale.AttemptID = "stale"
	if err := wal.PersistSending(stale); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("mismatched Prepared->Sending err=%v", err)
	}
	if err := wal.PersistSending(attempt); err != nil {
		t.Fatal(err)
	}
	if err := wal.PersistSent(attempt.Instance, stale.AttemptID, now.Add(time.Second)); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("mismatched Sending->Sent err=%v", err)
	}
	if err := wal.PersistSent(attempt.Instance, attempt.AttemptID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := wal.PersistSentUnknown(attempt.Instance, stale.AttemptID, now.Add(10*time.Minute)); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("mismatched Sent->SentUnknown err=%v", err)
	}
	if err := wal.PersistSentUnknown(attempt.Instance, attempt.AttemptID, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := wal.Complete(attempt.Instance, stale.AttemptID, ProbeAttemptSentUnknown); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("mismatched completion err=%v", err)
	}
	if err := wal.Complete(attempt.Instance, attempt.AttemptID, ProbeAttemptPrepared); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("wrong-phase completion err=%v", err)
	}
	if err := wal.Complete(attempt.Instance, attempt.AttemptID, ProbeAttemptSentUnknown); err != nil {
		t.Fatal(err)
	}
	if err := wal.PersistSentUnknown(attempt.Instance, attempt.AttemptID, now.Add(20*time.Minute)); !errors.Is(err, ErrProbeAttemptChanged) {
		t.Fatalf("absent transition err=%v", err)
	}
	persisted, err := store.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeAttempts[attempt.Instance]; ok {
		t.Fatalf("rejected transition recreated attempt: %#v", persisted.ProbeAttempts[attempt.Instance])
	}
}

func TestS6KPointRegistryMatchesSource(t *testing.T) {
	raw, err := os.ReadFile("probe_wal.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`kpoint:(K_PROBE_[A-Z_]+)`)
	m := re.FindAllStringSubmatch(string(raw), -1)
	var got []string
	for _, x := range m {
		got = append(got, x[1])
	}
	sort.Strings(got)
	want := []string{"K_PROBE_AFTER_HTTP", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_SENDING_WRITE", "K_PROBE_SENT_WRITE"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("source=%v registry=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("source=%v registry=%v", got, want)
		}
	}
}

func TestProbeKPointsReachableAndRegistered(t *testing.T) {
	points := []string{"K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP", "K_PROBE_SENT_WRITE"}
	now := time.Unix(5000, 0).UTC()
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			registry := testsupport.NewKPointRegistry(points...)
			crash := testsupport.NewCrashController(registry, point)
			store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
			wal := NewProbeWAL(store, crash)
			a := ProbeAttempt{Instance: 1, AttemptID: "k", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, CreatedAt: now, VerifyNotBefore: now}
			if _, seedErr := store.Update(func(state *PersistentState) error { state.ProbeAttempts[a.Instance] = a; return nil }); seedErr != nil {
				t.Fatal(seedErr)
			}
			var err error
			switch point {
			case "K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING":
				err = wal.PersistSending(a)
			case "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP":
				err = wal.ExecuteSend(func() error { return nil })
			case "K_PROBE_SENT_WRITE":
				_ = NewProbeWAL(store).PersistSending(a)
				err = wal.PersistSent(1, a.AttemptID, now)
			}
			if !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestMockGroupAProbeRecovery(t *testing.T) {
	t.Run("sending", TestProbeWALSendingRecoversVerifyFirstAndSuppresses)
}

func TestProbeDeadlineConsumedOnceWithoutSpin(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(9, ProbeWindowFiveHour, ProbeWindow{State: ProbeRetryWait, Deadline: now})
	if got := c.Advance(9, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now}); len(got) != 1 {
		t.Fatalf("first intents=%d", len(got))
	}
	if got := c.Advance(9, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now}); len(got) != 0 {
		t.Fatalf("deadline replay spun: %d intents", len(got))
	}
	w, _ := c.Window(9, ProbeWindowFiveHour)
	if w.State != ProbePendingCheck {
		t.Fatalf("state=%s", w.State)
	}
	if !w.Deadline.IsZero() {
		t.Fatalf("consumed deadline retained: %v", w.Deadline)
	}
	if next := c.NextDeadline(); !next.IsZero() {
		t.Fatalf("pending check remained timer-visible: %v", next)
	}
}

func TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression(t *testing.T) {
	now := time.Unix(4000, 0).UTC()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)
	a := ProbeAttempt{Instance: 3, AttemptID: "jump", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptPrepared, SendFenceSeq: 11, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second), SuppressUntil: now.Add(10 * time.Minute)}
	if _, err := store.Update(func(state *PersistentState) error { state.ProbeAttempts[a.Instance] = a; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := wal.PersistSending(a); err != nil {
		t.Fatal(err)
	}
	if got := wal.Recover(now.Add(2 * time.Second)); len(got) != 0 {
		t.Fatalf("early recovery=%v", got)
	}
	got := wal.Recover(now.Add(3 * time.Second))
	if len(got) != 1 || got[0].Class == OperationProbeSend {
		t.Fatalf("recovery=%v", got)
	}
}

func TestProbeSuppressionEntrancesAndVerifyResults(t *testing.T) {
	now := time.Unix(8000, 0).UTC()
	suppress := now.Add(10 * time.Minute)
	for _, entrance := range []ProbeWindowState{ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeSentAwaitingVerify} {
		for _, result := range []ProbeClassificationKind{ProbeActivatedNew, ProbeStillLazy, ProbeAmbiguous} {
			c := NewProbeController(now)
			base := ResetProbeBaseline(now.Add(-time.Hour), 80, 0)
			snap := QuotaSnapshot{Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}
			if result == ProbeActivatedNew {
				snap.ResetAt = ptrTime(now.Add(time.Hour))
			} else if result == ProbeAmbiguous {
				snap.ResetAt = nil
			}
			c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: entrance, Baseline: base})
			c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: snap}})
			w, _ := c.Window(1, ProbeWindowFiveHour)
			if result == ProbeActivatedNew {
				if w.State != ProbeConfirmed {
					t.Fatalf("entrance=%s result=%s state=%s", entrance, result, w.State)
				}
			} else {
				if w.State != ProbeRetryWait {
					t.Fatalf("entrance=%s result=%s state=%s", entrance, result, w.State)
				}
				if w.Deadline.Before(suppress) {
					w.Deadline = suppress
				}
				if !w.Deadline.Equal(suppress) {
					t.Fatalf("suppression=%s", w.Deadline)
				}
			}
		}
	}
}
func TestProbeTimeJumpsProduceAtMostOneSequence(t *testing.T) {
	now := time.Unix(9000, 0).UTC()
	for _, jump := range []time.Duration{-time.Second, 0, 72 * time.Hour} {
		c := NewProbeController(now)
		c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset, Deadline: now})
		first := c.Advance(1, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now.Add(jump)})
		second := c.Advance(1, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now.Add(jump)})
		if len(first) > 1 || len(second) != 0 {
			t.Fatalf("jump=%s first=%d second=%d", jump, len(first), len(second))
		}
	}
}

func TestProbeWALRecoveryDeadlineTracksEarliestRecoverableAttempt(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)

	if _, ok := wal.RecoveryDeadline(); ok {
		t.Fatal("empty WAL reported a recovery deadline")
	}

	seed := map[AuthInstanceID]ProbeAttempt{
		1: {Instance: 1, AttemptID: "a-sent-unknown", Phase: ProbeAttemptSentUnknown, VerifyNotBefore: now.Add(3 * time.Second)},
		2: {Instance: 2, AttemptID: "b-sending", Phase: ProbeAttemptSending, VerifyNotBefore: now.Add(30 * time.Second)},
	}
	if _, err := store.Update(func(s *PersistentState) error {
		for id, a := range seed {
			s.ProbeAttempts[id] = a
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	deadline, ok := wal.RecoveryDeadline()
	if !ok {
		t.Fatal("recoverable attempts exist but no deadline reported")
	}
	if !deadline.Equal(now.Add(3 * time.Second)) {
		t.Fatalf("deadline = %s, want earliest VerifyNotBefore %s", deadline, now.Add(3*time.Second))
	}

	if _, err := store.Update(func(s *PersistentState) error {
		s.ProbeAttempts = map[AuthInstanceID]ProbeAttempt{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := wal.RecoveryDeadline(); ok {
		t.Fatal("deadline reported with no recoverable attempts")
	}
}
