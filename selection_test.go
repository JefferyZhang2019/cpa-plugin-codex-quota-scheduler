package main

import (
	"testing"
	"time"
)

// oracleClassify is a test-side decision table: hard exclusions are spec rows,
// then cache/exhaustion facts index a result table. It intentionally shares no
// production predicates or helper control flow.
func oracleClassify(a AccountView, now time.Time) AvailabilityClass {
	hardStops := []bool{a.AuthBlocked, a.Circuit == CircuitOpen, a.TemporaryUnavailable, a.Trial != TrialNone, a.Exhausted && a.ResetAt.After(now)}
	for _, stop := range hardStops {
		if stop {
			return Excluded
		}
	}
	type fact struct {
		cache            CacheClass
		exhausted, known bool
	}
	table := map[fact]AvailabilityClass{
		{CacheFresh, false, false}: Preferred, {CacheFresh, false, true}: Preferred, {CacheAging, false, false}: Preferred, {CacheAging, false, true}: Preferred,
		{CacheFresh, true, false}: Opportunistic, {CacheFresh, true, true}: Opportunistic, {CacheAging, true, false}: Opportunistic, {CacheAging, true, true}: Opportunistic,
		{CacheUnknown, false, false}: Opportunistic, {CacheUnknown, false, true}: Opportunistic, {CacheUnknown, true, false}: Opportunistic, {CacheUnknown, true, true}: Opportunistic,
		{CacheStale, false, true}: Opportunistic, {CacheStale, true, true}: Opportunistic,
	}
	if class, ok := table[fact{a.Cache, a.Exhausted, a.LastKnownAvailable}]; ok {
		return class
	}
	return Excluded
}

func oracleSelect(snapshot SchedulerSnapshot, candidates []Candidate, now time.Time) SelectionResult {
	candidate := map[string]bool{}
	for _, c := range candidates {
		if c.Provider == "codex" {
			candidate[c.ID] = true
		}
	}
	best := map[AvailabilityClass]*AccountView{}
	for i := range snapshot.Accounts {
		a := snapshot.Accounts[i]
		if !candidate[a.ID] {
			continue
		}
		if _, ok := snapshot.ActiveHighestTier[a.ID]; !ok {
			continue
		}
		class := oracleClassify(a, now)
		if class == Excluded {
			continue
		}
		current := best[class]
		if current == nil || oracleBefore(a, *current, snapshot.MonthlyMode) {
			copy := a
			best[class] = &copy
		}
	}
	for _, class := range []AvailabilityClass{Preferred, Opportunistic} {
		if a := best[class]; a != nil {
			result := SelectionResult{AuthID: a.ID, Instance: a.Instance, Class: class, Trial: class == Opportunistic, Reason: "selected"}
			if result.Trial {
				result.EvidenceSource = "trial_evidence"
			}
			return result
		}
	}
	return SelectionResult{Reason: "no_selectable_account", Fallback: snapshot.Fallback == FallbackFillFirst}
}

func oracleBefore(a, b AccountView, mode MonthlyMode) bool {
	if a.PluginPriority > b.PluginPriority {
		return true
	}
	if a.PluginPriority < b.PluginPriority {
		return false
	}
	if mode == MonthlyModePriority && a.Family != b.Family {
		return a.Family == AccountFamilyMonthly
	}
	if a.QuotaPressure != b.QuotaPressure {
		return a.QuotaPressure > b.QuotaPressure
	}
	if a.Expiry.IsZero() != b.Expiry.IsZero() {
		return !a.Expiry.IsZero()
	}
	if !a.Expiry.Equal(b.Expiry) {
		return a.Expiry.Before(b.Expiry)
	}
	if a.RemainingQuota != b.RemainingQuota {
		return a.RemainingQuota > b.RemainingQuota
	}
	return a.ID < b.ID
}
func TestMockGroupB(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	caches := []CacheClass{CacheFresh, CacheAging, CacheUnknown, CacheStale}
	exhausted := []struct {
		exhausted bool
		reset     time.Time
	}{
		{}, {true, now.Add(time.Hour)}, {true, now.Add(24 * time.Hour)}, {true, now.Add(7 * 24 * time.Hour)}, {true, now.Add(-time.Second)},
	}
	trials := []TrialState{TrialNone, TrialActive, TrialUnknown}
	count := 0
	for _, cache := range caches {
		for _, ex := range exhausted {
			for _, auth := range []bool{false, true} {
				for _, circuit := range []CircuitClass{CircuitClosed, CircuitOpen, CircuitHalfOpen} {
					for _, temp := range []bool{false, true} {
						for _, trial := range trials {
							for _, priority := range []int{0, 1} {
								a := AccountView{ID: "a", Instance: 1, Cache: cache, Exhausted: ex.exhausted, ResetAt: ex.reset, AuthBlocked: auth, Circuit: circuit, TemporaryUnavailable: temp, Trial: trial, PluginPriority: priority, LastKnownAvailable: true}
								if got, want := ClassifyAccount(a, now), oracleClassify(a, now); got != want {
									t.Fatalf("vector %d: got %v want %v: %#v", count, got, want, a)
								}
								count++
							}
						}
					}
				}
			}
		}
	}
	if count != 1440 {
		t.Fatalf("single-instance vectors=%d want 1440", count)
	}

	// Six representatives (three confidence classes x two plugin priorities),
	// exhaustively enumerated for N=1..3 and crossed with three candidate relations.
	reps := []AccountView{
		{ID: "p0", Instance: 1, Cache: CacheFresh}, {ID: "p1", Instance: 2, Cache: CacheFresh, PluginPriority: 1},
		{ID: "o0", Instance: 3, Cache: CacheUnknown}, {ID: "o1", Instance: 4, Cache: CacheUnknown, PluginPriority: 1},
		{ID: "x0", Instance: 5, Cache: CacheFresh, AuthBlocked: true}, {ID: "x1", Instance: 6, Cache: CacheFresh, AuthBlocked: true, PluginPriority: 1},
	}
	multi := 0
	for n := 1; n <= 3; n++ {
		total := 1
		for range n {
			total *= len(reps)
		}
		for code := 0; code < total; code++ {
			accounts := make([]AccountView, n)
			active := map[string]struct{}{}
			candidates := make([]Candidate, 0, n)
			x := code
			for i := 0; i < n; i++ {
				accounts[i] = reps[x%len(reps)]
				accounts[i].ID += string(rune('a' + i))
				active[accounts[i].ID] = struct{}{}
				candidates = append(candidates, Candidate{ID: accounts[i].ID, Provider: "codex"})
				x /= len(reps)
			}
			for relation := 0; relation < 3; relation++ {
				cs := append([]Candidate(nil), candidates...)
				if relation == 0 {
					cs = append(cs, Candidate{ID: "outside", Provider: "codex"})
				}
				if relation == 1 && len(cs) > 0 {
					cs = cs[:len(cs)-1]
				}
				if relation == 2 {
					cs = []Candidate{{ID: "outside", Provider: "codex"}}
				}
				snapshot := SchedulerSnapshot{Accounts: accounts, ActiveHighestTier: active, MonthlyMode: MonthlyModeExpiryOrder, Fallback: FallbackFillFirst}
				got, want := SelectAccount(snapshot, cs, now), oracleSelect(snapshot, cs, now)
				if got.AuthID != want.AuthID || got.Class != want.Class || got.Trial != want.Trial || got.Fallback != want.Fallback || got.EvidenceSource != want.EvidenceSource || got.Reason != want.Reason {
					t.Fatalf("n=%d code=%d relation=%d got=%#v want=%#v", n, code, relation, got, want)
				}
				multi++
			}
		}
	}
	if multi != 774 {
		t.Fatalf("multi-instance comparisons=%d want 774", multi)
	}
}

func TestSchedulingOracleDetectsPriorityBeforeClassMutation(t *testing.T) {
	now := time.Now()
	snapshot := SchedulerSnapshot{Accounts: []AccountView{{ID: "op", Instance: 1, Cache: CacheUnknown, PluginPriority: 9}, {ID: "preferred", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"op": {}, "preferred": {}}}
	cs := []Candidate{{ID: "op", Provider: "codex"}, {ID: "preferred", Provider: "codex"}}
	want := oracleSelect(snapshot, cs, now)
	mutant := "op"
	if want.AuthID == mutant {
		t.Fatal("oracle failed to kill priority-before-class mutant")
	}
}
func TestPreferredAcrossAllPrioritiesBeforeOpportunistic(t *testing.T) {
	now := time.Now()
	s := SchedulerSnapshot{Accounts: []AccountView{{ID: "op-high", Instance: 1, Cache: CacheUnknown, PluginPriority: 99}, {ID: "preferred-low", Instance: 2, Cache: CacheFresh, PluginPriority: 0}}, ActiveHighestTier: map[string]struct{}{"op-high": {}, "preferred-low": {}}}
	got := SelectAccount(s, []Candidate{{ID: "op-high", Provider: "codex"}, {ID: "preferred-low", Provider: "codex"}}, now)
	if got.AuthID != "preferred-low" {
		t.Fatalf("selected %q", got.AuthID)
	}
}

func TestSuiteScheduling(t *testing.T) {
	t.Run("mock group B", TestMockGroupB)
	t.Run("class before priority", TestPreferredAcrossAllPrioritiesBeforeOpportunistic)
	t.Run("trial CAS", TestTrialRegistryCASAndEvidence)
	t.Run("trial budget", TestTrialPendingAtSixtySecondsAndBudget)
	t.Run("real ABI snapshot", TestSchedulerPickABIPathSnapshotOnly)
	t.Run("oracle mutant", TestSchedulingOracleDetectsPriorityBeforeClassMutation)
	t.Run("trial expiry", TestTrialUnknownExpiresIntoEligibility)
	t.Run("trial backoff", TestTrialBackoffSequence)
	t.Run("trial retries", TestTrialThreeRetriesForceUnknown)
	t.Run("pending evidence race", TestMarkPendingAfterEvidenceDoesNotRecreateTrial)
	t.Run("production evidence", TestProductionEvidenceQueueAndDynamicTrial)
	t.Run("evidence queue", TestEvidenceConsumerMarksPendingAndQueueFullIsNonblocking)
	t.Run("immutable publication", TestSchedulerPickObservesOneImmutablePublication)
	t.Run("request success evidence", TestUsageSuccessHandlerClearsTrialButProbeSuccessDoesNot)
	t.Run("quota limit republish", TestQuotaLimitFeedbackRepublishesExhaustionWithoutRosterMutation)
}
