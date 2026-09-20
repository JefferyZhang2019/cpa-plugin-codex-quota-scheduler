package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	PluginID = "codex-quota-scheduler"

	chatGPTQuotaEndpoint = "https://chatgpt.com/backend-api/wham/usage"

	MonthlyModePriority    MonthlyMode = "priority"
	MonthlyModeExpiryOrder MonthlyMode = "expiry_order"

	FallbackFillFirst FallbackMode = "fill-first"
)

var pluginVersion = "0.3.1"

type MonthlyMode string

type FallbackMode string

type Config struct {
	HandleEnabled        bool
	QuotaRefreshInterval time.Duration
	// StaleAfter classifies cache age and permits pick-time recovery. It does
	// not own a normal-refresh deadline.
	StaleAfter                      time.Duration
	MonthlyMode                     MonthlyMode
	Fallback                        FallbackMode
	EnableUsageFeedback             bool
	EnableResetProbe                bool
	ProbeOnProvisionalRoster        bool
	MaxRefreshConcurrency           int
	QuotaEndpoint                   string
	RefreshActiveWindow             time.Duration
	RefreshAfterResetDelay          time.Duration
	RefreshRetryDelays              []time.Duration
	RefreshOnStartup                bool
	CircuitFailureThreshold         int
	CircuitOpenDuration             time.Duration
	CircuitHalfOpenSuccessThreshold int
	MaxLogEntries                   int
	LogRetention                    time.Duration
	// ScheduleAcrossPriorities lets the scheduler fall through to lower CPA
	// priority tiers when every account in the higher tiers is unavailable,
	// instead of delegating to the built-in scheduler. Higher tiers still win
	// whenever they have a selectable account. Hosts that do not send
	// across-priority candidates keep single-tier behavior regardless.
	ScheduleAcrossPriorities bool
	// EnableManagedQuotaDisable opts in to disabling Codex auths at the CPA
	// level after a confirmed usage_limit_reached and re-enabling them only
	// from fresh usable quota. Default false: the feature writes host auth
	// files.
	EnableManagedQuotaDisable bool
	LifecycleEventLimit       int

	// Retry chain. RetryEnabled is the kill switch: when it is false every
	// request keeps today's behavior. RetryChain is empty by default, which is
	// also inert, so shipping the feature cannot change routing by itself.
	RetryEnabled        bool            `json:"retry_enabled"`
	RetryShadow         bool            `json:"retry_shadow"`
	RetryAlways         bool            `json:"retry_always"`
	RetryMaxAttempts    int             `json:"retry_max_attempts"`
	RetryStallTimeout   time.Duration   `json:"retry_stall_timeout"`
	RetryHoldTimeout    time.Duration   `json:"retry_hold_timeout"`
	RetryChainDeadline  time.Duration   `json:"retry_chain_deadline"`
	RetryMaxFrames      int             `json:"retry_max_frames"`
	RetryMaxBytes       int64           `json:"retry_max_bytes"`
	RetryStripReasoning bool            `json:"retry_strip_reasoning"`
	RetryChain          []RetryChainRow `json:"retry_chain,omitempty"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler                 bool `json:"scheduler"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin,omitempty"`
	QuotaProvider             bool `json:"quota_provider,omitempty"`
	StreamChunkInterceptor    bool `json:"response_stream_interceptor,omitempty"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`

	// The retry chain is implemented as a model router plus a plugin executor.
	// The router claims only models that have a configured chain, and the
	// executor re-issues the request through the host so the built-in
	// auth/scheduler path still selects the account.
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type rawConfig struct {
	HandleEnabled                   *bool  `yaml:"handle_enabled"`
	QuotaRefreshInterval            string `yaml:"quota_refresh_interval"`
	StaleAfter                      string `yaml:"stale_after"`
	MonthlyMode                     string `yaml:"monthly_mode"`
	Fallback                        string `yaml:"fallback"`
	EnableUsageFeedback             *bool  `yaml:"enable_usage_feedback"`
	EnableResetProbe                *bool  `yaml:"enable_reset_probe"`
	ProbeOnProvisionalRoster        *bool  `yaml:"probe_on_provisional_roster"`
	MaxRefreshConcurrency           *int   `yaml:"max_refresh_concurrency"`
	QuotaEndpoint                   string `yaml:"quota_endpoint"`
	RefreshActiveWindow             string `yaml:"refresh_active_window"`
	RefreshAfterResetDelay          string `yaml:"refresh_after_reset_delay"`
	RefreshRetryDelays              string `yaml:"refresh_retry_delays"`
	RefreshOnStartup                *bool  `yaml:"refresh_on_startup"`
	CircuitFailureThreshold         *int   `yaml:"circuit_failure_threshold"`
	CircuitOpenDuration             string `yaml:"circuit_open_duration"`
	CircuitHalfOpenSuccessThreshold *int   `yaml:"circuit_half_open_success_threshold"`
	MaxLogEntries                   *int   `yaml:"max_log_entries"`
	LogRetention                    string `yaml:"log_retention"`
	ScheduleAcrossPriorities        *bool  `yaml:"schedule_across_priorities"`
	EnableManagedQuotaDisable       *bool  `yaml:"enable_managed_quota_disable"`
	LifecycleEventLimit             *int   `yaml:"lifecycle_event_limit"`

	RetryEnabled        *bool           `yaml:"retry_enabled"`
	RetryShadow         *bool           `yaml:"retry_shadow"`
	RetryAlways         *bool           `yaml:"retry_always"`
	RetryMaxAttempts    *int            `yaml:"retry_max_attempts"`
	RetryStallTimeout   string          `yaml:"retry_stall_timeout"`
	RetryHoldTimeout    string          `yaml:"retry_hold_timeout"`
	RetryChainDeadline  string          `yaml:"retry_chain_deadline"`
	RetryMaxFrames      *int            `yaml:"retry_max_frames"`
	RetryMaxBytes       string          `yaml:"retry_max_bytes"`
	RetryStripReasoning *bool           `yaml:"retry_strip_reasoning"`
	RetryChain          []RetryChainRow `yaml:"retry_chain"`
}

func DefaultConfig() Config {
	return Config{
		HandleEnabled:                   true,
		QuotaRefreshInterval:            30 * time.Minute,
		StaleAfter:                      5 * time.Hour,
		MonthlyMode:                     MonthlyModeExpiryOrder,
		Fallback:                        FallbackFillFirst,
		EnableUsageFeedback:             true,
		EnableResetProbe:                false,
		MaxRefreshConcurrency:           1,
		QuotaEndpoint:                   chatGPTQuotaEndpoint,
		RefreshActiveWindow:             time.Hour,
		RefreshAfterResetDelay:          time.Minute,
		RefreshRetryDelays:              []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		RefreshOnStartup:                false,
		CircuitFailureThreshold:         5,
		CircuitOpenDuration:             30 * time.Minute,
		CircuitHalfOpenSuccessThreshold: 2,
		MaxLogEntries:                   200,
		LogRetention:                    24 * time.Hour,
		ScheduleAcrossPriorities:        true,
		EnableManagedQuotaDisable:       false,
		LifecycleEventLimit:             50,

		RetryEnabled:        false,
		RetryShadow:         false,
		RetryAlways:         false,
		RetryMaxAttempts:    defaultRetryMaxAttempts,
		RetryStallTimeout:   defaultRetryStallTimeout,
		RetryHoldTimeout:    defaultRetryHoldTimeout,
		RetryChainDeadline:  defaultRetryChainDeadline,
		RetryMaxFrames:      defaultRetryMaxFrames,
		RetryMaxBytes:       defaultRetryMaxBytes,
		RetryStripReasoning: true,
	}
}

func NormalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.QuotaRefreshInterval <= 0 {
		cfg.QuotaRefreshInterval = defaults.QuotaRefreshInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaults.StaleAfter
	}
	if cfg.MonthlyMode == "" {
		cfg.MonthlyMode = defaults.MonthlyMode
	}
	if cfg.Fallback == "" {
		cfg.Fallback = defaults.Fallback
	}
	if cfg.MaxRefreshConcurrency <= 0 {
		cfg.MaxRefreshConcurrency = defaults.MaxRefreshConcurrency
	}
	if strings.TrimSpace(cfg.QuotaEndpoint) == "" {
		cfg.QuotaEndpoint = defaults.QuotaEndpoint
	}
	if cfg.RefreshActiveWindow <= 0 {
		cfg.RefreshActiveWindow = defaults.RefreshActiveWindow
	}
	if cfg.RefreshAfterResetDelay <= 0 {
		cfg.RefreshAfterResetDelay = defaults.RefreshAfterResetDelay
	}
	cfg.RefreshRetryDelays = normalizeRetryDelays(cfg.RefreshRetryDelays, defaults.RefreshRetryDelays)
	if cfg.CircuitFailureThreshold <= 0 {
		cfg.CircuitFailureThreshold = defaults.CircuitFailureThreshold
	}
	if cfg.CircuitOpenDuration <= 0 {
		cfg.CircuitOpenDuration = defaults.CircuitOpenDuration
	}
	if cfg.CircuitHalfOpenSuccessThreshold <= 0 {
		cfg.CircuitHalfOpenSuccessThreshold = defaults.CircuitHalfOpenSuccessThreshold
	}
	if cfg.MaxLogEntries <= 0 {
		cfg.MaxLogEntries = defaults.MaxLogEntries
	}
	if cfg.LogRetention <= 0 {
		cfg.LogRetention = defaults.LogRetention
	}
	cfg.RetryChain = NormalizeRetryChain(cfg.RetryChain)
	if cfg.RetryMaxAttempts <= 0 {
		cfg.RetryMaxAttempts = defaults.RetryMaxAttempts
	}
	if cfg.RetryMaxAttempts > maxRetryMaxAttempts {
		cfg.RetryMaxAttempts = maxRetryMaxAttempts
	}
	if cfg.RetryStallTimeout <= 0 {
		cfg.RetryStallTimeout = defaults.RetryStallTimeout
	}
	if cfg.RetryHoldTimeout <= 0 {
		cfg.RetryHoldTimeout = defaults.RetryHoldTimeout
	}
	if cfg.RetryChainDeadline <= 0 {
		cfg.RetryChainDeadline = defaults.RetryChainDeadline
	}
	if cfg.RetryMaxFrames <= 0 {
		cfg.RetryMaxFrames = defaults.RetryMaxFrames
	}
	if cfg.RetryMaxBytes <= 0 {
		cfg.RetryMaxBytes = defaults.RetryMaxBytes
	}
	return cfg
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}

	var decoded rawConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return Config{}, err
	}
	if decoded.HandleEnabled != nil {
		cfg.HandleEnabled = *decoded.HandleEnabled
	}
	if decoded.QuotaRefreshInterval != "" {
		d, err := time.ParseDuration(decoded.QuotaRefreshInterval)
		if err != nil {
			return Config{}, fmt.Errorf("quota_refresh_interval: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("quota_refresh_interval must be positive")
		}
		cfg.QuotaRefreshInterval = d
	}
	if decoded.StaleAfter != "" {
		d, err := time.ParseDuration(decoded.StaleAfter)
		if err != nil {
			return Config{}, fmt.Errorf("stale_after: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("stale_after must be positive")
		}
		cfg.StaleAfter = d
	}
	if decoded.MonthlyMode != "" {
		cfg.MonthlyMode = MonthlyMode(decoded.MonthlyMode)
	}
	if cfg.MonthlyMode != MonthlyModePriority && cfg.MonthlyMode != MonthlyModeExpiryOrder {
		return Config{}, fmt.Errorf("monthly_mode must be %q or %q", MonthlyModePriority, MonthlyModeExpiryOrder)
	}
	if decoded.Fallback != "" {
		cfg.Fallback = FallbackMode(decoded.Fallback)
	}
	if cfg.Fallback != "" && cfg.Fallback != FallbackFillFirst {
		return Config{}, fmt.Errorf("fallback must be empty or %q", FallbackFillFirst)
	}
	if decoded.EnableUsageFeedback != nil {
		cfg.EnableUsageFeedback = *decoded.EnableUsageFeedback
	}
	if decoded.EnableResetProbe != nil {
		cfg.EnableResetProbe = *decoded.EnableResetProbe
	}
	if decoded.ProbeOnProvisionalRoster != nil {
		cfg.ProbeOnProvisionalRoster = *decoded.ProbeOnProvisionalRoster
	}
	if decoded.MaxRefreshConcurrency != nil {
		if *decoded.MaxRefreshConcurrency <= 0 {
			return Config{}, fmt.Errorf("max_refresh_concurrency must be positive")
		}
		cfg.MaxRefreshConcurrency = *decoded.MaxRefreshConcurrency
	}
	if decoded.QuotaEndpoint != "" {
		endpoint, err := validateQuotaEndpoint(decoded.QuotaEndpoint)
		if err != nil {
			return Config{}, err
		}
		cfg.QuotaEndpoint = endpoint
	}
	if decoded.RefreshActiveWindow != "" {
		d, err := time.ParseDuration(decoded.RefreshActiveWindow)
		if err != nil {
			return Config{}, fmt.Errorf("refresh_active_window: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("refresh_active_window must be positive")
		}
		cfg.RefreshActiveWindow = d
	}
	if decoded.RefreshAfterResetDelay != "" {
		d, err := time.ParseDuration(decoded.RefreshAfterResetDelay)
		if err != nil {
			return Config{}, fmt.Errorf("refresh_after_reset_delay: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("refresh_after_reset_delay must be positive")
		}
		cfg.RefreshAfterResetDelay = d
	}
	if decoded.RefreshRetryDelays != "" {
		delays, err := parseDurationList(decoded.RefreshRetryDelays)
		if err != nil {
			return Config{}, fmt.Errorf("refresh_retry_delays: %w", err)
		}
		cfg.RefreshRetryDelays = delays
	}
	if decoded.RefreshOnStartup != nil {
		cfg.RefreshOnStartup = *decoded.RefreshOnStartup
	}
	if decoded.CircuitFailureThreshold != nil {
		if *decoded.CircuitFailureThreshold <= 0 {
			return Config{}, fmt.Errorf("circuit_failure_threshold must be positive")
		}
		cfg.CircuitFailureThreshold = *decoded.CircuitFailureThreshold
	}
	if decoded.CircuitOpenDuration != "" {
		d, err := time.ParseDuration(decoded.CircuitOpenDuration)
		if err != nil {
			return Config{}, fmt.Errorf("circuit_open_duration: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("circuit_open_duration must be positive")
		}
		cfg.CircuitOpenDuration = d
	}
	if decoded.CircuitHalfOpenSuccessThreshold != nil {
		if *decoded.CircuitHalfOpenSuccessThreshold <= 0 {
			return Config{}, fmt.Errorf("circuit_half_open_success_threshold must be positive")
		}
		cfg.CircuitHalfOpenSuccessThreshold = *decoded.CircuitHalfOpenSuccessThreshold
	}
	if decoded.MaxLogEntries != nil {
		if *decoded.MaxLogEntries <= 0 {
			return Config{}, fmt.Errorf("max_log_entries must be positive")
		}
		cfg.MaxLogEntries = *decoded.MaxLogEntries
	}
	if decoded.LogRetention != "" {
		d, err := time.ParseDuration(decoded.LogRetention)
		if err != nil {
			return Config{}, fmt.Errorf("log_retention: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("log_retention must be positive")
		}
		cfg.LogRetention = d
	}
	if decoded.ScheduleAcrossPriorities != nil {
		cfg.ScheduleAcrossPriorities = *decoded.ScheduleAcrossPriorities
	}
	if decoded.EnableManagedQuotaDisable != nil {
		cfg.EnableManagedQuotaDisable = *decoded.EnableManagedQuotaDisable
	}
	if decoded.LifecycleEventLimit != nil {
		if *decoded.LifecycleEventLimit <= 0 {
			return Config{}, fmt.Errorf("lifecycle_event_limit must be positive")
		}
		cfg.LifecycleEventLimit = *decoded.LifecycleEventLimit
	}
	if decoded.RetryEnabled != nil {
		cfg.RetryEnabled = *decoded.RetryEnabled
	}
	if decoded.RetryShadow != nil {
		cfg.RetryShadow = *decoded.RetryShadow
	}
	if decoded.RetryAlways != nil {
		cfg.RetryAlways = *decoded.RetryAlways
		if cfg.RetryAlways {
			cfg.RetryShadow = false
		}
	}
	if decoded.RetryMaxAttempts != nil {
		if *decoded.RetryMaxAttempts <= 0 || *decoded.RetryMaxAttempts > maxRetryMaxAttempts {
			return Config{}, fmt.Errorf("retry_max_attempts must be between 1 and %d", maxRetryMaxAttempts)
		}
		cfg.RetryMaxAttempts = *decoded.RetryMaxAttempts
	}
	for _, field := range []struct {
		name  string
		raw   string
		apply func(time.Duration)
	}{
		{"retry_stall_timeout", decoded.RetryStallTimeout, func(d time.Duration) { cfg.RetryStallTimeout = d }},
		{"retry_hold_timeout", decoded.RetryHoldTimeout, func(d time.Duration) { cfg.RetryHoldTimeout = d }},
		{"retry_chain_deadline", decoded.RetryChainDeadline, func(d time.Duration) { cfg.RetryChainDeadline = d }},
	} {
		if field.raw == "" {
			continue
		}
		d, err := time.ParseDuration(field.raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", field.name, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%s must be positive", field.name)
		}
		field.apply(d)
	}
	if decoded.RetryMaxFrames != nil {
		if *decoded.RetryMaxFrames <= 0 {
			return Config{}, fmt.Errorf("retry_max_frames must be positive")
		}
		cfg.RetryMaxFrames = *decoded.RetryMaxFrames
	}
	if decoded.RetryMaxBytes != "" {
		n, err := parseByteSize(decoded.RetryMaxBytes)
		if err != nil {
			return Config{}, fmt.Errorf("retry_max_bytes: %w", err)
		}
		cfg.RetryMaxBytes = n
	}
	if decoded.RetryStripReasoning != nil {
		cfg.RetryStripReasoning = *decoded.RetryStripReasoning
	}
	if decoded.RetryChain != nil {
		chain := NormalizeRetryChain(decoded.RetryChain)
		if err := validateRetryChain(chain); err != nil {
			return Config{}, err
		}
		cfg.RetryChain = chain
	}
	return NormalizeConfig(cfg), nil
}

// parseByteSize accepts "8388608" or a suffixed size such as "8MB" or "1.5GiB".
func parseByteSize(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("size must not be empty")
	}
	multiplier := int64(1)
	upper := strings.ToUpper(trimmed)
	// KB/MB/GB are binary here so a value the settings page renders round-trips
	// through parse unchanged. formatByteSize writes binary magnitudes under
	// those suffixes, and a decimal reading would shrink the budget on every
	// save.
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"KIB", 1024},
		{"MIB", 1024 * 1024},
		{"GIB", 1024 * 1024 * 1024},
		{"KB", 1024},
		{"MB", 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, unit.suffix) {
			multiplier = unit.factor
			trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(unit.suffix)])
			break
		}
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, err
	}
	size := int64(value * float64(multiplier))
	if size <= 0 {
		return 0, fmt.Errorf("size must be positive")
	}
	return size, nil
}

func normalizeRetryDelays(values, fallback []time.Duration) []time.Duration {
	normalized := make([]time.Duration, 0, len(values))
	for _, value := range values {
		if value > 0 {
			normalized = append(normalized, value)
		}
	}
	if len(normalized) == 0 {
		return append([]time.Duration(nil), fallback...)
	}
	return normalized
}

func parseDurationList(raw string) ([]time.Duration, error) {
	parts := strings.Split(raw, ",")
	delays := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("duration must be positive")
		}
		delays = append(delays, d)
	}
	if len(delays) == 0 {
		return nil, fmt.Errorf("at least one duration is required")
	}
	return delays, nil
}

func validateQuotaEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", nil
	}
	if endpoint != chatGPTQuotaEndpoint {
		return "", fmt.Errorf("quota_endpoint must be %s", chatGPTQuotaEndpoint)
	}
	return endpoint, nil
}

func PluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginID,
			Version:          pluginVersion,
			Author:           "Jeffery",
			GitHubRepository: "https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
		},
		Capabilities: registrationCapabilities{
			Scheduler:                 true,
			UsagePlugin:               true,
			ManagementAPI:             true,
			RequestLifecyclePlugin:    true,
			QuotaProvider:             true,
			StreamChunkInterceptor:    true,
			SchedulerAcrossPriorities: true,

			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    "static",
			ExecutorInputFormats:  append([]string(nil), retryChainInputFormats...),
			ExecutorOutputFormats: append([]string(nil), retryChainInputFormats...),
		},
	}
}
