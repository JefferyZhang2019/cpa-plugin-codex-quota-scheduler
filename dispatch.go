package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	currentConfig          atomic.Value
	globalState            = NewPluginState(DefaultConfig())
	globalTrials           = NewTrialRegistry()
	globalEvidenceIntents  = make(chan EvidenceIntent, 64)
	evidenceConsumerOnce   sync.Once
	refresherMu            sync.Mutex
	globalRefresher        *QuotaRefresher
	globalRosterController *RosterController
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		startGlobalRefresher()
		return okEnvelope(PluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsageHandle(request)
	case pluginabi.MethodRequestComplete:
		return handleRequestComplete(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(handleQuotaIdentifier())
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(handleQuotaDescribe())
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetchMethod(request, time.Now())
	case pluginabi.MethodPluginQuiesce:
		return handlePluginQuiesce()
	case pluginabi.MethodManagementRegister:
		return okEnvelope(RegisterManagement())
	case pluginabi.MethodManagementHandle:
		return handleManagementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := DecodeConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	disk, loadedDisk, err := loadUserDataWithMigration(semanticStatePaths(defaultStatePath()), OSFileHooks(), nil)
	if err == nil && loadedDisk {
		cfg = disk.Config
	} else {
		disk = PluginDiskState{Config: cfg}
	}
	currentConfig.Store(cfg)
	globalState.ReplaceConfig(cfg)
	globalState.SetAnnotations(AnnotationState{Accounts: disk.Accounts, Groups: disk.Groups})
	startEvidenceConsumer()
	publishSchedulerState(globalState, nil, time.Now())
	return nil
}

func startEvidenceConsumer() {
	evidenceConsumerOnce.Do(func() {
		go func() {
			for intent := range globalEvidenceIntents {
				consumeEvidenceIntent(intent)
			}
		}()
	})
}

func consumeEvidenceIntent(intent EvidenceIntent) {
	globalTrials.MarkEvidencePending(intent.Instance, true)
	refresherMu.Lock()
	r := globalRefresher
	refresherMu.Unlock()
	if r != nil {
		r.RefreshOneSoon(intent.AuthID)
	}
}

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	decision := schedulerPickPublished(req, time.Now())
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:          decision.AuthID,
		DelegateBuiltin: decision.DelegateBuiltin,
		Handled:         decision.Handled,
	})
}

func schedulerPickSnapshot(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision {
	return PickCodexAccount(req, snapshot, now)
}

func logSchedulerDecision(store *PluginState, req pluginapi.SchedulerPickRequest, decision PickDecision, now time.Time) {
	if store == nil {
		return
	}
	level := "info"
	event := "scheduler.unhandled"
	message := "请求未由插件接管"
	fields := map[string]any{
		"model":         req.Model,
		"provider":      req.Provider,
		"reason":        decision.Reason,
		"ordered_count": len(decision.Ordered),
	}
	if decision.AuthID != "" {
		event = "scheduler.selected"
		message = "请求已由插件接管"
		fields["auth_id"] = decision.AuthID
		if selected, ok := findScheduledAccount(decision.Ordered, decision.AuthID); ok {
			fields["selected_queue_status"] = string(selected.QueueStatus)
			fields["selected_sort_time"] = selected.SortTime.Format(time.RFC3339)
			fields["selected_cpa_priority"] = selected.CPAPriority
			fields["selected_scheduler_priority"] = selected.SchedulerPriority
		}
	} else if decision.DelegateBuiltin != "" {
		event = "scheduler.fallback"
		message = "插件触发内置调度 fallback"
		fields["fallback"] = decision.DelegateBuiltin
		fields["unavailable_summary"] = unavailableSummary(decision.Ordered)
	} else if decision.Handled {
		event = "scheduler.handled"
		message = "插件已处理但未选择账号"
	}
	store.RecordLog(level, event, message, fields, now)
}

func findScheduledAccount(accounts []ScheduledAccount, authID string) (ScheduledAccount, bool) {
	for _, account := range accounts {
		if account.AuthID == authID {
			return account, true
		}
	}
	return ScheduledAccount{}, false
}

func unavailableSummary(accounts []ScheduledAccount) string {
	if len(accounts) == 0 {
		return "no ordered candidates"
	}
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		reason := account.UnavailableReason
		if reason == "" && account.Available {
			reason = "available"
		}
		if reason == "" {
			reason = string(account.QueueStatus)
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", account.AuthID, account.QueueStatus, reason))
	}
	return strings.Join(parts, "; ")
}

func handleUsageHandle(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	refresherMu.Lock()
	rosterController := globalRosterController
	refresherMu.Unlock()
	if rosterController != nil {
		go func() { _, _ = rosterController.WakeForActivity(context.Background()) }()
	}
	HandleUsageFeedback(globalState, record, now)
	evidenceKind := EvidenceUnknown
	quotaLimitFeedback := false
	if record.Provider == "codex" && !record.Failed {
		evidenceKind = EvidenceRequestSuccess
	} else if _, ok := DetectQuotaFailure(record, now); ok {
		evidenceKind = EvidenceUsageFeedback
		quotaLimitFeedback = true
	}
	snapshot := publishedSchedulerSnapshot.Load()
	if snapshot != nil && snapshot.Trials != nil {
		for _, account := range snapshot.Accounts {
			matches := record.AuthID != "" && account.ID == record.AuthID
			if record.AuthID == "" && record.AuthIndex != "" {
				matches = account.AuthIndex == record.AuthIndex
			}
			if evidenceKind != EvidenceUnknown && matches {
				snapshot.Trials.ObserveEvidence(account.Instance, Evidence{Kind: evidenceKind, At: now})
				break
			}
		}
	}
	if quotaLimitFeedback && snapshot != nil {
		publishSchedulerState(globalState, snapshot.ActiveHighestTier, now)
	}
	return okEnvelope(map[string]any{})
}

func handleManagementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	refresherMu.Lock()
	rosterController := globalRosterController
	refresher := globalRefresher
	refresherMu.Unlock()
	if isResourcePath(req.Path) {
		return okEnvelope(HandleManagementRequest(globalState, req, time.Now()))
	}
	if rosterController == nil {
		now := time.Now()
		lifecycle := ManagementLifecycleSnapshot{Roster: ActiveRoster{Capability: CapabilityB, Health: RosterWaiting}}
		return okEnvelope(HandleManagementRequestWithLifecycle(globalState, req, now, lifecycle))
	}
	active, _ := rosterController.WakeForManagement(context.Background())
	now := time.Now()
	lifecycle := ManagementLifecycleSnapshot{Roster: active, CredentialAmbiguous: managementCredentialAmbiguous(refresher, active, now)}
	if refresher != nil {
		lifecycle.ResolveCredential = func(ctx context.Context, authID string, action CredentialResolutionAction) error {
			return refresher.ResolveCredentialAmbiguity(ctx, active, authID, action)
		}
	}
	return okEnvelope(HandleManagementRequestWithLifecycle(globalState, req, now, lifecycle))
}

func managementCredentialAmbiguous(refresher *QuotaRefresher, active ActiveRoster, now time.Time) bool {
	if len(active.Instances) == 0 || refresher == nil || refresher.runtimeStore == nil {
		return false
	}
	state, err := refresher.runtimeStore.PersistentSnapshot()
	if err != nil {
		return false
	}
	for _, authID := range active.Instances {
		binding, ok := state.Bindings[authID]
		if !ok || binding.AuthID != authID || binding.Instance == 0 {
			continue
		}
		chain, ok := state.CredentialChains[binding.Instance]
		if !ok {
			continue
		}
		if len(chain.Transitions) > 0 {
			last := chain.Transitions[len(chain.Transitions)-1]
			if last.Phase == TransitionPlanned || last.Phase == TransitionOutcomeUnknown {
				return true
			}
		}
		if ClassifyObservedCredentialAt(chain, binding.Fingerprint, now).Kind == CredentialAmbiguous {
			return true
		}
	}
	return false
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func startGlobalRefresher() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher == nil {
		return
	}
	refresher.Start()
	if globalState.Config().RefreshOnStartup {
		refresher.RefreshSoon()
	}
}

func refreshGlobalRefresherSoon() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		globalState.RecordLog("info", "quota.refresh_requested", "已请求后台刷新额度", nil, time.Now())
		refresher.RefreshSoon()
	}
}

func refreshGlobalRefresherOneSoon(authID string) {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		globalState.RecordLog("info", "quota.refresh_one_requested", "已请求后台刷新单个账号额度", map[string]any{"auth_id": authID}, time.Now())
		refresher.RefreshOneSoon(authID)
	}
}

func refreshGlobalRefresherDueSoon(req pluginapi.SchedulerPickRequest, admissionVersion uint64, now time.Time) {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		refresher.OnSchedulerPick(req, admissionVersion, now)
	}
}
