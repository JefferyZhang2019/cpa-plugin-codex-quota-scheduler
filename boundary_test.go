package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type BoundarySentinels struct {
	AuthID       string
	AccountID    string
	Alias        string
	QuotaValue   string
	ResetRFC3339 string
	LogMessage   string
}

func (s BoundarySentinels) Values() []string {
	return []string{s.AuthID, s.AccountID, s.Alias, s.QuotaValue, s.ResetRFC3339, s.LogMessage}
}

func boundaryLeaks(body string, sentinels []string) []string {
	var leaked []string
	for _, sentinel := range sentinels {
		if strings.Contains(body, sentinel) {
			leaked = append(leaked, sentinel)
		}
	}
	return leaked
}

func TestSuiteBoundary(t *testing.T) {
	sentinels := BoundarySentinels{
		AuthID:       "SENTINEL_AUTH_X9K",
		AccountID:    "SENTINEL_ACCOUNT_Q7M",
		Alias:        "SENTINEL_ALIAS_P4V",
		QuotaValue:   "987654321",
		ResetRFC3339: "2099-12-31T23:58:57Z",
		LogMessage:   "SENTINEL_LOG_N6R",
	}
	store := seedBoundarySentinelsForTest(t, sentinels)

	for _, route := range registeredResourceRoutesForTest(t) {
		body := requestResourceForTest(t, store, route)
		if leaked := boundaryLeaks(body, sentinels.Values()); len(leaked) != 0 {
			t.Fatalf("%s leaked runtime sentinels %q", route, leaked)
		}
		for _, identifier := range []string{"quota", "scheduler_priority", "MANAGEMENT_BASE"} {
			if !strings.Contains(body, identifier) {
				t.Errorf("%s static shell missing dynamic-loading identifier %q", route, identifier)
			}
		}
	}

	t.Run("mutation scanner detects a test-owned leak", func(t *testing.T) {
		clean := "<!doctype html><title>static shell</title>"
		if leaked := boundaryLeaks(clean, sentinels.Values()); len(leaked) != 0 {
			t.Fatalf("clean sample reported leaks %q", leaked)
		}
		leakedSample := clean + sentinels.LogMessage
		leaked := boundaryLeaks(leakedSample, sentinels.Values())
		if len(leaked) != 1 || leaked[0] != sentinels.LogMessage {
			t.Fatalf("leaked sample reported %q, want %q", leaked, sentinels.LogMessage)
		}
	})
	resp := requestResourceMutationForTest(t, store, "/refresh")
	if resp.StatusCode < 400 {
		t.Fatalf("resource mutation status = %d", resp.StatusCode)
	}
}

func TestMockGroupDBoundary(t *testing.T) {
	TestSuiteBoundary(t)
}

func TestResourceBoundaryRejectsSensitiveBusinessDataLeak(t *testing.T) {
	sentinels := BoundarySentinels{AuthID: "LEAK_AUTH", AccountID: "LEAK_ACCOUNT", Alias: "LEAK_ALIAS", QuotaValue: "998877", ResetRFC3339: "2098-01-02T03:04:05Z", LogMessage: "LEAK_LOG"}
	store := seedBoundarySentinelsForTest(t, sentinels)
	for _, route := range registeredResourceRoutesForTest(t) {
		body := requestResourceForTest(t, store, route)
		if leaked := boundaryLeaks(body, sentinels.Values()); len(leaked) != 0 {
			t.Fatalf("Resource route %s leaked sensitive business data %q", route, leaked)
		}
		mutated := body + sentinels.AccountID
		if leaked := boundaryLeaks(mutated, sentinels.Values()); len(leaked) != 1 || leaked[0] != sentinels.AccountID {
			t.Fatalf("sensitive Resource leak guard accepted mutated response: %q", leaked)
		}
	}
}

func seedBoundarySentinelsForTest(t *testing.T, sentinels BoundarySentinels) *PluginState {
	t.Helper()
	resetAt, err := time.Parse(time.RFC3339, sentinels.ResetRFC3339)
	if err != nil {
		t.Fatalf("parse reset sentinel: %v", err)
	}
	used := 987654321.0
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{
		Observed: true,
		Priority: 7,
		AuthIDs:  map[string]struct{}{sentinels.AuthID: {}},
	})
	store.UpsertQuota(AccountState{
		AuthID:           sentinels.AuthID,
		AuthIndex:        sentinels.AccountID,
		ChatGPTAccountID: sentinels.AccountID,
		Provider:         "codex",
		Priority:         7,
		Family:           AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family: AccountFamilyWeekly,
			LongWindow: &QuotaWindow{
				Kind:        WindowWeekly,
				UsedPercent: &used,
				ResetAt:     resetAt,
			},
		},
		LastSuccessAt: resetAt.Add(-time.Hour),
	})
	store.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{
		"auth:" + sentinels.AuthID: {Alias: sentinels.Alias},
	}})
	store.RecordLog("info", "boundary.sentinel", sentinels.LogMessage, map[string]any{
		"quota":    sentinels.QuotaValue,
		"reset_at": sentinels.ResetRFC3339,
	}, resetAt)
	return store
}

func registeredResourceRoutesForTest(t *testing.T) []string {
	t.Helper()
	registration := RegisterManagement()
	if len(registration.Resources) == 0 {
		t.Fatal("no Resource routes registered")
	}
	routes := make([]string, 0, len(registration.Resources))
	for _, route := range registration.Resources {
		routes = append(routes, route.Path)
	}
	return routes
}

func requestResourceForTest(t *testing.T, store *PluginState, route string) string {
	t.Helper()
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource" + managementBasePath + route,
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET Resource %s status = %d; body=%s", route, resp.StatusCode, resp.Body)
	}
	return string(resp.Body)
}

func requestResourceMutationForTest(t *testing.T, store *PluginState, route string) pluginapi.ManagementResponse {
	t.Helper()
	return HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource" + managementBasePath + route,
	}, time.Now())
}
