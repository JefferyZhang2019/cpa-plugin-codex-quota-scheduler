package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Managed quota disable-and-recovery, ported from dos1989's
// feature/managed-quota-recovery (MIT) and hardened:
//
//   - every ownership record carries the credential fingerprint computed from
//     the exact auth JSON that was disabled, and recovery refuses to enable a
//     different credential that happens to live under the same auth file name;
//   - recovery evidence must show BOTH windows usable (the original design
//     promised this; the fork only checked the long window);
//   - planned records that crashed before their host write are reconciled on
//     the next recovery pass instead of leaking forever;
//   - the feature is opt-in (enable_managed_quota_disable, default false).
//
// Safety invariants: a pre-existing disabled flag without plugin ownership is
// manual operator state and is never touched; recovery never enables from
// elapsed time alone — only from a fresh, usable quota read.

// LifecycleEvent is an auditable, bounded history entry for an account the
// plugin disabled after a confirmed upstream quota failure.
type LifecycleEvent struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// ManagedDisableRecord is the durable ownership record. Its presence means the
// plugin, rather than an operator, owns the corresponding disabled flag.
type ManagedDisableRecord struct {
	DisabledAt       time.Time             `json:"disabled_at"`
	DisableApplied   bool                  `json:"disable_applied"`
	Fingerprint      CredentialFingerprint `json:"fingerprint,omitempty"`
	Last429At        time.Time             `json:"last_429_at,omitempty"`
	ResetAt          time.Time             `json:"reset_at,omitempty"`
	LastQuotaCheckAt time.Time             `json:"last_quota_check_at,omitempty"`
	NextQuotaCheckAt time.Time             `json:"next_quota_check_at,omitempty"`
	Events           []LifecycleEvent      `json:"events,omitempty"`
}

// managedAuthFingerprint fingerprints the credential that would be disabled or
// enabled, so ownership cannot leak across credential rotation under the same
// auth file name.
func managedAuthFingerprint(authIndex string, credentials CodexCredentials) CredentialFingerprint {
	return NewCredentialFingerprint(credentials.ChatGPTAccountID, credentials.RefreshToken, authIndex)
}

// ApplyManagedQuotaDisable records plugin ownership before changing CPA auth
// metadata, then sets the top-level disabled flag on the auth file. A
// pre-existing disabled flag without plugin ownership is manual operator state
// and is deliberately left untouched.
func (r *QuotaRefresher) ApplyManagedQuotaDisable(ctx context.Context, event QuotaFailureEvent) error {
	if r == nil || r.host == nil || r.runtimeStore == nil || !r.state.Config().EnableManagedQuotaDisable || event.AuthIndex == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := r.host.GetAuth(event.AuthIndex)
	if err != nil {
		return err
	}
	if !json.Valid(response.JSON) {
		return errors.New("managed quota disable requires valid auth JSON")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.JSON, &raw); err != nil {
		return err
	}
	var disabled bool
	if value, ok := raw["disabled"]; ok {
		_ = json.Unmarshal(value, &disabled)
	}
	credentials, credErr := ExtractCodexCredentials(response.JSON)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	record, owned := persisted.ManagedLifecycle[event.AuthIndex]
	if disabled && !owned {
		return nil
	}
	if owned && credErr == nil && record.Fingerprint.ValidHashes() && record.Fingerprint != managedAuthFingerprint(event.AuthIndex, credentials) {
		// The credential rotated after our disable: drop ownership instead of
		// writing a flag the operator may not want for the new credential.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error {
			delete(state.ManagedLifecycle, event.AuthIndex)
			return nil
		})
		return err
	}
	now := r.now()
	var fingerprint CredentialFingerprint
	if credErr == nil {
		fingerprint = managedAuthFingerprint(event.AuthIndex, credentials)
	} else if owned {
		fingerprint = record.Fingerprint
	}
	if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
		record := state.ManagedLifecycle[event.AuthIndex]
		if record.DisabledAt.IsZero() {
			record.DisabledAt = now
		}
		record.Last429At = now
		record.ResetAt = event.ResetAt
		record.NextQuotaCheckAt = event.ResetAt
		if fingerprint.ValidHashes() {
			record.Fingerprint = fingerprint
		}
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: "quota_429", At: now, Detail: event.Reason}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[event.AuthIndex] = record
		return nil
	}); err != nil {
		return err
	}
	if disabled {
		return nil
	}
	raw["disabled"] = json.RawMessage("true")
	next, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	name := response.Name
	if name == "" {
		name = event.AuthIndex
	}
	if err := r.host.SaveAuth(name, next); err != nil {
		return err
	}
	_, err = r.runtimeStore.Update(func(state *PersistentState) error {
		record := state.ManagedLifecycle[event.AuthIndex]
		record.DisableApplied = true
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: "disabled", At: r.now()}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[event.AuthIndex] = record
		return nil
	})
	return err
}

// RecoverManagedQuotaAccounts reconciles planned records, then checks only
// accounts with a durable applied ownership record. Operators keep their
// manually disabled accounts; recovered quota accounts return to CPA
// automatically.
func (r *QuotaRefresher) RecoverManagedQuotaAccounts(ctx context.Context) error {
	if r == nil || r.host == nil || r.runtimeStore == nil || !r.state.Config().EnableManagedQuotaDisable {
		return nil
	}
	if err := r.reconcilePlannedManagedRecords(ctx); err != nil {
		return err
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	indices := make([]string, 0, len(persisted.ManagedLifecycle))
	for index, record := range persisted.ManagedLifecycle {
		if record.DisableApplied && (record.NextQuotaCheckAt.IsZero() || !record.NextQuotaCheckAt.After(r.now())) {
			indices = append(indices, index)
		}
	}
	sort.Strings(indices)
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.recoverManagedQuotaAccount(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

// reconcilePlannedManagedRecords settles ownership records whose host write
// never confirmed: if the auth file is not disabled the plan is stale and the
// record is dropped; if it is disabled the write evidently happened and the
// record is completed. Without this, a crash between persisting the plan and
// the host write would leak a ghost record forever.
func (r *QuotaRefresher) reconcilePlannedManagedRecords(ctx context.Context) error {
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	for index, record := range persisted.ManagedLifecycle {
		if record.DisableApplied {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		response, err := r.host.GetAuth(index)
		if err != nil {
			continue
		}
		var raw map[string]json.RawMessage
		if !json.Valid(response.JSON) || json.Unmarshal(response.JSON, &raw) != nil {
			continue
		}
		var disabled bool
		if value, ok := raw["disabled"]; ok {
			_ = json.Unmarshal(value, &disabled)
		}
		_, updateErr := r.runtimeStore.Update(func(state *PersistentState) error {
			record, ok := state.ManagedLifecycle[index]
			if !ok {
				return nil
			}
			if disabled {
				record.DisableApplied = true
				record = appendLifecycleEvent(record, LifecycleEvent{Kind: "disable_confirmed", At: r.now()}, r.state.Config().LifecycleEventLimit)
			} else {
				delete(state.ManagedLifecycle, index)
				return nil
			}
			state.ManagedLifecycle[index] = record
			return nil
		})
		if updateErr != nil {
			return updateErr
		}
	}
	return nil
}

// managedRecoveryDeadline is the earliest time the recovery lane must run.
func (r *QuotaRefresher) managedRecoveryDeadline(now time.Time) time.Time {
	if r == nil || r.runtimeStore == nil || !r.state.Config().EnableManagedQuotaDisable {
		return time.Time{}
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return time.Time{}
	}
	var deadline time.Time
	for _, record := range persisted.ManagedLifecycle {
		if !record.DisableApplied {
			continue
		}
		candidate := record.NextQuotaCheckAt
		if candidate.IsZero() || candidate.Before(now) {
			candidate = now
		}
		if deadline.IsZero() || candidate.Before(deadline) {
			deadline = candidate
		}
	}
	return deadline
}

func (r *QuotaRefresher) recoverManagedQuotaAccount(ctx context.Context, authIndex string) error {
	now := r.now()
	response, err := r.host.GetAuth(authIndex)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if !json.Valid(response.JSON) || json.Unmarshal(response.JSON, &raw) != nil {
		return errors.New("managed quota recovery requires valid auth JSON")
	}
	var disabled bool
	if value, ok := raw["disabled"]; ok {
		_ = json.Unmarshal(value, &disabled)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		return err
	}
	record, owned := persisted.ManagedLifecycle[authIndex]
	if !disabled {
		// An operator (or the host) enabled it: drop ownership, never write.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error { delete(state.ManagedLifecycle, authIndex); return nil })
		return err
	}
	credentials, credErr := ExtractCodexCredentials(response.JSON)
	if credErr != nil || credentials.ChatGPTAccountID == "" {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	if owned && record.Fingerprint.ValidHashes() && record.Fingerprint != managedAuthFingerprint(authIndex, credentials) {
		// The credential under this file rotated after our disable. Do not
		// enable someone else's credential: drop ownership and leave the flag
		// for the operator.
		_, err := r.runtimeStore.Update(func(state *PersistentState) error {
			delete(state.ManagedLifecycle, authIndex)
			return nil
		})
		if err == nil {
			r.state.RecordLog("warn", "managed.fingerprint_rotation", "凭据轮换后放弃托管停用所有权，交由操作员处理", map[string]any{"auth_index": authIndex}, r.now())
		}
		return err
	}
	quotaResp, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: r.state.Config().QuotaEndpoint, Headers: http.Header{
		"Authorization": []string{"Bearer " + credentials.AccessToken}, "Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID}, "Content-Type": []string{"application/json"}, "User-Agent": []string{quotaUserAgent},
	}}, false)
	if err != nil || quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	quota, err := ParseCodexUsagePayload(quotaResp.Body, now)
	if err != nil || !managedQuotaUsable(quota, now, now, r.state.Config().QuotaRefreshInterval) {
		return r.recordManagedQuotaCheck(authIndex, now)
	}
	raw["disabled"] = json.RawMessage("false")
	next, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	name := response.Name
	if name == "" {
		name = authIndex
	}
	if err := r.host.SaveAuth(name, next); err != nil {
		return err
	}
	_, err = r.runtimeStore.Update(func(state *PersistentState) error {
		delete(state.ManagedLifecycle, authIndex)
		return nil
	})
	if err == nil {
		r.state.RecordLog("info", "managed.reenabled", "额度恢复，已自动重新启用账号", map[string]any{"auth_index": authIndex}, r.now())
	}
	return err
}

func (r *QuotaRefresher) recordManagedQuotaCheck(authIndex string, at time.Time) error {
	_, err := r.runtimeStore.Update(func(state *PersistentState) error {
		record, ok := state.ManagedLifecycle[authIndex]
		if !ok {
			return nil
		}
		record.LastQuotaCheckAt = at
		record.NextQuotaCheckAt = at.Add(r.state.Config().QuotaRefreshInterval)
		record = appendLifecycleEvent(record, LifecycleEvent{Kind: "quota_checked", At: at}, r.state.Config().LifecycleEventLimit)
		state.ManagedLifecycle[authIndex] = record
		return nil
	})
	return err
}

func appendLifecycleEvent(record ManagedDisableRecord, event LifecycleEvent, limit int) ManagedDisableRecord {
	if limit <= 0 {
		return record
	}
	record.Events = append(record.Events, event)
	if overflow := len(record.Events) - limit; overflow > 0 {
		record.Events = append([]LifecycleEvent(nil), record.Events[overflow:]...)
	}
	return record
}

// managedQuotaUsable is the enable evidence: the long window must be present
// and usable, and a present five-hour window must be usable too.
func managedQuotaUsable(quota ParsedQuota, observedAt, now time.Time, staleAfter time.Duration) bool {
	window := quota.LongWindow
	if window == nil || observedAt.IsZero() || observedAt.After(now) || staleAfter <= 0 || now.Sub(observedAt) > staleAfter {
		return false
	}
	if windowExhausted(window, now) {
		return false
	}
	if window.UsedPercent != nil && *window.UsedPercent >= 100 {
		return false
	}
	if quota.FiveHour != nil && windowExhausted(quota.FiveHour, now) {
		return false
	}
	return true
}
