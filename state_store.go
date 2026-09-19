package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const CurrentStateSchema = 2

var ErrStateReadOnly = errors.New("state store is read-only")

type PersistentState struct {
	SchemaVersion       int                                                `json:"schema_version"`
	ReservedCeiling     uint64                                             `json:"reserved_ceiling"`
	NextSaveSeq         uint64                                             `json:"next_save_seq"`
	TierGeneration      TierGeneration                                     `json:"tier_generation,omitempty"`
	AdmissionEpochs     map[AuthInstanceID]InstanceAdmissionEpoch          `json:"admission_epochs,omitempty"`
	CredentialChains    map[AuthInstanceID]TransitionChain                 `json:"credential_chains,omitempty"`
	FenceUnsafe         bool                                               `json:"fence_unsafe,omitempty"`
	Bindings            map[string]RuntimeBinding                          `json:"bindings,omitempty"`
	ProbeAttempts       map[AuthInstanceID]ProbeAttemptSeam                `json:"probe_attempts,omitempty"`
	ProbeAttemptSeq     uint64                                             `json:"probe_attempt_seq,omitempty"`
	ProbeWindows        map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow `json:"probe_windows,omitempty"`
	LastConfirmedRoster *PersistedConfirmedRoster                          `json:"last_confirmed_roster,omitempty"`
	ManagedLifecycle    map[string]ManagedDisableRecord                    `json:"managed_lifecycle,omitempty"`
}

type PersistedRosterEntry struct {
	ID          string                `json:"id"`
	AuthIndex   string                `json:"auth_index"`
	Priority    int                   `json:"priority"`
	Fingerprint CredentialFingerprint `json:"fingerprint"`
}

type PersistedConfirmedRoster struct {
	Generation  uint64                 `json:"generation"`
	ConfirmedAt time.Time              `json:"confirmed_at"`
	Entries     []PersistedRosterEntry `json:"entries"`
}

func NewPersistentState() PersistentState {
	return PersistentState{SchemaVersion: CurrentStateSchema, AdmissionEpochs: map[AuthInstanceID]InstanceAdmissionEpoch{}, CredentialChains: map[AuthInstanceID]TransitionChain{}, Bindings: map[string]RuntimeBinding{}, ProbeAttempts: map[AuthInstanceID]ProbeAttemptSeam{}, ProbeWindows: map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}, ManagedLifecycle: map[string]ManagedDisableRecord{}}
}
func clonePersistentState(s PersistentState) PersistentState {
	raw, _ := json.Marshal(s)
	var out PersistentState
	_ = json.Unmarshal(raw, &out)
	out.FenceUnsafe = s.FenceUnsafe
	if out.AdmissionEpochs == nil {
		out.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
	}
	if out.ManagedLifecycle == nil {
		out.ManagedLifecycle = map[string]ManagedDisableRecord{}
	}
	if out.CredentialChains == nil {
		out.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	if out.Bindings == nil {
		out.Bindings = map[string]RuntimeBinding{}
	}
	if out.ProbeAttempts == nil {
		out.ProbeAttempts = map[AuthInstanceID]ProbeAttemptSeam{}
	}
	if out.ProbeWindows == nil {
		out.ProbeWindows = map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}
	}
	return out
}

type RecoveryReport struct{ Migrated, ReadOnly, UsedBackup, RecoveredEmpty, FenceUnsafe bool }
type FileHooks struct {
	Observe  func(string)
	Fail     func(string) error
	Replace  func(string, string) error
	SyncDir  func(string) error
	ReadFile func(string) ([]byte, error)
}

func OSFileHooks() FileHooks {
	return FileHooks{Replace: replaceFileAtomic, SyncDir: syncDirectory, ReadFile: os.ReadFile}
}
func (h FileHooks) event(op string) error {
	if h.Observe != nil {
		h.Observe(op)
	}
	if h.Fail != nil {
		return h.Fail(op)
	}
	return nil
}

type StateStore struct {
	mu               sync.Mutex
	path             string
	hooks            FileHooks
	crash            CrashHitter
	readOnly, loaded bool
	state            PersistentState
}

func NewStateStore(path string, hooks FileHooks, crash CrashHitter) *StateStore {
	defaults := OSFileHooks()
	if hooks.Replace == nil {
		hooks.Replace = defaults.Replace
	}
	if hooks.SyncDir == nil {
		hooks.SyncDir = defaults.SyncDir
	}
	if hooks.ReadFile == nil {
		hooks.ReadFile = defaults.ReadFile
	}
	return &StateStore{path: path, hooks: hooks, crash: crash}
}
func (s *StateStore) hit(name string) error {
	if s.crash != nil {
		return s.crash.Hit(name)
	}
	return nil
}
func (s *StateStore) Load() (PersistentState, RecoveryReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}
func (s *StateStore) loadLocked() (PersistentState, RecoveryReport, error) {
	state, err := s.readPersistentState(s.path)
	report := RecoveryReport{}
	if err != nil {
		primaryErr := err
		if !errors.Is(primaryErr, os.ErrNotExist) && !isStateCorruption(primaryErr) {
			return PersistentState{}, report, primaryErr
		}
		state, err = s.readPersistentState(s.path + ".bak")
		if err == nil {
			report.UsedBackup = true
			if isStateCorruption(primaryErr) {
				if qerr := s.quarantine(s.path); qerr != nil {
					return PersistentState{}, report, qerr
				}
				raw, _ := json.MarshalIndent(state, "", "  ")
				raw = append(raw, '\n')
				if werr := s.writeAtomic(s.path, raw, "primary"); werr != nil {
					return PersistentState{}, report, werr
				}
			}
		} else if errors.Is(primaryErr, os.ErrNotExist) && errors.Is(err, os.ErrNotExist) {
			state = NewPersistentState()
		} else if !errors.Is(err, os.ErrNotExist) && !isStateCorruption(err) {
			return PersistentState{}, report, err
		} else {
			if isStateCorruption(primaryErr) {
				if qerr := s.quarantine(s.path); qerr != nil {
					return PersistentState{}, report, qerr
				}
			}
			if isStateCorruption(err) {
				if qerr := s.quarantine(s.path + ".bak"); qerr != nil {
					return PersistentState{}, report, qerr
				}
			}
			state = NewPersistentState()
			state.FenceUnsafe = true
			report.RecoveredEmpty = true
			report.FenceUnsafe = true
			if writeErr := s.writeLocked(state); writeErr != nil {
				return PersistentState{}, report, writeErr
			}
			return clonePersistentState(state), report, nil
		}
	}
	if state.SchemaVersion > CurrentStateSchema {
		s.readOnly = true
		report.ReadOnly = true
	}
	if state.SchemaVersion < CurrentStateSchema {
		state.SchemaVersion = CurrentStateSchema
		report.Migrated = true
	}
	if state.CredentialChains == nil {
		state.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	if state.AdmissionEpochs == nil {
		state.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
	}
	if state.Bindings == nil {
		state.Bindings = map[string]RuntimeBinding{}
	}
	if state.ProbeAttempts == nil {
		state.ProbeAttempts = map[AuthInstanceID]ProbeAttemptSeam{}
	}
	if state.ProbeWindows == nil {
		state.ProbeWindows = map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}
	}
	s.state = state
	s.loaded = true
	if state.FenceUnsafe {
		report.FenceUnsafe = true
	}
	return clonePersistentState(state), report, nil
}

func (s *StateStore) quarantine(path string) error {
	if _, err := s.hooks.ReadFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	target := path + ".corrupt"
	if _, err := s.hooks.ReadFile(target); err == nil {
		return fmt.Errorf("corrupt evidence already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.hooks.Replace(path, target); err != nil {
		return err
	}
	return s.hooks.SyncDir(filepath.Dir(path))
}

var errStateCorrupt = errors.New("state corrupt")

func isStateCorruption(err error) bool { return errors.Is(err, errStateCorrupt) }
func (s *StateStore) readPersistentState(path string) (PersistentState, error) {
	raw, err := s.hooks.ReadFile(path)
	if err != nil {
		return PersistentState{}, err
	}
	var st PersistentState
	if err = json.Unmarshal(raw, &st); err != nil {
		return PersistentState{}, fmt.Errorf("%w: %v", errStateCorrupt, err)
	}
	return st, nil
}
func (s *StateStore) PersistentSnapshot() (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		if _, _, err := s.loadLocked(); err != nil {
			return PersistentState{}, err
		}
	}
	return clonePersistentState(s.state), nil
}
func (s *StateStore) Update(fn func(*PersistentState) error) (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		if _, _, err := s.loadLocked(); err != nil {
			return PersistentState{}, err
		}
	}
	next := clonePersistentState(s.state)
	if err := fn(&next); err != nil {
		return PersistentState{}, err
	}
	if err := s.writeLocked(next); err != nil {
		return PersistentState{}, err
	}
	return clonePersistentState(s.state), nil
}
func (s *StateStore) UpdateMirrored(fn func(*PersistentState) error) (PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		if _, _, err := s.loadLocked(); err != nil {
			return PersistentState{}, err
		}
	}
	next := clonePersistentState(s.state)
	if err := fn(&next); err != nil {
		return PersistentState{}, err
	}
	if err := s.writeLockedMode(next, true); err != nil {
		return PersistentState{}, err
	}
	return clonePersistentState(s.state), nil
}
func (s *StateStore) WriteThrough(state PersistentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(state)
}
func (s *StateStore) writeLocked(state PersistentState) error {
	return s.writeLockedMode(state, false)
}
func (s *StateStore) writeLockedMode(state PersistentState, mirrorBackup bool) error {
	if s.readOnly {
		return ErrStateReadOnly
	}
	if s.loaded && s.state.FenceUnsafe {
		state.FenceUnsafe = true
	}
	state.SchemaVersion = CurrentStateSchema
	if state.CredentialChains == nil {
		state.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	if state.AdmissionEpochs == nil {
		state.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
	}
	if state.Bindings == nil {
		state.Bindings = map[string]RuntimeBinding{}
	}
	if state.ProbeAttempts == nil {
		state.ProbeAttempts = map[AuthInstanceID]ProbeAttemptSeam{}
	}
	if state.ProbeWindows == nil {
		state.ProbeWindows = map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	current, readErr := s.hooks.ReadFile(s.path)
	if mirrorBackup {
		current = raw
		readErr = nil
	}
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		current = raw
	}
	if err = s.writeAtomic(s.path+".bak", current, "backup"); err != nil {
		return err
	}
	if err = s.writeAtomic(s.path, raw, "primary"); err != nil {
		return err
	}
	s.state = clonePersistentState(state)
	s.loaded = true
	return nil
}
func (s *StateStore) writeAtomic(target string, raw []byte, prefix string) error {
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	cleanup := func(e error) error { _ = f.Close(); return e }
	if err = s.hooks.event(prefix + "-write"); err != nil {
		return cleanup(err)
	}
	if _, err = f.Write(raw); err != nil {
		return cleanup(err)
	}
	if err = s.hooks.event(prefix + "-fsync"); err != nil {
		return cleanup(err)
	}
	if err = f.Sync(); err != nil {
		return cleanup(err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if prefix == "primary" { //kpoint:K_STATE_BEFORE_RENAME
		if err = s.hit("K_STATE_BEFORE_RENAME"); err != nil {
			return err
		}
		if err = s.hooks.event("rename-before"); err != nil {
			return err
		}
	}
	if err = s.hooks.Replace(tmp, target); err != nil {
		return err
	}
	if prefix == "primary" {
		if err = s.hooks.event("rename-after"); err != nil {
			return err
		} //kpoint:K_STATE_AFTER_RENAME
		if err = s.hit("K_STATE_AFTER_RENAME"); err != nil {
			return err
		}
	}
	if err = s.hooks.event(prefix + "-dir-fsync"); err != nil {
		return err
	}
	return s.hooks.SyncDir(filepath.Dir(target))
}
