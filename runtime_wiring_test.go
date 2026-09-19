package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRuntimeSemanticPathsAndLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	paths := semanticStatePaths(legacy)
	if paths.UserData != filepath.Join(dir, ".user-data.json") || paths.Runtime != filepath.Join(dir, ".runtime-state.json") {
		t.Fatalf("paths = %#v", paths)
	}
	want := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "legacy"}}}
	if err := SavePluginDiskState(legacy, want); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserDataWithMigration(paths, OSFileHooks(), nil)
	if err != nil || !loaded || got.Accounts["a"].Alias != "legacy" {
		t.Fatalf("got=%#v loaded=%v err=%v", got, loaded, err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("legacy not retained: %v", err)
	}
	raw, err := os.ReadFile(paths.UserData)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.SchemaVersion != CurrentUserDataSchema {
		t.Fatalf("new user data lacks schema: %s", raw)
	}

	if err := os.WriteFile(legacy, []byte(`{"accounts":{"a":{"alias":"ignored"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, _, err = loadUserDataWithMigration(paths, OSFileHooks(), nil)
	if err != nil || got.Accounts["a"].Alias != "legacy" {
		t.Fatalf("new file not authoritative: %#v %v", got, err)
	}
}

func TestRuntimeCorruptLegacyIsPreserved(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	if err := os.WriteFile(legacy, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil)
	if err == nil {
		t.Fatal("expected corrupt legacy error")
	}
	if raw, readErr := os.ReadFile(legacy); readErr != nil || string(raw) != "{bad" {
		t.Fatalf("legacy changed: %q %v", raw, readErr)
	}
	if _, statErr := os.Stat(legacy + ".migrated"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected migrated file: %v", statErr)
	}
}

func TestLegacyMigrationRejectsSensitiveAndUnknownFieldsWithoutRename(t *testing.T) {
	for name, raw := range map[string]string{
		"refresh_token": `{"config":{},"refresh_token":"secret"}`,
		"authorization": `{"config":{},"Authorization":"Bearer secret"}`,
		"unknown":       `{"config":{},"credential_blob":{"token":"secret"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			if err := os.WriteFile(legacy, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil)
			if err == nil {
				t.Fatal("expected strict migration rejection")
			}
			got, e := os.ReadFile(legacy)
			if e != nil || string(got) != raw {
				t.Fatalf("legacy=%q err=%v", got, e)
			}
			if _, e := os.Stat(legacy + ".migrated"); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("unexpected migrated evidence: %v", e)
			}
		})
	}
}

func TestLegacyMigrationRejectsCredentialSyntaxInsideAllowedStringValues(t *testing.T) {
	cases := map[string]string{
		"alias authorization":        `{"config":{},"accounts":{"a":{"alias":"Authorization: Bearer SECRET_X"}}}`,
		"group notes cookie":         `{"config":{},"groups":{"g":{"notes":"Cookie: session=SECRET_X"}}}`,
		"tag refresh assignment":     `{"config":{},"accounts":{"a":{"tags":["refresh_token=SECRET_X"]}}}`,
		"config credential material": `{"config":{"monthly_mode":"Authorization: Bearer SECRET_X"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			if err := os.WriteFile(legacy, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			paths := semanticStatePaths(legacy)
			if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), nil); err == nil {
				t.Fatal("expected credential-value rejection")
			}
			got, e := os.ReadFile(legacy)
			if e != nil || string(got) != raw {
				t.Fatalf("legacy=%q err=%v", got, e)
			}
			for _, path := range []string{legacy + ".migrated", paths.UserData} {
				if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("unexpected artifact %s: %v", path, e)
				}
			}
		})
	}
}

func TestLegacyMigrationAllowsBenignTokenMentionAndScansRealArtifact(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	state := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "token budgeting notes", Notes: "No credential syntax here", Tags: []string{"token-awareness"}}}}
	if err := SavePluginDiskState(legacy, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacy + ".migrated")
	if err != nil {
		t.Fatal(err)
	}
	if containsSensitiveCredentialMaterial(string(raw)) {
		t.Fatalf("real migrated artifact contains credential syntax: %s", raw)
	}
}

func TestValidLegacyMigrationScansActualRetainedArtifact(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "custom-legacy.json")
	state := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "safe"}}}
	if err := SavePluginDiskState(legacy, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacy + ".migrated")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	if containsSensitiveCredentialMaterial(lower) {
		t.Fatalf("credential syntax in actual migrated artifact: %s", raw)
	}
}

func TestUserDataRecoversFromAtomicBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "second"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserData(path)
	if err != nil || !loaded || got.Accounts["a"].Alias != "first" {
		t.Fatalf("backup recovery got=%#v loaded=%v err=%v", got, loaded, err)
	}
}

func TestUserDataMigrationCrashPointsConverge(t *testing.T) {
	points := []string{"K_USER_MIGRATION_BEFORE_NEW_RENAME", "K_USER_MIGRATION_AFTER_NEW_RENAME", "K_USER_MIGRATION_BEFORE_LEGACY_RENAME", "K_USER_MIGRATION_AFTER_LEGACY_RENAME", "K_USER_MIGRATION_AFTER_READBACK", "K_USER_PRIMARY_TEMP_WRITE", "K_USER_PRIMARY_TEMP_FSYNC", "K_USER_BACKUP_TEMP_WRITE", "K_USER_BACKUP_TEMP_FSYNC", "K_USER_BACKUP_BEFORE_REPLACE", "K_USER_BACKUP_AFTER_REPLACE", "K_USER_PRIMARY_DIR_FSYNC", "K_USER_AFTER_PRIMARY_DIR_FSYNC", "K_USER_BACKUP_DIR_FSYNC", "K_USER_AFTER_BACKUP_DIR_FSYNC"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			paths := semanticStatePaths(legacy)
			if err := SavePluginDiskState(legacy, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "safe"}}}); err != nil {
				t.Fatal(err)
			}
			registry := testsupport.NewKPointRegistry(points...)
			crash := testsupport.NewCrashController(registry, point)
			if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), crash); !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("crash err=%v", err)
			}
			if _, err := os.Stat(paths.Legacy); err != nil {
				if _, migratedErr := os.Stat(paths.Legacy + ".migrated"); migratedErr != nil {
					t.Fatalf("neither old evidence exists: %v / %v", err, migratedErr)
				}
			}
			got, loaded, err := loadUserDataWithMigration(paths, OSFileHooks(), nil)
			if err != nil || !loaded || got.Accounts["a"].Alias != "safe" {
				t.Fatalf("recovery got=%#v loaded=%v err=%v", got, loaded, err)
			}
		})
	}
}

func TestRuntimeAndUserArtifactsNeverPersistSensitiveValues(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	paths := semanticStatePaths(legacy)
	if err := SavePluginDiskState(legacy, PluginDiskState{Config: DefaultConfig()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(paths.Runtime, OSFileHooks(), nil)
	if _, err := store.Update(func(s *PersistentState) error {
		s.CredentialChains[1] = TransitionChain{Cursor: NewCredentialFingerprint("subject-secret", "refresh-token-secret", "Authorization: Bearer header-secret")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{paths.Runtime, paths.UserData} {
		primary, err := os.ReadFile(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".tmp", ".corrupt", ".migrated"} {
			if err := os.WriteFile(base+suffix, primary, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, suffix := range []string{"", ".bak", ".tmp", ".corrupt", ".migrated"} {
		for _, base := range []string{paths.Runtime, paths.UserData} {
			raw, err := os.ReadFile(base + suffix)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"subject-secret", "refresh-token-secret", "header-secret"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("secret %q in %s", secret, base+suffix)
				}
			}
		}
	}
}

func TestProbeAttemptSeamRuntimeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	sent := time.Unix(11, 0).UTC()
	want := ProbeAttemptSeam{Instance: 7, AttemptID: "attempt-1", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 9, CreatedAt: time.Unix(10, 0).UTC(), SentAt: &sent, VerifyNotBefore: time.Unix(20, 0).UTC(), SuppressUntil: time.Unix(30, 0).UTC()}
	if _, err := store.Update(func(s *PersistentState) error { s.ProbeAttempts[want.Instance] = want; return nil }); err != nil {
		t.Fatal(err)
	}
	fresh := NewStateStore(path, OSFileHooks(), nil)
	got, _, err := fresh.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ProbeAttempts[7], want) {
		t.Fatalf("round trip = %#v", got.ProbeAttempts[7])
	}
}

type runtimeCredentialHost struct {
	current map[AuthInstanceID]HostAuth
	getErr  map[AuthInstanceID]error
	saves   int
	gets    int
}

func (h *runtimeCredentialHost) SaveAuth(_ context.Context, id AuthInstanceID, auth HostAuth) error {
	h.saves++
	h.current[id] = auth
	return nil
}
func (h *runtimeCredentialHost) GetAuth(_ context.Context, id AuthInstanceID) (HostAuth, error) {
	h.gets++
	if err := h.getErr[id]; err != nil {
		return HostAuth{}, err
	}
	a, ok := h.current[id]
	if !ok {
		return HostAuth{}, os.ErrNotExist
	}
	return a, nil
}

func TestProductionRosterSyncReconcilesCredentialTailsAfterRestart(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	prev := NewCredentialFingerprint("subject", "refresh-prev", "meta")
	next := NewCredentialFingerprint("subject", "refresh-next", "meta")
	third := NewCredentialFingerprint("other", "refresh-third", "meta")
	tests := []struct {
		name       string
		observed   CredentialFingerprint
		getErr     error
		wantPhase  TransitionPhase
		ambiguous  bool
		wantGetOne bool
	}{
		{name: "observed-prev-aborts", observed: prev, wantPhase: TransitionAborted, wantGetOne: true},
		{name: "observed-next-applies", observed: next, wantPhase: TransitionApplied, wantGetOne: true},
		{name: "observed-third-stays-ambiguous", observed: third, wantPhase: TransitionOutcomeUnknown, ambiguous: true, wantGetOne: true},
		{name: "host-error-stays-ambiguous", getErr: errors.New("credential host unavailable"), wantPhase: TransitionOutcomeUnknown, ambiguous: true, wantGetOne: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			instance := legacyAuthInstanceID("active")
			persistent := NewPersistentState()
			persistent.TierGeneration = 4
			persistent.AdmissionEpochs[instance] = 1
			persistent.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx-active", Instance: instance, Admission: 1, Generation: 4, Login: 1, Token: 1, Fingerprint: prev}
			persistent.CredentialChains[instance] = TransitionChain{Cursor: prev, Transitions: []CredentialTransition{{Prev: prev, Next: next, SaveSeq: 1, Phase: TransitionOutcomeUnknown, CreatedAt: now.Add(-time.Minute)}}}
			if err := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil).WriteThrough(persistent); err != nil {
				t.Fatal(err)
			}
			host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{instance: {Fingerprint: tc.observed}}, getErr: map[AuthInstanceID]error{instance: tc.getErr}}
			roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Generation: 4, Entries: []RosterEntry{{ID: "active", AuthIndex: "idx-active", Provider: "codex", Priority: intPtr(9)}}}
			r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), host, roster, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
				t.Fatalf("PublishAuthoritativeRoster: %v", err)
			}
			persisted, err := r.runtimeStore.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			chain := persisted.CredentialChains[instance]
			got := tc.wantPhase
			if tc.ambiguous {
				got = chain.Transitions[0].Phase
			} else if chain.Cursor != tc.observed || len(chain.Transitions) != 0 {
				t.Fatalf("resolved chain=%#v observed=%#v", chain, tc.observed)
			}
			if got != tc.wantPhase || (host.gets == 1) != tc.wantGetOne {
				t.Fatalf("phase=%s gets=%d, want phase=%s getOne=%v", got, host.gets, tc.wantPhase, tc.wantGetOne)
			}
			_, tailErr := persisted.CredentialChains[instance].SaveTail()
			if tc.ambiguous != errors.Is(tailErr, ErrCredentialUnresolved) {
				t.Fatalf("SaveTail err=%v ambiguous=%v", tailErr, tc.ambiguous)
			}
			active := ActiveRoster{Instances: []string{"active"}}
			if gotAmbiguous := managementCredentialAmbiguous(r, active, now); gotAmbiguous != tc.ambiguous {
				t.Fatalf("Management ambiguity=%v, want %v", gotAmbiguous, tc.ambiguous)
			}
			before := clonePersistentState(persisted)
			_ = buildCurrentStatusPayloadWithLifecycle(r.state, now, ManagementLifecycleSnapshot{Roster: active, CredentialAmbiguous: tc.ambiguous})
			after, _ := r.runtimeStore.PersistentSnapshot()
			if !reflect.DeepEqual(before, after) || host.gets != 1 {
				t.Fatalf("Management projection mutated reconciliation state or called host: before=%#v after=%#v gets=%d", before, after, host.gets)
			}
		})
	}
}

func TestProductionRosterSyncCredentialReconcileFailureIsPerInstance(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	prev := NewCredentialFingerprint("subject", "refresh-prev", "meta")
	next := NewCredentialFingerprint("subject", "refresh-next", "meta")
	persistent := NewPersistentState()
	persistent.TierGeneration = 4
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}, getErr: map[AuthInstanceID]error{}}
	entries := make([]RosterEntry, 0, 2)
	for _, id := range []string{"broken", "healthy"} {
		instance := legacyAuthInstanceID(id)
		persistent.AdmissionEpochs[instance] = 1
		persistent.Bindings[id] = RuntimeBinding{AuthID: id, AuthIndex: "idx-" + id, Instance: instance, Admission: 1, Generation: 4, Login: 1, Token: 1, Fingerprint: prev}
		persistent.CredentialChains[instance] = TransitionChain{Cursor: prev, Transitions: []CredentialTransition{{Prev: prev, Next: next, SaveSeq: uint64(len(entries) + 1), Phase: TransitionPlanned, CreatedAt: now.Add(-time.Minute)}}}
		entries = append(entries, RosterEntry{ID: id, AuthIndex: "idx-" + id, Provider: "codex", Priority: intPtr(9)})
		host.current[instance] = HostAuth{Fingerprint: next}
	}
	host.getErr[legacyAuthInstanceID("broken")] = errors.New("broken auth")
	if err := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil).WriteThrough(persistent); err != nil {
		t.Fatal(err)
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Generation: 4, Entries: entries}
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), host, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatalf("one credential reconciliation blocked roster publication: %v", err)
	}
	persisted, _ := r.runtimeStore.PersistentSnapshot()
	if got := persisted.CredentialChains[legacyAuthInstanceID("broken")].Transitions[0].Phase; got != TransitionPlanned {
		t.Fatalf("broken phase=%s", got)
	}
	healthy := persisted.CredentialChains[legacyAuthInstanceID("healthy")]
	if healthy.Cursor != next || len(healthy.Transitions) != 0 {
		t.Fatalf("healthy chain=%#v, unrelated failure blocked reconciliation", healthy)
	}
	if host.gets != 2 {
		t.Fatalf("GetAuth calls=%d, want one per active unresolved instance", host.gets)
	}
}

func TestAutomaticExternalLoginPublishesConfirmedRosterFromCommittedBinding(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	instance := legacyAuthInstanceID("active")
	oldFingerprint := NewCredentialFingerprint("subject", "old", "idx-active")
	external := NewCredentialFingerprint("external", "new", "idx-active")
	persistent := NewPersistentState()
	persistent.TierGeneration = 5
	persistent.AdmissionEpochs[instance] = 3
	persistent.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "idx-active", Instance: instance, Admission: 3, Generation: 5, Login: 7, Token: 11, Fingerprint: oldFingerprint}
	persistent.CredentialChains[instance] = TransitionChain{Cursor: oldFingerprint}
	if err := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil).WriteThrough(persistent); err != nil {
		t.Fatal(err)
	}
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{instance: {Fingerprint: external}}, getErr: map[AuthInstanceID]error{}}
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Generation: 5, Entries: []RosterEntry{{ID: "active", AuthIndex: "idx-active", Provider: "codex", Priority: intPtr(9)}}}
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), host, roster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	after, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding := after.Bindings["active"]
	if binding.Fingerprint != external || binding.Generation != 6 || after.TierGeneration != 6 {
		t.Fatalf("external binding not committed: binding=%#v G=%d", binding, after.TierGeneration)
	}
	if after.LastConfirmedRoster == nil || after.LastConfirmedRoster.Generation != 6 || len(after.LastConfirmedRoster.Entries) != 1 || after.LastConfirmedRoster.Entries[0].Fingerprint != external {
		t.Fatalf("confirmed roster persisted stale pre-reconcile binding: %#v", after.LastConfirmedRoster)
	}
	if published := r.runtimeRoster(); published.Generation != 6 {
		t.Fatalf("runtime roster generation=%d, want committed 6", published.Generation)
	}
}

func TestProductionRosterSyncAuthIndexChangeDoesNotReconcileResetChain(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	instance := legacyAuthInstanceID("active")
	prev := NewCredentialFingerprint("subject", "refresh-prev", "old-index")
	next := NewCredentialFingerprint("subject", "refresh-next", "old-index")
	replaced := NewCredentialFingerprint("subject", "refresh-live", "new-index")
	persistent := NewPersistentState()
	persistent.TierGeneration = 4
	persistent.AdmissionEpochs[instance] = 1
	persistent.Bindings["active"] = RuntimeBinding{AuthID: "active", AuthIndex: "old-index", Instance: instance, Admission: 1, Generation: 4, Login: 1, Token: 1, Fingerprint: prev}
	persistent.CredentialChains[instance] = TransitionChain{Cursor: prev, Transitions: []CredentialTransition{{Prev: prev, Next: next, SaveSeq: 1, Phase: TransitionOutcomeUnknown, CreatedAt: now.Add(-time.Minute)}}}
	if err := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil).WriteThrough(persistent); err != nil {
		t.Fatal(err)
	}
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{instance: {Fingerprint: replaced}}}
	startupRoster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Generation: 4, Entries: []RosterEntry{{ID: "active", AuthIndex: "old-index", Provider: "codex", Priority: intPtr(9)}}}
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), host, startupRoster, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	changed := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Generation: 5, Entries: []RosterEntry{{ID: "active", AuthIndex: "new-index", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), changed); err != nil {
		t.Fatalf("PublishAuthoritativeRoster after AuthIndex change: %v", err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	chain := persisted.CredentialChains[instance]
	if len(chain.Transitions) != 0 || chain.Cursor != replaced {
		t.Fatalf("reset chain was reconciled from stale manager state: %#v", chain)
	}
	if host.gets != 1 {
		t.Fatalf("GetAuth calls=%d, want only binding replacement observation", host.gets)
	}
}

func TestBindingGenesisAndInjectedCredentialWAL(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), ".runtime-state.json"), OSFileHooks(), nil)
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(3)}}}
	observed := HostAuth{Fingerprint: NewCredentialFingerprint("subject", "refresh", `{"account":"a"}`)}
	host.current[legacyAuthInstanceID("auth-a")] = observed
	binding, created, err := registry.ObserveAuthoritative(context.Background(), roster, "auth-a", host)
	if err != nil || !created {
		t.Fatalf("binding=%#v created=%v err=%v", binding, created, err)
	}
	if binding.Login != 1 || binding.Token != 1 || binding.Admission != 1 || binding.Generation != 1 || binding.Fingerprint != observed.Fingerprint {
		t.Fatalf("bad genesis: %#v", binding)
	}
	if host.gets != 1 || host.saves != 0 {
		t.Fatalf("genesis calls get=%d save=%d", host.gets, host.saves)
	}
	token := binding.ExecutionToken(41)
	manager, err := NewCredentialManager(store, host, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := HostAuth{Fingerprint: NewCredentialFingerprint("subject", "refresh-2", `{"account":"a"}`)}
	if _, err := manager.SaveVersioned(context.Background(), binding.Instance, next, token); err != nil {
		t.Fatal(err)
	}
	if host.saves != 1 {
		t.Fatalf("save calls=%d", host.saves)
	}
	fresh, err := NewBindingRegistry(NewStateStore(store.path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.Lookup("auth-a"); !ok {
		t.Fatal("binding not persistent across restart")
	}
}

func TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), ".runtime-state.json"), OSFileHooks(), nil)
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ObserveAuthoritative(context.Background(), HostRosterSnapshot{Capability: CapabilityB}, "auth-a", host); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	if host.gets != 0 || host.saves != 0 {
		t.Fatalf("Capability-B made calls: get=%d save=%d", host.gets, host.saves)
	}
	if err := registry.MarkAuthBlocked("auth-a"); err != nil {
		t.Fatal(err)
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(1)}}}
	host.current[legacyAuthInstanceID("auth-a")] = HostAuth{Fingerprint: fp("s", "r", "m")}
	binding, _, err := registry.ObserveAuthoritative(context.Background(), roster, "auth-a", host)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.AuthBlocked {
		t.Fatal("genesis unlocked AuthBlocked")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewBindingRegistry(NewStateStore(store.path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := fresh.Lookup("auth-a")
	if !ok || !recovered.AuthBlocked {
		t.Fatalf("primary deletion bypassed AuthBlocked: %#v ok=%v", recovered, ok)
	}
}

func TestBindingRosterGenerationDoesNotRegressAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{legacyAuthInstanceID("a"): {Name: "a.json", Fingerprint: fp("a", "ra", "ma")}, legacyAuthInstanceID("b"): {Name: "b.json", Fingerprint: fp("b", "rb", "mb")}}}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	first := HostRosterSnapshot{Capability: CapabilityA, Generation: 9, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(7)}}}
	if _, err = registry.ReconcileRoster(context.Background(), first, host); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	sameAfterRestart := HostRosterSnapshot{Capability: CapabilityA, Generation: 1, Entries: first.Entries}
	result, err := restarted.ReconcileRoster(context.Background(), sameAfterRestart, host)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Bindings["a"].Generation; got != 9 {
		t.Fatalf("durable generation regressed to %d", got)
	}
	changed := HostRosterSnapshot{Capability: CapabilityA, Generation: 2, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(7)}, {ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(7)}}}
	result, err = restarted.ReconcileRoster(context.Background(), changed, host)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bindings["a"].Generation != 10 || result.Bindings["b"].Generation != 10 {
		t.Fatalf("changed generations=%d/%d", result.Bindings["a"].Generation, result.Bindings["b"].Generation)
	}
}

func TestBindingAdmissionEpochMonotonicAcrossDeleteReaddAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{
		legacyAuthInstanceID("keep"):   {Name: "keep.json", Fingerprint: fp("keep", "rk", "mk")},
		legacyAuthInstanceID("target"): {Name: "target.json", Fingerprint: fp("target", "rt", "mt")},
	}}
	registry, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	withTarget := func(g uint64) HostRosterSnapshot {
		return HostRosterSnapshot{Capability: CapabilityA, Generation: g, Entries: []RosterEntry{{ID: "keep", AuthIndex: "keep", Provider: "codex", Priority: intPtr(7)}, {ID: "target", AuthIndex: "target", Provider: "codex", Priority: intPtr(7)}}}
	}
	withoutTarget := func(g uint64) HostRosterSnapshot {
		return HostRosterSnapshot{Capability: CapabilityA, Generation: g, Entries: []RosterEntry{{ID: "keep", AuthIndex: "keep", Provider: "codex", Priority: intPtr(7)}}}
	}
	result, err := registry.ReconcileRoster(context.Background(), withTarget(1), host)
	if err != nil || result.Bindings["target"].Admission != 1 {
		t.Fatalf("first admission=%d err=%v", result.Bindings["target"].Admission, err)
	}
	if _, err = registry.ReconcileRoster(context.Background(), withoutTarget(2), host); err != nil {
		t.Fatal(err)
	}
	result, err = registry.ReconcileRoster(context.Background(), withTarget(3), host)
	if err != nil || result.Bindings["target"].Admission != 2 {
		t.Fatalf("second admission=%d err=%v", result.Bindings["target"].Admission, err)
	}
	if _, err = registry.ReconcileRoster(context.Background(), withoutTarget(4), host); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	result, err = restarted.ReconcileRoster(context.Background(), withTarget(1), host)
	if err != nil || result.Bindings["target"].Admission != 3 {
		t.Fatalf("third admission after restart=%d err=%v", result.Bindings["target"].Admission, err)
	}
}

func TestBindingChangedFingerprintReaddResetsCredentialChainAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	keepInstance := legacyAuthInstanceID("keep")
	targetInstance := legacyAuthInstanceID("target")
	oldFingerprint := fp("target", "old-refresh", "target-index")
	newFingerprint := fp("target", "new-refresh", "target-index")
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{
		keepInstance:   {Name: "keep.json", Fingerprint: fp("keep", "rk", "keep-index")},
		targetInstance: {Name: "target.json", Fingerprint: oldFingerprint},
	}}
	registry, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	withTarget := HostRosterSnapshot{Capability: CapabilityA, Generation: 1, Entries: []RosterEntry{{ID: "keep", AuthIndex: "keep-index", Provider: "codex", Priority: intPtr(7)}, {ID: "target", AuthIndex: "target-index", Provider: "codex", Priority: intPtr(7)}}}
	withoutTarget := HostRosterSnapshot{Capability: CapabilityA, Generation: 2, Entries: withTarget.Entries[:1]}
	if _, err = registry.ReconcileRoster(context.Background(), withTarget, host); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.store.Update(func(s *PersistentState) error {
		chain := s.CredentialChains[targetInstance]
		chain.Transitions = []CredentialTransition{{Prev: oldFingerprint, Next: fp("target", "intermediate", "target-index"), SaveSeq: 1, Phase: TransitionApplied}}
		s.CredentialChains[targetInstance] = chain
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.ReconcileRoster(context.Background(), withoutTarget, host); err != nil {
		t.Fatal(err)
	}
	host.current[targetInstance] = HostAuth{Name: "target-new.json", Fingerprint: newFingerprint}
	restarted, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.ReconcileRoster(context.Background(), withTarget, host)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bindings["target"].Admission != 2 || result.Bindings["target"].Fingerprint != newFingerprint {
		t.Fatalf("readded binding=%#v", result.Bindings["target"])
	}
	persisted, err := restarted.store.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	chain := persisted.CredentialChains[targetInstance]
	if chain.Cursor != newFingerprint || len(chain.Transitions) != 0 {
		t.Fatalf("readded chain not reset: %#v", chain)
	}
}

func TestBindingReconcileFailsBeforeHostReadWhenDurableStateUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.loaded = false
	store.hooks.ReadFile = func(string) ([]byte, error) { return nil, errors.New("durable read failed") }
	store.mu.Unlock()
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{legacyAuthInstanceID("a"): {Name: "a.json", Fingerprint: fp("a", "r", "m")}}}
	roster := HostRosterSnapshot{Capability: CapabilityA, Generation: 1, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(7)}}}
	if _, err = registry.ReconcileRoster(context.Background(), roster, host); err == nil {
		t.Fatal("durable read failure was ignored")
	}
	if host.gets != 0 {
		t.Fatalf("host GetAuth ran before durable state was available: %d", host.gets)
	}
}

func TestBindingAuthIndexChangeAdvancesGenerationAndReobservesAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	instance := legacyAuthInstanceID("a")
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{instance: {Name: "old.json", Fingerprint: fp("a", "r-old", "old-index")}}}
	registry, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	oldRoster := HostRosterSnapshot{Capability: CapabilityA, Generation: 5, Entries: []RosterEntry{{ID: "a", AuthIndex: "old-index", Provider: "codex", Priority: intPtr(7)}}}
	result, err := registry.ReconcileRoster(context.Background(), oldRoster, host)
	if err != nil {
		t.Fatal(err)
	}
	old := result.Bindings["a"]
	host.current[instance] = HostAuth{Name: "new.json", Fingerprint: fp("a", "r-new", "new-index")}
	restarted, err := NewBindingRegistry(NewStateStore(path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	newRoster := HostRosterSnapshot{Capability: CapabilityA, Generation: 1, Entries: []RosterEntry{{ID: "a", AuthIndex: "new-index", Provider: "codex", Priority: intPtr(7)}}}
	result, err = restarted.ReconcileRoster(context.Background(), newRoster, host)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Bindings["a"]
	if got.Generation != old.Generation+1 || got.Admission != old.Admission+1 {
		t.Fatalf("generation/admission=%d/%d want %d/%d", got.Generation, got.Admission, old.Generation+1, old.Admission+1)
	}
	if got.AuthIndex != "new-index" || got.AuthName != "new.json" || got.Fingerprint != host.current[instance].Fingerprint {
		t.Fatalf("changed binding not reobserved: %#v", got)
	}
	if host.gets != 2 {
		t.Fatalf("GetAuth calls=%d want initial + changed metadata", host.gets)
	}
	persisted, err := restarted.store.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	chain := persisted.CredentialChains[instance]
	if chain.Cursor != host.current[instance].Fingerprint || len(chain.Transitions) != 0 {
		t.Fatalf("AuthIndex replacement chain not reset: %#v", chain)
	}
}

func TestAuthoritativeDurableGenerationSurvivesFailureObservation(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	clock := now
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	runtimePath := semanticStatePaths(legacyPath).Runtime
	instance := legacyAuthInstanceID("a")
	fingerprint := fp("acct", "refresh", "idx")
	seed := NewStateStore(runtimePath, OSFileHooks(), nil)
	if _, err := seed.Update(func(s *PersistentState) error {
		s.TierGeneration = 9
		s.AdmissionEpochs[instance] = 1
		s.Bindings["a"] = RuntimeBinding{AuthID: "a", AuthIndex: "idx", AuthName: "a.json", Instance: instance, Admission: 1, Generation: 9, Login: 1, Token: 1, Fingerprint: fingerprint}
		s.CredentialChains[instance] = TransitionChain{Cursor: fingerprint}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{"idx": {AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	runtime, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = runtime.bindings
	defer runtime.coordinator.Close()
	priority := 7
	lister := &rosterTestHost{entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: &priority}}}
	controller := NewRosterController(RosterControllerOptions{
		Host: lister,
		Now:  func() time.Time { return clock },
		Publish: func(ctx context.Context, active ActiveRoster) (ActiveRoster, error) {
			if err := runtime.PublishAuthoritativeRoster(ctx, hostRosterSnapshotFromActive(active)); err != nil {
				return active, err
			}
			active.Generation = runtime.runtimeRoster().Generation
			return active, nil
		},
		Observe: runtime.ObserveRosterLifecycle,
	})
	confirmed, err := controller.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Generation != 9 || runtime.runtimeRoster().Generation != 9 {
		t.Fatalf("confirmed controller/runtime generation=%d/%d", confirmed.Generation, runtime.runtimeRoster().Generation)
	}
	lister.setError(errors.New("offline"))
	clock = clock.Add(rosterActiveTTL)
	degraded, _ := controller.WakeForManagement(context.Background())
	if degraded.Generation != 9 || runtime.runtimeRoster().Generation != 9 || degraded.Health != RosterDegraded {
		t.Fatalf("degraded controller/runtime=%#v / %#v", degraded, runtime.runtimeRoster())
	}
}

func TestProductionProvisionalRecoveryPersistsAndRestarts(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"idx-a": {AuthIndex: "idx-a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"ACCESS_SECRET","refresh_token":"REFRESH_SECRET","account_id":"acct-a"}`)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	confirmed := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Generation: 7, ConfirmedAt: now, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}}}
	if err = r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	r.coordinator.Close()

	getsBefore := host.get
	restartAdapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	restarted, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), restartAdapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.coordinator.Close()
	restartAdapter.bindings = restarted.bindings
	provisional := restarted.ProvisionalRoster()
	if provisional == nil || provisional.Capability != CapabilityB || provisional.Confirmed || !provisional.Provisional || provisional.BackgroundAllowed || provisional.Health != RosterWaiting {
		t.Fatalf("provisional=%#v", provisional)
	}
	if provisional.Generation != 7 || !provisional.ConfirmedAt.Equal(now) || provisional.HighestPriority != 9 || len(provisional.Entries) != 1 || provisional.Entries[0].AuthIndex != "idx-a" {
		t.Fatalf("recovered metadata=%#v", provisional)
	}
	if host.get != getsBefore || host.http != 0 || host.save != 0 {
		t.Fatalf("Capability-B restart calls get=%d->%d http=%d save=%d", getsBefore, host.get, host.http, host.save)
	}
	raw, err := os.ReadFile(semanticStatePaths(legacyPath).Runtime)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ACCESS_SECRET", "REFRESH_SECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("runtime state leaked %q: %s", secret, raw)
		}
	}

	expired, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), restartAdapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now.Add(provisionalMaxAge) })
	if err != nil {
		t.Fatal(err)
	}
	defer expired.coordinator.Close()
	if got := expired.ProvisionalRoster(); got != nil {
		t.Fatalf("age >= 4h recovered provisional: %#v", got)
	}
}

func TestProductionProvisionalRecoveryRejectsCorruptFingerprintMetadata(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	bad := CredentialFingerprint{}
	bad.CompositeHash[0] = 1
	valid := NewCredentialFingerprint("acct", "refresh", "idx")
	for _, tc := range []struct {
		name        string
		confirmedAt time.Time
		entries     []PersistedRosterEntry
	}{
		{name: "fingerprint", confirmedAt: now, entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: bad}}},
		{name: "mixed priorities", confirmedAt: now, entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: valid}, {ID: "b", AuthIndex: "idx-b", Priority: 1, Fingerprint: NewCredentialFingerprint("acct-b", "refresh-b", "idx-b")}}},
		{name: "future confirmation", confirmedAt: now.Add(time.Nanosecond), entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: valid}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			store := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil)
			if _, err := store.Update(func(s *PersistentState) error {
				s.LastConfirmedRoster = &PersistedConfirmedRoster{Generation: 1, ConfirmedAt: tc.confirmedAt, Entries: tc.entries}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			host := &countingProductionHost{}
			r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer r.coordinator.Close()
			if got := r.ProvisionalRoster(); got != nil {
				t.Fatalf("corrupt roster recovered: %#v", got)
			}
			if host.total() != 0 {
				t.Fatalf("corrupt recovery made calls: %#v", host)
			}
		})
	}
}

func TestProductionProvisionalVerificationMissingAndErrorMakeZeroOpenAI(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	fingerprint := NewCredentialFingerprint("acct", "refresh", "idx")
	for _, tc := range []struct {
		name   string
		auth   map[string]pluginapi.HostAuthGetResponse
		getErr map[string]error
	}{
		{name: "missing", auth: map[string]pluginapi.HostAuthGetResponse{}},
		{name: "error", auth: map[string]pluginapi.HostAuthGetResponse{"idx": {}}, getErr: map[string]error{"idx": errors.New("offline")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			store := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil)
			if _, err := store.Update(func(s *PersistentState) error {
				s.LastConfirmedRoster = &PersistedConfirmedRoster{Generation: 2, ConfirmedAt: now, Entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: fingerprint}}}
				s.Bindings["a"] = RuntimeBinding{AuthID: "a", AuthIndex: "idx", Instance: legacyAuthInstanceID("a"), Generation: 2, Fingerprint: fingerprint}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			host := &countingProductionHost{auth: tc.auth, getErr: tc.getErr}
			r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer r.coordinator.Close()
			provisional := r.ProvisionalRoster()
			if provisional == nil {
				t.Fatal("missing valid provisional")
			}
			if r.VerifyProvisionalRoster(context.Background(), *provisional) {
				t.Fatal("invalid GetAuth verified")
			}
			if host.http != 0 || host.probe != 0 {
				t.Fatalf("invalid verification made OpenAI calls: %#v", host)
			}
		})
	}
}

func TestProductionProvisionalVerificationTracksRiskConfig(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	fingerprint := NewCredentialFingerprint("acct", "refresh", "idx")
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil)
	if _, err := store.Update(func(s *PersistentState) error {
		s.LastConfirmedRoster = &PersistedConfirmedRoster{Generation: 2, ConfirmedAt: now, Entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: fingerprint}}}
		s.Bindings["a"] = RuntimeBinding{AuthID: "a", AuthIndex: "idx", Instance: legacyAuthInstanceID("a"), Generation: 2, Fingerprint: fingerprint}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{"idx": {AuthIndex: "idx", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}}}
	state := NewPluginState(DefaultConfig())
	r, err := NewProductionQuotaRefresher(host, state, &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer r.coordinator.Close()
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	if r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) || host.get != 0 {
		t.Fatalf("default-off verified or called GetAuth: get=%d", host.get)
	}
	cfg := state.Config()
	cfg.ProbeOnProvisionalRoster = true
	state.ReplaceConfig(cfg)
	if !r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) || host.get != 1 {
		t.Fatalf("enabled verification failed: get=%d", host.get)
	}
	cfg.ProbeOnProvisionalRoster = false
	state.ReplaceConfig(cfg)
	if r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) || host.get != 1 {
		t.Fatalf("disabled verification retained access: get=%d", host.get)
	}
}

func TestProductionProvisionalVerificationRejectsCorruptBindingIdentity(t *testing.T) {
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	fingerprint := NewCredentialFingerprint("acct", "refresh", "idx")
	valid := RuntimeBinding{AuthID: "a", AuthIndex: "idx", Instance: legacyAuthInstanceID("a"), Generation: 7, Fingerprint: fingerprint}
	for _, tc := range []struct {
		name   string
		mutate func(*RuntimeBinding)
	}{
		{name: "auth id", mutate: func(b *RuntimeBinding) { b.AuthID = "other" }},
		{name: "zero instance", mutate: func(b *RuntimeBinding) { b.Instance = 0 }},
		{name: "wrong instance", mutate: func(b *RuntimeBinding) { b.Instance++ }},
		{name: "generation", mutate: func(b *RuntimeBinding) { b.Generation = 6 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "state.json")
			binding := valid
			tc.mutate(&binding)
			store := NewStateStore(semanticStatePaths(legacyPath).Runtime, OSFileHooks(), nil)
			if _, err := store.Update(func(s *PersistentState) error {
				s.LastConfirmedRoster = &PersistedConfirmedRoster{Generation: 7, ConfirmedAt: now, Entries: []PersistedRosterEntry{{ID: "a", AuthIndex: "idx", Priority: 9, Fingerprint: fingerprint}}}
				s.Bindings["a"] = binding
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{"idx": {AuthIndex: "idx", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}}}
			state := NewPluginState(DefaultConfig())
			cfg := state.Config()
			cfg.ProbeOnProvisionalRoster = true
			state.ReplaceConfig(cfg)
			r, err := NewProductionQuotaRefresher(host, state, &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer r.coordinator.Close()
			provisional := r.ProvisionalRoster()
			if provisional == nil {
				t.Fatal("missing provisional metadata")
			}
			if r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) {
				t.Fatal("corrupt binding identity verified")
			}
			if host.get != 0 || host.http != 0 {
				t.Fatalf("corrupt binding made calls: get=%d http=%d", host.get, host.http)
			}
		})
	}
}

func TestMarkAuthBlockedPropagatesFirstWriteAndBackupFailures(t *testing.T) {
	for _, op := range []string{"backup-write", "backup-fsync"} {
		t.Run(op, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".runtime-state.json")
			hooks := OSFileHooks()
			hooks.Fail = func(got string) error {
				if got == op {
					return errors.New("injected " + op)
				}
				return nil
			}
			registry, err := NewBindingRegistry(NewStateStore(path, hooks, nil))
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.MarkAuthBlocked("a"); err == nil {
				t.Fatal("expected persistence error")
			}
			if _, ok := registry.Lookup("a"); ok {
				t.Fatal("failed persistence mutated registry")
			}
		})
	}
}

func TestQuotaRefresherProductionDependenciesAreInjected(t *testing.T) {
	dir := t.TempDir()
	credentialHost := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), credentialHost, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(dir, "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if r.runtimeStore == nil || r.bindings == nil || r.credentials == nil {
		t.Fatalf("runtime dependencies missing: %#v", r)
	}
	if _, _, err := r.BootstrapBinding(context.Background(), "candidate-only"); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	if credentialHost.gets != 0 || credentialHost.saves != 0 {
		t.Fatalf("Capability-B external calls get=%d save=%d", credentialHost.gets, credentialHost.saves)
	}
}

func TestProductionRosterAdapterGenesisAndWALSmoke(t *testing.T) {
	host := &fakeHostClient{authList: []pluginapi.HostAuthFileEntry{{ID: "auth-a", AuthIndex: "idx-a", Name: "codex-a.json", Provider: "codex", Priority: 3}}, authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(3)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, roster, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	binding, created, err := r.BootstrapBinding(context.Background(), "auth-a")
	if err != nil || !created {
		t.Fatalf("binding=%#v created=%v err=%v", binding, created, err)
	}
	nextRaw := []byte(`{"access_token":"access-2","refresh_token":"refresh-2","account_id":"acct"}`)
	next := HostAuth{Name: "codex-a.json", Raw: nextRaw, Fingerprint: NewCredentialFingerprint("acct", "refresh-2", "idx-a")}
	if _, err := r.credentials.SaveVersioned(context.Background(), binding.Instance, next, binding.ExecutionToken(1)); err != nil {
		t.Fatal(err)
	}
	if string(host.saved["codex-a.json"]) != string(nextRaw) || host.httpCalls != 0 {
		t.Fatalf("saved=%s httpCalls=%d", host.saved["codex-a.json"], host.httpCalls)
	}
}

func TestRuntimeConstructionFailureAuthorizesZeroExternalCalls(t *testing.T) {
	credentialHost := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	badPath := filepath.Join(t.TempDir()+string(rune(0)), "state.json")
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), credentialHost, HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(1)}}}, badPath, time.Now)
	if err == nil || r != nil {
		t.Fatalf("runtime=%#v err=%v", r, err)
	}
	if credentialHost.gets != 0 || credentialHost.saves != 0 {
		t.Fatalf("construction failure made calls get=%d save=%d", credentialHost.gets, credentialHost.saves)
	}
}

func TestProductionRefresherCapabilityBActualPathMakesZeroCalls(t *testing.T) {
	host := &countingProductionHost{}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	if err := r.RefreshOnce(); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RefreshOnce err=%v", err)
	}
	r.RefreshSoon()
	r.RefreshDueSoon()
	r.RefreshOneSoon("candidate")
	r.RefreshDueCandidatesSoon(pluginapi.SchedulerPickRequest{}, 0)
	if host.total() != 0 {
		t.Fatalf("Capability-B production calls=%#v", host)
	}
}

func TestProductionRosterPublicationBootstrapsHighestTierAndUsesWAL(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"new","refresh_token":"r1","account_id":"acct"}`)}, auth: map[string]pluginapi.HostAuthGetResponse{
		"high": {AuthIndex: "high", Name: "high.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"r0","account_id":"acct"}`)},
		"low":  {AuthIndex: "low", Name: "low.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"r-low","account_id":"low"}`)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "low", AuthIndex: "low", Provider: "codex", Priority: intPtr(1)}, {ID: "high", AuthIndex: "high", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if !running {
		t.Fatal("A publication did not activate requested runtime")
	}
	if _, ok := r.bindings.Lookup("high"); !ok {
		t.Fatal("highest-tier binding missing")
	}
	if _, ok := r.bindings.Lookup("low"); !ok {
		t.Fatal("lower-tier binding missing; across-priority tiers must bind for availability tracking")
	}
	version := r.state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"high": {}}})
	current := CodexCredentials{AccessToken: "old", RefreshToken: "r0", ChatGPTAccountID: "acct", ExpiresAt: time.Unix(1, 0)}
	if _, err := r.refreshAndSaveCredentials(host.auth["high"], current, "high", version); err != nil {
		t.Fatal(err)
	}
	if host.save != 1 || host.savedName != "high.json" {
		t.Fatalf("save=%d name=%q", host.save, host.savedName)
	}
}

func TestProductionRosterPublicationEnablesManualRefresh(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &countingProductionHost{
		httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)},
		auth: map[string]pluginapi.HostAuthGetResponse{
			"a":   {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"a","refresh_token":"ra","account_id":"acct-a"}`)},
			"b":   {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"b","refresh_token":"rb","account_id":"acct-b"}`)},
			"low": {AuthIndex: "low", Name: "low.json", JSON: json.RawMessage(`{"access_token":"low","refresh_token":"rl","account_id":"acct-low"}`)},
		},
	}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)},
		{ID: "low", AuthIndex: "low", Provider: "codex", Priority: intPtr(1)},
	}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}

	admission, _ := state.CPAAdmissionVersioned()
	if !admission.Observed || admission.Priority != 9 || len(admission.AuthIDs) != 3 {
		t.Fatalf("admission = %#v, want observed priority 9 with a, b, and the lower tier", admission)
	}
	for _, id := range []string{"a", "b", "low"} {
		if _, ok := admission.AuthIDs[id]; !ok {
			t.Fatalf("admission missing %q: %#v", id, admission)
		}
	}
	if len(admission.Tiers) != 2 || admission.Tiers[1].Priority != 1 {
		t.Fatalf("admission tiers = %#v, want both tiers with descending priorities", admission.Tiers)
	}

	getBefore, httpBefore := host.get, host.http
	if err := r.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	// All admitted tiers refresh: the lower tier must be tracked to know when
	// it becomes reachable.
	if got := host.get - getBefore; got != 3 {
		t.Fatalf("refresh GetAuth delta = %d, want 3", got)
	}
	if got := host.http - httpBefore; got < 3 {
		t.Fatalf("refresh HTTP delta = %d, want at least 3", got)
	}
	if host.list != 0 {
		t.Fatalf("host ListAuths calls = %d, want confirmed-roster scan", host.list)
	}
	snapshot := state.Snapshot(now)
	if snapshot.LastAuthScanAt.IsZero() || snapshot.CodexAuthCount != 3 || len(snapshot.Accounts) != 3 {
		t.Fatalf("snapshot = %#v, want scanned and populated every admitted tier", snapshot)
	}
	for _, account := range snapshot.Accounts {
		want := 9
		if account.AuthID == "low" {
			want = 1
		}
		if account.Priority != want {
			t.Fatalf("account %s priority = %d, want %d", account.AuthID, account.Priority, want)
		}
	}
	for _, account := range snapshot.Accounts {
		if account.Quota.FiveHour != nil || account.Quota.LongWindow == nil || account.Family != AccountFamilyWeekly {
			t.Fatalf("account quota = %#v, want secondary-only weekly", account)
		}
		status, available, reason, _ := accountQueueState(account, now)
		if status != QueueStatusAvailable || !available || reason != "" {
			t.Fatalf("account %s queue = %s %v %q", account.AuthID, status, available, reason)
		}
	}
}

func TestProductionRosterReplacementReplacesAdmissionAndFencesStaleRefresh(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"a","refresh_token":"ra","account_id":"acct-a"}`)},
		"b": {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"b","refresh_token":"rb","account_id":"acct-b"}`)},
		"c": {AuthIndex: "c", Name: "c.json", JSON: json.RawMessage(`{"access_token":"c","refresh_token":"rc","account_id":"acct-c"}`)},
	}}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	initial := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(1)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(1)},
	}}
	if err := r.PublishAuthoritativeRoster(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	oldAdmission, oldVersion := state.CPAAdmissionVersioned()
	if !oldAdmission.Observed {
		t.Fatalf("initial admission = %#v", oldAdmission)
	}
	state.UpsertQuota(AccountState{AuthID: "a", Provider: "codex", Family: AccountFamilyWeekly})
	state.UpsertQuota(AccountState{AuthID: "b", Provider: "codex", Family: AccountFamilyWeekly})

	replacement := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(1)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(1)},
		{ID: "c", AuthIndex: "c", Provider: "codex", Priority: intPtr(9)},
	}}
	if err := r.PublishAuthoritativeRoster(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	next, nextVersion := state.CPAAdmissionVersioned()
	if nextVersion <= oldVersion || !next.Observed || next.Priority != 9 || len(next.AuthIDs) != 3 {
		t.Fatalf("replacement admission=%#v version=%d old=%d", next, nextVersion, oldVersion)
	}
	if _, ok := next.AuthIDs["c"]; !ok {
		t.Fatalf("replacement admission missing c: %#v", next)
	}
	if state.BeginCPAAdmissionVersionCall(oldVersion) {
		t.Fatal("stale admission version remained current")
	}
	// Across-priority admission keeps demoted accounts at their own tier
	// instead of deleting them; only accounts absent from every tier drop.
	accounts := state.Snapshot(now).Accounts
	if len(accounts) != 2 {
		t.Fatalf("demoted account state = %#v, want both retained at tier 1", accounts)
	}
	for _, account := range accounts {
		if account.Priority != 1 {
			t.Fatalf("demoted account priority = %d, want 1: %#v", account.Priority, account)
		}
	}
}

type rosterPublicationHost struct {
	mu      sync.Mutex
	auth    map[string]pluginapi.HostAuthGetResponse
	gets    []string
	watch   string
	watched chan struct{}
}

func (h *rosterPublicationHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
func (h *rosterPublicationHost) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	h.mu.Lock()
	h.gets = append(h.gets, authIndex)
	response := h.auth[authIndex]
	watch, watched := h.watch, h.watched
	h.mu.Unlock()
	if authIndex == watch && watched != nil {
		select {
		case watched <- struct{}{}:
		default:
		}
	}
	return response, nil
}
func (h *rosterPublicationHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *rosterPublicationHost) Do(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)}, nil
}
func (h *rosterPublicationHost) Log(string, string, map[string]any) {}

func TestProductionRosterPublicationDoesNotExposeNewAdmissionWithOldRoster(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &rosterPublicationHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"old": {AuthIndex: "old", Name: "a.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"ro","account_id":"acct"}`)},
		"new": {AuthIndex: "new", Name: "a.json", JSON: json.RawMessage(`{"access_token":"new","refresh_token":"rn","account_id":"acct"}`)},
	}}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	initial := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{{ID: "a", AuthIndex: "old", Provider: "codex", Priority: intPtr(1)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), initial); err != nil {
		t.Fatal(err)
	}

	publishPaused := make(chan struct{})
	releasePublish := make(chan struct{})
	r.finalPublicationHook = func() {
		close(publishPaused)
		<-releasePublish
	}
	replacement := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{{ID: "a", AuthIndex: "new", Provider: "codex", Priority: intPtr(9)}}}
	publishDone := make(chan error, 1)
	go func() { publishDone <- r.PublishAuthoritativeRoster(context.Background(), replacement) }()
	select {
	case <-publishPaused:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final roster publication")
	}

	host.mu.Lock()
	host.watch = "old"
	host.watched = make(chan struct{}, 1)
	oldGet := host.watched
	host.mu.Unlock()
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- r.RefreshOnce() }()
	select {
	case <-oldGet:
		t.Fatal("new admission authorized refresh against old roster auth index")
	case <-time.After(200 * time.Millisecond):
	}
	close(releasePublish)
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	gets := append([]string(nil), host.gets...)
	host.mu.Unlock()
	if len(gets) == 0 || gets[len(gets)-1] != "new" {
		t.Fatalf("GetAuth sequence = %#v, want refreshed new auth index", gets)
	}
}

func TestProductionCandidateRefreshCannotMutateAuthoritativeAdmission(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &rosterPublicationHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"a","refresh_token":"ra","account_id":"acct-a"}`)},
	}}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	want, wantVersion := state.CPAAdmissionVersioned()
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "candidate", Provider: "codex", Priority: 99}}}
	if err := r.RefreshDueCandidatesOnce(req); err != nil {
		t.Fatal(err)
	}
	got, gotVersion := state.CPAAdmissionVersioned()
	if gotVersion != wantVersion || !equalCPAAdmission(got, want) {
		t.Fatalf("candidate refresh mutated authoritative admission: got=%#v version=%d want=%#v version=%d", got, gotVersion, want, wantVersion)
	}
}

func TestProductionCandidateRefreshCannotOverwriteAdmissionDuringPublication(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &rosterPublicationHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"old": {AuthIndex: "old", Name: "a.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"ro","account_id":"acct"}`)},
		"new": {AuthIndex: "new", Name: "a.json", JSON: json.RawMessage(`{"access_token":"new","refresh_token":"rn","account_id":"acct"}`)},
	}}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	initial := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{{ID: "a", AuthIndex: "old", Provider: "codex", Priority: intPtr(1)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), initial); err != nil {
		t.Fatal(err)
	}

	publishPaused := make(chan struct{})
	releasePublish := make(chan struct{})
	r.finalPublicationHook = func() {
		close(publishPaused)
		<-releasePublish
	}
	replacement := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{{ID: "a", AuthIndex: "new", Provider: "codex", Priority: intPtr(9)}}}
	publishDone := make(chan error, 1)
	go func() { publishDone <- r.PublishAuthoritativeRoster(context.Background(), replacement) }()
	select {
	case <-publishPaused:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final roster publication")
	}

	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "candidate", Provider: "codex", Priority: 99}}}
	candidateDone := make(chan error, 1)
	go func() { candidateDone <- r.RefreshDueCandidatesOnce(req) }()
	time.Sleep(200 * time.Millisecond)
	close(releasePublish)
	if err := <-publishDone; err != nil {
		t.Fatal(err)
	}
	if err := <-candidateDone; err != nil {
		t.Fatal(err)
	}
	got, _ := state.CPAAdmissionVersioned()
	if !got.Observed || got.Priority != 9 || len(got.AuthIDs) != 1 {
		t.Fatalf("candidate refresh overwrote publication admission: %#v", got)
	}
	if _, ok := got.AuthIDs["a"]; !ok {
		t.Fatalf("published admission missing a: %#v", got)
	}
}

func TestProductionRosterReplacementPersistsFencesBeforePublication(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{
		"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"a","refresh_token":"ra","account_id":"acct-a"}`)},
		"b": {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"b","refresh_token":"rb","account_id":"acct-b"}`)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	initial := HostRosterSnapshot{Capability: CapabilityA, Generation: 1, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)},
	}}
	if err = r.PublishAuthoritativeRoster(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	b, ok := r.bindings.Lookup("b")
	if !ok {
		t.Fatal("initial b binding missing")
	}
	if _, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeRetryWait, Deadline: now.Add(time.Minute)}}
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "removed", Phase: ProbeAttemptPrepared}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(b.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeRetryWait, Deadline: now.Add(time.Minute)})

	replacement := HostRosterSnapshot{Capability: CapabilityA, Generation: 2, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)}}}
	if err = r.PublishAuthoritativeRoster(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, ok = r.bindings.Lookup("b"); ok {
		t.Fatal("removed binding retained")
	}
	a, ok := r.bindings.Lookup("a")
	if !ok || a.Generation != 2 {
		t.Fatalf("retained binding generation=%d ok=%v", a.Generation, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok = persisted.ProbeWindows[b.Instance]; ok {
		t.Fatal("removed Probe windows retained")
	}
	if _, ok = persisted.ProbeAttempts[b.Instance]; ok {
		t.Fatal("removed Probe attempt retained")
	}
	if got := highestTierSet(r.runtimeRoster()); len(got) != 1 {
		t.Fatalf("published roster=%v", got)
	}
}

type countingProductionHost struct {
	list, get, http, save, probe int
	auth                         map[string]pluginapi.HostAuthGetResponse
	savedName                    string
	httpResp                     pluginapi.HTTPResponse
	getErr                       map[string]error
	requests                     []pluginapi.HTTPRequest
}

func (h *countingProductionHost) total() int { return h.list + h.get + h.http + h.save + h.probe }
func (h *countingProductionHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	h.list++
	return nil, nil
}
func (h *countingProductionHost) GetAuth(i string) (pluginapi.HostAuthGetResponse, error) {
	h.get++
	if err := h.getErr[i]; err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	return h.auth[i], nil
}
func (h *countingProductionHost) SaveAuth(n string, _ json.RawMessage) error {
	h.save++
	h.savedName = n
	return nil
}
func (h *countingProductionHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.http++
	h.requests = append(h.requests, req)
	if req.URL == codexResetProbeEndpoint {
		h.probe++
	}
	return h.httpResp, nil
}

func TestProductionRosterLifecycleGatesBackgroundRequests(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	probeCfg := DefaultConfig()
	probeCfg.EnableResetProbe = true
	state := NewPluginState(probeCfg)
	version := state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 7, AuthIDs: map[string]struct{}{"a": {}}})
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings

	healthy := ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Instances: []string{"a"}}
	r.ObserveRosterLifecycle(healthy)
	if !r.normalBackgroundAllowed() || !r.probeBackgroundAllowed() {
		t.Fatal("healthy roster did not authorize normal and probe background work")
	}

	degraded := healthy
	degraded.Health = RosterDegraded
	degraded.DegradedSince = time.Now().Add(-time.Minute)
	r.ObserveRosterLifecycle(degraded)
	if _, err := r.doWithAdmissionPermit("a", version, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/quota"}); err != nil {
		t.Fatal(err)
	}
	if len(host.requests) != 1 || host.requests[0].Headers.Get(rosterLifecycleRequestHeader) != rosterLifecycleDegraded {
		t.Fatalf("degraded request marker=%q requests=%#v", host.requests[0].Headers.Get(rosterLifecycleRequestHeader), host.requests)
	}

	failClosed := degraded
	failClosed.Health = RosterFailClosed
	failClosed.BackgroundAllowed = false
	r.ObserveRosterLifecycle(failClosed)
	if r.normalBackgroundAllowed() || r.probeBackgroundAllowed() {
		t.Fatal("fail-closed roster authorized background work")
	}
	if _, err := r.doWithAdmissionPermit("a", version, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/quota"}); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("fail-closed HTTP err=%v", err)
	}
	if len(host.requests) != 1 {
		t.Fatalf("fail-closed started HTTP: %#v", host.requests)
	}

	cfg := state.Config()
	cfg.ProbeOnProvisionalRoster = true
	state.ReplaceConfig(cfg)
	provisional := ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true, ConfirmedAt: r.now(), Instances: []string{"a"}}
	r.ObserveRosterLifecycle(provisional)
	if r.normalBackgroundAllowed() || !r.probeBackgroundAllowed() {
		t.Fatalf("provisional gates normal=%v probe=%v", r.normalBackgroundAllowed(), r.probeBackgroundAllowed())
	}
}

func TestQuotaRefresherLifecycleCannotBeBypassedWithoutRuntimeStore(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	r := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
	for _, probe := range []bool{false, true} {
		if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, probe); !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("probe=%v err=%v", probe, err)
		}
	}
	if host.http != 0 {
		t.Fatalf("nil runtime store bypassed lifecycle: calls=%d", host.http)
	}
}

func TestProductionCandidateRefreshDeniedBeforeAdmissionMutation(t *testing.T) {
	host := &countingProductionHost{}
	state := NewPluginState(DefaultConfig())
	initial := CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"old": {}}}
	initialVersion := state.ReplaceCPAAdmission(initial)
	r := NewQuotaRefresher(host, state, time.Now)
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "new", Provider: "codex", Priority: 9}}}
	if err := r.RefreshDueCandidatesOnce(req); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	got, version := state.CPAAdmissionVersioned()
	if version != initialVersion || got.Priority != initial.Priority {
		t.Fatalf("admission mutated while fail-closed: state=%#v version=%d want=%d", got, version, initialVersion)
	}
	if _, ok := got.AuthIDs["old"]; !ok || len(got.AuthIDs) != 1 {
		t.Fatalf("admission IDs mutated while fail-closed: %#v", got.AuthIDs)
	}
	if host.total() != 0 {
		t.Fatalf("candidate refresh made host calls=%#v", host)
	}
}

type lifecycleFenceHost struct {
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
}

func (h *lifecycleFenceHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
func (h *lifecycleFenceHost) GetAuth(string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{}, nil
}
func (h *lifecycleFenceHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *lifecycleFenceHost) Log(string, string, map[string]any)     {}
func (h *lifecycleFenceHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	select {
	case <-h.entered:
	default:
		close(h.entered)
	}
	<-h.release
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
}

func TestProductionLifecyclePublicationFencesHTTPStart(t *testing.T) {
	host := &lifecycleFenceHost{entered: make(chan struct{}), release: make(chan struct{})}
	r := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterDegraded, BackgroundAllowed: true})
	requestDone := make(chan error, 1)
	go func() {
		_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, false)
		requestDone <- err
	}()
	<-host.entered
	observeStarted := make(chan struct{})
	observeDone := make(chan struct{})
	go func() {
		close(observeStarted)
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		close(observeDone)
	}()
	<-observeStarted
	select {
	case <-observeDone:
		t.Fatal("FailClosed published while an authorized HTTP start boundary was open")
	case <-time.After(50 * time.Millisecond):
	}
	close(host.release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	<-observeDone
	if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, false); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("post-publication request err=%v", err)
	}
}

func TestProductionProbeLifecycleHostCallGates(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	r, err := NewProductionQuotaRefresher(host, NewPluginState(cfg), &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })

	t.Run("enqueue denial", func(t *testing.T) {
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("err=%v", err)
		}
		if host.http != 0 {
			t.Fatalf("denied Probe made host HTTP calls=%d", host.http)
		}
	})

	t.Run("final start denial", func(t *testing.T) {
		r.rosterMu.Lock()
		started := make(chan error, 1)
		go func() {
			_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/probe"}, true)
			started <- err
		}()
		r.roster = hostRosterSnapshotFromActive(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		r.rosterMu.Unlock()
		if err := <-started; !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("err=%v", err)
		}
		if host.http != 0 {
			t.Fatalf("final-start denial made host HTTP calls=%d", host.http)
		}
	})

	t.Run("provisional marker", func(t *testing.T) {
		cfg := r.state.Config()
		cfg.ProbeOnProvisionalRoster = true
		r.state.ReplaceConfig(cfg)
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true, ConfirmedAt: r.now()})
		if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/probe"}, true); err != nil {
			t.Fatal(err)
		}
		if got := host.requests[len(host.requests)-1].Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("marker=%q", got)
		}
	})
}
func (h *countingProductionHost) Log(string, string, map[string]any) {}

func TestStateStoreQuarantinesCorruptionAndRepairsFromBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	if _, err := store.Update(func(s *PersistentState) error { s.ReservedCeiling = 7; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(s *PersistentState) error { s.ReservedCeiling = 8; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad-primary"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := NewStateStore(path, OSFileHooks(), nil)
	got, report, err := fresh.Load()
	if err != nil || !report.UsedBackup || got.ReservedCeiling != 7 {
		t.Fatalf("got=%#v report=%#v err=%v", got, report, err)
	}
	if raw, err := os.ReadFile(path + ".corrupt"); err != nil || string(raw) != "{bad-primary" {
		t.Fatalf("primary evidence=%q err=%v", raw, err)
	}
	if _, _, err := NewStateStore(path, OSFileHooks(), nil).Load(); err != nil {
		t.Fatalf("repaired primary unreadable: %v", err)
	}
}

func TestRosterPublicationStaysFailClosedUntilAllGenesisSucceeds(t *testing.T) {
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"x","refresh_token":"ra","account_id":"a"}`)}, "b": {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"x","refresh_token":"rb","account_id":"b"}`)}}, getErr: map[string]error{"b": errors.New("bootstrap failed")}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err == nil {
		t.Fatal("expected second genesis failure")
	}
	if r.runtimeRoster().Capability != CapabilityB {
		t.Fatal("partial genesis published Capability-A")
	}
	before := host.total()
	if err := r.RefreshOnce(); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("refresh err=%v", err)
	}
	r.RefreshSoon()
	r.RefreshDueSoon()
	r.RefreshOneSoon("a")
	r.RefreshDueCandidatesSoon(pluginapi.SchedulerPickRequest{}, 0)
	if host.total() != before {
		t.Fatalf("failed publication authorized calls before=%d after=%d", before, host.total())
	}
	delete(host.getErr, "b")
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	if r.runtimeRoster().Capability != CapabilityA {
		t.Fatal("successful retry did not publish A")
	}
}

func TestStateStoreDualCorruptionPreservesBothEvidenceAndFences(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	os.WriteFile(path, []byte("{p"), 0600)
	os.WriteFile(path+".bak", []byte("{b"), 0600)
	got, report, err := NewStateStore(path, OSFileHooks(), nil).Load()
	if err != nil || !report.FenceUnsafe || !got.FenceUnsafe {
		t.Fatalf("got=%#v report=%#v err=%v", got, report, err)
	}
	for name, want := range map[string]string{path + ".corrupt": "{p", path + ".bak.corrupt": "{b"} {
		raw, e := os.ReadFile(name)
		if e != nil || string(raw) != want {
			t.Fatalf("%s=%q err=%v", name, raw, e)
		}
	}
}

func TestUserDataSchemaMigrationAndCorruptionEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":0,"config":{},"accounts":{"a":{"alias":"old"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserData(path)
	if err != nil || !loaded || got.Accounts["a"].Alias != "old" {
		t.Fatalf("got=%#v loaded=%v err=%v", got, loaded, err)
	}
	raw, _ := os.ReadFile(path)
	var env struct {
		SchemaVersion int `json:"schema_version"`
	}
	json.Unmarshal(raw, &env)
	if env.SchemaVersion != CurrentUserDataSchema {
		t.Fatalf("schema=%d raw=%s", env.SchemaVersion, raw)
	}
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "new"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad-user"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserData(path); err != nil {
		t.Fatal(err)
	}
	evidence, e := os.ReadFile(path + ".corrupt")
	if e != nil || string(evidence) != "{bad-user" {
		t.Fatalf("evidence=%q err=%v", evidence, e)
	}
}

func TestUserDataFutureSchemaFailsSafeAndDualCorruptionPreservesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	future := []byte(`{"schema_version":99,"config":{}}`)
	if err := os.WriteFile(path, future, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserData(path); !errors.Is(err, ErrUserDataFutureSchema) {
		t.Fatalf("future err=%v", err)
	}
	raw, _ := os.ReadFile(path)
	if !reflect.DeepEqual(raw, future) {
		t.Fatal("future schema rewritten")
	}
	os.Remove(path)
	os.WriteFile(path, []byte("{p"), 0600)
	os.WriteFile(path+".bak", []byte("{b"), 0600)
	_, _, report, err := loadUserDataRecover(path)
	if err != nil || !report.RecoveredDefault {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	for name, want := range map[string]string{path + ".corrupt": "{p", path + ".bak.corrupt": "{b"} {
		got, e := os.ReadFile(name)
		if e != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v", name, got, e)
		}
	}
}

func intPtr(v int) *int { return &v }

func TestSuiteRuntimeWiring(t *testing.T) {
	t.Run("semantic migration", TestRuntimeSemanticPathsAndLegacyMigration)
	t.Run("corrupt legacy", TestRuntimeCorruptLegacyIsPreserved)
	t.Run("strict legacy", TestLegacyMigrationRejectsSensitiveAndUnknownFieldsWithoutRename)
	t.Run("legacy value secrets", TestLegacyMigrationRejectsCredentialSyntaxInsideAllowedStringValues)
	t.Run("benign token text", TestLegacyMigrationAllowsBenignTokenMentionAndScansRealArtifact)
	t.Run("actual migrated scan", TestValidLegacyMigrationScansActualRetainedArtifact)
	t.Run("user backup", TestUserDataRecoversFromAtomicBackup)
	t.Run("migration crashes", TestUserDataMigrationCrashPointsConverge)
	t.Run("sensitive scan", TestRuntimeAndUserArtifactsNeverPersistSensitiveValues)
	t.Run("probe seam", TestProbeAttemptSeamRuntimeRoundTrip)
	t.Run("genesis wal", TestBindingGenesisAndInjectedCredentialWAL)
	t.Run("fail closed", TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked)
	t.Run("blocked errors", TestMarkAuthBlockedPropagatesFirstWriteAndBackupFailures)
	t.Run("production injection", TestQuotaRefresherProductionDependenciesAreInjected)
	t.Run("adapter wal smoke", TestProductionRosterAdapterGenesisAndWALSmoke)
	t.Run("construction failure", TestRuntimeConstructionFailureAuthorizesZeroExternalCalls)
	t.Run("actual B path", TestProductionRefresherCapabilityBActualPathMakesZeroCalls)
	t.Run("roster publication", TestProductionRosterPublicationBootstrapsHighestTierAndUsesWAL)
	t.Run("runtime corruption repair", TestStateStoreQuarantinesCorruptionAndRepairsFromBackup)
	t.Run("atomic roster publication", TestRosterPublicationStaysFailClosedUntilAllGenesisSucceeds)
	t.Run("runtime dual corruption", TestStateStoreDualCorruptionPreservesBothEvidenceAndFences)
	t.Run("user schema corruption", TestUserDataSchemaMigrationAndCorruptionEvidence)
	t.Run("user future dual corruption", TestUserDataFutureSchemaFailsSafeAndDualCorruptionPreservesEvidence)
}
