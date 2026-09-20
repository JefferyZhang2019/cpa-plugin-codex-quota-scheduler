package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, err := DecodeConfig(nil)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if !cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = false, want true")
	}
	if cfg.MonthlyMode != MonthlyModeExpiryOrder {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModeExpiryOrder)
	}
	if cfg.Fallback != FallbackFillFirst {
		t.Fatalf("Fallback = %q, want %q", cfg.Fallback, FallbackFillFirst)
	}
	if cfg.QuotaRefreshInterval != 30*time.Minute {
		t.Fatalf("QuotaRefreshInterval = %s, want 30m", cfg.QuotaRefreshInterval)
	}
	if cfg.StaleAfter != 5*time.Hour {
		t.Fatalf("StaleAfter = %s, want 5h", cfg.StaleAfter)
	}
	if cfg.MaxRefreshConcurrency != 1 {
		t.Fatalf("MaxRefreshConcurrency = %d, want 1", cfg.MaxRefreshConcurrency)
	}
	if cfg.MaxLogEntries != 200 {
		t.Fatalf("MaxLogEntries = %d, want 200", cfg.MaxLogEntries)
	}
	if cfg.LogRetention != 24*time.Hour {
		t.Fatalf("LogRetention = %s, want 24h", cfg.LogRetention)
	}
}

func TestProbeOnProvisionalRosterIsExplicitRiskOption(t *testing.T) {
	if DefaultConfig().ProbeOnProvisionalRoster {
		t.Fatal("risk option must default off")
	}
	cfg, err := DecodeConfig([]byte("probe_on_provisional_roster: true\n"))
	if err != nil || !cfg.ProbeOnProvisionalRoster {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}

func TestPluginRegistrationUsesV031SourceVersion(t *testing.T) {
	if got := PluginRegistration().Metadata.Version; got != "0.3.1" {
		t.Fatalf("plugin registration version = %q, want 0.3.1", got)
	}
}

func TestReleaseVersionMetadataConsistent(t *testing.T) {
	for _, check := range []struct {
		path string
		want []string
	}{
		{"config.go", []string{`var pluginVersion = "0.3.1"`}},
		{"Makefile", []string{"VERSION ?= 0.3.1"}},
		{"README.md", []string{"## v0.3.1 Highlights", "make package VERSION=0.3.1", "make checksums VERSION=0.3.1", "git tag -a v0.3.1 -m \"v0.3.1\"", "git push origin v0.3.1"}},
		{"README.zh-CN.md", []string{"## v0.3.1 主要更新", "make package VERSION=0.3.1", "make checksums VERSION=0.3.1", "git tag -a v0.3.1 -m \"v0.3.1\"", "git push origin v0.3.1"}},
	} {
		contents, err := os.ReadFile(check.path)
		if err != nil {
			t.Fatalf("read %s: %v", check.path, err)
		}
		for _, want := range check.want {
			if !strings.Contains(string(contents), want) {
				t.Errorf("%s missing %q", check.path, want)
			}
		}
	}
}

func TestDefaultConfigAdaptiveRefreshDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshActiveWindow != time.Hour {
		t.Fatalf("RefreshActiveWindow = %s, want 1h", cfg.RefreshActiveWindow)
	}
	if cfg.RefreshAfterResetDelay != time.Minute {
		t.Fatalf("RefreshAfterResetDelay = %s, want 1m", cfg.RefreshAfterResetDelay)
	}
	if got := cfg.RefreshRetryDelays; !reflect.DeepEqual(got, []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}) {
		t.Fatalf("RefreshRetryDelays = %#v, want 1m,5m,15m", got)
	}
	if cfg.RefreshOnStartup {
		t.Fatal("RefreshOnStartup = true, want false")
	}
	if cfg.CircuitFailureThreshold != 5 {
		t.Fatalf("CircuitFailureThreshold = %d, want 5", cfg.CircuitFailureThreshold)
	}
	if cfg.CircuitOpenDuration != 30*time.Minute {
		t.Fatalf("CircuitOpenDuration = %s, want 30m", cfg.CircuitOpenDuration)
	}
	if cfg.CircuitHalfOpenSuccessThreshold != 2 {
		t.Fatalf("CircuitHalfOpenSuccessThreshold = %d, want 2", cfg.CircuitHalfOpenSuccessThreshold)
	}
	if cfg.MaxLogEntries != 200 {
		t.Fatalf("MaxLogEntries = %d, want 200", cfg.MaxLogEntries)
	}
}

func TestStaleAfterRemainsClassificationOnlyConfig(t *testing.T) {
	cfg, err := DecodeConfig([]byte("quota_refresh_interval: 30m\nstale_after: 31m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StaleAfter != 31*time.Minute {
		t.Fatalf("StaleAfter = %s", cfg.StaleAfter)
	}
}

func TestDefaultConfigDisablesResetProbe(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnableResetProbe {
		t.Fatal("EnableResetProbe = true, want false")
	}
}

func TestDecodeConfigEnableResetProbe(t *testing.T) {
	cfg, err := DecodeConfig([]byte("enable_reset_probe: true\n"))
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if !cfg.EnableResetProbe {
		t.Fatal("EnableResetProbe = false, want true")
	}
}

func TestDecodeConfigAdaptiveRefreshOverrides(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`
refresh_active_window: 2h
refresh_after_reset_delay: 30s
refresh_retry_delays: 30s, 2m, 10m
refresh_on_startup: true
`))
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if cfg.RefreshActiveWindow != 2*time.Hour {
		t.Fatalf("RefreshActiveWindow = %s, want 2h", cfg.RefreshActiveWindow)
	}
	if cfg.RefreshAfterResetDelay != 30*time.Second {
		t.Fatalf("RefreshAfterResetDelay = %s, want 30s", cfg.RefreshAfterResetDelay)
	}
	if got := cfg.RefreshRetryDelays; !reflect.DeepEqual(got, []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}) {
		t.Fatalf("RefreshRetryDelays = %#v", got)
	}
	if !cfg.RefreshOnStartup {
		t.Fatal("RefreshOnStartup = false, want true")
	}
}

func TestDecodeConfigRejectsInvalidAdaptiveRefresh(t *testing.T) {
	tests := []string{
		"refresh_active_window: 0s\n",
		"refresh_after_reset_delay: -1s\n",
		"refresh_retry_delays: 1m,0s\n",
		"refresh_retry_delays: ', ,'\n",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(raw)); err == nil {
				t.Fatalf("DecodeConfig accepted invalid adaptive refresh config %q", raw)
			}
		})
	}
}

func TestDecodeConfigOverrides(t *testing.T) {
	raw := []byte(`
handle_enabled: false
quota_refresh_interval: 30s
stale_after: 2m
monthly_mode: priority
fallback: fill-first
enable_usage_feedback: false
max_refresh_concurrency: 8
quota_endpoint: https://chatgpt.com/backend-api/wham/usage
max_log_entries: 50
log_retention: 2h
`)
	cfg, err := DecodeConfig(raw)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = true, want false")
	}
	if cfg.MonthlyMode != MonthlyModePriority {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModePriority)
	}
	if cfg.QuotaRefreshInterval != 30*time.Second {
		t.Fatalf("QuotaRefreshInterval = %s, want 30s", cfg.QuotaRefreshInterval)
	}
	if cfg.StaleAfter != 2*time.Minute {
		t.Fatalf("StaleAfter = %s, want 2m", cfg.StaleAfter)
	}
	if cfg.Fallback != FallbackFillFirst {
		t.Fatalf("Fallback = %q, want %q", cfg.Fallback, FallbackFillFirst)
	}
	if cfg.EnableUsageFeedback {
		t.Fatalf("EnableUsageFeedback = true, want false")
	}
	if cfg.MaxRefreshConcurrency != 8 {
		t.Fatalf("MaxRefreshConcurrency = %d, want 8", cfg.MaxRefreshConcurrency)
	}
	if cfg.QuotaEndpoint != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("QuotaEndpoint = %q, want %q", cfg.QuotaEndpoint, "https://chatgpt.com/backend-api/wham/usage")
	}
	if cfg.MaxLogEntries != 50 {
		t.Fatalf("MaxLogEntries = %d, want 50", cfg.MaxLogEntries)
	}
	if cfg.LogRetention != 2*time.Hour {
		t.Fatalf("LogRetention = %s, want 2h", cfg.LogRetention)
	}
}

func TestDecodeConfigRejectsInvalidMonthlyMode(t *testing.T) {
	_, err := DecodeConfig([]byte("monthly_mode: unsupported\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted invalid monthly mode")
	}
}

func TestDecodeConfigRejectsNonPositiveQuotaRefreshInterval(t *testing.T) {
	_, err := DecodeConfig([]byte("quota_refresh_interval: 0s\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive quota refresh interval")
	}
}

func TestDecodeConfigRejectsNonPositiveStaleAfter(t *testing.T) {
	_, err := DecodeConfig([]byte("stale_after: -1s\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive stale after")
	}
}

func TestDecodeConfigRejectsNonPositiveMaxRefreshConcurrency(t *testing.T) {
	_, err := DecodeConfig([]byte("max_refresh_concurrency: 0\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive max refresh concurrency")
	}
}

func TestDecodeConfigRejectsInvalidLogRetention(t *testing.T) {
	if _, err := DecodeConfig([]byte("log_retention: 0s\n")); err == nil {
		t.Fatalf("DecodeConfig accepted non-positive log retention")
	}
	if _, err := DecodeConfig([]byte("max_log_entries: 0\n")); err == nil {
		t.Fatalf("DecodeConfig accepted non-positive max log entries")
	}
}

func TestDecodeConfigRejectsNonChatGPTQuotaEndpoint(t *testing.T) {
	_, err := DecodeConfig([]byte("quota_endpoint: https://example.test/usage\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-ChatGPT quota endpoint")
	}
}

func TestPluginRegistrationDeclaresCapabilitiesAndFields(t *testing.T) {
	reg := PluginRegistration()
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name == "" || reg.Metadata.Version == "" || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata missing required CPA fields: %#v", reg.Metadata)
	}
	if len(reg.Metadata.ConfigFields) != 0 {
		t.Fatalf("ConfigFields len = %d, want 0; fields=%#v", len(reg.Metadata.ConfigFields), reg.Metadata.ConfigFields)
	}
	if !reg.Capabilities.Scheduler || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v, want scheduler, usage_plugin, management_api", reg.Capabilities)
	}
}
