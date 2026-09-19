package main

import (
	"context"
	"sort"
	"time"
)

type HostCapability uint8

const (
	CapabilityB HostCapability = iota
	CapabilityA
)

// RosterEntry is the normalized host.auth.list boundary shape. Priority is a
// pointer because the v7.2.42 typed ABI uses int and cannot distinguish a
// missing JSON field from an explicit zero.
type RosterEntry struct {
	ID        string
	AuthIndex string
	Provider  string
	Priority  *int
}

type HostRosterSnapshot struct {
	Capability        HostCapability
	Confirmed         bool
	Provisional       bool
	BackgroundAllowed bool
	Health            RosterHealth
	Generation        uint64
	LifecycleRevision uint64
	Entries           []RosterEntry
	ConfirmedAt       time.Time
	DegradedSince     time.Time
}

func hostRosterSnapshotFromActive(active ActiveRoster) HostRosterSnapshot {
	return HostRosterSnapshot{
		Capability: active.Capability, Confirmed: active.Confirmed, Provisional: active.Provisional,
		BackgroundAllowed: active.BackgroundAllowed, Health: active.Health, Generation: active.Generation, LifecycleRevision: active.LifecycleRevision,
		Entries: append([]RosterEntry(nil), active.Entries...), ConfirmedAt: active.ConfirmedAt, DegradedSince: active.DegradedSince,
	}
}

func normalizeHostRosterLifecycle(roster HostRosterSnapshot) HostRosterSnapshot {
	if roster.Capability == CapabilityA && roster.Health == "" {
		roster.Confirmed = true
		roster.BackgroundAllowed = true
		roster.Health = RosterHealthy
	}
	return roster
}

type HostAuthLister interface {
	ListHostAuths(context.Context) ([]RosterEntry, error)
}

type HostAuthListerFunc func(context.Context) ([]RosterEntry, error)

func (f HostAuthListerFunc) ListHostAuths(ctx context.Context) ([]RosterEntry, error) {
	return f(ctx)
}

func DetectHostRoster(ctx context.Context, host HostAuthLister, now time.Time) HostRosterSnapshot {
	if host == nil {
		return HostRosterSnapshot{Capability: CapabilityB}
	}
	entries, err := host.ListHostAuths(ctx)
	if err != nil || len(entries) == 0 {
		return HostRosterSnapshot{Capability: CapabilityB}
	}
	for _, entry := range entries {
		if entry.Priority == nil {
			return HostRosterSnapshot{Capability: CapabilityB}
		}
	}
	return HostRosterSnapshot{
		Capability:  CapabilityA,
		Entries:     append([]RosterEntry(nil), entries...),
		ConfirmedAt: now,
	}
}

func HighestCodexTier(entries []RosterEntry) (priority int, ids []string, ok bool) {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.ID == "" || entry.Provider != "codex" || entry.Priority == nil {
			continue
		}
		if !ok || *entry.Priority > priority {
			priority = *entry.Priority
			ids = ids[:0]
			clear(seen)
			ok = true
		}
		if *entry.Priority == priority {
			if _, exists := seen[entry.ID]; !exists {
				seen[entry.ID] = struct{}{}
				ids = append(ids, entry.ID)
			}
		}
	}
	if !ok {
		return 0, nil, false
	}
	sort.Strings(ids)
	return priority, ids, true
}

// CodexTierGroups groups roster entries by CPA priority, descending. It is the
// tier-aware generalization of HighestCodexTier used when
// schedule_across_priorities is enabled.
func CodexTierGroups(entries []RosterEntry) ([]TierAdmission, bool) {
	byPriority := make(map[int]map[string]struct{})
	priorities := make([]int, 0, 4)
	for _, entry := range entries {
		if entry.ID == "" || entry.Provider != "codex" || entry.Priority == nil {
			continue
		}
		priority := *entry.Priority
		tier, exists := byPriority[priority]
		if !exists {
			tier = make(map[string]struct{})
			byPriority[priority] = tier
			priorities = append(priorities, priority)
		}
		tier[entry.ID] = struct{}{}
	}
	if len(priorities) == 0 {
		return nil, false
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	tiers := make([]TierAdmission, 0, len(priorities))
	for _, priority := range priorities {
		tiers = append(tiers, TierAdmission{Priority: priority, AuthIDs: byPriority[priority]})
	}
	return tiers, true
}
