package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSection12A07LegacyManualQuotaConcurrent(t *testing.T) {
	runSection12A07LegacyManualQuotaConcurrent(t)
}

func TestSection12B03ConcurrentPicksOneTrial(t *testing.T) {
	runSection12B03ConcurrentPicksOneTrial(t)
}

func TestSection12B04SeededPropertyAndShrink(t *testing.T) {
	runSection12B04SeededPropertyAndShrink(t)
}

func TestSection12C04AllProbePathsAtEveryKPoint(t *testing.T) {
	runSection12C04AllProbePathsAtEveryKPoint(t)
}

func TestSection12D01CredentialIdentityMatrix(t *testing.T) {
	runSection12D01CredentialIdentityMatrix(t)
}

func TestSection12E01RestrictedEventInterleavings(t *testing.T) {
	runSection12E01RestrictedEventInterleavings(t)
}

func TestInvariant12FirstRealRequestSingleRefresh(t *testing.T) {
	runInvariant12FirstRealRequestSingleRefresh(t)
}

func TestInvariant12ConcurrentRequestsRejectDuplicateRefresh(t *testing.T) {
	runInvariant12ConcurrentRequestsRejectDuplicateRefresh(t)
}

func runSection12A07LegacyManualQuotaConcurrent(t *testing.T) {
	legacyStarted := make(chan struct{})
	releaseLegacy := make(chan struct{})
	var quotaStarts atomic.Int64
	c := NewCoordinator(CoordinatorOptions{AllocateReadSeq: func() (uint64, error) { return 77, nil }, Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		if intent.Class == OperationLegacyRefresh {
			close(legacyStarted)
			<-releaseLegacy
		} else if intent.Class == OperationQuotaRead {
			quotaStarts.Add(1)
		}
		return OperationResult{Token: intent.Token, ReadStartSeq: intent.ReadStartSeq}
	}})
	t.Cleanup(c.Close)
	legacy := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, Token: ExecutionToken{Instance: 1, Tier: 1}})
	<-legacyStarted
	manual := c.SubmitTyped(Intent{Instance: 1, Generation: 1, Class: OperationQuotaRead, Source: SourceManualRefresh})
	quota := c.SubmitTyped(Intent{Instance: 1, Generation: 1, Class: OperationQuotaRead, Source: SourceSchedulerInterval})
	if quotaStarts.Load() != 0 {
		t.Fatal("manual/quota intent entered the legacy envelope")
	}
	close(releaseLegacy)
	if got := legacy.Await(context.Background()); got.Disposition != ResultApplied {
		t.Fatalf("legacy disposition=%q", got.Disposition)
	}
	manualResult, quotaResult := manual.Await(context.Background()), quota.Await(context.Background())
	if manualResult.Disposition != ResultApplied || quotaResult.Disposition != ResultApplied || manualResult.ReadStartSeq != 77 || quotaResult.ReadStartSeq != 77 {
		t.Fatalf("manual=%#v quota=%#v", manualResult, quotaResult)
	}
	if quotaStarts.Load() != 1 {
		t.Fatalf("deduplicated quota starts=%d, want 1", quotaStarts.Load())
	}
}

func concurrentSnapshotPicks(t *testing.T, n int) (picked int, intents int, state TrialState) {
	t.Helper()
	now := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	trials := NewTrialRegistry()
	evidence := make(chan EvidenceIntent, n)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Trials: trials, EvidenceIntents: evidence, Accounts: []AccountView{{ID: "unknown", Instance: 71, Cache: CacheUnknown}}, ActiveHighestTier: map[string]struct{}{"unknown": {}}})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unknown", Provider: "codex"}}}
	start := make(chan struct{})
	results := make(chan PickDecision, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- schedulerPickPublished(req, now)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		if result.AuthID == "unknown" {
			picked++
		}
	}
	return picked, len(evidence), trials.State(71, now)
}

func runSection12B03ConcurrentPicksOneTrial(t *testing.T) {
	for n := 2; n <= 4; n++ {
		t.Run(fmt.Sprintf("pickers-%d", n), func(t *testing.T) {
			picked, intents, state := concurrentSnapshotPicks(t, n)
			if picked != 1 || intents != 1 || state != TrialActive {
				t.Fatalf("picked=%d intents=%d trial=%v", picked, intents, state)
			}
		})
	}
}

func randomSelectionVector(rng *rand.Rand, index int, now time.Time) (SchedulerSnapshot, []Candidate) {
	count := 1 + rng.Intn(4)
	accounts := make([]AccountView, count)
	active := make(map[string]struct{}, count)
	candidates := make([]Candidate, 0, count+1)
	for i := range accounts {
		id := fmt.Sprintf("a-%d-%d", index, i)
		accounts[i] = AccountView{ID: id, Instance: AuthInstanceID(i + 1), Cache: CacheClass(rng.Intn(4)), Exhausted: rng.Intn(2) == 0, ResetAt: now.Add(time.Duration(rng.Intn(3)-1) * time.Hour), AuthBlocked: rng.Intn(5) == 0, Circuit: CircuitClass(rng.Intn(3)), TemporaryUnavailable: rng.Intn(5) == 0, PluginPriority: rng.Intn(3), LastKnownAvailable: rng.Intn(2) == 0, RemainingQuota: float64(rng.Intn(101))}
		active[id] = struct{}{}
		if rng.Intn(4) != 0 {
			candidates = append(candidates, Candidate{ID: id, Provider: "codex"})
		}
	}
	if rng.Intn(2) == 0 {
		candidates = append(candidates, Candidate{ID: "outside", Provider: "codex"})
	}
	return SchedulerSnapshot{Accounts: accounts, ActiveHighestTier: active, MonthlyMode: MonthlyModeExpiryOrder, Fallback: FallbackFillFirst}, candidates
}

func selectionMismatch(snapshot SchedulerSnapshot, candidates []Candidate, now time.Time) bool {
	got, want := SelectAccount(snapshot, candidates, now), oracleSelect(snapshot, candidates, now)
	return got.AuthID != want.AuthID || got.Class != want.Class || got.Trial != want.Trial || got.Fallback != want.Fallback || got.Reason != want.Reason
}

func shrinkSelectionCase(snapshot SchedulerSnapshot, candidates []Candidate, now time.Time, mismatch func(SchedulerSnapshot, []Candidate, time.Time) bool) (SchedulerSnapshot, []Candidate) {
	for changed := true; changed; {
		changed = false
		for i := range snapshot.Accounts {
			candidate := snapshot
			candidate.Accounts = append(append([]AccountView(nil), snapshot.Accounts[:i]...), snapshot.Accounts[i+1:]...)
			if len(candidate.Accounts) > 0 && mismatch(candidate, candidates, now) {
				snapshot, changed = candidate, true
				break
			}
		}
	}
	return snapshot, candidates
}

func runSection12B04SeededPropertyAndShrink(t *testing.T) {
	const seed int64 = 0x5EED1204
	now := time.Date(2026, 7, 14, 2, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 512; i++ {
		snapshot, candidates := randomSelectionVector(rng, i, now)
		if selectionMismatch(snapshot, candidates, now) {
			minimal, minimalCandidates := shrinkSelectionCase(snapshot, candidates, now, selectionMismatch)
			t.Fatalf("seed=%d row=%d minimal_accounts=%#v candidates=%#v", seed, i, minimal.Accounts, minimalCandidates)
		}
	}
	regression := SchedulerSnapshot{Accounts: []AccountView{{ID: "z", Instance: 1, Cache: CacheFresh}, {ID: "a", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"z": {}, "a": {}}, Fallback: FallbackFillFirst}
	candidates := []Candidate{{ID: "z", Provider: "codex"}, {ID: "a", Provider: "codex"}}
	mutantMismatch := func(snapshot SchedulerSnapshot, candidates []Candidate, now time.Time) bool {
		want := oracleSelect(snapshot, candidates, now)
		mutant := SelectAccount(snapshot, candidates, now)
		if len(snapshot.Accounts) > 1 {
			mutant.AuthID = snapshot.Accounts[0].ID // stable regression: wrong tie direction
		}
		return mutant.AuthID != want.AuthID
	}
	if !mutantMismatch(regression, candidates, now) {
		t.Fatal("seeded regression mutant did not fail")
	}
	minimal, _ := shrinkSelectionCase(regression, candidates, now, mutantMismatch)
	if len(minimal.Accounts) != 2 || minimal.Accounts[0].ID != "z" || minimal.Accounts[1].ID != "a" {
		t.Fatalf("seeded shrink regression changed: %#v", minimal.Accounts)
	}
}

func runInvariant12FirstRealRequestSingleRefresh(t *testing.T) {
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	trials := NewTrialRegistry()
	evidence := make(chan EvidenceIntent, 4)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Trials: trials, EvidenceIntents: evidence, Accounts: []AccountView{{ID: "unknown", Instance: 81, Cache: CacheUnknown}, {ID: "stale", Instance: 82, Cache: CacheStale, LastKnownAvailable: true}}, ActiveHighestTier: map[string]struct{}{"unknown": {}, "stale": {}}})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unknown", Provider: "codex"}, {ID: "stale", Provider: "codex"}}}
	decision := schedulerPickPublished(req, now)
	if !decision.Handled || decision.AuthID == "" || len(evidence) != 1 {
		t.Fatalf("decision=%#v evidence=%d", decision, len(evidence))
	}
}

func runInvariant12ConcurrentRequestsRejectDuplicateRefresh(t *testing.T) {
	picked, intents, _ := concurrentSnapshotPicks(t, 4)
	if picked != 1 || intents != 1 {
		t.Fatalf("picked=%d intents=%d", picked, intents)
	}
}

func runSection12C04AllProbePathsAtEveryKPoint(t *testing.T) {
	points := []string{"K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP", "K_PROBE_SENT_WRITE"}
	paths := []struct {
		name   string
		state  ProbeWindowState
		window ProbeWindowKind
	}{
		{name: "waiting-reset/five-hour", state: ProbeWaitingReset, window: ProbeWindowFiveHour},
		{name: "retry-wait/five-hour", state: ProbeRetryWait, window: ProbeWindowFiveHour},
		{name: "anomaly-hold/five-hour", state: ProbeAnomalyHold, window: ProbeWindowFiveHour},
		{name: "waiting-reset/long", state: ProbeWaitingReset, window: ProbeWindowLong},
		{name: "retry-wait/long", state: ProbeRetryWait, window: ProbeWindowLong},
		{name: "anomaly-hold/long", state: ProbeAnomalyHold, window: ProbeWindowLong},
	}
	now := time.Date(2026, 7, 14, 4, 0, 0, 0, time.UTC)
	for _, path := range paths {
		for _, point := range points {
			t.Run(path.name+"/"+point, func(t *testing.T) {
				controller := NewProbeController(now)
				windowLength := 5 * time.Hour
				windowKind := WindowFiveHour
				if path.window == ProbeWindowLong {
					windowLength = 7 * 24 * time.Hour
					windowKind = WindowWeekly
				}
				baseline := ResetProbeBaseline(now.Add(-time.Hour), 0, windowLength)
				baseline.WindowKind = windowKind
				baseline.SuspectedLazy = true
				controller.SetWindow(1, path.window, ProbeWindow{State: path.state, Baseline: baseline, Deadline: now})
				precheck := controller.Advance(1, ProbeEvent{Kind: ProbeEventDeadline, Window: path.window, Now: now})
				if len(precheck) != 1 || precheck[0].Class != OperationProbePrecheck {
					t.Fatalf("path=%s precheck=%#v", path.name, precheck)
				}
				send := controller.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Window: path.window, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{path.window: {Valid: true, ResetAt: ptrTime(now.Add(windowLength)), Usage: ptrFloat(0), WindowKind: windowKind, WindowLength: windowLength, WindowLengthKnown: true}}})
				if len(send) != 1 || send[0].Class != OperationProbeSend {
					t.Fatalf("path=%s send=%#v", path.name, send)
				}
				windows, ok := send[0].Payload.([]ProbeWindowKind)
				if !ok || len(windows) != 1 || windows[0] != path.window {
					t.Fatalf("path=%s windows=%#v", path.name, send[0].Payload)
				}
				registry := testsupport.NewKPointRegistry(points...)
				storePath := filepath.Join(t.TempDir(), "probe.json")
				store := NewStateStore(storePath, OSFileHooks(), nil)
				attempt := ProbeAttempt{Instance: 1, AttemptID: path.name, Windows: windows, Phase: ProbeAttemptPrepared, SendFenceSeq: 9, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second)}
				if _, err := store.Update(func(state *PersistentState) error {
					state.ProbeWindows = controller.Snapshot()
					state.ProbeAttempts[1] = attempt
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				wal := NewProbeWAL(store, testsupport.NewCrashController(registry, point))
				err := wal.PersistSending(attempt)
				if err == nil {
					err = wal.ExecuteSend(func() error { return nil })
				}
				if err == nil {
					err = wal.PersistSent(1, attempt.AttemptID, now.Add(time.Second))
				}
				if !errors.Is(err, testsupport.ErrInjectedCrash) {
					t.Fatalf("crash=%s err=%v", point, err)
				}
				restartStore := NewStateStore(storePath, OSFileHooks(), nil)
				restart := NewProbeWAL(restartStore)
				durableBefore, loadErr := restartStore.PersistentSnapshot()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				restartedController := NewProbeController(now.Add(4 * time.Second))
				restartedController.Load(durableBefore.ProbeWindows)
				runtime := &QuotaRefresher{runtimeStore: restartStore, probeController: restartedController, now: func() time.Time { return now.Add(4 * time.Second) }}
				if err := runtime.recoverPreparedProbeAttempts(); err != nil {
					t.Fatal(err)
				}
				intents, recoverErr := restart.RecoverChecked(now.Add(4 * time.Second))
				if recoverErr != nil {
					t.Fatal(recoverErr)
				}
				if point == "K_PROBE_SENDING_WRITE" {
					if len(intents) != 0 {
						t.Fatalf("pre-WAL crash recovered intents=%#v", intents)
					}
					state, stateErr := restartStore.PersistentSnapshot()
					persistedWindow := state.ProbeWindows[1][path.window]
					if stateErr != nil || len(state.ProbeAttempts) != 0 || persistedWindow.State != ProbeRetryWait || !persistedWindow.Deadline.Equal(now.Add(4*time.Second)) {
						t.Fatalf("pre-WAL terminal window=%#v attempts=%#v err=%v", persistedWindow, state.ProbeAttempts, stateErr)
					}
					return
				}
				if len(intents) != 1 || intents[0].Class != OperationProbeVerify || intents[0].StartedAfter != 9 || intents[0].AttemptID != path.name || !reflect.DeepEqual(intents[0].Payload, windows) {
					t.Fatalf("recovery intents=%#v", intents)
				}
				restartedController.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: path.window, Now: now.Add(4 * time.Second), Snapshots: map[ProbeWindowKind]QuotaSnapshot{path.window: {Valid: true, ResetAt: ptrTime(now.Add(windowLength)), Usage: ptrFloat(1), WindowKind: windowKind, WindowLength: windowLength, WindowLengthKnown: true}}})
				window, ok := restartedController.Window(1, path.window)
				if !ok || window.State != ProbeConfirmed {
					t.Fatalf("path=%s verify terminal window=%#v ok=%v", path.name, window, ok)
				}
				if err := restart.Complete(1, path.name, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown); err != nil {
					t.Fatal(err)
				}
				if err := runtime.persistProbeWindows(); err != nil {
					t.Fatal(err)
				}
				state, stateErr := restartStore.PersistentSnapshot()
				persistedWindow := state.ProbeWindows[1][path.window]
				if stateErr != nil || len(state.ProbeAttempts) != 0 || persistedWindow.State != ProbeConfirmed {
					t.Fatalf("path=%s durable terminal window=%#v attempts=%#v err=%v", path.name, persistedWindow, state.ProbeAttempts, stateErr)
				}
			})
		}
	}
}

func permutations(values []int) [][]int {
	var out [][]int
	var walk func([]int, []int)
	walk = func(prefix, rest []int) {
		if len(rest) == 0 {
			out = append(out, append([]int(nil), prefix...))
			return
		}
		for i, value := range rest {
			next := append(append([]int(nil), rest[:i]...), rest[i+1:]...)
			walk(append(prefix, value), next)
		}
	}
	walk(nil, values)
	return out
}

func runSection12D01CredentialIdentityMatrix(t *testing.T) {
	now := time.Date(2026, 7, 14, 5, 0, 0, 0, time.UTC)
	for length := 1; length <= 4; length++ {
		fingerprints := make([]CredentialFingerprint, length)
		for i := range fingerprints {
			fingerprints[i] = fp("subject", fmt.Sprintf("refresh-%d", i), "metadata")
		}
		chain := TransitionChain{Cursor: fingerprints[0]}
		for i := 1; i < length; i++ {
			chain.Transitions = append(chain.Transitions, CredentialTransition{Prev: fingerprints[i-1], Next: fingerprints[i], SaveSeq: uint64(i), Phase: TransitionApplied, CreatedAt: now})
		}
		indices := make([]int, length)
		for i := range indices {
			indices[i] = i
		}
		for _, order := range permutations(indices) {
			store := NewStateStore(filepath.Join(t.TempDir(), "observe.json"), OSFileHooks(), nil)
			state := NewPersistentState()
			state.TierGeneration = 9
			state.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx", Instance: 1, Admission: 3, Generation: 9, Login: 4, Token: 5, Fingerprint: fingerprints[0], AuthBlocked: true}
			state.CredentialChains[1] = chain
			if err := store.WriteThrough(state); err != nil {
				t.Fatal(err)
			}
			host := &walHost{}
			manager := mustCredentialManager(t, store, host, func() time.Time { return now }, nil)
			for _, index := range order {
				before, _ := store.PersistentSnapshot()
				beforeChain := before.CredentialChains[1]
				observation := ClassifyObservedCredentialAt(beforeChain, fingerprints[index], now)
				host.current = HostAuth{Fingerprint: fingerprints[index]}
				if _, err := manager.Reconcile(context.Background(), 1); err != nil {
					t.Fatalf("length=%d order=%v index=%d reconcile=%v", length, order, index, err)
				}
				after, _ := store.PersistentSnapshot()
				afterChain := after.CredentialChains[1]
				beforeBinding, afterBinding := before.Bindings["active"], after.Bindings["active"]
				switch observation.Kind {
				case CredentialSame, CredentialMetadataOnly, CredentialAmbiguous:
					if !reflect.DeepEqual(afterChain, beforeChain) || afterBinding != beforeBinding || after.TierGeneration != before.TierGeneration {
						t.Fatalf("length=%d order=%v index=%d kind=%s unexpectedly mutated production state", length, order, index, observation.Kind)
					}
				case CredentialOwnedRotation:
					if afterChain.Cursor != fingerprints[index] || len(afterChain.Transitions) != len(beforeChain.Transitions)-observation.Advance || afterBinding.Token != beforeBinding.Token+1 || afterBinding.Fingerprint != fingerprints[index] {
						t.Fatalf("length=%d order=%v index=%d owned state chain=%#v binding=%#v", length, order, index, afterChain, afterBinding)
					}
				case CredentialExternalLogin:
					if afterChain.Cursor != fingerprints[index] || len(afterChain.Transitions) != 0 || afterBinding.Login != beforeBinding.Login+1 || afterBinding.Admission != beforeBinding.Admission+1 || after.TierGeneration != before.TierGeneration+1 || afterBinding.AuthBlocked {
						t.Fatalf("length=%d order=%v index=%d external state chain=%#v binding=%#v G=%d", length, order, index, afterChain, afterBinding, after.TierGeneration)
					}
				}
			}
			external := fp("external", fmt.Sprintf("x-%d", length), "metadata")
			before, _ := store.PersistentSnapshot()
			host.current = HostAuth{Fingerprint: external}
			if _, err := manager.Reconcile(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			after, _ := store.PersistentSnapshot()
			if after.CredentialChains[1].Cursor != external || after.Bindings["active"].Login != before.Bindings["active"].Login+1 || after.TierGeneration != before.TierGeneration+1 {
				t.Fatalf("length=%d order=%v interleaved external did not mutate production state", length, order)
			}
		}
	}

	credentialPoints := []string{"K_CREDENTIAL_PLANNED_WRITE", "K_CREDENTIAL_AFTER_PLANNED", "K_CREDENTIAL_BEFORE_SAVEAUTH", "K_CREDENTIAL_AFTER_SAVEAUTH", "K_CREDENTIAL_BEFORE_TERMINAL", "K_CREDENTIAL_TERMINAL_WRITE"}
	outcomes := []struct {
		name string
		err  error
	}{{name: "success"}, {name: "failure", err: ErrCredentialRejected}, {name: "unknown", err: ErrCredentialOutcomeUnknown}}
	for length := 1; length <= 4; length++ {
		for _, outcome := range outcomes {
			for _, point := range credentialPoints {
				storePath := filepath.Join(t.TempDir(), "credential.json")
				store := NewStateStore(storePath, OSFileHooks(), nil)
				base := fp("subject", "base", "metadata")
				host := &walHost{current: HostAuth{Fingerprint: base}, saveErr: outcome.err}
				manager := mustCredentialManager(t, store, host, func() time.Time { return now }, crashController(point))
				chain := TransitionChain{Cursor: base}
				for i := 1; i < length; i++ {
					next := fp("subject", fmt.Sprintf("prior-%d", i), "metadata")
					chain.Transitions = append(chain.Transitions, CredentialTransition{Prev: chain.Tail(), Next: next, Phase: TransitionApplied, CreatedAt: now})
				}
				host.current = HostAuth{Fingerprint: chain.Tail()}
				if err := manager.setChain(1, chain); err != nil {
					t.Fatal(err)
				}
				nextFingerprint := fp("subject", "next", "metadata")
				_, err := manager.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: nextFingerprint}, ExecutionToken{Instance: 1})
				if !errors.Is(err, testsupport.ErrInjectedCrash) {
					t.Fatalf("length=%d outcome=%s point=%s err=%v", length, outcome.name, point, err)
				}
				persisted, snapshotErr := store.PersistentSnapshot()
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				persistedChain := persisted.CredentialChains[1]
				crashedBeforePlanned := point == "K_CREDENTIAL_PLANNED_WRITE"
				wantTransitions := length
				if crashedBeforePlanned {
					wantTransitions = length - 1
				}
				if len(persistedChain.Transitions) != wantTransitions {
					t.Fatalf("length=%d outcome=%s point=%s transitions=%d want=%d", length, outcome.name, point, len(persistedChain.Transitions), wantTransitions)
				}
				for i, transition := range persistedChain.Transitions {
					if !transition.Prev.ValidHashes() || !transition.Next.ValidHashes() || (i < length-1 && transition.Phase != TransitionApplied) {
						t.Fatalf("length=%d outcome=%s point=%s transition[%d]=%#v", length, outcome.name, point, i, transition)
					}
				}
				if !crashedBeforePlanned && persistedChain.Transitions[len(persistedChain.Transitions)-1].Phase != TransitionPlanned {
					t.Fatalf("length=%d outcome=%s point=%s WAL terminal written before recovery", length, outcome.name, point)
				}

				host.saveErr = nil
				afterHost := point == "K_CREDENTIAL_AFTER_SAVEAUTH" || point == "K_CREDENTIAL_BEFORE_TERMINAL" || point == "K_CREDENTIAL_TERMINAL_WRITE"
				if afterHost && outcome.name == "unknown" {
					host.current = HostAuth{Fingerprint: fp("third", "third", "metadata")}
				}
				freshStore := NewStateStore(storePath, OSFileHooks(), nil)
				fresh := mustCredentialManager(t, freshStore, host, func() time.Time { return now }, nil)
				report, reconcileErr := fresh.Reconcile(context.Background(), 1)
				if reconcileErr != nil {
					t.Fatalf("length=%d outcome=%s point=%s reconcile=%v", length, outcome.name, point, reconcileErr)
				}
				if crashedBeforePlanned {
					wantReport := CredentialRecoveryReport{}
					if length > 1 {
						wantReport.Phase = TransitionApplied
					}
					if report.Ambiguous != wantReport.Ambiguous || report.Phase != wantReport.Phase {
						t.Fatalf("pre-planned recovery report=%#v", report)
					}
					continue
				}
				wantPhase := TransitionAborted
				wantAmbiguous := false
				if afterHost && outcome.name == "success" {
					wantPhase = TransitionApplied
				} else if afterHost && outcome.name == "unknown" {
					wantPhase = TransitionPlanned
					wantAmbiguous = true
				}
				if report.Phase != wantPhase || report.Ambiguous != wantAmbiguous {
					t.Fatalf("length=%d outcome=%s point=%s report=%#v want phase=%s ambiguous=%v", length, outcome.name, point, report, wantPhase, wantAmbiguous)
				}
				finalState, _ := freshStore.PersistentSnapshot()
				finalChain := finalState.CredentialChains[1]
				if !wantAmbiguous && (credentialChainUnresolved(finalChain) || finalChain.Cursor != host.current.Fingerprint) {
					t.Fatalf("length=%d outcome=%s point=%s final chain=%#v host=%#v", length, outcome.name, point, finalChain, host.current.Fingerprint)
				}
			}
		}
	}

	f0, f1 := fp("subject", "amb-0", "metadata"), fp("subject", "amb-1", "metadata")
	ambiguousStore := NewStateStore(filepath.Join(t.TempDir(), "auto-clear.json"), OSFileHooks(), nil)
	ambiguousState := NewPersistentState()
	ambiguousState.TierGeneration = 2
	ambiguousState.Bindings["active"] = RuntimeBinding{AuthID: "active", Instance: 1, Admission: 1, Generation: 2, Login: 1, Token: 1, Fingerprint: f0, AuthBlocked: true}
	ambiguousState.CredentialChains[1] = TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, Phase: TransitionOutcomeUnknown, CreatedAt: now}}}
	if err := ambiguousStore.WriteThrough(ambiguousState); err != nil {
		t.Fatal(err)
	}
	ambiguousHost := &walHost{current: HostAuth{Fingerprint: f1}}
	ambiguousManager := mustCredentialManager(t, ambiguousStore, ambiguousHost, func() time.Time { return now }, nil)
	if report, err := ambiguousManager.Reconcile(context.Background(), 1); err != nil || report.Ambiguous {
		t.Fatalf("automatic reconcile report=%#v err=%v", report, err)
	}
	autoCleared, _ := ambiguousStore.PersistentSnapshot()
	if autoCleared.CredentialChains[1].Cursor != f1 || len(autoCleared.CredentialChains[1].Transitions) != 0 || autoCleared.Bindings["active"].Token != 2 {
		t.Fatalf("real reconciliation did not auto-clear ambiguity: %#v", autoCleared)
	}

	for _, action := range []CredentialResolutionAction{CredentialConfirmOwned, CredentialConfirmExternal, CredentialReread} {
		observed := fp("subject", "refresh-1", "metadata")
		if action == CredentialConfirmExternal {
			observed = fp("external", "external", "metadata")
		}
		refresher, store, _, roster := newCredentialManagementFixture(t, observed)
		if err := refresher.ResolveCredentialAmbiguity(context.Background(), roster, "active", action); err != nil {
			t.Fatalf("action=%s err=%v", action, err)
		}
		state, _ := store.PersistentSnapshot()
		binding := state.Bindings["active"]
		switch action {
		case CredentialConfirmOwned:
			if binding.Token != 12 || binding.Login != 7 || binding.Admission != 3 || state.TierGeneration != 5 || !binding.AuthBlocked {
				t.Fatalf("owned epochs=%#v G=%d", binding, state.TierGeneration)
			}
		case CredentialConfirmExternal:
			if binding.Login != 8 || binding.Admission != 4 || state.TierGeneration != 6 || binding.AuthBlocked {
				t.Fatalf("external epochs=%#v G=%d", binding, state.TierGeneration)
			}
		case CredentialReread:
			if binding.Login != 7 || binding.Admission != 3 || state.TierGeneration != 5 {
				t.Fatalf("reread epochs=%#v G=%d", binding, state.TierGeneration)
			}
		}
	}

	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "jwt-account"})
	identityBranches := []struct {
		name           string
		raw            json.RawMessage
		wantID         string
		wantReal       bool
		wantBackground bool
		wantErr        bool
	}{
		{name: "stored-account-id", raw: json.RawMessage(`{"access_token":"a","account_id":"stored"}`), wantID: "stored", wantReal: true, wantBackground: true},
		{name: "jwt-account-id", raw: json.RawMessage(`{"access_token":"a","id_token":"` + idToken + `"}`), wantID: "jwt-account", wantReal: true, wantBackground: true},
		{name: "unresolved-with-access", raw: json.RawMessage(`{"access_token":"a"}`), wantReal: true},
		{name: "unresolved-without-access", raw: json.RawMessage(`{"refresh_token":"r"}`), wantErr: true},
	}
	for _, branch := range identityBranches {
		credentials, err := ExtractCodexCredentials(branch.raw)
		allowsReal := credentials.AccessToken != ""
		allowsBackground := credentials.AccessToken != "" && credentials.ChatGPTAccountID != ""
		if (err != nil) != branch.wantErr || (!branch.wantErr && (credentials.ChatGPTAccountID != branch.wantID || allowsReal != branch.wantReal || allowsBackground != branch.wantBackground)) {
			t.Fatalf("branch=%s credentials=%#v err=%v", branch.name, credentials, err)
		}
	}
	unresolved := AccountState{AuthID: "same-file-name", AuthIndex: "same-file.json", Instance: 91, Provider: "codex", Priority: 9, Family: AccountFamilyWeekly, Quota: ParsedQuota{Family: AccountFamilyWeekly}}
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Trials: NewTrialRegistry(), EvidenceIntents: make(chan EvidenceIntent, 1), ActiveHighestTier: map[string]struct{}{unresolved.AuthID: {}}, Accounts: []AccountView{{ID: unresolved.AuthID, Instance: unresolved.Instance, Family: AccountFamilyWeekly, Cache: CacheUnknown}}})
	decision := schedulerPickPublished(requestWithCandidates(unresolved.AuthID), now)
	if decision.AuthID != unresolved.AuthID {
		t.Fatalf("unresolved identity did not participate in real pick: %#v", decision)
	}
	if trial := publishedSchedulerSnapshot.Load().Trials.State(unresolved.Instance, now); trial != TrialActive {
		t.Fatalf("unresolved real pick did not retain trial eligibility: %v", trial)
	}
	annotations := AnnotationState{Accounts: map[string]AccountAnnotation{"instance:91": {Alias: "old-unresolved"}}}
	relogged := unresolved
	relogged.Instance = 92
	if got := ApplyAnnotations([]AccountState{relogged}, annotations)[0].Annotation.Alias; got != "" {
		t.Fatalf("relogin inherited unresolved file-name configuration %q", got)
	}
	quotaBody := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(time.Hour).Format(time.RFC3339)))
	backgroundHost := &fakeHostClient{authJSON: map[string]json.RawMessage{
		"with-id":    json.RawMessage(`{"access_token":"a","account_id":"stored"}`),
		"without-id": json.RawMessage(`{"access_token":"a"}`),
	}, httpBody: quotaBody}
	backgroundCfg := DefaultConfig()
	backgroundCfg.EnableResetProbe = true
	background := &QuotaRefresher{host: backgroundHost, state: NewPluginState(backgroundCfg), now: func() time.Time { return now }, roster: HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy}}
	coordinator := NewCoordinator(CoordinatorOptions{})
	t.Cleanup(coordinator.Close)
	held := &HeldLease{coordinator: coordinator}
	for _, class := range []OperationClass{OperationQuotaRead, OperationProbePrecheck} {
		present := background.runTypedHeld(context.Background(), Intent{Instance: 1, Class: class, Payload: probeReadPayload{Binding: RuntimeBinding{AuthIndex: "with-id"}}}, held)
		if present.Err != nil {
			t.Fatalf("class=%s account ID blocked background path: %v", class, present.Err)
		}
	}
	presentCalls := backgroundHost.httpCallCount()
	if presentCalls != 2 {
		t.Fatalf("account ID background calls=%d, want quota+Probe", presentCalls)
	}
	for _, class := range []OperationClass{OperationQuotaRead, OperationProbePrecheck} {
		absent := background.runTypedHeld(context.Background(), Intent{Instance: 2, Class: class, Payload: probeReadPayload{Binding: RuntimeBinding{AuthIndex: "without-id"}}}, held)
		if !errors.Is(absent.Err, ErrAccountIdentityUnresolved) {
			t.Fatalf("class=%s unresolved background err=%v", class, absent.Err)
		}
	}
	if calls := backgroundHost.httpCallCount(); calls != presentCalls {
		t.Fatalf("unresolved identity issued quota/Probe HTTP: before=%d after=%d", presentCalls, calls)
	}
}

func eventPermutations(events []string, length int) [][]string {
	var out [][]string
	var walk func([]string, map[string]bool)
	walk = func(prefix []string, used map[string]bool) {
		if len(prefix) == length {
			out = append(out, append([]string(nil), prefix...))
			return
		}
		for _, event := range events {
			if used[event] {
				continue
			}
			used[event] = true
			walk(append(prefix, event), used)
			delete(used, event)
		}
	}
	walk(nil, map[string]bool{})
	return out
}

func runSection12E01RestrictedEventInterleavings(t *testing.T) {
	events := []string{"legacy", "manual", "interval", "precheck", "send", "verify"}
	var schedules [][]string
	for length := 2; length <= 3; length++ {
		schedules = append(schedules, eventPermutations(events, length)...)
	}
	if len(schedules) != 150 {
		t.Fatalf("schedule count=%d", len(schedules))
	}
	for index, schedule := range schedules {
		var seq atomic.Uint64
		seq.Store(100)
		var active atomic.Int64
		var maxActive atomic.Int64
		var mu sync.Mutex
		executed := map[OperationClass]int{}
		release := make(chan struct{})
		fingerprint := fp("subject", "refresh", "metadata")
		bindings := &BindingRegistry{bindings: map[string]RuntimeBinding{"active": {AuthID: "active", Instance: 1, Admission: 3, Generation: 5, Login: 7, Fingerprint: fingerprint}}}
		c := NewCoordinator(CoordinatorOptions{
			AllocateReadSeq: func() (uint64, error) { return seq.Add(1), nil },
			Validate: func(intent Intent, result OperationResult) bool {
				binding, ok := bindings.Lookup(intent.AuthID)
				return ok && ValidateWriteback(BindingVersion{Instance: binding.Instance, Admission: binding.Admission, Tier: TierGeneration(binding.Generation), Login: binding.Login, Fingerprint: binding.Fingerprint}, WritebackVersion{Token: result.Token, Login: result.Login, Fingerprint: result.Fingerprint})
			},
			Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
				current := active.Add(1)
				for current > maxActive.Load() && !maxActive.CompareAndSwap(maxActive.Load(), current) {
				}
				mu.Lock()
				executed[intent.Class]++
				mu.Unlock()
				<-release
				active.Add(-1)
				return OperationResult{Token: intent.Token, ReadStartSeq: intent.ReadStartSeq, Login: 7, Fingerprint: fingerprint}
			},
		})
		type scheduledFuture struct {
			event        string
			startedAfter uint64
			future       Future[OperationResult]
		}
		futures := make([]scheduledFuture, 0, len(schedule))
		for position, event := range schedule {
			intent := Intent{AuthID: "active", Instance: 1, Generation: 5, Token: ExecutionToken{Instance: 1, Admission: 3, Tier: 5, Fence: uint64(position + 1)}, AttemptID: fmt.Sprintf("%d-%d", index, position)}
			switch event {
			case "legacy":
				intent.Class, intent.Source = OperationLegacyRefresh, LegacyEnvelopeSource
				futures = append(futures, scheduledFuture{event: event, future: c.Submit(intent)})
			case "manual":
				intent.Class, intent.Source = OperationQuotaRead, SourceManualRefresh
				futures = append(futures, scheduledFuture{event: event, startedAfter: intent.StartedAfter, future: c.SubmitTyped(intent)})
			case "interval":
				intent.Class, intent.Source = OperationQuotaRead, SourceSchedulerInterval
				futures = append(futures, scheduledFuture{event: event, startedAfter: intent.StartedAfter, future: c.SubmitTyped(intent)})
			case "precheck":
				intent.Class, intent.Source, intent.StartedAfter = OperationProbePrecheck, SourceProbePrecheck, 99
				futures = append(futures, scheduledFuture{event: event, startedAfter: intent.StartedAfter, future: c.SubmitTyped(intent)})
			case "send":
				intent.Class, intent.Source = OperationProbeSend, SourceProbeActivation
				futures = append(futures, scheduledFuture{event: event, future: c.SubmitTyped(intent)})
			case "verify":
				intent.Class, intent.Source, intent.StartedAfter = OperationProbeVerify, SourceProbeVerify, 99
				futures = append(futures, scheduledFuture{event: event, startedAfter: intent.StartedAfter, future: c.SubmitTyped(intent)})
			}
		}
		close(release)
		for _, scheduled := range futures {
			result := scheduled.future.Await(context.Background())
			if result.Disposition != ResultApplied {
				t.Fatalf("schedule=%v event=%s result=%#v", schedule, scheduled.event, result)
			}
			isRead := scheduled.event == "manual" || scheduled.event == "interval" || scheduled.event == "precheck" || scheduled.event == "verify"
			if isRead && result.ReadStartSeq <= scheduled.startedAfter {
				t.Fatalf("schedule=%v event=%s read_start_seq=%d started_after=%d", schedule, scheduled.event, result.ReadStartSeq, scheduled.startedAfter)
			}
		}
		if maxActive.Load() > 1 {
			t.Fatalf("schedule=%v concurrent instance mutations=%d", schedule, maxActive.Load())
		}
		containsManual, containsInterval := false, false
		for _, event := range schedule {
			containsManual = containsManual || event == "manual"
			containsInterval = containsInterval || event == "interval"
		}
		if containsManual && containsInterval && executed[OperationQuotaRead] > 1 {
			t.Fatalf("schedule=%v quota executions=%d", schedule, executed[OperationQuotaRead])
		}
		stale := c.Submit(Intent{AuthID: "active", Instance: 1, Generation: 4, Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, Token: ExecutionToken{Instance: 1, Admission: 2, Tier: 4, Fence: 999}}).Await(context.Background())
		if stale.Disposition != ResultDiscardedStale {
			t.Fatalf("schedule=%v stale disposition=%q", schedule, stale.Disposition)
		}
		c.Close()
	}
}
