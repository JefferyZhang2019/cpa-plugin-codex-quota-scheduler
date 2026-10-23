package main

import (
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func pickBaselineFixture() (pluginapi.SchedulerPickRequest, StateSnapshot, time.Time) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.HandleEnabled = true
	return pluginapi.SchedulerPickRequest{
			Provider: "codex",
			Model:    "gpt-5-codex",
			Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "a", Provider: "codex", Priority: 10},
				{ID: "b", Provider: "codex", Priority: 10},
				{ID: "lower", Provider: "codex", Priority: 1},
			},
		}, StateSnapshot{
			Config: cfg,
			Accounts: []AccountState{
				{AuthID: "a", Provider: "codex", Priority: 10, Family: AccountFamilyWeekly, Quota: availableWeeklyQuota(now), LastSuccessAt: now},
				{AuthID: "b", Provider: "codex", Priority: 10, Family: AccountFamilyWeekly, Quota: availableWeeklyQuota(now), LastSuccessAt: now.Add(time.Second)},
			},
			Now: now,
		}, now
}

func availableWeeklyQuota(now time.Time) ParsedQuota {
	return ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(5 * time.Hour)},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}
}

func TestSchedulerPickConcurrentSnapshotOnly(t *testing.T) {
	req, snapshot, now := pickBaselineFixture()
	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup
	errCh := make(chan string, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				decision := schedulerPickSnapshot(req, snapshot, now)
				if decision.AuthID != "a" {
					errCh <- decision.AuthID
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for authID := range errCh {
		t.Fatalf("selected %q, want a", authID)
	}
}

func BenchmarkSchedulerPickSnapshot(b *testing.B) {
	req, snapshot, now := pickBaselineFixture()
	active := map[string]struct{}{"a": {}, "b": {}}
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, MonthlyMode: snapshot.Config.MonthlyMode, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}, {ID: "b", Instance: 2, Cache: CacheFresh}}, ActiveHighestTier: active})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		decision := schedulerPickPublished(req, now)
		if decision.AuthID == "" {
			b.Fatal("no selection")
		}
	}
}
