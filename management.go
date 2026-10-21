package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const managementBasePath = "/plugins/" + PluginID

var managementRefreshSoon = func() {}
var managementRefreshOneSoon = func(authID string) {}
var managementProvisionalRiskChanged = func(bool) {}

type StatusPayload struct {
	PluginID              string                 `json:"plugin_id"`
	GeneratedAt           time.Time              `json:"generated_at"`
	Shell                 bool                   `json:"shell,omitempty"`
	NextAuthID            string                 `json:"next_auth_id"`
	MonthlyMode           MonthlyMode            `json:"monthly_mode"`
	HandleEnabled         bool                   `json:"handle_enabled"`
	LastSelected          string                 `json:"last_selected"`
	LastReason            string                 `json:"last_reason"`
	RefreshActive         bool                   `json:"refresh_active"`
	RefreshState          string                 `json:"refresh_state"`
	LastCodexActivityText string                 `json:"last_codex_activity_text,omitempty"`
	LastAuthScanText      string                 `json:"last_auth_scan_text,omitempty"`
	CodexAuthCount        int                    `json:"codex_auth_count"`
	Roster                RosterLifecyclePayload `json:"roster"`
	EmptyState            EmptyStatePayload      `json:"empty_state,omitempty"`
	Settings              SettingsPayload        `json:"settings"`
	Accounts              []StatusAccount        `json:"accounts"`
	Groups                []StatusGroup          `json:"groups,omitempty"`
	Logs                  []LogEntry             `json:"logs"`
}

type ManagementLifecycleSnapshot struct {
	Roster              ActiveRoster
	CredentialAmbiguous bool
	ResolveCredential   func(context.Context, string, CredentialResolutionAction) error
	// ManagedLifecycle projects durable managed-disable records (keyed by
	// auth index) for the status payload.
	ManagedLifecycle map[string]ManagedDisableRecord
}

type RosterLifecyclePayload struct {
	Capability          HostCapability `json:"capability"`
	Health              RosterHealth   `json:"health"`
	Confirmed           bool           `json:"confirmed"`
	Provisional         bool           `json:"provisional"`
	Degraded            bool           `json:"degraded"`
	FailClosed          bool           `json:"fail_closed"`
	WaitingRoster       bool           `json:"waiting_roster"`
	BackgroundAllowed   bool           `json:"background_allowed"`
	HighestPriority     int            `json:"highest_priority"`
	Generation          uint64         `json:"generation"`
	AdmissionObserved   bool           `json:"admission_observed"`
	AdmissionVersion    uint64         `json:"admission_version"`
	AdmissionPriority   int            `json:"admission_priority"`
	AdmittedAuthCount   int            `json:"admitted_auth_count"`
	RosterEntryCount    int            `json:"roster_entry_count"`
	RosterInstanceCount int            `json:"roster_instance_count"`
	CredentialAmbiguous bool           `json:"credential_ambiguous"`
	RiskOptionEnabled   bool           `json:"risk_option_enabled"`
	RiskOptionAvailable bool           `json:"risk_option_available"`
	Warning             string         `json:"warning,omitempty"`
	RiskWarning         string         `json:"risk_warning,omitempty"`
}

type EmptyStatePayload struct {
	Reason  string `json:"reason,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

type SettingsPayload struct {
	HandleEnabled                   bool        `json:"handle_enabled"`
	MonthlyMode                     MonthlyMode `json:"monthly_mode"`
	QuotaRefreshInterval            string      `json:"quota_refresh_interval"`
	StaleAfter                      string      `json:"stale_after"`
	EnableUsageFeedback             bool        `json:"enable_usage_feedback"`
	EnableResetProbe                bool        `json:"enable_reset_probe"`
	ProbeOnProvisionalRoster        bool        `json:"probe_on_provisional_roster"`
	MaxRefreshConcurrency           int         `json:"max_refresh_concurrency"`
	QuotaEndpoint                   string      `json:"quota_endpoint"`
	RefreshActiveWindow             string      `json:"refresh_active_window"`
	RefreshAfterResetDelay          string      `json:"refresh_after_reset_delay"`
	RefreshRetryDelays              string      `json:"refresh_retry_delays"`
	AuthFailureRetryInterval        string      `json:"auth_failure_retry_interval"`
	RefreshOnStartup                bool        `json:"refresh_on_startup"`
	CircuitFailureThreshold         int         `json:"circuit_failure_threshold"`
	CircuitOpenDuration             string      `json:"circuit_open_duration"`
	CircuitHalfOpenSuccessThreshold int         `json:"circuit_half_open_success_threshold"`
	MaxLogEntries                   int         `json:"max_log_entries"`
	LogRetention                    string      `json:"log_retention"`
	ScheduleAcrossPriorities        bool        `json:"schedule_across_priorities"`
	EnableManagedQuotaDisable       bool        `json:"enable_managed_quota_disable"`
	LifecycleEventLimit             int         `json:"lifecycle_event_limit"`

	// Retry chain. Every field is inert until RetryEnabled is on and the chain
	// has at least one row, so a saved payload that omits them keeps today's
	// routing.
	RetryEnabled        bool            `json:"retry_enabled"`
	RetryShadow         bool            `json:"retry_shadow"`
	RetryAlways         bool            `json:"retry_always"`
	RetryMaxAttempts    int             `json:"retry_max_attempts"`
	RetryStallTimeout   string          `json:"retry_stall_timeout"`
	RetryHoldTimeout    string          `json:"retry_hold_timeout"`
	RetryChainDeadline  string          `json:"retry_chain_deadline"`
	RetryMaxFrames      int             `json:"retry_max_frames"`
	RetryMaxBytes       string          `json:"retry_max_bytes"`
	RetryStripReasoning bool            `json:"retry_strip_reasoning"`
	RetryChain          []RetryChainRow `json:"retry_chain"`
}

// ManagedDisableSummary projects the managed-quota lifecycle record of one
// account for the Management UI.
type ManagedDisableSummary struct {
	State          string `json:"state"`
	DisabledAt     string `json:"disabled_at,omitempty"`
	NextCheckAt    string `json:"next_check_at,omitempty"`
	LastQuotaCheck string `json:"last_quota_check_at,omitempty"`
}

type StatusAccount struct {
	Rank                         int                    `json:"rank"`
	AuthID                       string                 `json:"auth_id"`
	Alias                        string                 `json:"alias,omitempty"`
	Notes                        string                 `json:"notes,omitempty"`
	GroupID                      string                 `json:"group_id,omitempty"`
	Group                        string                 `json:"group,omitempty"`
	GroupNotes                   string                 `json:"group_notes,omitempty"`
	Tags                         []string               `json:"tags,omitempty"`
	CPAPriority                  int                    `json:"cpa_priority"`
	SchedulerPriority            int                    `json:"scheduler_priority"`
	Family                       AccountFamily          `json:"family"`
	QueueStatus                  QueueStatus            `json:"queue_status"`
	Available                    bool                   `json:"available"`
	UnavailableReason            string                 `json:"unavailable_reason,omitempty"`
	ResetExpiry                  time.Time              `json:"reset_expiry,omitempty"`
	ResetExpiryText              string                 `json:"reset_expiry_text,omitempty"`
	CacheAge                     string                 `json:"cache_age,omitempty"`
	CacheAgeSeconds              int64                  `json:"cache_age_seconds,omitempty"`
	QuotaSource                  string                 `json:"quota_source,omitempty"`
	QuotaObservedSecondsAgo      int64                  `json:"quota_observed_seconds_ago,omitempty"`
	ManagedDisable               *ManagedDisableSummary `json:"managed_disable,omitempty"`
	LastError                    string                 `json:"last_error,omitempty"`
	RefreshDueReason             string                 `json:"refresh_due_reason,omitempty"`
	NextRetryText                string                 `json:"next_retry_text,omitempty"`
	AuthFailure                  bool                   `json:"auth_failure"`
	StatusNote                   string                 `json:"status_note,omitempty"`
	FiveHour                     StatusWindow           `json:"five_hour"`
	LongWindow                   StatusWindow           `json:"long_window"`
	Circuit                      StatusCircuit          `json:"circuit"`
	ResetCreditsAvailableCount   *int                   `json:"reset_credits_available_count,omitempty"`
	ResetCreditsTotalEarnedCount *int                   `json:"reset_credits_total_earned_count,omitempty"`
	ResetCredits                 []ResetCredit          `json:"reset_credits,omitempty"`
	ResetProbes                  []StatusResetProbe     `json:"reset_probes,omitempty"`
}

type StatusResetProbe struct {
	WindowKind  WindowKind       `json:"window_kind"`
	Status      ResetProbeStatus `json:"status"`
	ResetAt     string           `json:"reset_at,omitempty"`
	NextCheckAt string           `json:"next_check_at,omitempty"`
	LastProbeAt string           `json:"last_probe_at,omitempty"`
	VerifiedAt  string           `json:"verified_at,omitempty"`
	Attempts    int              `json:"attempts,omitempty"`
	Error       string           `json:"error,omitempty"`
}

type StatusCircuit struct {
	State         CircuitState `json:"state"`
	Label         string       `json:"label"`
	FailureCount  int          `json:"failure_count"`
	SuccessCount  int          `json:"success_count"`
	NextProbeAt   time.Time    `json:"next_probe_at,omitempty"`
	NextProbeText string       `json:"next_probe_text,omitempty"`
	Reason        string       `json:"reason,omitempty"`
}

type StatusGroup struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Notes string   `json:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Color string   `json:"color,omitempty"`
}

type StatusWindow struct {
	Kind             WindowKind `json:"kind,omitempty"`
	Label            string     `json:"label"`
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	DisplayText      string     `json:"display_text"`
	Exhausted        bool       `json:"exhausted"`
	ResetAt          time.Time  `json:"reset_at,omitempty"`
	ResetText        string     `json:"reset_text,omitempty"`
	Missing          bool       `json:"missing"`
}

func RegisterManagement() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Codex Scheduler",
				Description: "Open scheduler quota status.",
			},
		},
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementBasePath + "/status", Description: "Scheduler quota status."},
			{Method: http.MethodGet, Path: managementBasePath + "/settings", Description: "Read scheduler settings."},
			{Method: http.MethodPut, Path: managementBasePath + "/settings", Description: "Update scheduler settings."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh", Description: "Refresh quota status soon."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh/account", Description: "Refresh one account quota soon."},
			{Method: http.MethodGet, Path: managementBasePath + "/logs", Description: "Read scheduler logs."},
			{Method: http.MethodGet, Path: managementBasePath + "/export", Description: "Export scheduler configuration."},
			{Method: http.MethodPost, Path: managementBasePath + "/import", Description: "Import scheduler configuration."},
			{Method: http.MethodGet, Path: managementBasePath + "/annotations", Description: "Read quota annotations."},
			{Method: http.MethodPut, Path: managementBasePath + "/annotations", Description: "Replace quota annotations."},
			{Method: http.MethodPatch, Path: managementBasePath + "/annotations/account", Description: "Update one account annotation."},
			{Method: http.MethodPatch, Path: managementBasePath + "/annotations/group", Description: "Update one group annotation."},
			{Method: http.MethodPost, Path: managementBasePath + "/credentials/resolve", Description: "Resolve an active credential ambiguity."},
			{Method: http.MethodGet, Path: managementBasePath + "/retry/status", Description: "Retry chain counters and attempt history."},
		},
	}
}

func HandleManagementRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	return handleManagementRequest(store, req, now, nil)
}

func HandleManagementRequestWithLifecycle(store *PluginState, req pluginapi.ManagementRequest, now time.Time, lifecycle ManagementLifecycleSnapshot) pluginapi.ManagementResponse {
	return handleManagementRequest(store, req, now, &lifecycle)
}

func handleManagementRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time, lifecycle *ManagementLifecycleSnapshot) pluginapi.ManagementResponse {
	if store == nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": "plugin state unavailable"})
	}
	if now.IsZero() {
		now = time.Now()
	}

	method := strings.ToUpper(req.Method)
	path := normalizeManagementPath(req.Path)
	if isResourcePath(req.Path) && !resourceRouteAllowed(method, path) {
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	switch {
	case method == http.MethodGet && path == "/status":
		return handleStatusRequest(store, req, now, lifecycle)
	case method == http.MethodGet && path == "/settings":
		return jsonManagementResponse(http.StatusOK, SettingsFromConfig(store.Config()))
	case method == http.MethodPut && path == "/settings":
		return handlePutSettings(store, req, now)
	case method == http.MethodPost && path == "/refresh":
		triggerRefreshSoon()
		store.RecordLog("info", "ui.refresh_requested", "页面请求刷新额度", nil, now)
		return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodPost && path == "/refresh/account":
		return handleRefreshAccountRequest(store, req, now)
	case method == http.MethodGet && path == "/logs":
		return jsonManagementResponse(http.StatusOK, map[string]any{"logs": store.Snapshot(now).Logs})
	case method == http.MethodGet && path == "/retry/status":
		return jsonManagementResponse(http.StatusOK, retryStatusSnapshot(store.Config()))
	case method == http.MethodGet && path == "/export":
		return handleExportState(store, now)
	case method == http.MethodPost && path == "/import":
		return handleImportState(store, req.Body, now)
	case method == http.MethodGet && path == "/annotations":
		return jsonManagementResponse(http.StatusOK, store.Annotations())
	case method == http.MethodPut && path == "/annotations":
		return handlePutAnnotations(store, req)
	case method == http.MethodPatch && path == "/annotations/account":
		return handlePatchAccountAnnotation(store, req, now)
	case method == http.MethodPatch && path == "/annotations/group":
		return handlePatchGroupAnnotation(store, req, now)
	case method == http.MethodPost && path == "/credentials/resolve":
		return handleCredentialResolution(store, req, now, lifecycle)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func handleCredentialResolution(store *PluginState, req pluginapi.ManagementRequest, now time.Time, lifecycle *ManagementLifecycleSnapshot) pluginapi.ManagementResponse {
	if lifecycle == nil || lifecycle.ResolveCredential == nil {
		return jsonManagementResponse(http.StatusConflict, map[string]string{"error": "credential resolution unavailable"})
	}
	var payload struct {
		AuthID string                     `json:"auth_id"`
		Action CredentialResolutionAction `json:"action"`
	}
	if err := json.Unmarshal(req.Body, &payload); err != nil || strings.TrimSpace(payload.AuthID) == "" || !payload.Action.Valid() {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "auth_id and valid action are required"})
	}
	if err := lifecycle.ResolveCredential(context.Background(), payload.AuthID, payload.Action); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrCredentialResolutionScope) || errors.Is(err, ErrBindingNotRosterConfirmed) {
			status = http.StatusConflict
		}
		return jsonManagementResponse(status, map[string]string{"error": err.Error()})
	}
	store.RecordLog("info", "credential.ambiguity_resolved", "credential ambiguity resolution completed", map[string]any{"auth_id": payload.AuthID, "action": payload.Action}, now)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func isResourcePath(path string) bool {
	return path == "/v0/resource"+managementBasePath || strings.HasPrefix(path, "/v0/resource"+managementBasePath+"/")
}

// resourceRouteAllowed is the Resource/Management security boundary: Resource
// exposes only the static status shell; business data and mutations stay under
// the authenticated Management routes.
func resourceRouteAllowed(method, path string) bool {
	if method == http.MethodGet {
		switch path {
		case "/status":
			return true
		default:
			return false
		}
	}
	return false
}

func SettingsFromConfig(cfg Config) SettingsPayload {
	return SettingsPayload{
		HandleEnabled:                   cfg.HandleEnabled,
		MonthlyMode:                     cfg.MonthlyMode,
		QuotaRefreshInterval:            cfg.QuotaRefreshInterval.String(),
		StaleAfter:                      cfg.StaleAfter.String(),
		EnableUsageFeedback:             cfg.EnableUsageFeedback,
		EnableResetProbe:                cfg.EnableResetProbe,
		ProbeOnProvisionalRoster:        cfg.ProbeOnProvisionalRoster,
		MaxRefreshConcurrency:           cfg.MaxRefreshConcurrency,
		QuotaEndpoint:                   cfg.QuotaEndpoint,
		RefreshActiveWindow:             cfg.RefreshActiveWindow.String(),
		RefreshAfterResetDelay:          cfg.RefreshAfterResetDelay.String(),
		RefreshRetryDelays:              formatDurationList(cfg.RefreshRetryDelays),
		AuthFailureRetryInterval:        cfg.AuthFailureRetryInterval.String(),
		RefreshOnStartup:                cfg.RefreshOnStartup,
		CircuitFailureThreshold:         cfg.CircuitFailureThreshold,
		CircuitOpenDuration:             cfg.CircuitOpenDuration.String(),
		CircuitHalfOpenSuccessThreshold: cfg.CircuitHalfOpenSuccessThreshold,
		MaxLogEntries:                   cfg.MaxLogEntries,
		LogRetention:                    cfg.LogRetention.String(),
		ScheduleAcrossPriorities:        cfg.ScheduleAcrossPriorities,
		EnableManagedQuotaDisable:       cfg.EnableManagedQuotaDisable,
		LifecycleEventLimit:             cfg.LifecycleEventLimit,
		RetryEnabled:                    cfg.RetryEnabled,
		RetryShadow:                     cfg.RetryShadow,
		RetryAlways:                     cfg.RetryAlways,
		RetryMaxAttempts:                cfg.RetryMaxAttempts,
		RetryStallTimeout:               formatRetryDuration(cfg.RetryStallTimeout),
		RetryHoldTimeout:                formatRetryDuration(cfg.RetryHoldTimeout),
		RetryChainDeadline:              formatRetryDuration(cfg.RetryChainDeadline),
		RetryMaxFrames:                  cfg.RetryMaxFrames,
		RetryMaxBytes:                   formatByteSize(cfg.RetryMaxBytes),
		RetryStripReasoning:             cfg.RetryStripReasoning,
		RetryChain:                      normalizedRetryChainPayload(cfg.RetryChain),
	}
}

// formatRetryDuration renders a retry budget the way the page documents it. A
// whole number of seconds becomes "60s" rather than Go's "1m0s", so the value
// the page loads is a value the page's own validator accepts.
func formatRetryDuration(value time.Duration) string {
	if value <= 0 {
		return "0s"
	}
	if value%time.Second == 0 {
		return strconv.FormatInt(int64(value/time.Second), 10) + "s"
	}
	return value.String()
}

// normalizedRetryChainPayload keeps the API payload stable for an unset chain,
// so the page can tell "no chain" from "not reported" and render an empty list
// rather than a null.
func normalizedRetryChainPayload(rows []RetryChainRow) []RetryChainRow {
	normalized := NormalizeRetryChain(rows)
	if normalized == nil {
		return []RetryChainRow{}
	}
	return normalized
}

func formatByteSize(size int64) string {
	if size <= 0 {
		return "0"
	}
	const (
		mib = int64(1) << 20
		kib = int64(1) << 10
	)
	switch {
	case size%mib == 0:
		return strconv.FormatInt(size/mib, 10) + "MB"
	case size%kib == 0:
		return strconv.FormatInt(size/kib, 10) + "KB"
	default:
		return strconv.FormatInt(size, 10) + "B"
	}
}

func ConfigFromSettings(base Config, payload SettingsPayload) (Config, error) {
	cfg := NormalizeConfig(base)
	if payload.MonthlyMode != "" {
		cfg.MonthlyMode = payload.MonthlyMode
	}
	if cfg.MonthlyMode != MonthlyModePriority && cfg.MonthlyMode != MonthlyModeExpiryOrder {
		return Config{}, jsonError("monthly_mode must be expiry_order or priority")
	}
	if payload.QuotaRefreshInterval != "" {
		d, err := time.ParseDuration(payload.QuotaRefreshInterval)
		if err != nil || d <= 0 {
			return Config{}, jsonError("quota_refresh_interval must be a positive duration")
		}
		cfg.QuotaRefreshInterval = d
	}
	if payload.StaleAfter != "" {
		d, err := time.ParseDuration(payload.StaleAfter)
		if err != nil || d <= 0 {
			return Config{}, jsonError("stale_after must be a positive duration")
		}
		cfg.StaleAfter = d
	}
	cfg.HandleEnabled = payload.HandleEnabled
	cfg.ScheduleAcrossPriorities = payload.ScheduleAcrossPriorities
	cfg.EnableManagedQuotaDisable = payload.EnableManagedQuotaDisable
	if payload.LifecycleEventLimit > 0 {
		cfg.LifecycleEventLimit = payload.LifecycleEventLimit
	}
	cfg.EnableUsageFeedback = payload.EnableUsageFeedback
	cfg.EnableResetProbe = payload.EnableResetProbe
	cfg.ProbeOnProvisionalRoster = payload.ProbeOnProvisionalRoster
	if payload.MaxRefreshConcurrency <= 0 {
		return Config{}, jsonError("max_refresh_concurrency must be positive")
	}
	cfg.MaxRefreshConcurrency = payload.MaxRefreshConcurrency
	if strings.TrimSpace(payload.QuotaEndpoint) != "" {
		endpoint, err := validateQuotaEndpoint(payload.QuotaEndpoint)
		if err != nil {
			return Config{}, err
		}
		cfg.QuotaEndpoint = endpoint
	}
	if payload.RefreshActiveWindow != "" {
		d, err := time.ParseDuration(payload.RefreshActiveWindow)
		if err != nil || d <= 0 {
			return Config{}, jsonError("refresh_active_window must be a positive duration")
		}
		cfg.RefreshActiveWindow = d
	}
	if payload.RefreshAfterResetDelay != "" {
		d, err := time.ParseDuration(payload.RefreshAfterResetDelay)
		if err != nil || d <= 0 {
			return Config{}, jsonError("refresh_after_reset_delay must be a positive duration")
		}
		cfg.RefreshAfterResetDelay = d
	}
	if payload.RefreshRetryDelays != "" {
		delays, err := parseDurationList(payload.RefreshRetryDelays)
		if err != nil {
			return Config{}, jsonError("refresh_retry_delays must be positive comma-separated durations")
		}
		cfg.RefreshRetryDelays = delays
	}
	if payload.AuthFailureRetryInterval != "" {
		d, err := time.ParseDuration(payload.AuthFailureRetryInterval)
		if err != nil || d <= 0 {
			return Config{}, jsonError("auth_failure_retry_interval must be a positive duration")
		}
		cfg.AuthFailureRetryInterval = d
	}
	cfg.RefreshOnStartup = payload.RefreshOnStartup
	if payload.CircuitFailureThreshold > 0 {
		cfg.CircuitFailureThreshold = payload.CircuitFailureThreshold
	}
	if payload.CircuitOpenDuration != "" {
		d, err := time.ParseDuration(payload.CircuitOpenDuration)
		if err != nil || d <= 0 {
			return Config{}, jsonError("circuit_open_duration must be a positive duration")
		}
		cfg.CircuitOpenDuration = d
	}
	if payload.CircuitHalfOpenSuccessThreshold > 0 {
		cfg.CircuitHalfOpenSuccessThreshold = payload.CircuitHalfOpenSuccessThreshold
	}
	if payload.MaxLogEntries > 0 {
		cfg.MaxLogEntries = payload.MaxLogEntries
	}
	if payload.LogRetention != "" {
		d, err := time.ParseDuration(payload.LogRetention)
		if err != nil || d <= 0 {
			return Config{}, jsonError("log_retention must be a positive duration")
		}
		cfg.LogRetention = d
	}
	cfg.RetryEnabled = payload.RetryEnabled
	cfg.RetryShadow = payload.RetryShadow
	cfg.RetryAlways = payload.RetryAlways
	if cfg.RetryAlways {
		cfg.RetryShadow = false
	}
	cfg.RetryStripReasoning = payload.RetryStripReasoning
	if payload.RetryMaxAttempts > 0 {
		if payload.RetryMaxAttempts > maxRetryMaxAttempts {
			return Config{}, jsonError("retry_max_attempts must be between 1 and " + strconv.Itoa(maxRetryMaxAttempts))
		}
		cfg.RetryMaxAttempts = payload.RetryMaxAttempts
	}
	for _, field := range []struct {
		name  string
		raw   string
		apply func(time.Duration)
	}{
		{"retry_stall_timeout", payload.RetryStallTimeout, func(d time.Duration) { cfg.RetryStallTimeout = d }},
		{"retry_hold_timeout", payload.RetryHoldTimeout, func(d time.Duration) { cfg.RetryHoldTimeout = d }},
		{"retry_chain_deadline", payload.RetryChainDeadline, func(d time.Duration) { cfg.RetryChainDeadline = d }},
	} {
		if field.raw == "" {
			continue
		}
		d, err := time.ParseDuration(field.raw)
		if err != nil || d <= 0 {
			return Config{}, jsonError(field.name + " must be a positive duration")
		}
		field.apply(d)
	}
	if payload.RetryMaxFrames > 0 {
		cfg.RetryMaxFrames = payload.RetryMaxFrames
	}
	if strings.TrimSpace(payload.RetryMaxBytes) != "" {
		size, err := parseByteSize(payload.RetryMaxBytes)
		if err != nil {
			return Config{}, jsonError("retry_max_bytes must be a positive size such as 8MB")
		}
		cfg.RetryMaxBytes = size
	}
	if payload.RetryChain != nil {
		chain := NormalizeRetryChain(payload.RetryChain)
		if err := validateRetryChain(chain); err != nil {
			return Config{}, jsonError(err.Error())
		}
		cfg.RetryChain = chain
	}
	return NormalizeConfig(cfg), nil
}

func formatDurationList(values []time.Duration) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value > 0 {
			parts = append(parts, value.String())
		}
	}
	return strings.Join(parts, ",")
}

type jsonError string

func (e jsonError) Error() string { return string(e) }

func handlePutSettings(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var payload SettingsPayload
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := saveSettingsPayload(store, payload)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.settings_saved", "页面保存调度设置", nil, now)
	}
	return resp
}

func saveSettingsPayload(store *PluginState, payload SettingsPayload) pluginapi.ManagementResponse {
	previousRisk := store.Config().ProbeOnProvisionalRoster
	cfg, err := ConfigFromSettings(store.Config(), payload)
	if err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	disk := diskStateFromStore(store)
	disk.Config = cfg
	if err := SaveUserData(semanticStatePaths(defaultStatePath()).UserData, disk); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.ReplaceConfig(cfg)
	currentConfig.Store(cfg)
	if cfg.ProbeOnProvisionalRoster != previousRisk {
		managementProvisionalRiskChanged(cfg.ProbeOnProvisionalRoster)
	}
	return jsonManagementResponse(http.StatusOK, SettingsFromConfig(cfg))
}

type refreshAccountPayload struct {
	AuthID string `json:"auth_id"`
}

func handleRefreshAccountRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	authID := strings.TrimSpace(req.Query.Get("auth_id"))
	if authID == "" && len(req.Body) > 0 {
		var payload refreshAccountPayload
		if err := json.Unmarshal(req.Body, &payload); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		authID = strings.TrimSpace(payload.AuthID)
	}
	if authID == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "auth_id is required"})
	}
	if !store.IsAuthAdmitted(authID) {
		return jsonManagementResponse(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("auth %s is outside the active CPA priority tier", authID),
		})
	}
	triggerRefreshOneSoon(authID)
	store.RecordLog("info", "ui.refresh_one_requested", "页面请求刷新单个账号额度", map[string]any{"auth_id": authID}, now)
	return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
}

func handleExportState(store *PluginState, now time.Time) pluginapi.ManagementResponse {
	state := diskStateFromStore(store)
	store.RecordLog("info", "ui.config_exported", "页面导出插件配置", nil, now)
	return jsonManagementResponse(http.StatusOK, normalizePluginDiskState(state))
}

func handleImportState(store *PluginState, body []byte, now time.Time) pluginapi.ManagementResponse {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "data is required"})
	}
	var state PluginDiskState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if _, err := validateQuotaEndpoint(state.Config.QuotaEndpoint); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	state = normalizePluginDiskState(state)
	if err := SaveUserData(semanticStatePaths(defaultStatePath()).UserData, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.ReplaceConfig(state.Config)
	currentConfig.Store(state.Config)
	store.SetAnnotations(AnnotationState{Accounts: state.Accounts, Groups: state.Groups})
	store.RecordLog("info", "ui.config_imported", "页面导入插件配置", nil, now)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func BuildStatusPayload(snapshot StateSnapshot, ordered []ScheduledAccount) StatusPayload {
	return buildStatusPayload(snapshot, ordered, nil)
}

func buildStatusPayload(snapshot StateSnapshot, ordered []ScheduledAccount, lifecycle *ManagementLifecycleSnapshot) StatusPayload {
	accountsByAuthID := make(map[string]AccountState, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID != "" {
			accountsByAuthID[account.AuthID] = account
		}
	}

	payload := StatusPayload{
		PluginID:              PluginID,
		GeneratedAt:           snapshot.Now,
		MonthlyMode:           snapshot.Config.MonthlyMode,
		HandleEnabled:         snapshot.Config.HandleEnabled,
		LastSelected:          snapshot.LastSelected,
		LastReason:            snapshot.LastReason,
		RefreshActive:         refreshActiveFromSnapshot(snapshot),
		LastCodexActivityText: formatTime(snapshot.LastCodexActivityAt),
		LastAuthScanText:      formatTime(snapshot.LastAuthScanAt),
		CodexAuthCount:        snapshot.CodexAuthCount,
		Settings:              SettingsFromConfig(snapshot.Config),
		Accounts:              make([]StatusAccount, 0, len(ordered)),
		Groups:                make([]StatusGroup, 0, len(snapshot.Annotations.Groups)),
		Logs:                  cloneLogs(snapshot.Logs),
	}
	if lifecycle != nil {
		payload.Roster = rosterLifecyclePayload(*lifecycle, snapshot, snapshot.Now)
	}
	if payload.RefreshActive {
		payload.RefreshState = "active"
	} else {
		payload.RefreshState = "sleeping"
	}
	payload.NextAuthID = nextStatusAuthID(ordered)
	for id, group := range snapshot.Annotations.Groups {
		payload.Groups = append(payload.Groups, StatusGroup{
			ID:    id,
			Name:  group.Name,
			Notes: group.Notes,
			Tags:  cloneStringSlice(group.Tags),
			Color: group.Color,
		})
	}
	for i, scheduled := range ordered {
		account := accountsByAuthID[scheduled.AuthID]
		groupID := scheduled.Annotation.GroupID
		groupName := groupID
		if group, ok := snapshot.Annotations.Groups[groupID]; ok && group.Name != "" {
			groupName = group.Name
		}
		groupNotes := ""
		if group, ok := snapshot.Annotations.Groups[groupID]; ok {
			groupNotes = group.Notes
		}

		status := StatusAccount{
			Rank:              i + 1,
			AuthID:            scheduled.AuthID,
			Alias:             scheduled.Annotation.Alias,
			Notes:             scheduled.Annotation.Notes,
			GroupID:           groupID,
			Group:             groupName,
			GroupNotes:        groupNotes,
			Tags:              cloneStringSlice(scheduled.Annotation.Tags),
			CPAPriority:       scheduled.CPAPriority,
			SchedulerPriority: scheduled.SchedulerPriority,
			Family:            scheduled.Family,
			QueueStatus:       scheduled.QueueStatus,
			Available:         scheduled.Available,
			UnavailableReason: scheduled.UnavailableReason,
			ResetExpiry:       scheduled.SortTime,
			ResetExpiryText:   formatTime(scheduled.SortTime),
			LastError:         account.LastError,
			NextRetryText:     formatTime(account.Refresh.NextRetryAt),
			AuthFailure:       account.Refresh.AuthFailure,
			FiveHour:          statusWindow(account.Quota.FiveHour, "5 小时额度"),
			LongWindow:        statusWindow(account.Quota.LongWindow, longWindowLabelCN(account.Family)),
		}
		_, status.RefreshDueReason = accountRefreshDue(account, snapshot.Config, snapshot.Now)
		status.StatusNote = accountStatusNote(account, status.RefreshDueReason, snapshot, payload.RefreshActive)
		status.Circuit = statusCircuit(account.Circuit, snapshot.Now)
		status.ResetCreditsAvailableCount = cloneIntPtr(account.Quota.ResetCreditsAvailableCount)
		status.ResetCreditsTotalEarnedCount = cloneIntPtr(account.Quota.ResetCreditsTotalEarnedCount)
		status.ResetCredits = append([]ResetCredit(nil), account.Quota.ResetCredits...)
		status.ResetProbes = statusResetProbes(account.ResetProbes)
		if !account.LastSuccessAt.IsZero() {
			age := snapshot.Now.Sub(account.LastSuccessAt)
			if age < 0 {
				age = 0
			}
			status.CacheAge = age.Round(time.Second).String()
			status.CacheAgeSeconds = int64(age.Seconds())
		}
		// Quota source: "observed" when the newest data came from a real
		// response stream (rate_limits frame or usage headers), "polled" from
		// the generic quota endpoint.
		switch {
		case !account.LastObservedAt.IsZero() && !account.LastSuccessAt.After(account.LastObservedAt):
			status.QuotaSource = "observed"
			if age := snapshot.Now.Sub(account.LastObservedAt); age > 0 {
				status.QuotaObservedSecondsAgo = int64(age.Seconds())
			}
		case !account.LastSuccessAt.IsZero():
			status.QuotaSource = "polled"
		}
		if lifecycle != nil && account.AuthIndex != "" {
			if record, ok := lifecycle.ManagedLifecycle[account.AuthIndex]; ok {
				summary := &ManagedDisableSummary{}
				switch {
				case !record.DisableApplied:
					summary.State = "planned"
				case record.NextQuotaCheckAt.After(snapshot.Now):
					summary.State = "disabled"
				default:
					summary.State = "checking"
				}
				if !record.DisabledAt.IsZero() {
					summary.DisabledAt = record.DisabledAt.UTC().Format(time.RFC3339)
				}
				if !record.NextQuotaCheckAt.IsZero() {
					summary.NextCheckAt = record.NextQuotaCheckAt.UTC().Format(time.RFC3339)
				}
				if !record.LastQuotaCheckAt.IsZero() {
					summary.LastQuotaCheck = record.LastQuotaCheckAt.UTC().Format(time.RFC3339)
				}
				status.ManagedDisable = summary
			}
		}
		payload.Accounts = append(payload.Accounts, status)
	}
	if len(payload.Accounts) == 0 {
		payload.EmptyState = emptyStatePayload(snapshot, payload.RefreshActive)
	}
	return payload
}

func rosterLifecyclePayload(lifecycle ManagementLifecycleSnapshot, snapshot StateSnapshot, now time.Time) RosterLifecyclePayload {
	roster := lifecycle.Roster
	available := !lifecycle.CredentialAmbiguous && roster.Capability == CapabilityB && roster.Provisional && !roster.Confirmed && provisionalAgeValid(now, roster.ConfirmedAt)
	payload := RosterLifecyclePayload{
		Capability: roster.Capability, Health: roster.Health, Confirmed: roster.Confirmed,
		Provisional: roster.Provisional, Degraded: roster.Health == RosterDegraded,
		FailClosed: roster.Health == RosterFailClosed, WaitingRoster: roster.Health == RosterWaiting,
		BackgroundAllowed: roster.BackgroundAllowed, HighestPriority: roster.HighestPriority,
		Generation: roster.Generation, CredentialAmbiguous: lifecycle.CredentialAmbiguous,
		AdmissionObserved: snapshot.CPAAdmission.Observed, AdmissionVersion: snapshot.CPAAdmissionVersion,
		AdmissionPriority: snapshot.CPAAdmission.Priority, AdmittedAuthCount: len(snapshot.CPAAdmission.AuthIDs),
		RosterEntryCount: len(roster.Entries), RosterInstanceCount: len(roster.Instances),
		RiskOptionEnabled: snapshot.Config.ProbeOnProvisionalRoster, RiskOptionAvailable: available,
	}
	switch {
	case lifecycle.CredentialAmbiguous:
		payload.Warning = "CredentialAmbiguous: credential-chain conversion is frozen until a later observation can be classified; existing non-AuthBlocked credentials remain usable."
	case payload.FailClosed:
		payload.Warning = "FailClosed: authoritative roster synchronization exceeded the degraded limit; background requests are stopped."
	case payload.Degraded:
		payload.Warning = "Degraded: Management is showing the last confirmed active roster while synchronization recovers."
	case payload.WaitingRoster:
		payload.Warning = "WaitingRoster: no current authoritative roster is confirmed; normal background refresh is stopped."
	}
	if payload.RiskOptionEnabled {
		if available {
			payload.RiskWarning = "Provisional roster Probe risk mode is enabled. Tier membership cannot be proven; each Probe requires fresh credential verification."
		} else {
			payload.RiskWarning = "Provisional roster Probe risk mode is enabled but unavailable without a fresh provisional Capability-B roster."
		}
	} else if available {
		payload.RiskWarning = "A fresh provisional roster is available, but Probe risk mode is disabled."
	}
	return payload
}

func refreshActiveFromSnapshot(snapshot StateSnapshot) bool {
	if snapshot.LastCodexActivityAt.IsZero() {
		return false
	}
	window := NormalizeConfig(snapshot.Config).RefreshActiveWindow
	return snapshot.Now.Before(snapshot.LastCodexActivityAt.Add(window))
}

func emptyStatePayload(snapshot StateSnapshot, refreshActive bool) EmptyStatePayload {
	cfg := NormalizeConfig(snapshot.Config)
	if !snapshot.LastAuthScanAt.IsZero() && snapshot.CodexAuthCount == 0 {
		return EmptyStatePayload{
			Reason:  "no_codex_auth",
			Title:   "未发现 Codex 账号",
			Message: "最近一次扫描认证文件中没有 Codex 账号，请先进行 OS 登录，然后发送一次 Codex 请求或手动刷新额度。",
		}
	}
	if snapshot.LastCodexActivityAt.IsZero() {
		return EmptyStatePayload{
			Reason:  "sleeping_no_activity",
			Title:   "调度器处于休眠状态",
			Message: fmt.Sprintf("最近 %s 内没有观察到 Codex 请求，系统暂不主动扫描账号。发送第一次 Codex 请求后将自动获取账号额度信息。", cfg.RefreshActiveWindow),
		}
	}
	if !refreshActive {
		return EmptyStatePayload{
			Reason:  "sleeping_inactive_window",
			Title:   "调度器处于休眠状态",
			Message: fmt.Sprintf("最近 %s 内没有 Codex 请求，调度器已暂停后台刷新。发送一次 Codex 请求后会重新进入活跃窗口并获取账号额度信息。", cfg.RefreshActiveWindow),
		}
	}
	return EmptyStatePayload{
		Reason:  "waiting_for_refresh",
		Title:   "等待账号额度数据",
		Message: "已观察到 Codex 请求，调度器处于活跃窗口。账号额度刷新完成后，这里会显示账号卡片。",
	}
}

func accountStatusNote(account AccountState, dueReason string, snapshot StateSnapshot, refreshActive bool) string {
	cfg := NormalizeConfig(snapshot.Config)
	if account.Refresh.AuthFailure {
		return "认证信息异常，请重新登录。"
	}
	if dueReason == "retry_wait" && !account.Refresh.NextRetryAt.IsZero() {
		return "上次额度刷新失败，调度器正在等待下次自动重试。"
	}
	if !refreshActive {
		switch dueReason {
		case "stale", "never_refreshed", "retry_due", "refresh_interval_due", "five_hour_reset_due", "long_window_reset_due", "temporary_reset_due":
			return fmt.Sprintf("账号额度已过期或待刷新，但最近 %s 内没有 Codex 请求，调度器处于休眠状态。发送一次 Codex 请求后会获取账号额度信息。", cfg.RefreshActiveWindow)
		}
	}
	switch dueReason {
	case "stale":
		return "账号额度已过期，调度器处于活跃窗口，将按刷新队列更新。"
	case "never_refreshed":
		return "账号尚未获取额度信息，调度器处于活跃窗口，将按刷新队列更新。"
	case "refresh_interval_due":
		return "已达到额度刷新间隔，调度器将按刷新队列更新。"
	case "retry_due":
		return "上次额度刷新失败，当前已到重试时间。"
	case "five_hour_reset_due", "long_window_reset_due", "temporary_reset_due":
		return "额度重置时间已到，调度器将按刷新队列更新。"
	case "circuit_wait":
		return "账号处于熔断等待中，半开探测时间到达后会重试。"
	case "auth_failure":
		return "认证信息异常，请重新登录。"
	case "local_failure":
		return "本地认证信息缺失或格式异常，请检查登录状态。"
	}
	return ""
}

func nextStatusAuthID(ordered []ScheduledAccount) string {
	for _, scheduled := range ordered {
		if scheduled.Available {
			return scheduled.AuthID
		}
	}
	return ""
}

func statusWindow(window *QuotaWindow, label string) StatusWindow {
	if window == nil {
		return StatusWindow{Label: label, DisplayText: "暂无数据", Missing: true}
	}
	used := 0.0
	hasUsagePercent := window.UsedPercent != nil
	if window.UsedPercent != nil {
		used = *window.UsedPercent
	}
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	remaining := 100 - used
	if remaining < 0 {
		remaining = 0
	}
	exhausted := window.Exhausted
	if hasUsagePercent {
		exhausted = used >= 100
	}
	if exhausted {
		remaining = 0
	}
	displayText := fmt.Sprintf("剩余 %.0f%%", remaining)
	if exhausted {
		displayText = "已用完"
	}
	return StatusWindow{
		Kind:             window.Kind,
		Label:            label,
		UsedPercent:      used,
		RemainingPercent: remaining,
		DisplayText:      displayText,
		Exhausted:        exhausted,
		ResetAt:          window.ResetAt,
		ResetText:        formatTime(window.ResetAt),
	}
}

func statusCircuit(circuit CircuitBreakerState, now time.Time) StatusCircuit {
	circuit = effectiveCircuitState(circuit, now)
	label := "全开"
	switch circuit.EffectiveState {
	case CircuitStateOpen:
		label = "熔断"
	case CircuitStateHalfOpen:
		label = "半开"
	}
	return StatusCircuit{
		State:         circuit.EffectiveState,
		Label:         label,
		FailureCount:  circuit.FailureCount,
		SuccessCount:  circuit.SuccessCount,
		NextProbeAt:   circuit.NextProbeAt,
		NextProbeText: formatTime(circuit.NextProbeAt),
		Reason:        circuit.Reason,
	}
}

func statusResetProbes(probes map[WindowKind]ResetProbeState) []StatusResetProbe {
	if len(probes) == 0 {
		return nil
	}
	kinds := make([]WindowKind, 0, len(probes))
	for kind := range probes {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		return string(kinds[i]) < string(kinds[j])
	})
	statuses := make([]StatusResetProbe, 0, len(kinds))
	for _, kind := range kinds {
		probe := probes[kind]
		windowKind := probe.WindowKind
		if windowKind == "" {
			windowKind = kind
		}
		statuses = append(statuses, StatusResetProbe{
			WindowKind:  windowKind,
			Status:      probe.Status,
			ResetAt:     formatTime(probe.ResetAt),
			NextCheckAt: formatTime(probe.NextCheckAt),
			LastProbeAt: formatTime(probe.LastProbeAt),
			VerifiedAt:  formatTime(probe.VerifiedAt),
			Attempts:    probe.Attempts,
			Error:       sanitizeResetProbeError(probe.Error),
		})
	}
	return statuses
}

func sanitizeResetProbeError(message string) string {
	if message == "" {
		return ""
	}
	redacted := redactPublicStatusString(message)
	lower := strings.ToLower(redacted)
	for _, forbidden := range []string{"access_token", "refresh_token", "id_token", "bearer", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			return "redacted reset probe error"
		}
	}
	return redacted
}

func redactPublicStatusString(message string) string {
	redacted := redactSecrets(message)
	for _, endpoint := range []string{chatGPTQuotaEndpoint, codexResetProbeEndpoint} {
		redacted = strings.ReplaceAll(redacted, endpoint, "[redacted]")
	}
	return redacted
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func longWindowLabel(family AccountFamily) string {
	if family == AccountFamilyMonthly {
		return "月额度"
	}
	return "周额度"
}

func longWindowLabelCN(family AccountFamily) string {
	if family == AccountFamilyMonthly {
		return "月额度"
	}
	return "周额度"
}

func RenderStatusHTML(payload StatusPayload) []byte {
	var buf bytes.Buffer
	if err := statusTemplateV2.Execute(&buf, payload); err != nil {
		return []byte("<!doctype html><html><body>status unavailable</body></html>")
	}
	return buf.Bytes()
}

func BuildStatusShellPayload(now time.Time) StatusPayload {
	cfg := DefaultConfig()
	return StatusPayload{
		PluginID:      PluginID,
		GeneratedAt:   now,
		Shell:         true,
		MonthlyMode:   cfg.MonthlyMode,
		HandleEnabled: cfg.HandleEnabled,
		Settings:      SettingsFromConfig(cfg),
		EmptyState: EmptyStatePayload{
			Reason:  "management_key_required",
			Title:   "需要 CPA 管理密钥",
			Message: "填写 CPA 管理密钥后将动态加载账号队列、调度日志和当前调度状态。发送 Codex 请求后，调度器会在活跃窗口内刷新额度。",
		},
	}
}

func handleStatusRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time, lifecycles ...*ManagementLifecycleSnapshot) pluginapi.ManagementResponse {
	if isResourcePath(req.Path) {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlManagementHeaders(),
			Body:       RenderStatusHTML(BuildStatusShellPayload(now)),
		}
	}
	payload := buildCurrentStatusPayload(store, now)
	if len(lifecycles) > 0 && lifecycles[0] != nil {
		payload = buildCurrentStatusPayloadWithLifecycle(store, now, *lifecycles[0])
	}
	if req.Query.Get("format") == "json" {
		return jsonManagementResponse(http.StatusOK, payload)
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    htmlManagementHeaders(),
		Body:       RenderStatusHTML(payload),
	}
}

// htmlManagementHeaders keeps the management page out of any HTTP cache. The
// markup and scripts ship inside the plugin binary, so a cached copy keeps
// serving a previous release's UI (and its validation rules) after an upgrade.
func htmlManagementHeaders() http.Header {
	return http.Header{
		"Content-Type":  []string{"text/html; charset=utf-8"},
		"Cache-Control": []string{"no-store, no-cache, must-revalidate"},
		"Pragma":        []string{"no-cache"},
		"Expires":       []string{"0"},
	}
}

func handlePublicStatusDataRequest(store *PluginState, now time.Time) pluginapi.ManagementResponse {
	payload := buildCurrentStatusPayload(store, now)
	return jsonManagementResponse(http.StatusOK, sanitizePublicStatusPayload(payload))
}

func sanitizePublicStatusPayload(payload StatusPayload) StatusPayload {
	payload.Shell = false
	payload.Settings.QuotaEndpoint = ""
	for i := range payload.Accounts {
		payload.Accounts[i].LastError = sanitizeResetProbeError(payload.Accounts[i].LastError)
		for j := range payload.Accounts[i].ResetProbes {
			payload.Accounts[i].ResetProbes[j].Error = sanitizeResetProbeError(payload.Accounts[i].ResetProbes[j].Error)
		}
	}
	for i := range payload.Logs {
		payload.Logs[i].Message = redactPublicStatusString(payload.Logs[i].Message)
		for key, value := range payload.Logs[i].Fields {
			text, ok := value.(string)
			if !ok {
				continue
			}
			if key == "error" {
				payload.Logs[i].Fields[key] = sanitizeResetProbeError(text)
				continue
			}
			payload.Logs[i].Fields[key] = redactPublicStatusString(text)
		}
	}
	return payload
}

func buildCurrentStatusPayload(store *PluginState, now time.Time) StatusPayload {
	snapshot := store.Snapshot(now)
	ordered := buildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now, globalTrials)
	return BuildStatusPayload(snapshot, ordered)
}

func buildCurrentStatusPayloadWithLifecycle(store *PluginState, now time.Time, lifecycle ManagementLifecycleSnapshot) StatusPayload {
	snapshot := store.Snapshot(now)
	active := make(map[string]struct{}, len(lifecycle.Roster.Instances))
	for _, authID := range lifecycle.Roster.Instances {
		if authID != "" {
			active[authID] = struct{}{}
		}
	}
	filtered := snapshot
	filtered.Accounts = make([]AccountState, 0, len(active))
	for _, account := range snapshot.Accounts {
		if _, ok := active[account.AuthID]; !ok {
			continue
		}
		account.Priority = lifecycle.Roster.HighestPriority
		filtered.Accounts = append(filtered.Accounts, account)
	}
	ordered := buildOrderedAccounts(syntheticRosterStatusRequest(filtered, lifecycle.Roster), filtered, now, globalTrials)
	return buildStatusPayload(filtered, ordered, &lifecycle)
}

func syntheticRosterStatusRequest(snapshot StateSnapshot, roster ActiveRoster) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID == "" {
			continue
		}
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: account.AuthID, Provider: "codex", Priority: roster.HighestPriority})
	}
	return pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: candidates}
}

func syntheticStatusRequest(snapshot StateSnapshot) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID == "" {
			continue
		}
		provider := account.Provider
		if provider == "" {
			provider = "codex"
		}
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:       account.AuthID,
			Provider: provider,
			Priority: account.Priority,
			Status:   "active",
		})
	}
	return pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Providers:  []string{"codex"},
		Candidates: candidates,
	}
}

func handlePutAnnotations(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var state AnnotationState
	if err := json.Unmarshal(req.Body, &state); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func handlePatchAccountAnnotation(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := applyAccountAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.account_saved", "页面保存账号卡片", map[string]any{"auth_id": patch.AuthID, "key": patch.Key}, now)
	}
	return resp
}

func applyAccountAnnotationPatch(store *PluginState, patch annotationPatch) pluginapi.ManagementResponse {
	key := patch.accountKey()
	if key == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "annotation key is required"})
	}

	state := store.Annotations()
	annotation := state.Accounts[key]
	if len(patch.Annotation) > 0 {
		if err := json.Unmarshal(patch.Annotation, &annotation); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	if patch.Alias != nil {
		annotation.Alias = *patch.Alias
	}
	if patch.Notes != nil {
		annotation.Notes = *patch.Notes
	}
	if patch.Tags != nil {
		annotation.Tags = patch.Tags
	}
	if patch.GroupID != nil {
		annotation.GroupID = *patch.GroupID
	}
	if patch.SchedulerPriority != nil {
		annotation.SchedulerPriority = *patch.SchedulerPriority
	}
	state.Accounts[key] = annotation
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func handlePatchGroupAnnotation(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := applyGroupAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.group_saved", "页面保存账号分组", map[string]any{"group_id": patch.ID, "key": patch.Key}, now)
	}
	return resp
}

func applyGroupAnnotationPatch(store *PluginState, patch annotationPatch) pluginapi.ManagementResponse {
	key := patch.groupKey()
	if key == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "group key is required"})
	}

	state := store.Annotations()
	annotation := state.Groups[key]
	if len(patch.Annotation) > 0 {
		if err := json.Unmarshal(patch.Annotation, &annotation); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	if patch.Name != nil {
		annotation.Name = *patch.Name
	}
	if patch.Notes != nil {
		annotation.Notes = *patch.Notes
	}
	if patch.Tags != nil {
		annotation.Tags = patch.Tags
	}
	if patch.Color != nil {
		annotation.Color = *patch.Color
	}
	state.Groups[key] = annotation
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

type annotationPatch struct {
	Key               string          `json:"key"`
	ID                string          `json:"id"`
	AuthID            string          `json:"auth_id"`
	Alias             *string         `json:"alias"`
	Name              *string         `json:"name"`
	Notes             *string         `json:"notes"`
	Tags              []string        `json:"tags"`
	GroupID           *string         `json:"group_id"`
	SchedulerPriority *int            `json:"scheduler_priority"`
	Color             *string         `json:"color"`
	Annotation        json.RawMessage `json:"annotation"`
}

func (p annotationPatch) accountKey() string {
	if p.Key != "" {
		return p.Key
	}
	if p.ID != "" {
		return p.ID
	}
	if p.AuthID != "" {
		return "auth:" + p.AuthID
	}
	return ""
}

func (p annotationPatch) groupKey() string {
	if p.Key != "" {
		return p.Key
	}
	return p.ID
}

func persistAnnotations(store *PluginState) error {
	return persistAnnotationState(store, store.Annotations())
}

func persistAnnotationState(store *PluginState, state AnnotationState) error {
	if store == nil {
		return nil
	}
	disk := diskStateFromStore(store)
	disk.Accounts = state.Accounts
	disk.Groups = state.Groups
	return SaveUserData(semanticStatePaths(defaultStatePath()).UserData, disk)
}

func triggerRefreshSoon() {
	managementRefreshSoon()
}

func triggerRefreshOneSoon(authID string) {
	managementRefreshOneSoon(authID)
}

func normalizeManagementPath(path string) string {
	for _, prefix := range []string{
		"/v0/management" + managementBasePath,
		"/v0/resource" + managementBasePath,
		managementBasePath,
	} {
		if path == prefix {
			return "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if path == "/v0/resource"+managementBasePath+"/status" {
		return "/status"
	}
	if path == "" {
		return "/"
	}
	return path
}

func jsonManagementResponse(status int, body any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

