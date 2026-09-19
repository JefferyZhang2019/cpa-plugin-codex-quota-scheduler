package main

import (
	"fmt"
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

var pluginVersion = "0.2.2"

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
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler              bool `json:"scheduler"`
	UsagePlugin            bool `json:"usage_plugin"`
	ManagementAPI          bool `json:"management_api"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin,omitempty"`
	QuotaProvider          bool `json:"quota_provider,omitempty"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor,omitempty"`
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
	return NormalizeConfig(cfg), nil
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
			Scheduler:              true,
			UsagePlugin:            true,
			ManagementAPI:          true,
			RequestLifecyclePlugin: true,
			QuotaProvider:          true,
			StreamChunkInterceptor: true,
		},
	}
}
