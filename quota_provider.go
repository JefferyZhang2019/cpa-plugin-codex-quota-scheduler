package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// quotaProviderKey is the provider key this plugin can describe quota for.
const quotaProviderKey = "codex"

func handleQuotaIdentifier() any {
	return struct {
		Identifier string `json:"identifier"`
	}{Identifier: quotaProviderKey}
}

func handleQuotaDescribe() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{quotaProviderKey},
		DisplayName:        "Codex Quota Scheduler",
		SupportsReset:      false,
	}
}

func handleQuotaFetchMethod(raw []byte, now time.Time) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	return okEnvelope(quotaFetchResponseForAuth(globalState, req, now))
}

// quotaFetchResponseForAuth projects the cached scheduler state for one
// credential into the host's normalized quota shape. It is a read-only cache
// lookup: no network, no disk, no refresh is triggered, so it is safe to call
// from management contexts at any time.
func quotaFetchResponseForAuth(store *PluginState, req pluginapi.QuotaFetchRequest, now time.Time) pluginapi.QuotaFetchResponse {
	if store == nil || !strings.EqualFold(req.Provider, quotaProviderKey) {
		return pluginapi.QuotaFetchResponse{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	var account AccountState
	found := false
	for _, candidate := range store.Snapshot(now).Accounts {
		if req.AuthID != "" && candidate.AuthID == req.AuthID {
			account, found = candidate, true
			break
		}
		if req.AuthID == "" && req.AuthIndex != "" && candidate.AuthIndex == req.AuthIndex {
			account, found = candidate, true
			break
		}
	}
	if !found {
		return pluginapi.QuotaFetchResponse{}
	}
	response := pluginapi.QuotaFetchResponse{}
	if account.Quota.PlanType != "" {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: account.Quota.PlanType}
	}
	if group := quotaGroupFromWindow("5-hour window", "5h", account.Quota.FiveHour); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	longLabel, longWindow := "Long window", "long"
	switch account.Family {
	case AccountFamilyWeekly:
		longLabel, longWindow = "Weekly window", "7d"
	case AccountFamilyMonthly:
		longLabel, longWindow = "Monthly window", "30d"
	}
	if group := quotaGroupFromWindow(longLabel, longWindow, account.Quota.LongWindow); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	return response
}

func quotaGroupFromWindow(displayName, windowLabel string, window *QuotaWindow) *pluginapi.QuotaGroup {
	if window == nil {
		return nil
	}
	bucket := pluginapi.QuotaBucket{
		Window:            windowLabel,
		RemainingFraction: windowRemainingFraction(window),
	}
	if !window.ResetAt.IsZero() {
		bucket.ResetTime = window.ResetAt.UTC().Format(time.RFC3339)
	}
	return &pluginapi.QuotaGroup{DisplayName: displayName, Buckets: []pluginapi.QuotaBucket{bucket}}
}

func windowRemainingFraction(window *QuotaWindow) float64 {
	if window == nil {
		return 0
	}
	if window.UsedPercent == nil {
		return 1
	}
	fraction := (100 - *window.UsedPercent) / 100
	if fraction < 0 {
		return 0
	}
	if fraction > 1 {
		return 1
	}
	return fraction
}

// handlePluginQuiesce settles background work while the plugin stays loaded.
// The host calls it around reconfiguration; a subsequent register/reconfigure
// restarts the refresher through startGlobalRefresher.
func handlePluginQuiesce() ([]byte, error) {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		refresher.Stop()
	}
	globalState.RecordLog("info", "plugin.quiesced", "插件已静默，后台刷新暂停", nil, time.Now())
	return okEnvelope(struct{}{})
}
