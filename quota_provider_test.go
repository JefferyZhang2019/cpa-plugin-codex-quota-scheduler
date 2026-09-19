package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaProviderIdentifierAndDescribe(t *testing.T) {
	identifier, err := handleMethod(pluginabi.MethodQuotaIdentifier, nil)
	if err != nil {
		t.Fatal(err)
	}
	var idEnvelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Identifier string `json:"identifier"`
		} `json:"result"`
	}
	if err := json.Unmarshal(identifier, &idEnvelope); err != nil {
		t.Fatal(err)
	}
	if !idEnvelope.OK || idEnvelope.Result.Identifier != "codex" {
		t.Fatalf("identifier envelope = %s", identifier)
	}

	describe, err := handleMethod(pluginabi.MethodQuotaDescribe, nil)
	if err != nil {
		t.Fatal(err)
	}
	var describeEnvelope struct {
		Result pluginapi.QuotaDescribeResponse `json:"result"`
	}
	if err := json.Unmarshal(describe, &describeEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(describeEnvelope.Result.SupportedProviders) != 1 || describeEnvelope.Result.SupportedProviders[0] != "codex" {
		t.Fatalf("supported providers = %#v", describeEnvelope.Result.SupportedProviders)
	}
	if describeEnvelope.Result.SupportsReset {
		t.Fatal("SupportsReset must be false; the plugin cannot reset upstream quota")
	}
}

func TestQuotaFetchProjectsCachedWindows(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	used := 25.0
	longUsed := 40.0
	store.UpsertQuota(AccountState{
		AuthID:    "team",
		AuthIndex: "idx-team",
		Provider:  "codex",
		Family:    AccountFamilyWeekly,
		Quota: ParsedQuota{
			PlanType:   "team",
			Family:     AccountFamilyWeekly,
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: now.Add(3 * time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &longUsed, ResetAt: now.Add(72 * time.Hour)},
		},
		LastSuccessAt: now,
	})

	response := quotaFetchResponseForAuth(store, pluginapi.QuotaFetchRequest{Provider: "codex", AuthID: "team"}, now)
	if response.Subscription == nil || response.Subscription.Plan != "team" {
		t.Fatalf("subscription = %#v", response.Subscription)
	}
	if len(response.Groups) != 2 {
		t.Fatalf("groups = %#v", response.Groups)
	}
	if got := response.Groups[0].Buckets[0].RemainingFraction; got != 0.75 {
		t.Fatalf("five-hour remaining fraction = %v, want 0.75", got)
	}
	if response.Groups[0].Buckets[0].ResetTime == "" {
		t.Fatal("five-hour bucket missing reset time")
	}
	if got := response.Groups[1].Buckets[0].RemainingFraction; got != 0.6 {
		t.Fatalf("long window remaining fraction = %v, want 0.6", got)
	}
}

func TestQuotaFetchUnknownAuthAndForeignProviderStayEmpty(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now})

	if response := quotaFetchResponseForAuth(store, pluginapi.QuotaFetchRequest{Provider: "codex", AuthID: "missing"}, now); len(response.Groups) != 0 {
		t.Fatalf("unknown auth returned groups: %#v", response.Groups)
	}
	if response := quotaFetchResponseForAuth(store, pluginapi.QuotaFetchRequest{Provider: "gemini", AuthID: "team"}, now); len(response.Groups) != 0 {
		t.Fatalf("foreign provider returned groups: %#v", response.Groups)
	}
}

func TestRequestCompleteTerminalOutcomesLoggedAndSuccessRecorded(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "team", AuthIndex: "idx-team", Provider: "codex", LastSuccessAt: now})
	store.MarkAccountTemporaryExhausted("team", now.Add(3*time.Hour), usageLimitReachedReason)

	// Rejected requests never reach usage.handle; the terminal event is the
	// only signal.
	handleRequestCompletionEvent(store, pluginapi.RequestCompletion{
		RequestID: "req-1", Outcome: pluginapi.RequestCompletionRejected, StatusCode: 429,
	}, now)
	var logged bool
	for _, entry := range store.Snapshot(now).Logs {
		if entry.Event == "request.terminal" && entry.Fields["outcome"] == "rejected" {
			logged = true
		}
	}
	if !logged {
		t.Fatal("rejected request completion was not logged")
	}

	// A succeeded completion with metadata attribution clears the temporary
	// marker as a supplementary success signal.
	handleRequestCompletionEvent(store, pluginapi.RequestCompletion{
		RequestID: "req-2", Outcome: pluginapi.RequestCompletionSucceeded,
		Metadata: map[string]any{"selected_auth_id": "team"},
	}, now)
	if account := accountByAuthID(t, store.Snapshot(now), "team"); account.TemporaryExhausted {
		t.Fatalf("temporary exhaustion survived a metadata-attributed success: %#v", account)
	}

	// Without attribution the success path is a no-op (no panic, no change).
	handleRequestCompletionEvent(store, pluginapi.RequestCompletion{
		RequestID: "req-3", Outcome: pluginapi.RequestCompletionSucceeded,
	}, now)
}
