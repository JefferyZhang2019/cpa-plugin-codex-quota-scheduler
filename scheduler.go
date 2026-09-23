package main

import (
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type PickDecision struct {
	AuthID          string
	Handled         bool
	DelegateBuiltin string
	Reason          string
	Ordered         []ScheduledAccount
	// UnavailableSummary explains a fallback decision from the published
	// snapshot when the Ordered list is not populated (the pick path never
	// builds it), e.g. "auth_failure=2,temporary_exhausted=1".
	UnavailableSummary string
}

type ScheduledAccount struct {
	AuthID            string
	CPAPriority       int
	SchedulerPriority int
	Family            AccountFamily
	QueueStatus       QueueStatus
	Available         bool
	UnavailableReason string
	SortTime          time.Time
	Annotation        AccountAnnotation
	selectionClass    AvailabilityClass
	selectionView     AccountView
}

type QueueStatus string

const (
	QueueStatusAvailable           QueueStatus = "available"
	QueueStatusFiveHourExhausted   QueueStatus = "five_hour_exhausted"
	QueueStatusLongWindowExhausted QueueStatus = "long_window_exhausted"
	QueueStatusUnavailable         QueueStatus = "unavailable"
)

func HighestPriorityCodexAdmission(req pluginapi.SchedulerPickRequest) (CPAAdmissionState, bool) {
	return CodexAdmissionFromRequest(req, false)
}

// CodexAdmissionFromRequest builds the admission view of a pick request's
// codex candidates. With acrossPriorities disabled it collapses to the single
// highest tier (legacy behavior); otherwise it keeps every tier so selection
// can fall through to lower tiers when higher ones are exhausted.
func CodexAdmissionFromRequest(req pluginapi.SchedulerPickRequest, acrossPriorities bool) (CPAAdmissionState, bool) {
	candidates := make([]RosterEntry, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		priority := candidate.Priority
		candidates = append(candidates, RosterEntry{ID: candidate.ID, Provider: candidate.Provider, Priority: &priority})
	}
	tiers, ok := CodexTierGroups(candidates)
	if !ok {
		return CPAAdmissionState{}, false
	}
	if !acrossPriorities {
		tiers = tiers[:1]
	}
	union := make(map[string]struct{})
	for _, tier := range tiers {
		for id := range tier.AuthIDs {
			union[id] = struct{}{}
		}
	}
	return CPAAdmissionState{Observed: true, Priority: tiers[0].Priority, AuthIDs: union, Tiers: tiers}, true
}

func codexCandidateCount(req pluginapi.SchedulerPickRequest) int {
	seen := make(map[string]struct{}, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		seen[candidate.ID] = struct{}{}
	}
	return len(seen)
}

func PickCodexAccount(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision {
	if !snapshot.Config.HandleEnabled {
		return PickDecision{Reason: "handle_disabled"}
	}
	if !requestIncludesCodex(req) {
		return PickDecision{Reason: "provider_not_codex"}
	}

	ordered := BuildOrderedAccounts(req, snapshot, now)
	if len(ordered) == 0 {
		return PickDecision{Reason: "no_codex_candidates", Ordered: ordered}
	}
	for _, account := range ordered {
		if account.Available {
			return PickDecision{
				AuthID:  account.AuthID,
				Handled: true,
				Reason:  "selected",
				Ordered: ordered,
			}
		}
	}

	if snapshot.Config.Fallback == FallbackFillFirst {
		return PickDecision{
			Handled:         true,
			DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
			Reason:          "fallback_fill_first",
			Ordered:         ordered,
		}
	}
	return PickDecision{Reason: "no_selectable_account", Ordered: ordered}
}

func BuildOrderedAccounts(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) []ScheduledAccount {
	return buildOrderedAccounts(req, snapshot, now, nil)
}

func buildOrderedAccounts(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time, trials *TrialRegistry) []ScheduledAccount {
	admission, ok := CodexAdmissionFromRequest(req, snapshot.Config.ScheduleAcrossPriorities)
	if !ok {
		return nil
	}

	accountsByAuthID := make(map[string]AccountState, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID == "" {
			continue
		}
		accountsByAuthID[account.AuthID] = account
	}

	ordered := make([]ScheduledAccount, 0, len(req.Candidates))
	seen := make(map[string]struct{}, len(req.Candidates))
	priorityByAuthID := make(map[string]int, len(admission.AuthIDs))
	for _, tier := range admission.Tiers {
		for id := range tier.AuthIDs {
			// Keep the highest tier when a duplicate candidate appears at
			// several priorities.
			if existing, ok := priorityByAuthID[id]; !ok || tier.Priority > existing {
				priorityByAuthID[id] = tier.Priority
			}
		}
	}
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		if _, ok := admission.AuthIDs[candidate.ID]; !ok {
			continue
		}
		if _, ok := seen[candidate.ID]; ok {
			continue
		}
		seen[candidate.ID] = struct{}{}
		candidatePriority, ok := priorityByAuthID[candidate.ID]
		if !ok {
			candidatePriority = candidate.Priority
		}

		account, ok := accountsByAuthID[candidate.ID]
		if !ok {
			ordered = append(ordered, ScheduledAccount{
				AuthID:            candidate.ID,
				CPAPriority:       candidatePriority,
				Family:            AccountFamilyUnknown,
				QueueStatus:       QueueStatusUnavailable,
				UnavailableReason: "unknown_account",
				selectionClass:    Excluded,
				selectionView:     AccountView{ID: candidate.ID},
			})
			continue
		}

		queueStatus, available, reason, sortTime := accountQueueState(account, now)
		view := accountViewFromState(account, snapshot.Config, now, trials)
		// The request's candidate priority is the live tier truth; the stored
		// account priority can lag an admission update.
		view.CPAPriority = candidatePriority
		selectionClass := ClassifyAccount(view, now)
		if selectionClass == Excluded && available && view.Trial != TrialNone {
			queueStatus = QueueStatusUnavailable
			available = false
			reason = "quota_probe_wait"
			sortTime = time.Time{}
		}
		ordered = append(ordered, ScheduledAccount{
			AuthID:            candidate.ID,
			CPAPriority:       candidatePriority,
			SchedulerPriority: account.Annotation.SchedulerPriority,
			Family:            account.Family,
			QueueStatus:       queueStatus,
			Available:         available,
			UnavailableReason: reason,
			SortTime:          sortTime,
			Annotation:        account.Annotation,
			selectionClass:    selectionClass,
			selectionView:     view,
		})
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.selectionClass != right.selectionClass {
			return left.selectionClass < right.selectionClass
		}
		if left.selectionClass != Excluded {
			// Higher CPA priority tiers lead within an availability class so a
			// lower tier is only reached when higher tiers have no selectable
			// account.
			if left.CPAPriority != right.CPAPriority {
				return left.CPAPriority > right.CPAPriority
			}
			return accountViewLess(left.selectionView, right.selectionView, snapshot.Config.MonthlyMode)
		}
		if !left.SortTime.Equal(right.SortTime) {
			if left.SortTime.IsZero() {
				return false
			}
			if right.SortTime.IsZero() {
				return true
			}
			return left.SortTime.Before(right.SortTime)
		}
		return left.AuthID < right.AuthID
	})
	return ordered
}

func accountAvailable(account AccountState, now time.Time) (bool, string) {
	_, available, reason, _ := accountQueueState(account, now)
	return available, reason
}

func accountQueueState(account AccountState, now time.Time) (QueueStatus, bool, string, time.Time) {
	if account.Refresh.AuthFailure {
		return QueueStatusUnavailable, false, "auth_failure", time.Time{}
	}
	if account.Refresh.LastFailureKind == RefreshFailureLocal {
		return QueueStatusUnavailable, false, "local_failure", time.Time{}
	}
	if account.Stale {
		return QueueStatusUnavailable, false, "stale_quota", time.Time{}
	}
	circuit := effectiveCircuitState(account.Circuit, now)
	if circuit.EffectiveState == CircuitStateOpen {
		return QueueStatusUnavailable, false, "circuit_open", circuit.NextProbeAt
	}
	if circuit.EffectiveState == CircuitStateClosed && circuit.FailureCount > 0 && !circuit.NextProbeAt.IsZero() && circuit.NextProbeAt.After(now) {
		return QueueStatusUnavailable, false, "quota_probe_wait", circuit.NextProbeAt
	}
	switch account.Family {
	case AccountFamilyWeekly:
		if account.Quota.LongWindow == nil {
			return QueueStatusUnavailable, false, "missing_weekly_window", time.Time{}
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return QueueStatusUnavailable, false, "missing_weekly_reset", time.Time{}
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return QueueStatusLongWindowExhausted, false, "weekly_exhausted", account.Quota.LongWindow.ResetAt
		}
		if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
			return QueueStatusFiveHourExhausted, false, "temporary_exhausted", account.TemporaryResetAt
		}
		if account.Quota.FiveHour != nil {
			if account.Quota.FiveHour.ResetAt.IsZero() {
				return QueueStatusUnavailable, false, "missing_five_hour_reset", time.Time{}
			}
			if windowExhausted(account.Quota.FiveHour, now) {
				return QueueStatusFiveHourExhausted, false, "five_hour_exhausted", account.Quota.FiveHour.ResetAt
			}
		}
		return QueueStatusAvailable, true, "", account.Quota.LongWindow.ResetAt
	case AccountFamilyMonthly:
		if account.Quota.LongWindow == nil {
			return QueueStatusUnavailable, false, "missing_monthly_window", time.Time{}
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return QueueStatusUnavailable, false, "missing_monthly_reset", time.Time{}
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return QueueStatusLongWindowExhausted, false, "monthly_exhausted", account.Quota.LongWindow.ResetAt
		}
		if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
			return QueueStatusFiveHourExhausted, false, "temporary_exhausted", account.TemporaryResetAt
		}
		if account.Quota.FiveHour != nil {
			if account.Quota.FiveHour.ResetAt.IsZero() {
				return QueueStatusUnavailable, false, "missing_five_hour_reset", time.Time{}
			}
			if windowExhausted(account.Quota.FiveHour, now) {
				return QueueStatusFiveHourExhausted, false, "five_hour_exhausted", account.Quota.FiveHour.ResetAt
			}
		}
		return QueueStatusAvailable, true, "", account.Quota.LongWindow.ResetAt
	default:
		if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
			return QueueStatusFiveHourExhausted, false, "temporary_exhausted", account.TemporaryResetAt
		}
		return QueueStatusUnavailable, false, "unknown_family", time.Time{}
	}
}

func requestIncludesCodex(req pluginapi.SchedulerPickRequest) bool {
	if req.Provider == "codex" {
		return true
	}
	for _, provider := range req.Providers {
		if provider == "codex" {
			return true
		}
	}
	return false
}

func accountSortTime(account AccountState) time.Time {
	if account.Quota.LongWindow != nil {
		return account.Quota.LongWindow.ResetAt
	}
	return time.Time{}
}

func windowExhausted(window *QuotaWindow, now time.Time) bool {
	if window == nil || !window.Exhausted {
		return false
	}
	return window.ResetAt.IsZero() || window.ResetAt.After(now)
}
