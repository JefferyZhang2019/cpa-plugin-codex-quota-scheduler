package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	State           string `json:"state"`
	DisabledAt      string `json:"disabled_at,omitempty"`
	NextCheckAt     string `json:"next_check_at,omitempty"`
	LastQuotaCheck  string `json:"last_quota_check_at,omitempty"`
}

type StatusAccount struct {
	Rank                         int                `json:"rank"`
	AuthID                       string             `json:"auth_id"`
	Alias                        string             `json:"alias,omitempty"`
	Notes                        string             `json:"notes,omitempty"`
	GroupID                      string             `json:"group_id,omitempty"`
	Group                        string             `json:"group,omitempty"`
	GroupNotes                   string             `json:"group_notes,omitempty"`
	Tags                         []string           `json:"tags,omitempty"`
	CPAPriority                  int                `json:"cpa_priority"`
	SchedulerPriority            int                `json:"scheduler_priority"`
	Family                       AccountFamily      `json:"family"`
	QueueStatus                  QueueStatus        `json:"queue_status"`
	Available                    bool               `json:"available"`
	UnavailableReason            string             `json:"unavailable_reason,omitempty"`
	ResetExpiry                  time.Time          `json:"reset_expiry,omitempty"`
	ResetExpiryText              string             `json:"reset_expiry_text,omitempty"`
	CacheAge                     string             `json:"cache_age,omitempty"`
	CacheAgeSeconds              int64              `json:"cache_age_seconds,omitempty"`
	QuotaSource                  string             `json:"quota_source,omitempty"`
	QuotaObservedSecondsAgo      int64              `json:"quota_observed_seconds_ago,omitempty"`
	ManagedDisable               *ManagedDisableSummary `json:"managed_disable,omitempty"`
	LastError                    string             `json:"last_error,omitempty"`
	RefreshDueReason             string             `json:"refresh_due_reason,omitempty"`
	NextRetryText                string             `json:"next_retry_text,omitempty"`
	AuthFailure                  bool               `json:"auth_failure"`
	StatusNote                   string             `json:"status_note,omitempty"`
	FiveHour                     StatusWindow       `json:"five_hour"`
	LongWindow                   StatusWindow       `json:"long_window"`
	Circuit                      StatusCircuit      `json:"circuit"`
	ResetCreditsAvailableCount   *int               `json:"reset_credits_available_count,omitempty"`
	ResetCreditsTotalEarnedCount *int               `json:"reset_credits_total_earned_count,omitempty"`
	ResetCredits                 []ResetCredit      `json:"reset_credits,omitempty"`
	ResetProbes                  []StatusResetProbe `json:"reset_probes,omitempty"`
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

var statusTemplateV2 = template.Must(template.New("status-v2").Funcs(template.FuncMap{
	"join": strings.Join,
	"json": jsonForTemplate,
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex 额度调度器</title>
<style>
.warning{margin-top:14px;border:1px solid #fbbf24;background:#fffbeb;color:#78350f;border-radius:7px;padding:10px 11px;font-size:12px;line-height:1.45}.warning strong{display:block;color:#92400e;font-size:13px;margin-bottom:3px}
*{box-sizing:border-box}body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;color:#1f2937;background:#f6f7f9}button,input,select,textarea{font:inherit}button{border:0;border-radius:7px;padding:9px 12px;background:#2563eb;color:#fff;cursor:pointer;font-weight:650}button.secondary{background:#eef2ff;color:#1e40af}button.ghost{background:#f3f4f6;color:#374151}button:disabled{opacity:.55;cursor:not-allowed}[hidden]{display:none!important}code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}.shell{display:grid;grid-template-columns:310px minmax(0,1fr);min-height:100vh}.sidebar{background:#fff;border-right:1px solid #e5e7eb;padding:18px;position:sticky;top:0;height:100vh;overflow:auto}.main{padding:20px 22px 32px;overflow:auto}.brand{display:grid;gap:5px;margin-bottom:18px}.brand h1{font-size:20px;line-height:1.2;margin:0;color:#111827}.brand p{font-size:12px;line-height:1.45;color:#6b7280;margin:0}.section{border-top:1px solid #eef0f3;padding-top:16px;margin-top:16px}.section h2{font-size:14px;margin:0 0 12px;color:#111827}.collapsible summary{cursor:pointer;list-style:none;display:flex;align-items:flex-start;gap:8px}.collapsible summary::-webkit-details-marker{display:none}.summary-toggle{display:inline-flex;align-items:center;justify-content:center;width:18px;height:18px;border-radius:999px;background:#eef2ff;color:#1e40af;font-weight:750;line-height:1;transition:transform .16s ease}.collapsible[open] .summary-toggle{transform:rotate(90deg)}.summary-text{display:grid;gap:3px}.summary-title{font-size:14px;font-weight:750;color:#111827}.summary-subtitle{font-size:12px;line-height:1.35;color:#6b7280}.collapsible-body{padding-top:14px}.primary-actions{margin-top:12px}.field{display:grid;gap:6px;margin-bottom:12px}.field span{font-size:12px;color:#4b5563;font-weight:650}.field input,.field select,.field textarea{width:100%;border:1px solid #d1d5db;border-radius:7px;background:#fff;color:#111827;padding:8px 10px}.field textarea{min-height:84px;resize:vertical}.toggle{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px}.toggle span{font-size:13px;font-weight:650}.setting-with-help{margin-bottom:12px}.setting-with-help .toggle{margin-bottom:4px}.setting-help{margin:0;color:#6b7280;font-size:12px;line-height:1.45}.actions{display:flex;gap:8px;flex-wrap:wrap}.notice{margin-top:12px;border-radius:7px;padding:10px 11px;background:#ecfdf5;color:#065f46;font-size:12px;line-height:1.45}.notice.error{background:#fef2f2;color:#991b1b}.staticHint{background:#eff6ff;color:#1e3a8a}.toolbar{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:18px}.toolbar h2{font-size:22px;margin:0;color:#111827}.toolbar p{font-size:13px;color:#6b7280;margin:5px 0 0}.metrics{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}.metric{border:1px solid #e5e7eb;background:#fff;border-radius:7px;padding:8px 10px;font-size:12px;color:#374151}.queue{display:grid;grid-template-columns:repeat(auto-fill,minmax(310px,1fr));gap:12px}.card{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px;display:grid;gap:12px;box-shadow:0 1px 2px rgba(15,23,42,.04)}.card.next{border-color:#2563eb;box-shadow:0 0 0 1px rgba(37,99,235,.18),0 8px 24px rgba(37,99,235,.08)}.cardTop{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}.rank{display:inline-flex;align-items:center;justify-content:center;min-width:30px;height:30px;border-radius:7px;background:#111827;color:#fff;font-weight:750;font-size:13px}.identity{min-width:0;display:grid;gap:5px}.titleLine{display:flex;align-items:center;gap:7px;flex-wrap:wrap}.title{font-weight:750;color:#111827;overflow-wrap:anywhere}.groupPill{border-radius:999px;background:#f0fdf4;color:#166534;padding:3px 7px;font-size:11px}.sub{font-size:12px;color:#6b7280;overflow-wrap:anywhere}.badges{display:flex;gap:6px;flex-wrap:wrap}.badge{border-radius:999px;background:#f3f4f6;color:#374151;padding:4px 8px;font-size:12px}.badge.ok{background:#dcfce7;color:#166534}.badge.no{background:#fee2e2;color:#991b1b}.badge.next{background:#dbeafe;color:#1d4ed8}.kv{display:grid;grid-template-columns:88px minmax(0,1fr);gap:6px 10px;font-size:12px}.kv span:nth-child(odd){color:#6b7280}.kv span:nth-child(even){color:#111827;overflow-wrap:anywhere}.chips{display:flex;gap:6px;flex-wrap:wrap;min-height:24px}.chip{border-radius:999px;background:#eef2ff;color:#3730a3;padding:4px 8px;font-size:12px}.quotaList{display:grid;gap:10px}.quota-row{display:grid;gap:5px}.quota-head{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px;font-size:12px;color:#374151}.quota-title{font-weight:650;color:#111827}.quota-reset{grid-column:1/-1;color:#6b7280}.localTime{color:#374151}.quota-bar{height:8px;border-radius:999px;background:#e5e7eb;overflow:hidden}.quota-fill{height:100%;border-radius:999px;background:#2f7d5f}.quota-fill.warn{background:#b7791f}.quota-fill.danger{background:#dc2626}.metaLine{display:flex;gap:6px;flex-wrap:wrap}.noteBlock{font-size:12px;line-height:1.45;color:#4b5563;background:#f9fafb;border-radius:7px;padding:8px 9px;display:grid;gap:3px}.cardActions{display:flex;justify-content:flex-end;gap:8px}.pinAccount{display:inline-flex;align-items:center;justify-content:center;padding:9px;color:#6b7280}.pinAccount.pinned{background:#dbeafe;color:#2563eb}.pinAccount svg{width:18px;height:18px;fill:currentColor}.empty{background:#fff;border:1px dashed #d1d5db;border-radius:8px;padding:28px;text-align:center;color:#6b7280}.logs{margin-top:20px;background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px}.logsHeader{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:10px}.logsHeader h2{font-size:16px;margin:0}.logList{display:grid;gap:8px;max-height:260px;overflow:auto}.logItem{border-left:3px solid #d1d5db;padding:7px 9px;background:#f9fafb;border-radius:0 7px 7px 0}.logItem.info{border-left-color:#2563eb}.logItem.warn{border-left-color:#b7791f}.logItem.error{border-left-color:#dc2626}.logMeta{font-size:11px;color:#6b7280;margin-bottom:3px}.logMsg{font-size:12px;color:#111827;line-height:1.45}dialog{border:0;border-radius:8px;padding:0;width:min(560px,calc(100vw - 28px));box-shadow:0 24px 64px rgba(15,23,42,.28)}dialog::backdrop{background:rgba(15,23,42,.38)}.dialogBody{padding:18px}.dialogHead{display:flex;justify-content:space-between;gap:12px;align-items:flex-start;margin-bottom:14px}.dialogHead h2{font-size:18px;margin:0;color:#111827}.dialogHead p{font-size:12px;color:#6b7280;margin:4px 0 0}.dialogGrid{display:grid;grid-template-columns:1fr 1fr;gap:10px}.dialogGrid .wide{grid-column:1/-1}.dialogActions{display:flex;justify-content:flex-end;gap:8px;margin-top:14px}@media(max-width:860px){.shell{grid-template-columns:1fr}.sidebar{height:auto;position:relative;border-right:0;border-bottom:1px solid #e5e7eb}.toolbar{display:grid}.metrics{justify-content:flex-start}.dialogGrid{grid-template-columns:1fr}}
.quota-fill.warn{background:#f59e0b}
.retryChainHead{display:flex;align-items:center;justify-content:space-between;gap:12px;margin:6px 0 8px}.retryChainHead h3{margin:0;font-size:15px;color:#111827}
.retryChainRow{border:1px solid #e5e7eb;border-radius:8px;padding:12px;margin-bottom:10px;background:#fbfbfd;display:grid;gap:10px}
.retryChainRowHead{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:10px;align-items:end}.retryChainRowHead .field{margin-bottom:0}
.retryRowActions{display:flex;gap:8px;flex-wrap:wrap}
.retryFallbacks{display:grid;gap:8px}
.retryFallback{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:10px;align-items:end;border-top:1px dashed #e5e7eb;padding-top:8px}
.retryFallback .field{margin-bottom:0}
.retryEmpty{border:1px dashed #d1d5db;border-radius:8px;padding:14px;color:#6b7280;font-size:12px}
.retryStats{display:flex;gap:8px;flex-wrap:wrap;margin-top:16px}
.retryStat{border:1px solid #e5e7eb;background:#fff;border-radius:7px;padding:8px 10px;font-size:12px;display:grid;gap:3px;min-width:96px}
.retryStatLabel{color:#6b7280}.retryStatValue{font-weight:700;color:#111827;font-size:15px}
.retryChainSummary{border:1px solid #e5e7eb;background:#fff;border-radius:7px;padding:8px 10px;font-size:12px;color:#374151;display:grid;gap:4px;flex-basis:100%}
.retryChainSummaryTitle{font-weight:650;color:#111827}
.retryChainLine{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;overflow-wrap:anywhere}
.retryCPAConfirm{margin-top:12px;padding-top:12px;border-top:1px solid #eef0f3}.retryCPAConfirm strong{display:block;margin-bottom:8px;color:#111827}.retryCPAConfirm table{width:100%;border-collapse:collapse;font-size:12px;margin:8px 0 12px}.retryCPAConfirm th,.retryCPAConfirm td{border:1px solid #e5e7eb;padding:7px 8px;text-align:left}.retryCPAConfirm th{background:#f9fafb;color:#4b5563}.retryCPAConfirm .diff{color:#b91c1c;background:#fef2f2;font-weight:650}
.retryCPAConfirm{margin-top:12px;padding-top:12px;border-top:1px solid #eef0f3}.retryCPAConfirm strong{display:block;margin-bottom:8px;color:#111827}.retryCPAConfirm table{width:100%;border-collapse:collapse;font-size:12px;margin:8px 0 12px}.retryCPAConfirm th,.retryCPAConfirm td{border:1px solid #e5e7eb;padding:7px 8px;text-align:left}.retryCPAConfirm th{background:#f9fafb;color:#4b5563}.retryCPAConfirm .diff{color:#b91c1c;background:#fef2f2;font-weight:650}
</style>
</head>
<body>
<div class="shell">
<aside class="sidebar">
<div class="brand"><h1 data-i18n="app.title">Codex 额度调度器</h1><p data-i18n="app.subtitle">优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。</p></div>
<label class="field"><span data-i18n="app.language">界面语言</span><select id="localeSelect"><option value="zh-CN">中文</option><option value="en">English</option></select></label>
<label class="field" id="managementKeyField"><span data-i18n="connection.managementKey">CPA 管理密钥</span><input id="managementKey" type="password" autocomplete="off" spellcheck="false"></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="connection.rememberManagementKey">在此浏览器中记住管理密钥</span><input id="rememberManagementKey" type="checkbox"></label><p class="setting-help" data-i18n="connection.rememberManagementKeyHelp">密钥将以未加密形式保存在浏览器本地存储中。请仅在受信任的设备上启用。</p></div>
<div class="actions primary-actions"><button id="loadData" type="button" data-i18n="actions.loadData">加载数据</button><button id="refreshQuota" type="button" class="secondary" data-i18n="actions.refreshQuota" hidden>刷新额度</button></div>
<div class="notice staticHint" data-i18n="connection.backgroundHint">只要调度器启动了，它就会在后台自动运行，无需保持页面开启。</div>
<div class="warning" id="resetProbeWarning" hidden><strong data-i18n="resetProbe.warningTitle">自动激活新的额度周期默认关闭</strong><span data-i18n="resetProbe.warningBody">开启后，调度器会在额度重置时间已到但新周期尚未生成时，发送一次极小的 Codex 请求尝试激活新周期。</span></div>
<details class="section collapsible" id="settingsPanel" hidden><summary><span class="summary-toggle" aria-hidden="true">&gt;</span><span class="summary-text"><span class="summary-title" data-i18n="settings.title">调度设置</span><span class="summary-subtitle" data-i18n="settings.summary">默认配置已经都设置好了，正常情况下不需要手动设置。</span></span></summary><div class="collapsible-body">
<label class="toggle"><span data-i18n="settings.handleEnabled">启用调度接管</span><input id="handleEnabled" type="checkbox"></label>
<label class="toggle"><span data-i18n="settings.usageFeedback">失败反馈标记额度耗尽</span><input id="usageFeedback" type="checkbox"></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.enableResetProbe">自动激活新的额度周期</span><input id="enableResetProbe" name="enable_reset_probe" type="checkbox"></label><p class="setting-help" data-i18n="settings.enableResetProbeHelp">即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短 30 分钟；只有检测到延迟启动的重置窗口时，才发送一次极小请求。可能消耗少量额度。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.provisionalProbe">账号列表未确认时仍允许额度探测（高风险）</span><input id="probeOnProvisionalRoster" name="probe_on_provisional_roster" type="checkbox"></label><p class="setting-help" data-i18n="settings.provisionalProbeHelp">CPA 暂时无法确认当前账号及优先级时，允许插件使用最近一次保存的账号列表执行额度重置探测。每次都会重新验证账号凭据，但仍无法保证账号未被删除或调整优先级。通常应保持关闭。</p></div>
<label class="field"><span data-i18n="settings.monthlyMode">月度账号使用方式</span><select id="monthlyMode"><option value="expiry_order" data-i18n="settings.expiryOrder">按到期时间排序</option><option value="priority" data-i18n="settings.monthlyPriority">优先使用月度账号</option></select></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.acrossPriorities">更高优先级耗尽时使用较低优先级账号</span><input id="scheduleAcrossPriorities" type="checkbox"></label><p class="setting-help" data-i18n="settings.acrossPrioritiesHelp">开启后，所有 CPA 优先级层级的账号都会纳入调度；更高层级只要还有可用账号就一定优先，仅在更高层级全部不可用时才会选用低层级。关闭则保持只调度最高优先级层。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.managedDisable">额度耗尽时在 CPA 层停用账号并自动恢复</span><input id="enableManagedQuotaDisable" type="checkbox"></label><p class="setting-help" data-i18n="settings.managedDisableHelp">收到确认的额度耗尽反馈时，把该账号的 CPA 凭据标记为 disabled；之后按刷新间隔只读检查额度，只有证实两个额度窗口都可用时才自动恢复启用。人工手动停用的账号绝不会被改动。默认关闭；开启后插件会写宿主的认证文件。</p></div>
<label class="field"><span data-i18n="settings.refreshInterval">额度刷新间隔</span><input id="refreshInterval" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.staleAfter">缓存过期判定</span><input id="staleAfter" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.refreshActiveWindow">活跃刷新窗口</span><input id="refreshActiveWindow" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.refreshAfterResetDelay">重置后刷新延迟</span><input id="refreshAfterResetDelay" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.refreshRetryDelays">刷新失败重试间隔</span><input id="refreshRetryDelays" spellcheck="false"></label>
<label class="toggle"><span data-i18n="settings.refreshOnStartup">启动时刷新额度</span><input id="refreshOnStartup" type="checkbox"></label>
<label class="field"><span data-i18n="settings.maxConcurrency">最大并发刷新</span><input id="maxConcurrency" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.circuitFailureThreshold">熔断失败阈值</span><input id="circuitFailureThreshold" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.circuitOpenDuration">熔断等待时间</span><input id="circuitOpenDuration" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.circuitHalfOpenSuccessThreshold">半开恢复成功次数</span><input id="circuitHalfOpenSuccessThreshold" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.maxLogEntries">最大日志条数</span><input id="maxLogEntries" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.lifecycleEventLimit">托管停用历史条数上限</span><input id="lifecycleEventLimit" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.logRetention">日志保留时间</span><input id="logRetention" spellcheck="false"></label>
<div class="actions"><button id="saveSettings" type="button" data-i18n="actions.saveSettings">保存设置</button><button id="exportConfig" type="button" class="ghost" data-i18n="actions.exportConfig">导出配置</button><button id="importConfig" type="button" class="ghost" data-i18n="actions.importConfig">导入配置</button><input id="importFile" type="file" accept="application/json,.json" hidden></div>
</div></details>
<details class="section collapsible" id="retryPanelWrap" hidden><summary><span class="summary-toggle" aria-hidden="true">&gt;</span><span class="summary-text"><span class="summary-title" data-i18n="retry.title">模型重试链</span><span class="summary-subtitle" data-i18n="retry.summary">上游在内容尚未发给客户端之前返回容量不足、超载或 429/502/503 时，自动改用链中的下一个模型重试。</span></span></summary><div class="collapsible-body" id="retryPanel">
<label class="toggle"><span data-i18n="retry.enabled">启用模型重试链</span><input id="retryEnabled" type="checkbox"></label>
<label class="toggle"><span data-i18n="retry.shadow">影子模式（只记录会重试的情况，不真正重试）</span><input id="retryShadow" type="checkbox"></label>
<div class="setting-with-help"><p class="setting-help" data-i18n="retry.shadowHelp">影子模式按今天的路径原样转发，只把「本应重试」的请求写入下面的记录，用于先观察再启用。</p></div>
<label class="toggle wide"><span data-i18n="retry.always">全局重试（所有模型）</span><input id="retryAlways" type="checkbox"></label>
<div class="retryChainHead"><h3 data-i18n="retry.chainTitle">重试链</h3><div class="actions"><button id="retryAddRow" type="button" class="ghost" data-i18n="retry.addRow">添加模型</button><button id="retryReloadCatalog" type="button" class="ghost" data-i18n="retry.reloadCatalog">载入模型列表</button></div></div>
<div class="retryCPAConfirm" id="retryCPAConfirm" hidden><strong data-i18n="retry.cpaConfirmTitle">确认修改 CPA 重试设置</strong><p id="retryCPAConfirmBody"></p><div class="actions"><button id="retryCPAConfirmYes" type="button" data-i18n="retry.cpaConfirmYes">应用推荐设置</button><button id="retryCPAConfirmNo" type="button" class="ghost" data-i18n="retry.cpaConfirmNo">取消</button></div></div>
<div class="actions"><button id="retryCheckCPA" type="button" class="ghost" data-i18n="retry.checkCPA">检查并修复 CPA 重试设置</button></div>
<p class="setting-help" id="retryCPAStatus"></p>
<p class="setting-help" data-i18n="retry.chainHelp">左列是客户端请求的模型；右侧按顺序填备用目标。提供方留空时由 CPA 自动选择，填写时使用 CPA 的提供方键，例如 codex、openai、openai-compatible-zed2api。只对开启了「启用模型重试链」且出现在这里的模型生效。</p>
<div id="retryChainEditor"></div>
<p class="setting-help" id="retryCatalogStatus"></p>
<label class="field"><span data-i18n="retry.maxAttempts">最大尝试次数（含首次）</span><input id="retryMaxAttempts" type="number" min="1" max="16" step="1"></label>
<label class="field"><span data-i18n="retry.maxFrames">保留帧上限</span><input id="retryMaxFrames" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="retry.stallTimeout">单次静默超时</span><input id="retryStallTimeout" spellcheck="false" placeholder="60s"></label>
<label class="field"><span data-i18n="retry.holdTimeout">等待可提交内容上限</span><input id="retryHoldTimeout" spellcheck="false" placeholder="90s"></label>
<label class="field"><span data-i18n="retry.chainDeadline">整条链总时限</span><input id="retryChainDeadline" spellcheck="false" placeholder="240s"></label>
<label class="field"><span data-i18n="retry.maxBytes">保留字节上限</span><input id="retryMaxBytes" spellcheck="false" placeholder="8MB"></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="retry.stripReasoning">切换模型时移除加密推理内容</span><input id="retryStripReasoning" type="checkbox"></label><p class="setting-help" data-i18n="retry.stripReasoningHelp">Codex 的加密推理条目由原模型签名，换模型后无法验证。保留开启可以避免换模型后请求被上游拒绝。</p></div>
<div class="actions"><button id="retrySave" type="button" data-i18n="actions.saveSettings">保存设置</button><button id="retryRefreshStats" type="button" class="ghost" data-i18n="retry.refreshStats">刷新记录</button></div>
<div id="retryStats" class="retryStats"></div>
<div id="retryEvents" class="logList"></div>
<datalist id="retryProviderOptions"></datalist>
<datalist id="retryModelOptions"></datalist>
</div></details>
<div id="notice" class="notice" hidden></div>
</aside>
<main class="main" id="protectedMain" hidden>
<div class="warning" id="rosterLifecycleWarning" hidden><strong id="rosterLifecycleTitle">Roster lifecycle</strong><span id="rosterLifecycleBody"></span></div>
<div class="toolbar"><div><h2 data-i18n="queue.title">账号队列</h2><p data-i18n="queue.description">账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。</p></div><div class="metrics"><span class="metric"><span data-i18n="metrics.nextAccount">下一账号</span><span class="localeColon">：</span><code id="metricNextAuthID">{{if .NextAuthID}}{{.NextAuthID}}{{else}}暂无{{end}}</code></span><span class="metric"><span data-i18n="metrics.monthlyMode">Monthly</span><span class="localeColon">：</span><code id="metricMonthlyMode">{{if eq .MonthlyMode "priority"}}优先使用{{else}}按到期时间{{end}}</code></span><span class="metric"><span data-i18n="metrics.lastSelected">最近选择</span><span class="localeColon">：</span><code id="metricLastSelected">{{if .LastSelected}}{{.LastSelected}}{{else}}暂无{{end}}</code></span></div></div>
<section class="queue" aria-label="账号卡片" data-i18n-aria="queue.aria">{{range .Accounts}}<article class="card {{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}next{{end}}" data-auth-id="{{.AuthID}}">
<div class="cardTop"><div class="identity"><div class="titleLine"><span class="title">{{if .Alias}}{{.Alias}}{{else}}{{.AuthID}}{{end}}</span>{{if .Group}}<span class="groupPill">{{.Group}}</span>{{end}}</div><div class="sub"><code>{{.AuthID}}</code></div>{{if .Tags}}<div class="metaLine">{{range .Tags}}<span class="chip">{{.}}</span>{{end}}</div>{{end}}</div><span class="rank">#{{.Rank}}</span></div>
<div class="badges">{{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}<span class="badge next">下一优先</span>{{end}}{{if .Available}}<span class="badge ok">可用</span>{{else}}<span class="badge no">{{.UnavailableReason}}</span>{{end}}<span class="badge">{{if eq .Family "weekly"}}Weekly{{else if eq .Family "monthly"}}Monthly{{else}}未知类型{{end}}</span><span class="badge">CPA 优先级 {{.CPAPriority}}</span><span class="badge">插件优先级 {{.SchedulerPriority}}</span><span class="badge">熔断：{{.Circuit.Label}}</span></div>
<div class="quotaList">{{if not .FiveHour.Missing}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.FiveHour.Label}}</span><span class="quota-remaining">{{.FiveHour.DisplayText}}</span><span class="quota-reset"><span class="quota-reset-label" data-label="5 小时额度">5 小时额度 重置：</span><span class="localTime quotaResetTime" data-time="{{.FiveHour.ResetText}}">{{.FiveHour.ResetText}}</span></span></div><div class="quota-bar"><div class="quota-fill {{if or .FiveHour.Exhausted (lt .FiveHour.RemainingPercent 30.0)}}danger{{else if lt .FiveHour.RemainingPercent 60.0}}warn{{end}}" style="width:{{printf "%.0f" .FiveHour.RemainingPercent}}%"></div></div></div>{{end}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.LongWindow.Label}}</span><span class="quota-remaining">{{.LongWindow.DisplayText}}</span>{{if not .LongWindow.Missing}}<span class="quota-reset"><span class="quota-reset-label" data-label="{{.LongWindow.Label}}">{{.LongWindow.Label}} 重置：</span><span class="localTime quotaResetTime" data-time="{{.LongWindow.ResetText}}">{{.LongWindow.ResetText}}</span></span>{{end}}</div><div class="quota-bar"><div class="quota-fill {{if not .LongWindow.Missing}}{{if or .LongWindow.Exhausted (lt .LongWindow.RemainingPercent 30.0)}}danger{{else if lt .LongWindow.RemainingPercent 60.0}}warn{{end}}{{end}}" style="width:{{printf "%.0f" .LongWindow.RemainingPercent}}%"></div></div></div></div>
<div class="kv"><span>缓存时间</span><span>{{if .CacheAge}}{{.CacheAge}}{{else}}暂无{{end}}</span><span>额度来源</span><span>{{if eq .QuotaSource "observed"}}响应观测{{else if eq .QuotaSource "polled"}}轮询刷新{{else}}暂无{{end}}</span><span>熔断计数</span><span>失败 {{.Circuit.FailureCount}} / 成功 {{.Circuit.SuccessCount}}{{if .Circuit.NextProbeText}}，半开 <span class="localTime" data-time="{{.Circuit.NextProbeText}}">{{.Circuit.NextProbeText}}</span>{{end}}</span><span>主动重置</span><span>{{if .ResetCreditsAvailableCount}}{{.ResetCreditsAvailableCount}} 次{{else}}暂无{{end}}{{if .ResetCreditsTotalEarnedCount}} / 累计 {{.ResetCreditsTotalEarnedCount}} 次{{end}}{{range .ResetCredits}}；{{if .Status}}{{.Status}} {{end}}有效期 <span class="localTime" data-time="{{.ExpiresAt}}">{{.ExpiresAt}}</span>{{end}}</span></div>
{{if or .Notes .GroupNotes}}<div class="noteBlock">{{if .Notes}}<div>账号备注：{{.Notes}}</div>{{end}}{{if .GroupNotes}}<div>分组备注：{{.GroupNotes}}</div>{{end}}</div>{{end}}
<div class="cardActions"><button type="button" class="ghost pinAccount {{if eq .SchedulerPriority 999}}pinned{{end}}" data-auth-id="{{.AuthID}}" data-pinned="{{if eq .SchedulerPriority 999}}true{{else}}false{{end}}" aria-pressed="{{if eq .SchedulerPriority 999}}true{{else}}false{{end}}" aria-label="固定账号" title="固定账号"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M16 3a1 1 0 0 1 .8 1.6l-1.35 1.8 2.15 2.15a1 1 0 0 1 0 1.41l-2.12 2.12a1 1 0 0 1-.7.3h-2.37l-2.2 2.2.71 3.54a1 1 0 0 1-1.69.9L5 14.8a1 1 0 0 1 .9-1.69l3.54.71 2.2-2.2V9.25a1 1 0 0 1 .3-.7l2.12-2.12L15.2 4.6A1 1 0 0 1 16 3Z"/></svg></button><button type="button" class="ghost refreshOne" data-auth-id="{{.AuthID}}">刷新额度</button><button type="button" class="secondary openEdit" data-auth-id="{{.AuthID}}">编辑</button></div>
</article>{{else}}<div class="empty">暂无账号数据。等待额度刷新后，这里会显示账号卡片。</div>{{end}}</section>
<section class="logs"><div class="logsHeader"><h2 data-i18n="logs.title">调度日志</h2><div class="actions"><button id="refreshLogs" type="button" class="ghost" data-i18n="actions.refreshLogs">刷新日志</button><button id="exportLogs" type="button" class="ghost" data-i18n="actions.exportLogs">导出日志</button></div></div><div id="logList" class="logList"></div></section>
</main>
</div>
<dialog id="editDialog"><form method="dialog" class="dialogBody"><div class="dialogHead"><div><h2 data-i18n="edit.title">编辑账号</h2><p id="editAuthID"></p></div><button type="button" id="closeDialog" class="ghost" data-i18n="actions.close">关闭</button></div><div class="dialogGrid"><label class="field"><span data-i18n="edit.alias">别名</span><input id="editAlias"></label><label class="field"><span data-i18n="account.schedulerPriority">插件优先级</span><input id="editSchedulerPriority" type="number" step="1" value="0"></label><label class="field"><span data-i18n="edit.groupID">分组 ID</span><input id="editGroupID" placeholder="team-a"></label><label class="field"><span data-i18n="edit.groupName">分组名称</span><input id="editGroupName"></label><label class="field"><span data-i18n="edit.tags">标签</span><input id="editTags" placeholder="team, paid"></label><label class="field wide"><span data-i18n="edit.notes">账号备注</span><textarea id="editNotes"></textarea></label><label class="field wide"><span data-i18n="edit.groupNotes">分组备注</span><textarea id="editGroupNotes"></textarea></label></div><div class="dialogActions"><button type="button" id="saveAccount" class="secondary" data-i18n="actions.saveAccount">保存账号</button><button type="button" id="cancelEdit" class="ghost" data-i18n="actions.cancel">取消</button></div></form></dialog>
<script>
let STATUS={{json .}};
const MANAGEMENT_BASE='/v0/management/plugins/codex-quota-scheduler';
const LOCALE_STORAGE_KEY='codex-quota-scheduler-locale-v1';
const MANAGEMENT_KEY_STORAGE_KEY='codex-quota-scheduler-management-key-v1';
const TRANSLATIONS={
  en:{
    'nav.retry':'Retry Chain','retry.title':'Model Retry Chain','retry.summary':'When an upstream reports capacity, overload, or 429/502/503 before any content reached the client, the request is retried against the next model in the chain.','retry.enabled':'Enable model retry chain','retry.shadow':'Shadow mode (record only, never retry)','retry.shadowHelp':'Shadow mode forwards every frame exactly as today and only records the requests a live chain would have retried, so you can watch it before enabling.','retry.chainTitle':'Retry chain','retry.chainHelp':'The left column is the model the client asks for; list the fallback targets in order on the right. Leave the provider empty to let CPA choose it, or use a CPA provider key such as codex, openai, or openai-compatible-zed2api. Only listed models with the chain enabled are affected.','retry.addRow':'Add model','retry.reloadCatalog':'Load model list','retry.rowModel':'Requested model','retry.provider':'Provider (optional)','retry.fallback':'Fallback','retry.addFallback':'Add fallback','retry.removeRow':'Remove model','retry.remove':'Remove','retry.maxAttempts':'Maximum attempts (including the first)','retry.stallTimeout':'Per-attempt silence timeout','retry.holdTimeout':'Hold budget before first commit','retry.chainDeadline':'Whole-chain deadline','retry.maxFrames':'Buffered frame limit','retry.maxBytes':'Buffered byte limit','retry.stripReasoning':'Drop encrypted reasoning when the model changes','retry.stripReasoningHelp':'Codex encrypted reasoning items are signed by the original model and cannot be verified by another one. Leaving this on avoids upstream rejections after a model switch.','retry.refreshStats':'Refresh records','retry.catalogLoading':'Loading provider and model list...','retry.catalogLoaded':'Provider and model list loaded.','retry.catalogFailed':'Could not load the provider and model list; type the values manually.','retry.catalogEmpty':'No providers reported; type the values manually.','retry.chainEmpty':'No retry chain configured. Add a model to start.','retry.statsTitle':'Retry records','retry.eventsTitle':'Recent attempts','retry.mode':'Mode','retry.modeOff':'Off','retry.modeLive':'Live','retry.modeShadow':'Shadow','retry.requests':'Requests','retry.retries':'Retries','retry.succeeded':'Succeeded','retry.failed':'Failed','retry.shadowHits':'Would have retried','retry.noEvents':'No retry activity recorded yet.','retry.attempt':'Attempt','retry.outcome':'Outcome','retry.invalidRow':'Every chain row needs a model and at least one fallback with a model.','retry.error.attempts':'Maximum attempts must be between 1 and 16.','retry.error.frames':'Buffered frame limit must be a whole number of at least 1.','retry.error.size':'Buffered byte limit must be a positive size such as 8MB.','retry.error.duration':'Timeouts must be positive durations such as 60s, 90s, or 240s.','notice.retrySaved':'Retry chain saved.',
    'app.title':'Codex Quota Scheduler','app.subtitle':'Optimized Fill First scheduling. Configuration, aliases, groups, tags, and notes are saved in the plugin state file.','app.language':'Language','connection.managementKey':'CPA management key','connection.rememberManagementKey':'Remember management key in this browser','connection.rememberManagementKeyHelp':'The key will be stored unencrypted in browser local storage. Enable this only on a trusted device.','connection.backgroundHint':'Once the scheduler is enabled, it runs in the background. This page does not need to stay open.',
    'resetProbe.warningTitle':'Automatic reset probe is off by default','resetProbe.warningBody':'Only after you check the box will the scheduler send one tiny Codex request when a reset looks lazy, nudging the next quota window to start.',
    'settings.title':'Scheduler Settings','settings.summary':'Default configuration is ready; normally no manual changes are needed.','settings.handleEnabled':'Enable scheduler takeover','settings.usageFeedback':'Mark quota exhausted from failure feedback','settings.enableResetProbe':'Enable automatic reset probe','settings.enableResetProbeHelp':'Probe performs read-only checks at the quota refresh interval with a 30-minute minimum, even while normal refresh is dormant, and sends one tiny request only after detecting a lazy reset window. This may consume a small amount of quota.','settings.provisionalProbe':'Allow quota probes when the account roster is unconfirmed (high risk)','settings.provisionalProbeHelp':'When CPA temporarily cannot confirm the current accounts and priorities, allow the plugin to use the most recently saved account roster for quota reset probes. Account credentials are revalidated every time, but the plugin still cannot guarantee that accounts have not been removed or reprioritized. This should normally remain off.','settings.monthlyMode':'Monthly mode','settings.acrossPriorities':'Use lower-priority accounts when higher tiers are exhausted','settings.acrossPrioritiesHelp':'When enabled, accounts from every CPA priority tier join scheduling: a higher tier always wins while it has a selectable account, and lower tiers are only reached when higher ones are all unavailable. Disable to schedule only the highest tier.','settings.managedDisable':'Disable accounts in CPA on quota exhaustion and recover automatically','settings.managedDisableHelp':'On a confirmed usage_limit_reached the account CPA credential is marked disabled; the plugin then checks quota read-only at the refresh interval and re-enables only when both quota windows are proven usable. Manually disabled accounts are never touched. Off by default; enabling lets the plugin write host auth files.','settings.expiryOrder':'Sort by expiry time','settings.monthlyPriority':'Prefer Monthly','settings.refreshInterval':'Quota refresh interval','settings.staleAfter':'Stale cache threshold','settings.refreshActiveWindow':'Refresh active window','settings.refreshAfterResetDelay':'Refresh after reset delay','settings.refreshRetryDelays':'Refresh retry delays','settings.refreshOnStartup':'Refresh on startup','settings.maxConcurrency':'Max refresh concurrency','settings.circuitFailureThreshold':'Circuit failure threshold','settings.circuitOpenDuration':'Circuit open duration','settings.circuitHalfOpenSuccessThreshold':'Half-open recovery successes','settings.maxLogEntries':'Max log entries','settings.lifecycleEventLimit':'Managed-disable history limit','settings.logRetention':'Log retention',
    'actions.loadData':'Load Data','actions.saveSettings':'Save Settings','actions.refreshQuota':'Refresh Quota','actions.exportConfig':'Export Config','actions.importConfig':'Import Config','actions.refreshLogs':'Refresh Logs','actions.exportLogs':'Export Logs','actions.close':'Close','actions.saveAccount':'Save Account','actions.cancel':'Cancel','actions.pinAccount':'Pin account','actions.unpinAccount':'Unpin account',
    'queue.title':'Account Queue','queue.description':'Account cards are sorted by the current scheduler priority. The first available account is preferred for the next Codex request.','queue.aria':'Account cards','metrics.nextAccount':'Next account','metrics.monthlyMode':'Monthly','metrics.lastSelected':'Last selected',
    'logs.title':'Scheduler Logs','logs.empty':'No logs yet. Send a request or refresh quota manually to show records here.',
    'edit.title':'Edit Account','edit.alias':'Alias','account.schedulerPriority':'Plugin priority','edit.groupID':'Group ID','edit.groupName':'Group name','edit.tags':'Tags','edit.notes':'Account notes','edit.groupNotes':'Group notes',
    'notice.settingsSaved':'Settings saved.','notice.statusLoaded':'Current settings loaded. Review them, then save again.','notice.refreshRequested':'Background quota refresh requested.','notice.accountSaved':'Account card saved.','notice.accountPinned':'Account pinned with plugin priority 999.','notice.accountUnpinned':'Account unpinned and plugin priority reset to 0.','notice.refreshOneRequested':'Quota refresh requested for this account.','notice.configExported':'Configuration exported.','notice.logsExported':'Logs exported.','notice.configImported':'Configuration imported.','error.requestFailed':'Request failed: {status}','error.managementKeyRequired':'CPA management key is required','error.schedulerPriorityInteger':'Plugin priority must be a safe integer.',
    'log.ui.refresh_requested':'UI requested quota refresh','log.ui.settings_saved':'UI saved scheduler settings','log.ui.refresh_one_requested':'UI requested one account quota refresh','log.ui.config_exported':'UI exported plugin configuration','log.ui.config_imported':'UI imported plugin configuration','log.ui.account_saved':'UI saved account card','log.ui.group_saved':'UI saved account group','log.scheduler.selected':'Request handled by plugin','log.scheduler.fallback':'Plugin delegated to the built-in scheduler','log.scheduler.unhandled':'Request was not handled by the plugin','log.scheduler.handled':'Plugin handled the request without selecting an account','log.quota.refresh_success':'Account quota refreshed successfully','log.quota.refresh_failed':'Account quota refresh failed','log.quota.refresh_one_failed':'Single-account quota refresh failed','log.quota.refresh_requested':'Background quota refresh requested','log.quota.refresh_one_requested':'Single-account quota refresh requested','log.quota.temporary_exhausted':'Account quota exhausted; waiting for reset','log.quota.temporary_recovered':'Manual refresh verified recovery; temporary exhaustion cleared','log.quota.manual_refresh_override':'Manual refresh overrode the host-side disabled/unavailable flag','log.circuit.success':'Successful request updated the circuit state','log.probe.precheck_started':'Checking a suspected lazy quota window','log.probe.activation_sent':'Tiny activation request sent','log.probe.verified':'Quota-window activation verified','log.probe.failed':'Quota-window probe failed and was handled safely'
  },
  'zh-CN':{
    'nav.retry':'模型重试链','retry.title':'模型重试链','retry.summary':'上游在内容尚未发给客户端之前返回容量不足、超载或 429/502/503 时，自动改用链中的下一个模型重试。','retry.enabled':'启用模型重试链','retry.shadow':'影子模式（只记录，不真正重试）','retry.shadowHelp':'影子模式完全按现有路径转发，只把「本应重试」的请求写入下面的记录，便于先观察再启用。','retry.chainTitle':'重试链','retry.chainHelp':'左列是客户端请求的模型；右侧按顺序填备用目标。提供方留空时由 CPA 自动选择，填写时使用 CPA 的提供方键，例如 codex、openai、openai-compatible-zed2api。只对开启了「启用模型重试链」且出现在这里的模型生效。','retry.addRow':'添加模型','retry.reloadCatalog':'载入模型列表','retry.rowModel':'请求的模型','retry.provider':'提供方（可留空）','retry.fallback':'备用目标','retry.addFallback':'添加备用','retry.removeRow':'删除该模型','retry.remove':'删除','retry.maxAttempts':'最大尝试次数（含首次）','retry.stallTimeout':'单次静默超时','retry.holdTimeout':'等待可提交内容上限','retry.chainDeadline':'整条链总时限','retry.maxFrames':'保留帧上限','retry.maxBytes':'保留字节上限','retry.stripReasoning':'切换模型时移除加密推理内容','retry.stripReasoningHelp':'Codex 的加密推理条目由原模型签名，换模型后无法验证。保留开启可以避免换模型后请求被上游拒绝。','retry.refreshStats':'刷新记录','retry.catalogLoading':'正在载入提供方与模型列表…','retry.catalogLoaded':'已载入提供方与模型列表。','retry.catalogFailed':'无法载入提供方与模型列表，请手动填写。','retry.catalogEmpty':'没有读到提供方，请手动填写。','retry.chainEmpty':'尚未配置重试链。添加一个模型即可开始。','retry.statsTitle':'重试记录','retry.eventsTitle':'最近尝试','retry.mode':'模式','retry.modeOff':'关闭','retry.modeLive':'生效','retry.modeShadow':'影子','retry.requests':'请求数','retry.retries':'重试次数','retry.succeeded':'成功','retry.failed':'失败','retry.shadowHits':'本应重试','retry.noEvents':'暂无重试记录。','retry.attempt':'尝试','retry.outcome':'结果','retry.invalidRow':'每个模型至少要有一个填了模型的备用目标。','retry.error.attempts':'最大尝试次数必须在 1 到 16 之间。','retry.error.frames':'保留帧上限必须是至少为 1 的整数。','retry.error.size':'保留字节上限必须是正数，例如 8MB。','retry.error.duration':'超时必须是正的时间段，例如 60s、90s 或 240s。','notice.retrySaved':'重试链已保存。',
    'app.title':'Codex 额度调度器','app.subtitle':'优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。','app.language':'界面语言','connection.managementKey':'CPA 管理密钥','connection.rememberManagementKey':'在此浏览器中记住管理密钥','connection.rememberManagementKeyHelp':'密钥将以未加密形式保存在浏览器本地存储中。请仅在受信任的设备上启用。','connection.backgroundHint':'只要调度器启动了，它就会在后台自动运行，无需保持页面开启。',
    'resetProbe.warningTitle':'自动激活新的额度周期默认关闭','resetProbe.warningBody':'开启后，调度器会在额度重置时间已到但新周期尚未生成时，发送一次极小的 Codex 请求尝试激活新周期。',
    'settings.title':'调度设置','settings.summary':'默认配置已经都设置好了，正常情况下不需要手动设置。','settings.handleEnabled':'启用调度接管','settings.usageFeedback':'失败反馈标记额度耗尽','settings.enableResetProbe':'自动激活新的额度周期','settings.enableResetProbeHelp':'即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短 30 分钟；只有检测到延迟启动的重置窗口时，才发送一次极小请求。可能消耗少量额度。','settings.provisionalProbe':'账号列表未确认时仍允许额度探测（高风险）','settings.provisionalProbeHelp':'CPA 暂时无法确认当前账号及优先级时，允许插件使用最近一次保存的账号列表执行额度重置探测。每次都会重新验证账号凭据，但仍无法保证账号未被删除或调整优先级。通常应保持关闭。','settings.monthlyMode':'月度账号使用方式','settings.acrossPriorities':'更高优先级耗尽时使用较低优先级账号','settings.acrossPrioritiesHelp':'开启后，所有 CPA 优先级层级的账号都会纳入调度：更高层级只要还有可用账号就一定优先，仅在更高层级全部不可用时才选用低层级。关闭则只调度最高优先级层。','settings.managedDisable':'额度耗尽时在 CPA 层停用账号并自动恢复','settings.managedDisableHelp':'收到确认的额度耗尽反馈时，把该账号的 CPA 凭据标记为 disabled；之后按刷新间隔只读检查额度，只有证实两个额度窗口都可用时才自动恢复启用。人工手动停用的账号绝不会被改动。默认关闭；开启后插件会写宿主的认证文件。','settings.expiryOrder':'按到期时间排序','settings.monthlyPriority':'优先使用月度账号','settings.refreshInterval':'额度刷新间隔','settings.staleAfter':'缓存过期判定','settings.refreshActiveWindow':'活跃刷新窗口','settings.refreshAfterResetDelay':'重置后刷新延迟','settings.refreshRetryDelays':'刷新失败重试间隔','settings.refreshOnStartup':'启动时刷新额度','settings.maxConcurrency':'最大并发刷新','settings.circuitFailureThreshold':'熔断失败阈值','settings.circuitOpenDuration':'熔断等待时间','settings.circuitHalfOpenSuccessThreshold':'半开恢复成功次数','settings.maxLogEntries':'最大日志条数','settings.lifecycleEventLimit':'托管停用历史条数上限','settings.logRetention':'日志保留时间',
    'actions.loadData':'加载数据','actions.saveSettings':'保存设置','actions.refreshQuota':'刷新额度','actions.exportConfig':'导出配置','actions.importConfig':'导入配置','actions.refreshLogs':'刷新日志','actions.exportLogs':'导出日志','actions.close':'关闭','actions.saveAccount':'保存账号','actions.cancel':'取消','actions.pinAccount':'固定账号','actions.unpinAccount':'取消固定账号',
    'queue.title':'账号队列','queue.description':'账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。','queue.aria':'账号卡片','metrics.nextAccount':'下一账号','metrics.monthlyMode':'Monthly','metrics.lastSelected':'最近选择',
    'logs.title':'调度日志','logs.empty':'暂无日志。发起请求或手动刷新额度后，这里会显示记录。',
    'edit.title':'编辑账号','edit.alias':'别名','account.schedulerPriority':'插件优先级','edit.groupID':'分组 ID','edit.groupName':'分组名称','edit.tags':'标签','edit.notes':'账号备注','edit.groupNotes':'分组备注',
    'notice.settingsSaved':'设置已保存，页面内容会自动更新。','notice.statusLoaded':'已加载当前设置。请确认后再次保存。','notice.refreshRequested':'已请求后台刷新额度，页面内容会自动更新。','notice.accountSaved':'账号卡片已保存，页面内容会自动更新。','notice.accountPinned':'账号已固定，插件优先级已设为 999。','notice.accountUnpinned':'已取消固定，插件优先级已重置为 0。','notice.refreshOneRequested':'已请求刷新该账号额度，页面内容会自动更新。','notice.configExported':'配置已导出。','notice.logsExported':'日志已导出。','notice.configImported':'配置已导入，页面内容会自动更新。','error.requestFailed':'请求失败：{status}','error.managementKeyRequired':'需要填写 CPA 管理密钥','error.schedulerPriorityInteger':'插件优先级必须是安全整数。'
  }
};
TRANSLATIONS.en['retry.always']='Always retry all models';
TRANSLATIONS.en['retry.alwaysHelp']='Retry every model, including models without a configured chain. When no fallback is configured, retry the same model. Mutually exclusive with Shadow mode.';
TRANSLATIONS.en['retry.modes']='Shadow mode and Always retry cannot both be enabled.';
TRANSLATIONS.en['retry.checkCPA']='Check & fix CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmTitle']='Review CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmYes']='Apply recommended settings';
TRANSLATIONS.en['retry.cpaConfirmNo']='Cancel';
TRANSLATIONS.en['retry.cpaChecking']='Checking CPA retry settings...';
TRANSLATIONS.en['retry.cpaOK']='CPA retry settings are correct.';
TRANSLATIONS.en['retry.cpaFixed']='CPA retry settings were fixed and hot-reloaded.';
TRANSLATIONS.en['retry.cpaFailed']='Could not update CPA retry settings: {error}';
TRANSLATIONS.en['retry.cpaUpgrade']='CPA {version} is too old for this feature. Please upgrade to v7.3.4 or newer.';
TRANSLATIONS.en['retry.cpaVersionUnknown']='Could not determine the CPA version; upgrade CPA to v7.3.4 or newer before changing these settings.';
TRANSLATIONS.en['retry.cpaConfirmTitle']='Review CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmYes']='Apply recommended settings';
TRANSLATIONS.en['retry.cpaConfirmNo']='Cancel';
TRANSLATIONS.en['retry.cpaConfirmBody']='Current: request-retry={requestRetry}, buffering={buffering}, timeout={timeout}, bootstrap-retries={bootstrapRetries}. Recommended: request-retry=3, buffering=true, timeout="0", bootstrap-retries=1.';
TRANSLATIONS.en['retry.cpaNoChanges']='No changes made.';
TRANSLATIONS.en['log.retry.attempt_started']='Retry chain attempt started';
TRANSLATIONS.en['log.retry.retry_scheduled']='Retry scheduled after a retryable upstream failure';
TRANSLATIONS.en['log.retry.aborted']='Retry stopped because the upstream failure was not retryable';
TRANSLATIONS.en['log.retry.exhausted']='Retry chain exhausted; returning the last failure';
TRANSLATIONS.en['log.retry.shadow_would_retry']='Shadow mode: this request would have been retried';
TRANSLATIONS['zh-CN']['retry.always']='全局重试（所有模型）';
TRANSLATIONS['zh-CN']['retry.alwaysHelp']='对没有配置模型链的请求也启用重试；没有备用模型时按同一模型再次尝试。与影子模式互斥。';
TRANSLATIONS['zh-CN']['retry.modes']='影子模式和全局重试不能同时启用。';
TRANSLATIONS['zh-CN']['retry.checkCPA']='检查并修复 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmTitle']='确认修改 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmYes']='应用推荐设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmNo']='取消';
TRANSLATIONS['zh-CN']['retry.cpaChecking']='正在检查 CPA 重试设置…';
TRANSLATIONS['zh-CN']['retry.cpaOK']='CPA 重试设置正确。';
TRANSLATIONS['zh-CN']['retry.cpaFixed']='CPA 重试设置已修复并热加载。';
TRANSLATIONS['zh-CN']['retry.cpaFailed']='无法更新 CPA 重试设置：{error}';
TRANSLATIONS['zh-CN']['retry.cpaUpgrade']='当前 CPA {version} 太旧，无法使用此功能。请升级到 v7.3.4 或更高版本。';
TRANSLATIONS['zh-CN']['retry.cpaVersionUnknown']='无法确定 CPA 版本；请先将 CPA 升级到 v7.3.4 或更高版本，再修改这些设置。';
TRANSLATIONS['zh-CN']['retry.cpaConfirmTitle']='确认 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmYes']='应用推荐设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmNo']='取消';
TRANSLATIONS['zh-CN']['retry.cpaConfirmBody']='当前值：request-retry={requestRetry}，buffering={buffering}，timeout={timeout}，bootstrap-retries={bootstrapRetries}。推荐值：request-retry=3，buffering=true，timeout="0"，bootstrap-retries=1。';
TRANSLATIONS['zh-CN']['retry.cpaNoChanges']='未做任何修改。';
TRANSLATIONS['zh-CN']['log.retry.attempt_started']='重试链开始尝试';
TRANSLATIONS['zh-CN']['log.retry.retry_scheduled']='上游失败可重试，已安排下一次尝试';
TRANSLATIONS['zh-CN']['log.retry.aborted']='上游失败不可重试，已停止重试';
TRANSLATIONS['zh-CN']['log.retry.exhausted']='重试链已用尽，返回最后一次失败';
TRANSLATIONS['zh-CN']['log.retry.shadow_would_retry']='影子模式：该请求本应重试';
const notice=document.getElementById('notice');
const editDialog=document.getElementById('editDialog');
const localeSelect=document.getElementById('localeSelect');
const settingsPanel=document.getElementById('settingsPanel');
const retryPanel=document.getElementById('retryPanel');
const retryChainEditor=document.getElementById('retryChainEditor');
const accountsByID=new Map();
function retryElement(id){return document.getElementById(id)||document.createElement('input')}
function retryTextValue(id,fallback){const node=document.getElementById(id);const raw=node?String(node.value||'').trim():'';return raw||fallback||''}
function retryNormalizeDuration(raw){const value=String(raw||'').trim();if(!value)return '';return /^\d+(\.\d+)?$/.test(value)?value+'s':value}
function retryDurationValue(id,fallback){const node=document.getElementById(id);const raw=node?retryNormalizeDuration(node.value):'';return raw||fallback||''}
function retryIntValue(id,fallback){const node=document.getElementById(id);const parsed=Number.parseInt(node?node.value:'',10);return Number.isFinite(parsed)&&parsed>0?parsed:fallback}
function collectRetryChainRows(){const rows=[];if(!retryChainEditor)return rows;for(const rowNode of retryChainEditor.querySelectorAll('[data-retry-row]')){const modelInput=rowNode.querySelector('[data-retry-model]');const model=modelInput?String(modelInput.value||'').trim():'';if(!model)continue;const fallbacks=[];for(const fallbackNode of rowNode.querySelectorAll('[data-retry-fallback]')){const fallbackModelInput=fallbackNode.querySelector('[data-retry-fallback-model]');const fallbackModel=fallbackModelInput?String(fallbackModelInput.value||'').trim():'';if(!fallbackModel)continue;fallbacks.push({provider:'',model:fallbackModel})}rows.push({model:model,fallbacks:fallbacks})}return rows}
function retryOptionNode(value){const option=document.createElement('option');option.value=value;return option}
function addRetryChainRow(){if(!retryChainEditor)return;retryClearEmptyState();retryChainEditor.append(retryChainRowNode({model:'',fallbacks:[{provider:'',model:''}]}))}
function renumberRetryFallbacks(container){if(!container)return;for(const [index,wrapper] of Array.from(container.querySelectorAll('[data-retry-fallback]')).entries()){const label=wrapper.querySelector('[data-retry-fallback-label]');if(label)label.textContent=index===0?t('retry.fallback'):t('retry.fallback')+' '+(index+1)}}
function retryFallbackNode(target,container,index){const wrapper=node('div','retryFallback');wrapper.dataset.retryFallback='1';const modelField=node('label','field');const fallbackLabel=node('span','',index>1?t('retry.fallback')+' '+index:t('retry.fallback'));fallbackLabel.dataset.retryFallbackLabel='1';modelField.append(fallbackLabel);const modelInput=document.createElement('input');modelInput.setAttribute('list','retryModelOptions');modelInput.setAttribute('spellcheck','false');modelInput.dataset.retryFallbackModel='1';modelInput.value=(target&&target.model)||'';modelField.append(modelInput);const remove=node('button','ghost retryRemove','');remove.type='button';remove.textContent=t('retry.remove');remove.addEventListener('click',()=>{const siblings=container?container.querySelectorAll('[data-retry-fallback]'):[];if(siblings.length<=1){modelInput.value=''}else{wrapper.remove();renumberRetryFallbacks(container)}});wrapper.append(modelField,remove);return wrapper}
function retryClearEmptyState(){if(!retryChainEditor)return;for(const empty of retryChainEditor.querySelectorAll('.retryEmpty'))empty.remove()}
function retryEnsureEmptyState(){if(!retryChainEditor)return;if(retryChainEditor.querySelector('[data-retry-row]'))return;if(retryChainEditor.querySelector('.retryEmpty'))return;retryChainEditor.append(node('div','retryEmpty',t('retry.chainEmpty')))}
function retryChainRowNode(row){const wrapper=node('div','retryChainRow');wrapper.dataset.retryRow='1';const head=node('div','retryChainRowHead');const modelField=node('label','field');modelField.append(node('span','',t('retry.rowModel')));const modelInput=document.createElement('input');modelInput.setAttribute('list','retryModelOptions');modelInput.setAttribute('spellcheck','false');modelInput.dataset.retryModel='1';modelInput.value=(row&&row.model)||'';modelField.append(modelInput);const actions=node('div','retryRowActions');const addFallback=node('button','ghost','');addFallback.type='button';addFallback.textContent=t('retry.addFallback');const removeRow=node('button','ghost','');removeRow.type='button';removeRow.textContent=t('retry.removeRow');actions.append(addFallback,removeRow);head.append(modelField,actions);const fallbacks=node('div','retryFallbacks');const source=row&&Array.isArray(row.fallbacks)&&row.fallbacks.length?row.fallbacks:[{provider:'',model:''}];source.forEach((target,index)=>fallbacks.append(retryFallbackNode(target,fallbacks,index+1)));addFallback.addEventListener('click',()=>{fallbacks.append(retryFallbackNode({provider:'',model:''},fallbacks,fallbacks.querySelectorAll('[data-retry-fallback]').length+1));renumberRetryFallbacks(fallbacks)});removeRow.addEventListener('click',()=>{wrapper.remove();retryEnsureEmptyState()});wrapper.append(head,fallbacks);return wrapper}
function renderRetryChainEditor(rows){if(!retryChainEditor)return;retryChainEditor.replaceChildren();for(const row of (Array.isArray(rows)?rows:[]))retryChainEditor.append(retryChainRowNode(row));retryEnsureEmptyState()}
function syncRetryModes(){const shadow=retryElement('retryShadow');const always=retryElement('retryAlways');if(shadow)shadow.disabled=always&&always.checked;if(always)always.disabled=shadow&&shadow.checked}
function fillRetrySettings(){const s=STATUS.settings||{};retryElement('retryEnabled').checked=s.retry_enabled===true;retryElement('retryShadow').checked=s.retry_shadow===true;retryElement('retryAlways').checked=s.retry_always===true;syncRetryModes();retryElement('retryStripReasoning').checked=s.retry_strip_reasoning!==false;retryElement('retryMaxAttempts').value=s.retry_max_attempts||4;retryElement('retryMaxFrames').value=s.retry_max_frames||4096;retryElement('retryStallTimeout').value=s.retry_stall_timeout||'60s';retryElement('retryHoldTimeout').value=s.retry_hold_timeout||'90s';retryElement('retryChainDeadline').value=s.retry_chain_deadline||'240s';retryElement('retryMaxBytes').value=s.retry_max_bytes||'8MB';renderRetryChainEditor(s.retry_chain||[])}
const RETRY_DURATION_RE=/^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/i;
const RETRY_SIZE_RE=/^\d+(\.\d+)?(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$/i;
const RETRY_DURATION_FIELDS=[['retry_stall_timeout','retry.stallTimeout'],['retry_hold_timeout','retry.holdTimeout'],['retry_chain_deadline','retry.chainDeadline']];
function retryPayloadError(payload){const attempts=Number(payload.retry_max_attempts);if(!Number.isSafeInteger(attempts)||attempts<1||attempts>16)return {key:'retry.error.attempts',field:'retry.maxAttempts',value:payload.retry_max_attempts};const frames=Number(payload.retry_max_frames);if(!Number.isSafeInteger(frames)||frames<1)return {key:'retry.error.frames',field:'retry.maxFrames',value:payload.retry_max_frames};if(!RETRY_SIZE_RE.test(String(payload.retry_max_bytes||'')))return {key:'retry.error.size',field:'retry.maxBytes',value:payload.retry_max_bytes};for(const pair of RETRY_DURATION_FIELDS){if(!RETRY_DURATION_RE.test(String(payload[pair[0]]||'')))return {key:'retry.error.duration',field:pair[1],value:payload[pair[0]]}}if(payload.retry_shadow&&payload.retry_always)return {key:'retry.modes'};if(payload.retry_enabled&&!payload.retry_always){if(!Array.isArray(payload.retry_chain)||payload.retry_chain.length===0)return {key:'retry.invalidRow'};for(const row of payload.retry_chain){if(!row.model||!Array.isArray(row.fallbacks)||row.fallbacks.length===0)return {key:'retry.invalidRow'}}}return null}
function retryProblemMessage(problem){const label=problem.field?t(problem.field):'';const value=problem.value===undefined||problem.value===null?'':String(problem.value).trim();const detail=[label,value].filter(Boolean).join(': ');return detail?t(problem.key)+' ('+detail+')':t(problem.key)}
async function saveRetrySettings(){try{if(!statusLoaded){await loadStatus();return}const payload=collectSettingsPayload();const problem=retryPayloadError(payload);if(problem){showNotice(retryProblemMessage(problem),true,problem.key);return}await requestManagement('/settings',{method:'PUT',body:payload});settingsDirty=false;showNotice(t('notice.retrySaved'),false,'notice.retrySaved');await refreshStatus({management:true,fillSettings:true});await refreshRetryStats()}catch(error){showNotice(error.message||String(error),true)}}
async function requestManagementRoot(path,query){const headers=authHeaders();let url='/v0/management'+path;if(query){const params=new URLSearchParams(query);url+='?'+params.toString()}const resp=await fetch(url,{method:'GET',headers});const data=await readJSON(resp);if(!resp.ok){const message=data.error||data.message||t('error.requestFailed',{status:resp.status});throw new Error(message)}return data}
async function requestManagementText(path,options){const opts=options||{};const headers=authHeaders();if(opts.contentType)headers['Content-Type']=opts.contentType;const init={method:opts.method||'GET',headers};if(Object.prototype.hasOwnProperty.call(opts,'body'))init.body=String(opts.body);const resp=await fetch('/v0/management'+path,init);const body=await resp.text();if(!resp.ok){let message=body;try{const data=JSON.parse(body);message=data.error||data.message||body}catch(error){}throw new Error(message||t('error.requestFailed',{status:resp.status}))}return body}
function cpaRetrySettingsCorrect(config){const codex=config&&config.codex||{};const streaming=config&&config.streaming||{};return Number(config&&config['request-retry'])===3&&codex['stream-bootstrap-buffering']===true&&String(codex['stream-bootstrap-timeout']===undefined?'':codex['stream-bootstrap-timeout'])==='0'&&Number(streaming['bootstrap-retries'])===1}
function yamlTopLevelValue(yaml,key,value){const lines=String(yaml||'').split(/\r?\n/);const prefix=key+':';for(let index=0;index<lines.length;index+=1){const line=lines[index];if(/^\S/.test(line)&&line.trim().startsWith(prefix)){lines[index]=key+': '+value;return lines.join('\n')}}lines.unshift(key+': '+value);return lines.join('\n')}
function yamlNestedValue(yaml,section,key,value){const lines=String(yaml||'').split(/\r?\n/);let sectionIndex=-1;let sectionIndent=-1;for(let index=0;index<lines.length;index+=1){const line=lines[index];const trimmed=line.trim();if(trimmed===section+':'||trimmed.startsWith(section+': #')){sectionIndex=index;sectionIndent=line.search(/\S/);break}}if(sectionIndex<0){if(lines.length&&lines[lines.length-1]!=='')lines.push('');lines.push(section+':','  '+key+': '+value);return lines.join('\n')}let insertAt=sectionIndex+1;let childIndent=sectionIndent+2;for(let index=sectionIndex+1;index<lines.length;index+=1){const line=lines[index];const trimmed=line.trim();if(!trimmed||trimmed.startsWith('#')){insertAt=index+1;continue}const indent=line.search(/\S/);if(indent<=sectionIndent)break;insertAt=index+1;childIndent=indent;if(trimmed.startsWith(key+':')){lines[index]=' '.repeat(indent)+key+': '+value;return lines.join('\n')}}lines.splice(insertAt,0,' '.repeat(childIndent)+key+': '+value);return lines.join('\n')}
function cpaVersionAtLeast(version,major,minor,patch){const match=String(version||'').match(/v?(\d+)\.(\d+)\.(\d+)/i);if(!match)return false;const actual=[Number(match[1]),Number(match[2]),Number(match[3])];const required=[major,minor,patch];for(let index=0;index<3;index+=1){if(actual[index]!==required[index])return actual[index]>required[index]}return true}
async function getCPAVersion(){const resp=await fetch('/v0/management/config',{method:'GET',headers:authHeaders()});return resp.headers.get('X-CPA-VERSION')||resp.headers.get('X-SERVER-VERSION')||''}
function confirmCPASettingsInline(config){const panel=document.getElementById('retryCPAConfirm');const body=document.getElementById('retryCPAConfirmBody');if(!panel||!body)return Promise.resolve(false);const codex=config&&config.codex||{};const streaming=config&&config.streaming||{};const values=[['request-retry',config&&config['request-retry'],3],['stream-bootstrap-buffering',codex['stream-bootstrap-buffering'],true],['stream-bootstrap-timeout',codex['stream-bootstrap-timeout'],'0'],['bootstrap-retries',streaming['bootstrap-retries'],1]];body.replaceChildren();const table=document.createElement('table');const head=document.createElement('tr');for(const label of [currentLocale==='en'?'Setting':'设置',currentLocale==='en'?'Current':'当前值',currentLocale==='en'?'Recommended':'推荐值'])head.append(node('th','',label));table.append(head);for(const row of values){const tr=document.createElement('tr');const different=String(row[1])!==String(row[2]);tr.append(node('td',different?'diff':'',row[0]),node('td',different?'diff':'',String(row[1])),node('td',different?'diff':'',String(row[2])));table.append(tr)}body.append(table);panel.hidden=false;return new Promise((resolve)=>{const yes=document.getElementById('retryCPAConfirmYes');const no=document.getElementById('retryCPAConfirmNo');const finish=(accepted)=>{panel.hidden=true;yes.removeEventListener('click',onYes);no.removeEventListener('click',onNo);resolve(accepted)};const onYes=()=>finish(true);const onNo=()=>finish(false);yes.addEventListener('click',onYes);no.addEventListener('click',onNo)})}
async function checkAndFixCPASettings(){const button=document.getElementById('retryCheckCPA');const status=document.getElementById('retryCPAStatus');if(button)button.disabled=true;if(status)status.textContent=t('retry.cpaChecking');try{const version=await getCPAVersion();if(!version){if(status)status.textContent=t('retry.cpaVersionUnknown');return}if(!cpaVersionAtLeast(version,7,3,4)){if(status)status.textContent=t('retry.cpaUpgrade',{version:version});return}const before=await requestManagementRoot('/config');if(cpaRetrySettingsCorrect(before)){if(status)status.textContent=t('retry.cpaOK');return}if(!await confirmCPASettingsInline(before)){if(status)status.textContent=t('retry.cpaNoChanges');return}let yaml=await requestManagementText('/config.yaml');yaml=yamlTopLevelValue(yaml,'request-retry','3');yaml=yamlNestedValue(yaml,'codex','stream-bootstrap-buffering','true');yaml=yamlNestedValue(yaml,'codex','stream-bootstrap-timeout','"0"');yaml=yamlNestedValue(yaml,'streaming','bootstrap-retries','1');await requestManagementText('/config.yaml',{method:'PUT',contentType:'application/yaml',body:yaml});const after=await requestManagementRoot('/config');if(!cpaRetrySettingsCorrect(after))throw new Error('CPA did not report the requested settings after hot reload');if(status)status.textContent=t('retry.cpaFixed')}catch(error){if(status)status.textContent=t('retry.cpaFailed',{error:error.message||String(error)})}finally{if(button)button.disabled=false}}
function addRetryProvider(providers,name){const value=String(name||'').trim();if(value)providers.add(value)}
function collectRetryProviders(config,providers){providers.add('codex');providers.add('openai');if(!config||typeof config!=='object')return;const compat=config['openai-compatibility'];if(Array.isArray(compat)){for(const entry of compat){const name=String((entry&&entry.name)||'').trim();if(!name)continue;providers.add('openai-compatible-'+name.toLowerCase())}}for(const pair of [['gemini-api-key','gemini'],['claude-api-key','claude'],['vertex-api-key','vertex'],['codex-api-key','codex'],['interactions-api-key','interactions']]){if(config[pair[0]])providers.add(pair[1])}}
function retryModelKey(value){return String(value||'').trim().toLowerCase().replace(/[^a-z0-9]+/g,'')}
function addRetryModel(models,value){const id=String(value||'').trim();const key=retryModelKey(id);if(!key)return;const current=models.get(key);if(!current||((current.includes(' ')||current.includes('_'))&&!id.includes(' ')))models.set(key,id)}
function collectRetryModels(config,models){if(!config||typeof config!=='object')return;const compat=config['openai-compatibility'];if(!Array.isArray(compat))return;for(const entry of compat){const list=entry&&Array.isArray(entry.models)?entry.models:[];for(const model of list){for(const value of [model&&model.alias,model&&model.name])addRetryModel(models,value)}}}
function retrySetCatalogStatus(text){const node=document.getElementById('retryCatalogStatus');if(node)node.textContent=text}
function renderRetryCatalogOptions(){const providerList=document.getElementById('retryProviderOptions');const modelList=document.getElementById('retryModelOptions');if(providerList){providerList.replaceChildren();for(const value of retryCatalog.providers)providerList.append(retryOptionNode(value))}if(modelList){modelList.replaceChildren();for(const value of retryCatalog.models)modelList.append(retryOptionNode(value))}}
async function loadRetryCatalog(){retrySetCatalogStatus(t('retry.catalogLoading'));try{let config=null;try{config=await requestManagementRoot('/config')}catch(error){config=null}const providers=new Set();const models=new Map();collectRetryProviders(config,providers);collectRetryModels(config,models);let files=[];try{const listing=await requestManagementRoot('/auth-files');files=Array.isArray(listing.files)?listing.files:[]}catch(error){files=[]}let queried=0;for(const file of files){addRetryProvider(providers,file&&file.provider);const name=String((file&&file.name)||'').trim();if(!name||queried>=40)continue;queried+=1;try{const detail=await requestManagementRoot('/auth-files/models',{name:name});for(const model of (detail.models||[]))addRetryModel(models,model&&model.id)}catch(error){}}retryCatalog.providers=Array.from(providers).sort();retryCatalog.models=Array.from(models.values()).sort();retryCatalog.loaded=true;renderRetryCatalogOptions();retrySetCatalogStatus(retryCatalog.models.length===0?t('retry.catalogEmpty'):t('retry.catalogLoaded'))}catch(error){retrySetCatalogStatus(t('retry.catalogFailed'))}}
async function refreshRetryStats(){try{const data=await requestManagement('/retry/status',{query:{format:'json'}});retryStats=data||{};retryStatsLoaded=true}catch(error){retryStatsLoaded=false}renderRetryStats()}
function renderRetryStats(){const container=document.getElementById('retryStats');const events=document.getElementById('retryEvents');if(!container||!events)return;container.replaceChildren();const mode=String(retryStats.mode||'off');const modeKey=mode==='live'?'retry.modeLive':mode==='shadow'?'retry.modeShadow':'retry.modeOff';const cards=[[t('retry.mode'),t(modeKey)],[t('retry.requests'),retryStats.requests||0],[t('retry.retries'),retryStats.retries||0],[t('retry.succeeded'),retryStats.succeeded||0],[t('retry.failed'),retryStats.failed||0],[t('retry.shadowHits'),retryStats.shadow_hits||0]];for(const card of cards){const box=node('div','retryStat');box.append(node('span','retryStatLabel',card[0]),node('span','retryStatValue',String(card[1])));container.append(box)}const chain=Array.isArray(retryStats.chain)?retryStats.chain:[];if(chain.length){const summary=node('div','retryChainSummary');summary.append(node('div','retryChainSummaryTitle',t('retry.chainTitle')));for(const line of chain)summary.append(node('div','retryChainLine',line));container.append(summary)}events.replaceChildren();const history=Array.isArray(retryStats.events)?retryStats.events.slice().reverse():[];if(history.length===0){events.append(node('div','empty',t('retry.noEvents')));return}for(const event of history){const row=node('div','logItem info');const meta=node('div','logMeta',[event.at?formatLogTime(event.at):'',event.model||'',event.target||''].filter(Boolean).join(' · '));const detail=[event.outcome||'',event.kind||'',event.reason||'',event.next_target?'-> '+event.next_target:''].filter(Boolean).join(' ');row.append(meta,node('div','logMsg',detail));events.append(row)}}
const groupsByID=new Map();
let editingAuthID='';
let currentLocale=detectLocale();
let statusLoaded=!STATUS.shell;
let settingsDirty=false;
let settingsInitialized=!STATUS.shell;
let retryCatalog={providers:[],models:[],loaded:false};
let retryStats={};
let retryStatsLoaded=false;
let statusPollID=0;
const INLINE_TRANSLATIONS=[
  ['下一优先','Next preferred'],['可用','Available'],['未知类型','unknown type'],['CPA 优先级','CPA priority'],['插件优先级','Plugin priority'],['熔断：','Circuit: '],['熔断','Circuit'],['全开','closed'],['半开','half-open'],
  ['按到期时间','by expiry time'],['优先使用','prefer Monthly'],['已启用','enabled'],['已关闭','disabled'],['暂无数据','No data'],['暂无','None'],['已用完','Used up'],
  ['5 小时额度','5-hour quota'],['周额度','Weekly quota'],['月额度','Monthly quota'],['剩余 ','Remaining '],['5 小时重置：','5-hour reset: '],['长额度重置：','Long quota reset: '],
  ['缓存时间','Cache age'],['额度来源','Quota source'],['响应观测','Response observation'],['轮询刷新','Polled refresh'],['托管停用','Managed disable'],['已停用，恢复中','disabled, recovering'],['检查额度中','checking quota'],['熔断计数','Circuit counts'],['失败','Failures'],['成功','Successes'],['主动重置','Reset credits'],['调度状态','Scheduler state'],[' 次',' times'],['累计','total'],['有效期','expires'],
  ['刷新额度','Refresh Quota'],['编辑','Edit'],['请重新登录','Please re-login'],['不可用','Unavailable'],['最后错误','Last error'],['下次重试','Next retry'],['长额度','Long quota'],[' 重置：',' reset: '],['Monthly：','Monthly: '],['调度：','Scheduler: '],['下一账号：','Next account: '],['最近选择：','Last selected: '],
  ['最近一次扫描认证文件中没有 Codex 账号，请先进行 OS 登录，然后发送一次 Codex 请求或手动刷新额度。','The most recent auth-file scan found no Codex accounts. Please sign in through OS login, then send a Codex request or refresh quota manually.'],
  [' 内没有观察到 Codex 请求，系统暂不主动扫描账号。','; the system will not actively scan accounts. '],['发送第一次 Codex 请求后将自动获取账号额度信息。','Send the first Codex request to fetch account quota automatically.'],
  [' 内没有 Codex 请求，调度器已暂停后台刷新。',' without a Codex request, so background refresh is paused. '],['发送一次 Codex 请求后会重新进入活跃窗口并获取账号额度信息。','Send one Codex request to re-enter the active window and fetch account quota.'],
  ['已观察到 Codex 请求，调度器处于活跃窗口。','A Codex request was observed and the scheduler is in its active window. '],['账号额度刷新完成后，这里会显示账号卡片。','Account cards will appear here after quota refresh finishes.'],['等待额度刷新后，这里会显示账号卡片。','Account cards will appear here after quota refresh.'],
  ['未发现 Codex 账号','No Codex accounts were found'],['最近一次扫描认证文件中没有 Codex 账号','The most recent auth-file scan found no Codex accounts'],['请先进行 OS 登录','please sign in through OS login'],['然后发送一次 Codex 请求或手动刷新额度','then send a Codex request or refresh quota manually'],
  ['账号额度已过期或待刷新，但最近 ','Quota data is stale or pending refresh, but no Codex requests were seen in the last '],[' 内没有 Codex 请求，调度器处于休眠状态。','; the scheduler is sleeping. '],['发送一次 Codex 请求后会获取账号额度信息。','Send one Codex request to fetch account quota.'],
  ['调度器处于休眠状态','The scheduler is sleeping'],['最近 ','No Codex requests were observed in the last '],[' 内没有观察到 Codex 请求',''],['系统暂不主动扫描账号','the system will not actively scan accounts'],['发送第一次 Codex 请求后将自动获取账号额度信息','Send the first Codex request to fetch account quota automatically'],
  ['内没有 Codex 请求，调度器已暂停后台刷新',' without a Codex request, so background refresh is paused'],['发送一次 Codex 请求后会重新进入活跃窗口并获取账号额度信息','Send one Codex request to re-enter the active window and fetch account quota'],
  ['等待账号额度数据','Waiting for account quota data'],['已观察到 Codex 请求，调度器处于活跃窗口','A Codex request was observed and the scheduler is in its active window'],['账号额度刷新完成后，这里会显示账号卡片','Account cards will appear here after quota refresh finishes'],['等待额度刷新后，这里会显示账号卡片','Account cards will appear here after quota refresh'],
  ['认证信息异常，请重新登录。','Authentication looks invalid. Please re-login.'],['上次额度刷新失败，调度器正在等待下次自动重试。','The last quota refresh failed. The scheduler is waiting for the next automatic retry.'],
  ['账号额度已过期，调度器处于活跃窗口，将按刷新队列更新。','Quota data is stale. The scheduler is active and will update it through the refresh queue.'],['账号尚未获取额度信息，调度器处于活跃窗口，将按刷新队列更新。','Quota has not been fetched yet. The scheduler is active and will update it through the refresh queue.'],
  ['上次额度刷新失败，当前已到重试时间。','The last quota refresh failed, and retry is due now.'],['额度重置时间已到，调度器将按刷新队列更新。','Quota reset time has arrived; the scheduler will update it through the refresh queue.'],
  ['账号处于熔断等待中，半开探测时间到达后会重试。','The account is in circuit wait and will retry after the half-open probe time.'],['本地认证信息缺失或格式异常，请检查登录状态。','Local auth data is missing or malformed. Please check sign-in state.'],
  ['填写 CPA 管理密钥后将动态加载账号队列、调度日志和当前调度状态','Enter the CPA management key to dynamically load the account queue, scheduler logs, and current scheduler state'],['发送 Codex 请求后，调度器会在活跃窗口内刷新额度','After a Codex request, the scheduler refreshes quota within the active window']
];
const SORTED_INLINE_TRANSLATIONS=[...INLINE_TRANSLATIONS].sort((left,right)=>right[0].length-left[0].length);
const DUE_REASON_LABELS={
  en:{stale:'Quota data is stale or pending refresh',never_refreshed:'Never refreshed',retry_due:'Retry is due now',retry_wait:'Waiting before retry',refresh_interval_due:'Refresh interval due',five_hour_reset_due:'5-hour reset is due',long_window_reset_due:'Long quota reset is due',temporary_reset_due:'Temporary reset is due',circuit_wait:'Circuit wait',circuit_probe_due:'Circuit probe due',auth_failure:'Authentication needs re-login',local_failure:'Local auth data needs attention'},
  'zh-CN':{stale:'额度缓存过期',never_refreshed:'尚未刷新',retry_due:'重试时间已到',retry_wait:'等待重试',refresh_interval_due:'刷新间隔已到',five_hour_reset_due:'5 小时重置已到',long_window_reset_due:'长额度重置已到',temporary_reset_due:'主动重置已到',circuit_wait:'熔断等待',circuit_probe_due:'熔断探测已到',auth_failure:'认证异常',local_failure:'本地认证异常'}
};
const UNAVAILABLE_REASON_LABELS={
  en:{auth_failure:'Authentication needs re-login',local_failure:'Local auth data needs attention',stale_quota:'Quota data is stale or pending refresh',circuit_open:'Circuit open',quota_probe_wait:'Quota probe wait',temporary_exhausted:'Temporarily exhausted',five_hour_exhausted:'5-hour quota exhausted',weekly_exhausted:'Weekly quota exhausted',monthly_exhausted:'Monthly quota exhausted',unknown_account:'Unknown account'},
  'zh-CN':{auth_failure:'认证异常',local_failure:'本地认证异常',stale_quota:'额度缓存过期',circuit_open:'熔断中',quota_probe_wait:'等待额度探测',temporary_exhausted:'临时额度耗尽',five_hour_exhausted:'5 小时额度耗尽',weekly_exhausted:'周额度耗尽',monthly_exhausted:'月额度耗尽',unknown_account:'未知账号'}
};
function normalizeLocale(raw){return String(raw||'').toLowerCase().startsWith('zh')?'zh-CN':'en'}
function storedLocale(){try{const locale=window.localStorage.getItem(LOCALE_STORAGE_KEY);return locale==='zh-CN'||locale==='en'?locale:''}catch(error){return''}}
function parentLocale(){try{if(window.parent===window)return'';const language=String(window.parent.document.documentElement.lang||'').trim().toLowerCase();if(!language)return'';return language.startsWith('zh')?'zh-CN':'en'}catch(error){return''}}
function detectLocale(){return storedLocale()||parentLocale()||'en'}
function watchParentLocale(){try{if(window.parent===window)return;const root=window.parent.document.documentElement;new MutationObserver(()=>{if(storedLocale())return;const next=parentLocale();if(next&&next!==currentLocale){currentLocale=next;applyLocale()}}).observe(root,{attributes:true,attributeFilter:['lang']})}catch(error){}}
function t(key,params){const dictionary=TRANSLATIONS[currentLocale]||TRANSLATIONS.en;let message=dictionary[key]||TRANSLATIONS.en[key]||key;for(const name of Object.keys(params||{})){message=message.split('{'+name+'}').join(String(params[name]))}return message}
function labelFrom(table,key,fallback){const dictionary=table[currentLocale]||table.en;return dictionary[key]||fallback||key||''}
function labelDueReason(reason){return labelFrom(DUE_REASON_LABELS,reason,reason)}
function labelUnavailableReason(reason){return labelFrom(UNAVAILABLE_REASON_LABELS,reason,reason||'不可用')}
function translateInlineText(raw){let text=String(raw||'');if(currentLocale!=='en')return text;for(const pair of SORTED_INLINE_TRANSLATIONS){text=text.split(pair[0]).join(pair[1])}return text}
function applyInlineTranslations(){const nodes=document.querySelectorAll('.badge,.quota-title,.quota-remaining,.quota-reset,.kv span,.cardActions button,.empty,.empty strong,.empty div');for(const node of nodes){if(node.children.length>0)continue;if(!node.dataset.rawText)node.dataset.rawText=node.textContent;node.textContent=translateInlineText(node.dataset.rawText)}formatLocalTimes()}
function applyLocale(){document.documentElement.lang=currentLocale;document.title=t('app.title');localeSelect.value=currentLocale;for(const node of document.querySelectorAll('[data-i18n]')){node.textContent=t(node.dataset.i18n)}for(const node of document.querySelectorAll('[data-i18n-aria]')){node.setAttribute('aria-label',t(node.dataset.i18nAria))}for(const node of document.querySelectorAll('.localeColon'))node.textContent=currentLocale==='en'?': ':'：';if(notice.dataset.i18nKey)notice.textContent=t(notice.dataset.i18nKey);for(const container of document.querySelectorAll('.retryFallbacks'))renumberRetryFallbacks(container);renderMetrics();renderAccounts(STATUS.accounts||[]);applyInlineTranslations();renderLogs(STATUS.logs||[])}
function changeLocale(locale){currentLocale=normalizeLocale(locale);try{window.localStorage.setItem(LOCALE_STORAGE_KEY,currentLocale)}catch(error){}applyLocale()}
let restoredManagementKey=false;
function syncManagementKeyVisibility(){const field=document.getElementById('managementKeyField');const remember=document.getElementById('rememberManagementKey');if(field&&remember)field.hidden=remember.checked&&restoredManagementKey}
function showManagementKeyInput(){restoredManagementKey=false;const field=document.getElementById('managementKeyField');const input=document.getElementById('managementKey');if(field)field.hidden=false;if(input){input.focus();input.select()}}
function restoreRememberedManagementKey(){try{const key=window.localStorage.getItem(MANAGEMENT_KEY_STORAGE_KEY);if(!key)return false;document.getElementById('managementKey').value=key;document.getElementById('rememberManagementKey').checked=true;restoredManagementKey=true;syncManagementKeyVisibility();return true}catch(error){return false}}
function syncRememberedManagementKey(){const remember=document.getElementById('rememberManagementKey').checked;const key=document.getElementById('managementKey').value.trim();syncManagementKeyVisibility();try{if(remember&&key)window.localStorage.setItem(MANAGEMENT_KEY_STORAGE_KEY,key);else window.localStorage.removeItem(MANAGEMENT_KEY_STORAGE_KEY)}catch(error){}}
function showNotice(text,isError,i18nKey){notice.hidden=false;notice.textContent=text;notice.className='notice'+(isError?' error':'');notice.dataset.i18nKey=i18nKey||''}
function rebuildDerivedState(){accountsByID.clear();groupsByID.clear();for(const account of STATUS.accounts||[]){if(account.auth_id)accountsByID.set(account.auth_id,account);if(account.group_id)groupsByID.set(account.group_id,{name:account.group||'',notes:account.group_notes||''})}for(const group of STATUS.groups||[]){if(group.id)groupsByID.set(group.id,{name:group.name||'',notes:group.notes||''})}}
function renderMetrics(){const empty=currentLocale==='en'?'None':'暂无';const monthlyMode=STATUS.monthly_mode==='priority'?(currentLocale==='en'?'prefer Monthly':'优先使用'):(currentLocale==='en'?'by expiry time':'按到期时间');const setText=(id,text)=>{const node=document.getElementById(id);if(node)node.textContent=text};setText('metricNextAuthID',STATUS.next_auth_id||empty);setText('metricMonthlyMode',monthlyMode);setText('metricLastSelected',STATUS.last_selected||empty)}
function hasManagementKey(){const input=document.getElementById('managementKey');return !!(input&&(input.value||'').trim())}
function settingsFocusedOrDirty(){const panels=[document.getElementById('settingsPanel'),document.getElementById('retryPanel')];return settingsDirty||panels.some((panel)=>panel&&panel.contains(document.activeElement))}
function updateResetProbeWarning(){const warning=document.getElementById('resetProbeWarning');if(warning)warning.hidden=!(statusLoaded&&STATUS.settings&&STATUS.settings.enable_reset_probe!==true)}
function updateProtectedVisibility(){const loaded=statusLoaded;const show=(id,visible)=>{const item=document.getElementById(id);if(item)item.hidden=!visible};show('settingsPanel',loaded);show('retryPanelWrap',loaded);show('protectedMain',loaded);show('refreshQuota',loaded);show('loadData',!loaded);updateResetProbeWarning()}
function renderRosterLifecycle(){const warning=document.getElementById('rosterLifecycleWarning');const title=document.getElementById('rosterLifecycleTitle');const body=document.getElementById('rosterLifecycleBody');if(!warning||!title||!body)return;const roster=STATUS.roster||{};const messages=[roster.warning,roster.risk_warning].filter(Boolean);warning.hidden=messages.length===0;title.textContent=[roster.capability,roster.health].filter(Boolean).join(' / ')||'Roster lifecycle';body.textContent=messages.join(' ')}
function applyStatus(data,options){STATUS=data;statusLoaded=true;rebuildDerivedState();const shouldFillSettings=(options&&options.fillSettings)||!settingsInitialized||!settingsFocusedOrDirty();if(shouldFillSettings){fillSettings();settingsInitialized=true}renderMetrics();renderRosterLifecycle();renderAccounts(STATUS.accounts||[]);renderLogs(STATUS.logs||[]);updateProtectedVisibility();applyLocale();if(statusLoaded)refreshRetryStats()}
async function refreshStatus(options){const opts=options||{};const data=await requestManagement('/status',{query:{format:'json'}});applyStatus(data,opts)}
function startStatusPolling(){if(statusPollID)return;statusPollID=window.setInterval(()=>refreshStatus({management:true}).catch(()=>{}),15000)}
async function loadStatus(){try{await refreshStatus({management:true,fillSettings:true});const remember=document.getElementById('rememberManagementKey');if(remember&&remember.checked&&hasManagementKey()){restoredManagementKey=true;syncManagementKeyVisibility()}showNotice(t('notice.statusLoaded'),false,'notice.statusLoaded');startStatusPolling()}catch(error){showNotice(error.message||String(error),true)}}
async function pollStatus(times,delayMs){for(let i=0;i<times;i++){await new Promise((resolve)=>window.setTimeout(resolve,delayMs));await refreshStatus()}}
function collectSettingsPayload(){return{handle_enabled:document.getElementById('handleEnabled').checked,enable_usage_feedback:document.getElementById('usageFeedback').checked,enable_reset_probe:document.getElementById('enableResetProbe').checked,probe_on_provisional_roster:document.getElementById('probeOnProvisionalRoster').checked,monthly_mode:document.getElementById('monthlyMode').value,schedule_across_priorities:document.getElementById('scheduleAcrossPriorities').checked,enable_managed_quota_disable:document.getElementById('enableManagedQuotaDisable').checked,lifecycle_event_limit:Number.parseInt(document.getElementById('lifecycleEventLimit').value,10)||50,quota_refresh_interval:document.getElementById('refreshInterval').value.trim(),stale_after:document.getElementById('staleAfter').value.trim(),refresh_active_window:document.getElementById('refreshActiveWindow').value.trim(),refresh_after_reset_delay:document.getElementById('refreshAfterResetDelay').value.trim(),refresh_retry_delays:document.getElementById('refreshRetryDelays').value.trim(),refresh_on_startup:document.getElementById('refreshOnStartup').checked,max_refresh_concurrency:Number.parseInt(document.getElementById('maxConcurrency').value,10)||1,circuit_failure_threshold:Number.parseInt(document.getElementById('circuitFailureThreshold').value,10)||5,circuit_open_duration:document.getElementById('circuitOpenDuration').value.trim(),circuit_half_open_success_threshold:Number.parseInt(document.getElementById('circuitHalfOpenSuccessThreshold').value,10)||2,max_log_entries:Number.parseInt(document.getElementById('maxLogEntries').value,10)||200,log_retention:document.getElementById('logRetention').value.trim(),retry_enabled:retryElement('retryEnabled').checked===true,retry_shadow:retryElement('retryShadow').checked===true,retry_always:retryElement('retryAlways').checked===true,retry_strip_reasoning:retryElement('retryStripReasoning').checked!==false,retry_max_attempts:retryIntValue('retryMaxAttempts',4),retry_max_frames:retryIntValue('retryMaxFrames',4096),retry_stall_timeout:retryDurationValue('retryStallTimeout','60s'),retry_hold_timeout:retryDurationValue('retryHoldTimeout','90s'),retry_chain_deadline:retryDurationValue('retryChainDeadline','240s'),retry_max_bytes:retryTextValue('retryMaxBytes','8MB'),retry_chain:collectRetryChainRows()}}
function node(tag,className,text){const item=document.createElement(tag);if(className)item.className=className;if(text!==undefined)item.textContent=text;return item}
function addKV(parent,key,value){parent.append(node('span','',key),node('span','',value||'暂无'))}
function quotaSourceLabel(source){if(source==='observed')return currentLocale==='en'?'Response observation':'响应观测';if(source==='polled')return currentLocale==='en'?'Polled refresh':'轮询刷新';return ''}
function managedDisableLabel(summary){const states={planned:currentLocale==='en'?'planned':'已计划',disabled:currentLocale==='en'?'disabled, recovering':'已停用，恢复中',checking:currentLocale==='en'?'checking quota':'检查额度中'};let text=states[summary.state]||summary.state;if(summary.next_check_at){const date=new Date(summary.next_check_at);if(!Number.isNaN(date.getTime()))text+='（'+date.toLocaleString(dateLocale(),{hour12:false})+'）'}return text}
function addBadge(text,className){return node('span','badge '+(className||''),text)}
function addQuota(windowData,label){const row=node('div','quota-row');const head=node('div','quota-head');head.append(node('span','quota-title',label||windowData.label||''),node('span','quota-remaining',windowData.display_text||''));if(!windowData.missing&&windowData.reset_text){const reset=node('span','quota-reset','');const rawLabel=label||windowData.label||'额度';const resetLabel=node('span','quota-reset-label','');resetLabel.dataset.label=rawLabel;const time=node('span','localTime quotaResetTime',windowData.reset_text);time.dataset.time=windowData.reset_text;reset.append(resetLabel,time);head.append(reset)}const bar=node('div','quota-bar');const fill=node('div','quota-fill '+quotaFillClass(windowData));fill.style.width=Math.max(0,Math.min(100,Math.round(windowData.remaining_percent||0)))+'%';bar.append(fill);row.append(head,bar);return row}
function quotaFillClass(windowData){if(windowData.missing)return'';const remaining=Number(windowData.remaining_percent||0);return windowData.exhausted||remaining<30?'danger':remaining<60?'warn':''}
function dateLocale(){return currentLocale==='en'?'en-US':'zh-CN'}
function resetCreditCount(value){if(currentLocale!=='en')return value+' 次';return value+' '+(Number(value)===1?'time':'times')}
function resetCreditSummary(account){const parts=[];if(account.reset_credits_available_count==null){parts.push(currentLocale==='en'?'None':'暂无')}else{parts.push(resetCreditCount(account.reset_credits_available_count))}if(account.reset_credits_total_earned_count!=null)parts.push(currentLocale==='en'?'/ total '+resetCreditCount(account.reset_credits_total_earned_count):'/ 累计 '+resetCreditCount(account.reset_credits_total_earned_count));for(const credit of account.reset_credits||[]){const raw=credit.expires_at||'';if(!raw)continue;const date=new Date(raw);const expiry=Number.isNaN(date.getTime())?raw:date.toLocaleString(dateLocale(),{hour12:false});const prefix=currentLocale==='en'?'; ':'；';const expires=currentLocale==='en'?'expires ':'有效期 ';parts.push(prefix+(credit.status?credit.status+' ':'')+expires+expiry)}return parts.join(' ')}
function renderEmptyState(){const empty=STATUS.empty_state||{};const title=empty.title||'暂无账号数据';const message=empty.message||'等待额度刷新后，这里会显示账号卡片。';const box=node('div','empty','');box.append(node('strong','',title),node('div','',message));return box}
function createPinButton(account){const pinned=Number(account.scheduler_priority)===999;const button=node('button','ghost pinAccount '+(pinned?'pinned':''),'');button.type='button';button.dataset.authId=account.auth_id||'';button.dataset.pinned=String(pinned);button.setAttribute('aria-pressed',String(pinned));const label=t(pinned?'actions.unpinAccount':'actions.pinAccount');button.setAttribute('aria-label',label);button.title=label;button.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M16 3a1 1 0 0 1 .8 1.6l-1.35 1.8 2.15 2.15a1 1 0 0 1 0 1.41l-2.12 2.12a1 1 0 0 1-.7.3h-2.37l-2.2 2.2.71 3.54a1 1 0 0 1-1.69.9L5 14.8a1 1 0 0 1 .9-1.69l3.54.71 2.2-2.2V9.25a1 1 0 0 1 .3-.7l2.12-2.12L15.2 4.6A1 1 0 0 1 16 3Z"/></svg>';button.addEventListener('click',()=>togglePin(account.auth_id||'',pinned,button));return button}
async function togglePin(authID,pinned,button){if(!authID)return;if(!hasManagementKey()){showNotice(t('error.managementKeyRequired'),true,'error.managementKeyRequired');return}if(button)button.disabled=true;const priority=pinned?0:999;const noticeKey=pinned?'notice.accountUnpinned':'notice.accountPinned';try{await requestManagement('/annotations/account',{method:'PATCH',body:{auth_id:authID,scheduler_priority:priority}});showNotice(t(noticeKey),false,noticeKey);await refreshStatus()}catch(error){showNotice(error.message||String(error),true);if(button)button.disabled=false}}
function renderAccounts(accounts){const queue=document.querySelector('section.queue');if(!queue)return;queue.replaceChildren();const items=Array.isArray(accounts)?accounts:[];if(items.length===0){queue.append(renderEmptyState());return}for(const account of items){const card=node('article','card '+(STATUS.next_auth_id&&STATUS.next_auth_id===account.auth_id?'next':''));card.dataset.authId=account.auth_id||'';const top=node('div','cardTop');const identity=node('div','identity');const titleLine=node('div','titleLine');titleLine.append(node('span','title',account.alias||account.auth_id||''));if(account.group)titleLine.append(node('span','groupPill',account.group));identity.append(titleLine);const sub=node('div','sub');const code=document.createElement('code');code.textContent=account.auth_id||'';sub.append(code);identity.append(sub);if(account.tags&&account.tags.length){const tags=node('div','metaLine');for(const tag of account.tags)tags.append(node('span','chip',tag));identity.append(tags)}top.append(identity,node('span','rank','#'+(account.rank||'')));card.append(top);const badges=node('div','badges');if(STATUS.next_auth_id&&STATUS.next_auth_id===account.auth_id)badges.append(addBadge('下一优先','next'));badges.append(account.available?addBadge('可用','ok'):addBadge(labelUnavailableReason(account.unavailable_reason),'no'));badges.append(addBadge(account.family||'未知类型'),addBadge('CPA 优先级 '+(account.cpa_priority||0)),addBadge('插件优先级 '+(account.scheduler_priority||0)),addBadge('熔断：'+((account.circuit&&account.circuit.label)||'')));if(account.auth_failure)badges.append(addBadge('请重新登录','no'));if(account.refresh_due_reason)badges.append(addBadge(labelDueReason(account.refresh_due_reason)));card.append(badges);const quotaList=node('div','quotaList');if(account.five_hour&&!account.five_hour.missing)quotaList.append(addQuota(account.five_hour,'5 小时额度'));if(account.long_window)quotaList.append(addQuota(account.long_window,account.long_window.label||'长额度'));card.append(quotaList);const kv=node('div','kv');addKV(kv,'缓存时间',account.cache_age||'暂无');addKV(kv,'额度来源',quotaSourceLabel(account.quota_source));if(account.managed_disable)addKV(kv,'托管停用',managedDisableLabel(account.managed_disable));addKV(kv,'熔断计数','失败 '+((account.circuit&&account.circuit.failure_count)||0)+' / 成功 '+((account.circuit&&account.circuit.success_count)||0));addKV(kv,'主动重置',resetCreditSummary(account));if(account.status_note)addKV(kv,'调度状态',account.status_note);if(account.last_error)addKV(kv,'最后错误',account.last_error);if(account.next_retry_text)addKV(kv,'下次重试',account.next_retry_text);card.append(kv);if(account.notes||account.group_notes){const notes=node('div','noteBlock');if(account.notes)notes.append(node('div','',account.notes));if(account.group_notes)notes.append(node('div','',account.group_notes));card.append(notes)}const actions=node('div','cardActions');const refresh=node('button','ghost refreshOne','刷新额度');refresh.type='button';refresh.addEventListener('click',()=>refreshOneQuota(account.auth_id||''));const edit=node('button','secondary openEdit','编辑');edit.type='button';edit.addEventListener('click',()=>openEdit(account.auth_id||''));actions.append(createPinButton(account),refresh,edit);card.append(actions);queue.append(card)}formatLocalTimes()}

async function readJSON(resp){const text=await resp.text();if(!text)return{};try{return JSON.parse(text)}catch{return{error:text}}}
function authHeaders(){const input=document.getElementById('managementKey');const key=(input&&input.value||'').trim();if(!key)throw new Error(t('error.managementKeyRequired'));const name='Author'+'ization';const scheme='Bea'+'rer ';const headers={};headers[name]=key.toLowerCase().startsWith(scheme.toLowerCase())?key:scheme+key;return headers}
async function requestManagement(path,options){const opts=options||{};const headers=authHeaders();let url=MANAGEMENT_BASE+path;if(opts.query){const params=new URLSearchParams(opts.query);url+='?'+params.toString()}const init={method:opts.method||'GET',headers};if(Object.prototype.hasOwnProperty.call(opts,'body')){headers['Content-Type']=opts.contentType||'application/json';init.body=typeof opts.body==='string'?opts.body:JSON.stringify(opts.body)}const resp=await fetch(url,init);const data=await readJSON(resp);const message=data.error||data.message||t('error.requestFailed',{status:resp.status});if(!resp.ok){if(resp.status===401||/invalid management key/i.test(message))showManagementKeyInput();throw new Error(message)}return data}
function fillSettings(){const s=STATUS.settings||{};document.getElementById('handleEnabled').checked=s.handle_enabled!==false;document.getElementById('usageFeedback').checked=s.enable_usage_feedback!==false;document.getElementById('enableResetProbe').checked=s.enable_reset_probe===true;document.getElementById('probeOnProvisionalRoster').checked=s.probe_on_provisional_roster===true;document.getElementById('monthlyMode').value=s.monthly_mode||'expiry_order';document.getElementById('scheduleAcrossPriorities').checked=s.schedule_across_priorities!==false;document.getElementById('enableManagedQuotaDisable').checked=s.enable_managed_quota_disable===true;document.getElementById('lifecycleEventLimit').value=s.lifecycle_event_limit||50;document.getElementById('refreshInterval').value=s.quota_refresh_interval||'30m0s';document.getElementById('staleAfter').value=s.stale_after||'5h0m0s';document.getElementById('refreshActiveWindow').value=s.refresh_active_window||'1h0m0s';document.getElementById('refreshAfterResetDelay').value=s.refresh_after_reset_delay||'1m0s';document.getElementById('refreshRetryDelays').value=s.refresh_retry_delays||'1m0s,5m0s,15m0s';document.getElementById('refreshOnStartup').checked=s.refresh_on_startup===true;document.getElementById('maxConcurrency').value=s.max_refresh_concurrency||1;document.getElementById('circuitFailureThreshold').value=s.circuit_failure_threshold||5;document.getElementById('circuitOpenDuration').value=s.circuit_open_duration||'30m0s';document.getElementById('circuitHalfOpenSuccessThreshold').value=s.circuit_half_open_success_threshold||2;document.getElementById('maxLogEntries').value=s.max_log_entries||200;document.getElementById('logRetention').value=s.log_retention||'24h0m0s';fillRetrySettings()}
async function saveSettings(){try{if(!statusLoaded){await loadStatus();return}await requestManagement('/settings',{method:'PUT',body:collectSettingsPayload()});settingsDirty=false;showNotice(t('notice.settingsSaved'),false,'notice.settingsSaved');await refreshStatus({management:true,fillSettings:true})}catch(error){showNotice(error.message||String(error),true)}}
async function refreshQuota(){try{await requestManagement('/refresh',{method:'POST'});showNotice(t('notice.refreshRequested'),false,'notice.refreshRequested');await refreshStatus({management:true});pollStatus(3,1200)}catch(error){showNotice(error.message||String(error),true)}}
function splitTags(text){return text.split(',').map((item)=>item.trim()).filter(Boolean)}
function openEdit(authID){if(!hasManagementKey()){showNotice(t('error.managementKeyRequired'),true,'error.managementKeyRequired');return}const account=accountsByID.get(authID)||{};editingAuthID=authID;document.getElementById('editAuthID').textContent=authID;document.getElementById('editAlias').value=account.alias||'';document.getElementById('editSchedulerPriority').value=account.scheduler_priority||0;document.getElementById('editNotes').value=account.notes||'';document.getElementById('editGroupID').value=account.group_id||'';document.getElementById('editGroupName').value=account.group||'';document.getElementById('editGroupNotes').value=account.group_notes||'';document.getElementById('editTags').value=(account.tags||[]).join(', ');editDialog.showModal()}
function fillGroupFromID(){const groupID=document.getElementById('editGroupID').value.trim();const group=groupsByID.get(groupID);if(!group)return;if(!document.getElementById('editGroupName').value.trim())document.getElementById('editGroupName').value=group.name||'';if(!document.getElementById('editGroupNotes').value.trim())document.getElementById('editGroupNotes').value=group.notes||''}
async function saveAccountModal(){if(!editingAuthID)return;const groupID=document.getElementById('editGroupID').value.trim();const groupName=document.getElementById('editGroupName').value.trim();const groupNotes=document.getElementById('editGroupNotes').value.trim();const schedulerPriority=document.getElementById('editSchedulerPriority').valueAsNumber;if(!Number.isSafeInteger(schedulerPriority)){showNotice(t('error.schedulerPriorityInteger'),true,'error.schedulerPriorityInteger');return}try{await requestManagement('/annotations/account',{method:'PATCH',body:{auth_id:editingAuthID,alias:document.getElementById('editAlias').value,notes:document.getElementById('editNotes').value,tags:splitTags(document.getElementById('editTags').value),group_id:groupID,scheduler_priority:schedulerPriority}});const existingGroup=groupsByID.get(groupID)||{name:'',notes:''};if(groupID&&(groupName!==existingGroup.name||groupNotes!==existingGroup.notes)){await requestManagement('/annotations/group',{method:'PATCH',body:{id:groupID,name:groupName,notes:groupNotes}});groupsByID.set(groupID,{name:groupName,notes:groupNotes})}showNotice(t('notice.accountSaved'),false,'notice.accountSaved');editDialog.close();await refreshStatus()}catch(error){showNotice(error.message||String(error),true)}}
async function refreshOneQuota(authID){if(!authID)return;try{await requestManagement('/refresh/account',{method:'POST',body:{auth_id:authID}});showNotice(t('notice.refreshOneRequested'),false,'notice.refreshOneRequested');await refreshStatus();pollStatus(3,1200)}catch(error){showNotice(error.message||String(error),true)}}
async function exportConfig(){try{const data=await requestManagement('/export');const blob=new Blob([JSON.stringify(data,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-config.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.configExported'),false,'notice.configExported');await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
async function exportLogs(){try{await refreshStatus({management:true});const payload={plugin_id:STATUS.plugin_id||'codex-quota-scheduler',exported_at:new Date().toISOString(),logs:STATUS.logs||[]};const blob=new Blob([JSON.stringify(payload,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-logs.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.logsExported'),false,'notice.logsExported')}catch(error){showNotice(error.message||String(error),true)}}
async function importConfigFile(file){if(!file)return;try{const text=await file.text();await requestManagement('/import',{method:'POST',body:text});settingsDirty=false;showNotice(t('notice.configImported'),false,'notice.configImported');await refreshStatus({management:true,fillSettings:true})}catch(error){showNotice(error.message||String(error),true)}}
function formatLogTime(value){if(!value)return'';const date=new Date(value);if(Number.isNaN(date.getTime()))return value;return date.toLocaleString(dateLocale(),{hour12:false})}
function resetRemainingText(date){const remaining=date.getTime()-Date.now();if(remaining<=0)return currentLocale==='en'?'expired':'已到期';const minute=60000;const hour=60*minute;const day=24*hour;if(remaining<day){if(remaining<hour){const value=Math.max(1,Math.floor(remaining/minute));return currentLocale==='en'?value+' min':value+'分钟'}const value=Math.floor(remaining/hour);return currentLocale==='en'?value+' hr':value+'小时'}const value=Math.floor(remaining/day);return currentLocale==='en'?value+' days':value+'天'}
function formatQuotaResetLabels(){for(const node of document.querySelectorAll('.quota-reset-label[data-label]')){const label=translateInlineText(node.dataset.label||'额度');node.textContent=currentLocale==='en'?label+' reset: ':label+' 重置：'}}
function formatLocalTimes(){formatQuotaResetLabels();for(const node of document.querySelectorAll('.localTime[data-time]')){const raw=node.dataset.time||'';const date=new Date(raw);if(Number.isNaN(date.getTime()))continue;const local=date.toLocaleString(dateLocale(),{hour12:false});if(node.classList.contains('quotaResetTime')){const remaining=resetRemainingText(date);node.textContent=currentLocale==='en'?local+' ('+remaining+')':local+' （'+remaining+'）'}else node.textContent=local}}
function localizedLogMessage(log){const key='log.'+(log.event||'');const dictionary=TRANSLATIONS[currentLocale]||TRANSLATIONS.en;return dictionary[key]||TRANSLATIONS.en[key]||translateInlineText(log.message||'')}
function renderLogs(logs){const list=document.getElementById('logList');list.replaceChildren();const items=(logs||[]).slice().reverse().slice(0,80);if(items.length===0){const empty=document.createElement('div');empty.className='empty';empty.textContent=t('logs.empty');list.appendChild(empty);return}for(const log of items){const row=document.createElement('div');row.className='logItem '+(log.level||'info');const meta=document.createElement('div');meta.className='logMeta';meta.textContent=[formatLogTime(log.time),log.event].filter(Boolean).join(' · ');const msg=document.createElement('div');msg.className='logMsg';const fields=log.fields?Object.entries(log.fields).map(([key,value])=>key+'='+value).join(currentLocale==='en'?', ':'，'):'';const message=localizedLogMessage(log);msg.textContent=fields?(currentLocale==='en'?message+' ('+fields+')':message+'（'+fields+'）'):message;row.append(meta,msg);list.appendChild(row)}}
async function refreshLogs(){try{await refreshStatus({management:true})}catch(error){showNotice(error.message||String(error),true)}}
localeSelect.addEventListener('change',()=>changeLocale(localeSelect.value));
document.getElementById('managementKey').addEventListener('input',syncRememberedManagementKey);
document.getElementById('managementKey').addEventListener('keydown',(event)=>{if(event.key==='Enter'){syncRememberedManagementKey();loadStatus()}});
document.getElementById('rememberManagementKey').addEventListener('change',syncRememberedManagementKey);
document.getElementById('loadData').addEventListener('click',loadStatus);
document.getElementById('saveSettings').addEventListener('click',saveSettings);
document.getElementById('refreshQuota').addEventListener('click',refreshQuota);
document.getElementById('refreshLogs').addEventListener('click',refreshLogs);
document.getElementById('exportLogs').addEventListener('click',exportLogs);
document.getElementById('exportConfig').addEventListener('click',exportConfig);
document.getElementById('importConfig').addEventListener('click',()=>document.getElementById('importFile').click());
document.getElementById('importFile').addEventListener('change',(event)=>importConfigFile(event.target.files&&event.target.files[0]));
document.getElementById('saveAccount').addEventListener('click',saveAccountModal);
settingsPanel.addEventListener('input',()=>{settingsDirty=true});
settingsPanel.addEventListener('change',()=>{settingsDirty=true});
retryPanel.addEventListener('input',()=>{settingsDirty=true});
retryPanel.addEventListener('change',()=>{settingsDirty=true});
document.getElementById('retryShadow').addEventListener('change',syncRetryModes);
document.getElementById('retryAlways').addEventListener('change',syncRetryModes);
document.getElementById('retryAddRow').addEventListener('click',()=>addRetryChainRow());
document.getElementById('retryReloadCatalog').addEventListener('click',loadRetryCatalog);
document.getElementById('retryPanelWrap').addEventListener('toggle',()=>{const wrap=document.getElementById('retryPanelWrap');if(wrap&&wrap.open&&!retryCatalog.loaded)loadRetryCatalog()});
document.getElementById('retryCheckCPA').addEventListener('click',checkAndFixCPASettings);
document.getElementById('retrySave').addEventListener('click',saveRetrySettings);
document.getElementById('retryRefreshStats').addEventListener('click',refreshRetryStats);
document.getElementById('editGroupID').addEventListener('input',fillGroupFromID);
document.getElementById('editGroupID').addEventListener('blur',fillGroupFromID);
document.getElementById('closeDialog').addEventListener('click',()=>editDialog.close());
document.getElementById('cancelEdit').addEventListener('click',()=>editDialog.close());
for(const button of document.querySelectorAll('.openEdit')){button.addEventListener('click',()=>openEdit(button.dataset.authId||''))}
for(const button of document.querySelectorAll('.refreshOne')){button.addEventListener('click',()=>refreshOneQuota(button.dataset.authId||''))}
for(const button of document.querySelectorAll('.pinAccount')){button.addEventListener('click',()=>togglePin(button.dataset.authId||'',button.dataset.pinned==='true',button))}
const rememberedManagementKey=restoreRememberedManagementKey();
watchParentLocale();
rebuildDerivedState();
fillSettings();
renderMetrics();
renderRosterLifecycle();
renderAccounts(STATUS.accounts||[]);
formatLocalTimes();
applyLocale();
updateProtectedVisibility();
if(statusLoaded)startStatusPolling();
else if(rememberedManagementKey)loadStatus();
if(statusLoaded)refreshRetryStats();
</script>
</body>
</html>`))

func jsonForTemplate(v any) template.JS {
	raw, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(raw)
}
