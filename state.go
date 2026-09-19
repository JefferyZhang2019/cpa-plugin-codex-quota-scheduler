package main

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

type PluginState struct {
	mu                  sync.RWMutex
	cfg                 Config
	accounts            map[string]AccountState
	cpaAdmission        CPAAdmissionState
	cpaAdmissionVersion uint64
	annotations         AnnotationState
	logs                []LogEntry
	lastSelected        string
	lastReason          string
	lastCodexActivityAt time.Time
	lastAuthScanAt      time.Time
	codexAuthCount      int
}

func NewPluginState(cfg Config) *PluginState {
	return &PluginState{
		cfg:         NormalizeConfig(cfg),
		accounts:    make(map[string]AccountState),
		annotations: NormalizeAnnotationState(AnnotationState{}),
	}
}

func (s *PluginState) cloneForLegacyTransaction() *PluginState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := NewPluginState(s.cfg)
	for k, v := range s.accounts {
		out.accounts[k] = cloneAccountState(v)
	}
	out.cpaAdmission = cloneCPAAdmission(s.cpaAdmission)
	out.cpaAdmissionVersion = s.cpaAdmissionVersion
	out.annotations = cloneAnnotationState(s.annotations)
	out.logs = cloneLogs(s.logs)
	out.lastSelected, out.lastReason, out.lastCodexActivityAt, out.lastAuthScanAt, out.codexAuthCount = s.lastSelected, s.lastReason, s.lastCodexActivityAt, s.lastAuthScanAt, s.codexAuthCount
	return out
}

func (s *PluginState) legacyEffectJournal(authID string, baselineLogs int) LegacyEffectJournal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j := LegacyEffectJournal{}
	for _, account := range s.accounts {
		if account.AuthID == authID {
			j.Accounts = append(j.Accounts, cloneAccountState(account))
		}
	}
	if baselineLogs < len(s.logs) {
		j.Logs = cloneLogs(s.logs[baselineLogs:])
	}
	return j
}

func (s *PluginState) applyLegacyEffectJournal(j LegacyEffectJournal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range j.Accounts {
		if key := accountStateKey(account); key != "" {
			s.accounts[key] = cloneAccountState(account)
		}
	}
	s.logs = append(s.logs, cloneLogs(j.Logs)...)
}

func (s *PluginState) ReplaceConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = NormalizeConfig(cfg)
}

func (s *PluginState) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *PluginState) SetAnnotations(state AnnotationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.annotations = cloneAnnotationState(NormalizeAnnotationState(state))
}

func (s *PluginState) Annotations() AnnotationState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAnnotationState(s.annotations)
}

func cloneCPAAdmission(value CPAAdmissionState) CPAAdmissionState {
	cloned := CPAAdmissionState{Observed: value.Observed, Priority: value.Priority}
	if len(value.AuthIDs) > 0 {
		cloned.AuthIDs = make(map[string]struct{}, len(value.AuthIDs))
		for authID := range value.AuthIDs {
			cloned.AuthIDs[authID] = struct{}{}
		}
	}
	return cloned
}

func equalCPAAdmission(left, right CPAAdmissionState) bool {
	if left.Observed != right.Observed || left.Priority != right.Priority || len(left.AuthIDs) != len(right.AuthIDs) {
		return false
	}
	for authID := range left.AuthIDs {
		if _, ok := right.AuthIDs[authID]; !ok {
			return false
		}
	}
	return true
}

func (s *PluginState) ReplaceCPAAdmission(value CPAAdmissionState) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if equalCPAAdmission(s.cpaAdmission, value) {
		return s.cpaAdmissionVersion
	}
	s.cpaAdmissionVersion++
	s.cpaAdmission = cloneCPAAdmission(value)
	if !value.Observed {
		return s.cpaAdmissionVersion
	}
	for key, account := range s.accounts {
		if _, ok := value.AuthIDs[account.AuthID]; !ok {
			delete(s.accounts, key)
			continue
		}
		account.Priority = value.Priority
		s.accounts[key] = account
	}
	return s.cpaAdmissionVersion
}

func (s *PluginState) BeginCPAAdmissionCall(authID string, version uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.admissionCurrentLocked(authID, version)
}

func (s *PluginState) BeginCPAAdmissionVersionCall(version uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cpaAdmission.Observed && s.cpaAdmissionVersion == version
}

func (s *PluginState) CPAAdmission() CPAAdmissionState {
	admission, _ := s.CPAAdmissionVersioned()
	return admission
}

func (s *PluginState) CPAAdmissionVersioned() (CPAAdmissionState, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCPAAdmission(s.cpaAdmission), s.cpaAdmissionVersion
}

func (s *PluginState) IsAuthAdmitted(authID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cpaAdmission.Observed || authID == "" {
		return false
	}
	_, ok := s.cpaAdmission.AuthIDs[authID]
	return ok
}

func (s *PluginState) ExecutionTokenCurrent(authID string, token ExecutionToken) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, ok := s.accounts["auth:"+authID]
	return ok && ValidateWriteback(BindingVersion{Instance: account.Instance, Admission: account.AdmissionEpoch, Tier: account.TierGeneration, Login: account.LoginEpoch, Fingerprint: account.CredentialFingerprint}, WritebackVersion{Token: token, Login: account.LoginEpoch, Fingerprint: account.CredentialFingerprint})
}

func (s *PluginState) AdmittedCPAPriority(authID string) (int, bool) {
	priority, _, ok := s.AdmittedCPAPriorityVersioned(authID)
	return priority, ok
}

func (s *PluginState) AdmittedCPAPriorityVersioned(authID string) (int, uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cpaAdmission.Observed || authID == "" {
		return 0, s.cpaAdmissionVersion, false
	}
	_, ok := s.cpaAdmission.AuthIDs[authID]
	return s.cpaAdmission.Priority, s.cpaAdmissionVersion, ok
}

func (s *PluginState) CPAAdmissionVersionCurrent(version uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cpaAdmissionVersion == version
}

func (s *PluginState) admissionCurrentLocked(authID string, version uint64) bool {
	if s.cpaAdmissionVersion != version || !s.cpaAdmission.Observed || authID == "" {
		return false
	}
	_, ok := s.cpaAdmission.AuthIDs[authID]
	return ok
}

func (s *PluginState) UpsertQuota(account AccountState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := accountStateKey(account)
	if key == "" {
		return
	}
	if !account.LastSuccessAt.IsZero() && account.LastError == "" {
		account.Refresh = AccountRefreshState{}
	}
	s.accounts[key] = cloneAccountState(account)
}

func (s *PluginState) MarkAccountTemporaryExhausted(authID string, resetAt time.Time, reason string) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts["auth:"+authID]
	if !ok {
		account = AccountState{AuthID: authID}
	}
	markTemporaryExhausted(&account, resetAt, reason)
	s.accounts["auth:"+authID] = account
}

func (s *PluginState) MarkAccountTemporaryExhaustedByAuthIndex(authIndex string, resetAt time.Time, reason string) {
	if authIndex == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.AuthIndex != authIndex {
			continue
		}
		markTemporaryExhausted(&account, resetAt, reason)
		s.accounts[key] = account
		return
	}
}

func (s *PluginState) RecordAccountFailure(authID, authIndex, reason string, resetAt, now time.Time) (AccountState, bool) {
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		if authID == "" {
			return AccountState{}, false
		}
		key = "auth:" + authID
		account = AccountState{AuthID: authID, AuthIndex: authIndex, Provider: "codex"}
	}
	applyCircuitFailure(&account, NormalizeConfig(s.cfg), reason, resetAt, now)
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) RecordAccountSuccess(authID, authIndex string, now time.Time) (AccountState, bool) {
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		return AccountState{}, false
	}
	// A real successful request is the strongest recovery evidence: upstream
	// quota percentages alone can misreport recovery (issue #11 negative
	// control), so the temporary-exhaustion marker is only reconciled here and
	// on operator-confirmed manual refreshes, never from a bare quota read.
	account.TemporaryExhausted = false
	account.TemporaryResetAt = time.Time{}
	applyCircuitSuccess(&account, NormalizeConfig(s.cfg), now)
	s.accounts[key] = account
	return cloneAccountState(account), true
}

// ClearAccountTemporaryExhausted drops a stale temporary-exhaustion marker for
// one account. Callers must hold verified recovery evidence: either a
// successful real request (RecordAccountSuccess) or an operator-requested
// refresh whose fresh quota snapshot shows remaining capacity in every known
// window (QuotaRefresher.reconcileTemporaryExhausted).
func (s *PluginState) ClearAccountTemporaryExhausted(authID string) bool {
	if authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, "")
	if !ok || !account.TemporaryExhausted {
		return false
	}
	account.TemporaryExhausted = false
	account.TemporaryResetAt = time.Time{}
	s.accounts[key] = account
	return true
}

// ObserveAccountQuota merges quota state observed inside a real response
// stream into the cached account. An observation is bound to an actual call,
// so it both refreshes the cache (deferring the polling refresh) and runs the
// same window-identity reconciliation as a polled refresh. Only the windows
// present in the observation are overwritten; everything else is preserved.
func (s *PluginState) ObserveAccountQuota(authID, authIndex string, quota ParsedQuota, now time.Time) (AccountState, bool) {
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		return AccountState{}, false
	}
	if quota.FiveHour != nil {
		account.Quota.FiveHour = quota.FiveHour
	}
	if quota.LongWindow != nil {
		account.Quota.LongWindow = quota.LongWindow
	}
	if quota.Family != "" {
		account.Quota.Family = quota.Family
		account.Family = quota.Family
	}
	account.LastRefreshAt = now
	account.LastSuccessAt = now
	account.LastObservedAt = now
	account.Stale = false
	if account.TemporaryExhausted && quotaRefreshConfirmsReset(account, now) {
		account.TemporaryExhausted = false
		account.TemporaryResetAt = time.Time{}
		account.LastError = ""
	}
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) findAccountLocked(authID, authIndex string) (string, AccountState, bool) {
	if authID != "" {
		if account, ok := s.accounts["auth:"+authID]; ok {
			return "auth:" + authID, account, true
		}
	}
	if authIndex != "" {
		for key, account := range s.accounts {
			if account.AuthIndex == authIndex {
				return key, account, true
			}
		}
	}
	return "", AccountState{}, false
}

func (s *PluginState) RecordSelection(authID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSelected = authID
	s.lastReason = reason
}

func (s *PluginState) RecordCodexActivity(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCodexActivityAt.IsZero() || now.After(s.lastCodexActivityAt) {
		s.lastCodexActivityAt = now
	}
}

func (s *PluginState) RecordAuthScan(codexAuthCount int, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	if codexAuthCount < 0 {
		codexAuthCount = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAuthScanAt = now
	s.codexAuthCount = codexAuthCount
}

func (s *PluginState) RecordAuthScanIfAdmissionCurrent(version uint64, codexAuthCount int, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	if codexAuthCount < 0 {
		codexAuthCount = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cpaAdmissionVersion != version {
		return false
	}
	s.lastAuthScanAt = now
	s.codexAuthCount = codexAuthCount
	return true
}

func (s *PluginState) RefreshActive(now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	return !s.lastCodexActivityAt.IsZero() && now.Before(s.lastCodexActivityAt.Add(cfg.RefreshActiveWindow))
}

func (s *PluginState) RecordRefreshFailure(authID, authIndex string, kind RefreshFailureKind, message string, now time.Time) (AccountState, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		if authID == "" {
			return AccountState{}, false
		}
		key = "auth:" + authID
		account = AccountState{AuthID: authID, AuthIndex: authIndex, Provider: "codex"}
	}
	account.LastError = message
	account.Refresh.LastFailureKind = kind
	account.Refresh.LastFailureAt = now
	if kind == RefreshFailureAuth || kind == RefreshFailureLocal {
		account.Refresh.AuthFailure = kind == RefreshFailureAuth
		account.Refresh.NextRetryAt = time.Time{}
	} else {
		account.Refresh.AuthFailure = false
		account.Refresh.RetryAttempt++
		account.Refresh.NextRetryAt = now.Add(retryDelayForAttempt(NormalizeConfig(s.cfg), account.Refresh.RetryAttempt))
	}
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) ApplyQuotaRefreshFailureIfAdmissionCurrent(account AccountState, version uint64, kind RefreshFailureKind, message string, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.admissionCurrentLocked(account.AuthID, version) {
		return false
	}
	account.Priority = s.cpaAdmission.Priority
	account.LastError = message
	account.Refresh.LastFailureKind = kind
	account.Refresh.LastFailureAt = now
	if kind == RefreshFailureAuth || kind == RefreshFailureLocal {
		account.Refresh.AuthFailure = kind == RefreshFailureAuth
		account.Refresh.NextRetryAt = time.Time{}
	} else {
		account.Refresh.AuthFailure = false
		account.Refresh.RetryAttempt++
		account.Refresh.NextRetryAt = now.Add(retryDelayForAttempt(NormalizeConfig(s.cfg), account.Refresh.RetryAttempt))
	}
	key := accountStateKey(account)
	if key == "" {
		return false
	}
	s.accounts[key] = cloneAccountState(account)
	return true
}

func (s *PluginState) ApplyQuotaRefreshSuccessIfAdmissionCurrent(account AccountState, version uint64, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.admissionCurrentLocked(account.AuthID, version) {
		return false
	}
	account.Priority = s.cpaAdmission.Priority
	if !account.LastSuccessAt.IsZero() && account.LastError == "" {
		account.Refresh = AccountRefreshState{}
	}
	// A successful quota read reconciles a stale temporary-exhaustion marker
	// when the fresh snapshot carries strict evidence that a real upstream
	// reset occurred: every known window must show remaining capacity AND the
	// window identity must have changed since the marker was recorded. A
	// same-window 100% reading alone does not clear the marker — generic quota
	// percentages can misreport recovery while model requests still hit
	// upstream 429 (issue #11's K12 negative control).
	if account.TemporaryExhausted && quotaRefreshConfirmsReset(account, now) {
		account.TemporaryExhausted = false
		account.TemporaryResetAt = time.Time{}
		account.LastError = ""
	}
	applyCircuitSuccess(&account, NormalizeConfig(s.cfg), now)
	key := accountStateKey(account)
	if key == "" {
		return false
	}
	s.accounts[key] = cloneAccountState(account)
	return true
}

// quotaRefreshConfirmsReset reports whether a fresh quota snapshot proves an
// actual upstream reset rather than a same-window misreport. With a five-hour
// window present, the window's reset deadline must differ from the deadline
// recorded when the marker was set (a reset mints a new window). Without one,
// a usable long window that resets after the recorded marker deadline is
// accepted as the weaker fallback evidence.
func quotaRefreshConfirmsReset(account AccountState, now time.Time) bool {
	if account.LastSuccessAt.IsZero() {
		return false
	}
	longWindow := account.Quota.LongWindow
	if longWindow != nil && !windowShowsRemainingCapacity(longWindow, now) {
		return false
	}
	if fiveHour := account.Quota.FiveHour; fiveHour != nil {
		if !windowShowsRemainingCapacity(fiveHour, now) {
			return false
		}
		return !fiveHour.ResetAt.Equal(account.TemporaryResetAt)
	}
	return longWindow != nil && !longWindow.ResetAt.IsZero() && longWindow.ResetAt.After(account.TemporaryResetAt)
}

func (s *PluginState) RecordLog(level, event, message string, fields map[string]any, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := LogEntry{
		Time:    now,
		Level:   level,
		Event:   event,
		Message: message,
		Fields:  cloneMap(fields),
	}
	s.logs = append(s.logs, entry)
	s.logs = retainedLogs(s.logs, NormalizeConfig(s.cfg), now)
}

func (s *PluginState) RecordLogIfAdmissionCurrent(authID string, version uint64, level, event, message string, fields map[string]any, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.admissionCurrentLocked(authID, version) {
		return false
	}
	entry := LogEntry{
		Time:    now,
		Level:   level,
		Event:   event,
		Message: message,
		Fields:  cloneMap(fields),
	}
	s.logs = append(s.logs, entry)
	s.logs = retainedLogs(s.logs, NormalizeConfig(s.cfg), now)
	return true
}

func (s *PluginState) Snapshot(now time.Time) StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]AccountState, 0, len(s.accounts))
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		cloned.Stale = !cloned.LastSuccessAt.IsZero() && now.Sub(cloned.LastSuccessAt) > s.cfg.StaleAfter
		cloned.Circuit = effectiveCircuitState(cloned.Circuit, now)
		accounts = append(accounts, cloned)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accountStateKey(accounts[i]) < accountStateKey(accounts[j])
	})
	accounts = ApplyAnnotations(accounts, s.annotations)

	return StateSnapshot{
		Config:              s.cfg,
		Accounts:            accounts,
		CPAAdmission:        cloneCPAAdmission(s.cpaAdmission),
		CPAAdmissionVersion: s.cpaAdmissionVersion,
		Annotations:         cloneAnnotationState(s.annotations),
		Logs:                cloneLogs(retainedLogs(s.logs, NormalizeConfig(s.cfg), now)),
		LastSelected:        s.lastSelected,
		LastReason:          s.lastReason,
		LastCodexActivityAt: s.lastCodexActivityAt,
		LastAuthScanAt:      s.lastAuthScanAt,
		CodexAuthCount:      s.codexAuthCount,
		Now:                 now,
	}
}

func (s *PluginState) DueAccounts(now time.Time) []AccountState {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	accounts := make([]AccountState, 0)
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		if due, reason := accountRefreshDue(cloned, cfg, now); due {
			cloned.Refresh.DueReason = reason
			accounts = append(accounts, cloned)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority > accounts[j].Priority
		}
		return accountStateKey(accounts[i]) < accountStateKey(accounts[j])
	})
	return accounts
}

func (s *PluginState) NextRefreshDueAt(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	if s.lastCodexActivityAt.IsZero() || !now.Before(s.lastCodexActivityAt.Add(cfg.RefreshActiveWindow)) {
		return time.Time{}
	}
	var next time.Time
	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if !t.After(now) {
			t = now
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, account := range s.accounts {
		if account.Refresh.AuthFailure || account.Refresh.LastFailureKind == RefreshFailureLocal {
			continue
		}
		if !account.Circuit.NextProbeAt.IsZero() {
			consider(account.Circuit.NextProbeAt)
			continue
		}
		if !account.Refresh.NextRetryAt.IsZero() {
			consider(account.Refresh.NextRetryAt)
			continue
		}
		if account.LastSuccessAt.IsZero() {
			continue
		}
		if cfg.EnableResetProbe {
			for _, probe := range account.ResetProbes {
				if probe.Status == ResetProbeStatusPending && !probe.NextCheckAt.IsZero() {
					consider(probe.NextCheckAt)
				}
			}
		}
		if account.Quota.FiveHour != nil && !account.Quota.FiveHour.ResetAt.IsZero() {
			triggerAt := account.Quota.FiveHour.ResetAt.Add(cfg.RefreshAfterResetDelay)
			if refreshTriggerPending(account.LastSuccessAt, triggerAt) {
				consider(triggerAt)
			}
		}
		if account.Quota.LongWindow != nil && !account.Quota.LongWindow.ResetAt.IsZero() {
			triggerAt := account.Quota.LongWindow.ResetAt.Add(cfg.RefreshAfterResetDelay)
			if refreshTriggerPending(account.LastSuccessAt, triggerAt) {
				consider(triggerAt)
			}
		}
		if account.TemporaryExhausted && !account.TemporaryResetAt.IsZero() {
			triggerAt := account.TemporaryResetAt.Add(cfg.RefreshAfterResetDelay)
			if refreshTriggerPending(account.LastSuccessAt, triggerAt) {
				consider(triggerAt)
			}
		}
	}
	return next
}

func retainedLogs(logs []LogEntry, cfg Config, now time.Time) []LogEntry {
	if len(logs) == 0 {
		return nil
	}
	cfg = NormalizeConfig(cfg)
	cutoff := now.Add(-cfg.LogRetention)
	retained := make([]LogEntry, 0, len(logs))
	for _, log := range logs {
		if log.Time.IsZero() || !log.Time.Before(cutoff) {
			retained = append(retained, log)
		}
	}
	if len(retained) > cfg.MaxLogEntries {
		retained = retained[len(retained)-cfg.MaxLogEntries:]
	}
	return retained
}

func accountRefreshDue(account AccountState, cfg Config, now time.Time) (bool, string) {
	cfg = NormalizeConfig(cfg)
	if account.Refresh.AuthFailure {
		return false, "auth_failure"
	}
	if account.Refresh.LastFailureKind == RefreshFailureLocal {
		return false, "local_failure"
	}
	if !account.Circuit.NextProbeAt.IsZero() {
		if account.Circuit.NextProbeAt.After(now) {
			return false, "circuit_wait"
		}
		if account.Circuit.State == CircuitStateOpen || account.Circuit.EffectiveState == CircuitStateHalfOpen || account.Circuit.FailureCount > 0 {
			return true, "circuit_probe_due"
		}
	}
	if !account.Refresh.NextRetryAt.IsZero() && account.Refresh.NextRetryAt.After(now) {
		return false, "retry_wait"
	}
	if !account.Refresh.NextRetryAt.IsZero() && !account.Refresh.NextRetryAt.After(now) {
		return true, "retry_due"
	}
	if account.LastSuccessAt.IsZero() {
		return true, "never_refreshed"
	}
	if now.Sub(account.LastSuccessAt) > cfg.StaleAfter {
		return true, "stale"
	}
	if !account.LastSuccessAt.Add(cfg.QuotaRefreshInterval).After(now) {
		return true, "refresh_interval_due"
	}
	if cfg.EnableResetProbe && resetProbeDueAny(account.ResetProbes, now) {
		return true, "reset_probe_check_due"
	}
	if resetDue(account.Quota.FiveHour, cfg.RefreshAfterResetDelay, account.LastSuccessAt, now) {
		return true, "five_hour_reset_due"
	}
	if resetDue(account.Quota.LongWindow, cfg.RefreshAfterResetDelay, account.LastSuccessAt, now) {
		return true, "long_window_reset_due"
	}
	if account.TemporaryExhausted && !account.TemporaryResetAt.IsZero() {
		triggerAt := account.TemporaryResetAt.Add(cfg.RefreshAfterResetDelay)
		if refreshTriggerPending(account.LastSuccessAt, triggerAt) && !triggerAt.After(now) {
			return true, "temporary_reset_due"
		}
	}
	return false, ""
}

func refreshTriggerPending(lastSuccessAt, triggerAt time.Time) bool {
	return !triggerAt.IsZero() && (lastSuccessAt.IsZero() || lastSuccessAt.Before(triggerAt))
}

func resetDue(window *QuotaWindow, delay time.Duration, lastSuccessAt, now time.Time) bool {
	if window == nil || window.ResetAt.IsZero() {
		return false
	}
	triggerAt := window.ResetAt.Add(delay)
	return refreshTriggerPending(lastSuccessAt, triggerAt) && !triggerAt.After(now)
}

func retryDelayForAttempt(cfg Config, attempt int) time.Duration {
	delays := NormalizeConfig(cfg).RefreshRetryDelays
	if len(delays) == 0 {
		return time.Minute
	}
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(delays) {
		index = len(delays) - 1
	}
	return delays[index]
}

func accountStateKey(account AccountState) string {
	if account.AuthID != "" {
		return "auth:" + account.AuthID
	}
	if account.ChatGPTAccountID != "" {
		return "chatgpt:" + account.ChatGPTAccountID
	}
	if account.Instance != 0 {
		return "instance:" + strconv.FormatUint(uint64(account.Instance), 10)
	}
	if account.Email != "" {
		return "email:" + account.Email
	}
	if account.AuthIndex != "" {
		return "index:" + account.AuthIndex
	}
	return ""
}

func markTemporaryExhausted(account *AccountState, resetAt time.Time, reason string) {
	account.TemporaryExhausted = true
	account.TemporaryResetAt = resetAt
	account.LastError = reason
	account.Circuit = CircuitBreakerState{State: CircuitStateClosed, EffectiveState: CircuitStateClosed}
}

func applyCircuitFailure(account *AccountState, cfg Config, reason string, resetAt, now time.Time) {
	circuit := effectiveCircuitState(account.Circuit, now)
	circuit.FailureCount++
	circuit.SuccessCount = 0
	circuit.Reason = reason
	circuit.LastFailureAt = now
	nextProbeAt := now.Add(cfg.CircuitOpenDuration)
	if reason == usageLimitNoResetReason && !resetAt.IsZero() && resetAt.After(now) {
		nextProbeAt = resetAt
	}
	if !resetAt.IsZero() && resetAt.After(nextProbeAt) {
		nextProbeAt = resetAt
	}
	if !resetAt.IsZero() && resetAt.After(now) && circuit.NextProbeAt.Before(nextProbeAt) {
		circuit.NextProbeAt = nextProbeAt
	}
	threshold := cfg.CircuitFailureThreshold
	if threshold <= 0 {
		threshold = 1
	}
	if circuit.State == CircuitStateHalfOpen || circuit.EffectiveState == CircuitStateHalfOpen || circuit.FailureCount >= threshold {
		circuit.State = CircuitStateOpen
		circuit.EffectiveState = CircuitStateOpen
		circuit.OpenedAt = now
		circuit.NextProbeAt = nextProbeAt
	}
	account.Circuit = circuit
	account.LastError = reason
}

func applyCircuitSuccess(account *AccountState, cfg Config, now time.Time) {
	circuit := effectiveCircuitState(account.Circuit, now)
	if circuit.EffectiveState == CircuitStateHalfOpen {
		circuit.SuccessCount++
		circuit.LastSuccessAt = now
		threshold := cfg.CircuitHalfOpenSuccessThreshold
		if threshold <= 0 {
			threshold = 1
		}
		if circuit.SuccessCount >= threshold {
			account.Circuit = CircuitBreakerState{State: CircuitStateClosed, EffectiveState: CircuitStateClosed}
			account.LastError = ""
			return
		}
		circuit.State = CircuitStateHalfOpen
		circuit.EffectiveState = CircuitStateHalfOpen
		circuit.NextProbeAt = time.Time{}
		account.Circuit = circuit
		account.LastError = ""
		return
	}
	if circuit.EffectiveState == CircuitStateOpen {
		circuit.LastSuccessAt = now
		account.Circuit = circuit
		return
	}
	account.Circuit = CircuitBreakerState{State: CircuitStateClosed, EffectiveState: CircuitStateClosed, LastSuccessAt: now}
	account.LastError = ""
}

func effectiveCircuitState(circuit CircuitBreakerState, now time.Time) CircuitBreakerState {
	if circuit.State == "" {
		circuit.State = CircuitStateClosed
	}
	circuit.EffectiveState = circuit.State
	if circuit.State == CircuitStateOpen && !circuit.NextProbeAt.IsZero() && !circuit.NextProbeAt.After(now) {
		circuit.EffectiveState = CircuitStateHalfOpen
	}
	return circuit
}

func cloneAnnotationState(state AnnotationState) AnnotationState {
	cloned := AnnotationState{
		Accounts: make(map[string]AccountAnnotation, len(state.Accounts)),
		Groups:   make(map[string]GroupAnnotation, len(state.Groups)),
	}
	for key, annotation := range state.Accounts {
		cloned.Accounts[key] = cloneAccountAnnotation(annotation)
	}
	for key, annotation := range state.Groups {
		cloned.Groups[key] = cloneGroupAnnotation(annotation)
	}
	return cloned
}

func cloneAccountAnnotation(annotation AccountAnnotation) AccountAnnotation {
	annotation.Tags = cloneStringSlice(annotation.Tags)
	return annotation
}

func cloneGroupAnnotation(annotation GroupAnnotation) GroupAnnotation {
	annotation.Tags = cloneStringSlice(annotation.Tags)
	return annotation
}

func cloneAccountState(account AccountState) AccountState {
	account.Quota = cloneParsedQuota(account.Quota)
	account.ResetProbes = cloneResetProbes(account.ResetProbes)
	account.Annotation = cloneAccountAnnotation(account.Annotation)
	return account
}

func cloneResetProbes(probes map[WindowKind]ResetProbeState) map[WindowKind]ResetProbeState {
	if len(probes) == 0 {
		return nil
	}
	cloned := make(map[WindowKind]ResetProbeState, len(probes))
	for kind, probe := range probes {
		cloned[kind] = probe
	}
	return cloned
}

func cloneParsedQuota(quota ParsedQuota) ParsedQuota {
	if quota.FiveHour != nil {
		window := *quota.FiveHour
		quota.FiveHour = &window
	}
	if quota.LongWindow != nil {
		window := *quota.LongWindow
		quota.LongWindow = &window
	}
	quota.CodeReviewWindows = cloneQuotaWindows(quota.CodeReviewWindows)
	quota.AdditionalWindows = cloneQuotaWindows(quota.AdditionalWindows)
	if quota.ResetCreditsAvailableCount != nil {
		count := *quota.ResetCreditsAvailableCount
		quota.ResetCreditsAvailableCount = &count
	}
	if quota.ResetCreditsTotalEarnedCount != nil {
		count := *quota.ResetCreditsTotalEarnedCount
		quota.ResetCreditsTotalEarnedCount = &count
	}
	if len(quota.ResetCredits) > 0 {
		quota.ResetCredits = append([]ResetCredit(nil), quota.ResetCredits...)
	}
	return quota
}

func cloneQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	if len(windows) == 0 {
		return nil
	}
	cloned := make([]QuotaWindow, len(windows))
	copy(cloned, windows)
	for i := range cloned {
		if cloned[i].UsedPercent != nil {
			used := *cloned[i].UsedPercent
			cloned[i].UsedPercent = &used
		}
		if cloned[i].LimitWindowSeconds != nil {
			limit := *cloned[i].LimitWindowSeconds
			cloned[i].LimitWindowSeconds = &limit
		}
	}
	return cloned
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneLogs(values []LogEntry) []LogEntry {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]LogEntry, len(values))
	for i, entry := range values {
		cloned[i] = entry
		cloned[i].Fields = cloneMap(entry.Fields)
	}
	return cloned
}

func cloneMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
