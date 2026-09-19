package main

import (
	"strings"
	"testing"
	"time"
)

func retryTestChainConfig() Config {
	cfg := DefaultConfig()
	cfg.RetryEnabled = true
	cfg.RetryMaxAttempts = 4
	cfg.RetryChain = []RetryChainRow{
		{
			Model: "gpt-5.6-sol",
			Fallbacks: []RetryTarget{
				{Model: "gpt-5.6-luna"},
				{Provider: "codex", Model: "gpt-5.5"},
			},
		},
		{
			Model:     "claude-sonnet-5",
			Fallbacks: []RetryTarget{{Provider: "openai-compatible-zed2api", Model: "kimi-k2"}},
		},
	}
	return cfg
}

func TestRetryPlanPreservesConfiguredOrder(t *testing.T) {
	cfg := retryTestChainConfig()
	plan, ok := cfg.RetryPlanFor("gpt-5.6-sol")
	if !ok {
		t.Fatal("RetryPlanFor rejected a configured model")
	}
	if plan.Key != "gpt-5.6-sol" || plan.Requested.Model != "gpt-5.6-sol" {
		t.Fatalf("plan key/requested = %q/%q", plan.Key, plan.Requested.Model)
	}
	if plan.TargetCount() != 3 {
		t.Fatalf("TargetCount = %d, want 3", plan.TargetCount())
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-luna", "codex/gpt-5.5"}
	for index, expected := range want {
		target, okTarget := plan.Target(index)
		if !okTarget {
			t.Fatalf("Target(%d) missing", index)
		}
		if got := FormatRetryTarget(target); got != expected {
			t.Fatalf("Target(%d) = %q, want %q", index, got, expected)
		}
	}
	if _, okTarget := plan.Target(3); okTarget {
		t.Fatal("Target(3) returned an attempt beyond the plan")
	}
	remaining := plan.Remaining(1)
	if len(remaining) != 1 || FormatRetryTarget(remaining[0]) != "codex/gpt-5.5" {
		t.Fatalf("Remaining(1) = %v", remaining)
	}
	if len(plan.Remaining(2)) != 0 {
		t.Fatalf("Remaining(2) = %v, want empty", plan.Remaining(2))
	}
}

func TestRetryPlanCapsAttemptsAtConfiguredBudget(t *testing.T) {
	cfg := retryTestChainConfig()
	cfg.RetryMaxAttempts = 2
	plan, ok := cfg.RetryPlanFor("gpt-5.6-sol")
	if !ok {
		t.Fatal("RetryPlanFor rejected a configured model")
	}
	if plan.TargetCount() != 2 {
		t.Fatalf("TargetCount = %d, want 2 (first plus one retry)", plan.TargetCount())
	}
	last, okTarget := plan.Target(1)
	if !okTarget || FormatRetryTarget(last) != "gpt-5.6-luna" {
		t.Fatalf("second attempt = %v/%v", last, okTarget)
	}
	cfg.RetryMaxAttempts = 1
	plan, _ = cfg.RetryPlanFor("gpt-5.6-sol")
	if plan.TargetCount() != 1 {
		t.Fatalf("TargetCount with budget 1 = %d, want 1", plan.TargetCount())
	}
}

func TestRetryPlanMissLeavesRoutingUntouched(t *testing.T) {
	cfg := retryTestChainConfig()
	if _, ok := cfg.RetryPlanFor("gpt-5.5"); ok {
		t.Fatal("RetryPlanFor claimed a model with no chain row")
	}
	if _, ok := cfg.RetryPlanFor(""); ok {
		t.Fatal("RetryPlanFor claimed an empty model")
	}
	cfg.RetryEnabled = false
	if _, ok := cfg.RetryPlanFor("gpt-5.6-sol"); !ok {
		t.Fatal("RetryPlanFor must still resolve a configured row when the chain is idle")
	}
	if cfg.RetryActive() {
		t.Fatal("RetryActive must be false while the chain is disabled")
	}
	cfg.RetryEnabled = true
	cfg.RetryChain = nil
	if cfg.RetryActive() {
		t.Fatal("RetryActive must be false for an empty chain")
	}
}

func TestRetryPlanMatchesModelCaseInsensitively(t *testing.T) {
	cfg := retryTestChainConfig()
	plan, ok := cfg.RetryPlanFor("GPT-5.6-SOL")
	if !ok {
		t.Fatal("case-insensitive lookup failed")
	}
	if plan.Key != "gpt-5.6-sol" {
		t.Fatalf("plan key = %q, want the configured spelling", plan.Key)
	}
}

func TestValidateRetryChainRejectsAmbiguousChains(t *testing.T) {
	cases := map[string][]RetryChainRow{
		"duplicate key": {
			{Model: "a", Fallbacks: []RetryTarget{{Model: "b"}}},
			{Model: "A", Fallbacks: []RetryTarget{{Model: "c"}}},
		},
		"missing fallbacks": {{Model: "a"}},
		"self fallback":     {{Model: "a", Fallbacks: []RetryTarget{{Model: "a"}}}},
		"duplicate fallback": {
			{Model: "a", Fallbacks: []RetryTarget{{Model: "b"}, {Provider: "", Model: "B"}}},
		},
		"provider with space": {
			{Model: "a", Fallbacks: []RetryTarget{{Provider: "openai compatible", Model: "b"}}},
		},
		"empty fallback model": {
			{Model: "a", Fallbacks: []RetryTarget{{Provider: "codex"}}},
		},
	}
	for name, rows := range cases {
		if err := validateRetryChain(NormalizeRetryChain(rows)); err == nil {
			t.Fatalf("%s: validateRetryChain accepted %+v", name, rows)
		}
	}
	valid := retryTestChainConfig().RetryChain
	if err := validateRetryChain(NormalizeRetryChain(valid)); err != nil {
		t.Fatalf("validateRetryChain rejected the shipped shape: %v", err)
	}
}

func TestNormalizeRetryChainDropsEmptyEntries(t *testing.T) {
	rows := []RetryChainRow{
		{Model: "  ", Fallbacks: []RetryTarget{{Model: "b"}}},
		{Model: " a ", Fallbacks: []RetryTarget{{Provider: " Codex ", Model: " b "}, {Model: " "}}},
	}
	normalized := NormalizeRetryChain(rows)
	if len(normalized) != 1 {
		t.Fatalf("normalized rows = %+v", normalized)
	}
	if normalized[0].Model != "a" {
		t.Fatalf("model = %q, want trimmed", normalized[0].Model)
	}
	if len(normalized[0].Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want the empty one dropped", normalized[0].Fallbacks)
	}
	if got := FormatRetryTarget(normalized[0].Fallbacks[0]); got != "codex/b" {
		t.Fatalf("fallback = %q, want codex/b", got)
	}
	if NormalizeRetryChain(nil) != nil {
		t.Fatal("NormalizeRetryChain(nil) must stay nil")
	}
	if NormalizeRetryChain([]RetryChainRow{{Model: " "}}) != nil {
		t.Fatal("a chain with only empty rows must normalize to nil")
	}
}

func TestParseRetryTargetRoundTrips(t *testing.T) {
	target, err := ParseRetryTarget("codex/gpt-5.5")
	if err != nil {
		t.Fatalf("ParseRetryTarget: %v", err)
	}
	if target.Provider != "codex" || target.Model != "gpt-5.5" {
		t.Fatalf("target = %+v", target)
	}
	if got := FormatRetryTarget(target); got != "codex/gpt-5.5" {
		t.Fatalf("FormatRetryTarget = %q", got)
	}
	bare, err := ParseRetryTarget("gpt-5.6-luna")
	if err != nil {
		t.Fatalf("ParseRetryTarget(bare): %v", err)
	}
	if bare.Provider != "" || bare.Model != "gpt-5.6-luna" {
		t.Fatalf("bare target = %+v", bare)
	}
	for _, bad := range []string{"", "   ", "codex/", "openai/gpt/5.5"} {
		if _, errBad := ParseRetryTarget(bad); errBad == nil {
			t.Fatalf("ParseRetryTarget(%q) accepted an invalid target", bad)
		}
	}
}

func TestRetryPlanForRequestIgnoresThinkingSuffix(t *testing.T) {
	cfg := retryTestChainConfig()
	plan, ok := retryPlanForRequest(cfg, "gpt-5.6-sol(high)")
	if !ok {
		t.Fatal("retryPlanForRequest missed the model behind a thinking suffix")
	}
	if plan.Key != "gpt-5.6-sol" {
		t.Fatalf("plan key = %q", plan.Key)
	}
	if _, okMiss := retryPlanForRequest(cfg, "gpt-5.6-unknown(high)"); okMiss {
		t.Fatal("retryPlanForRequest claimed an unknown model")
	}
	base, suffix, okSplit := splitRetryModelSuffix(" gpt-5.6-sol(high) ")
	if !okSplit || base != "gpt-5.6-sol" || suffix != "high" {
		t.Fatalf("splitRetryModelSuffix = %q/%q/%v", base, suffix, okSplit)
	}
	if _, _, okSplit := splitRetryModelSuffix("gpt-5.6-sol"); okSplit {
		t.Fatal("splitRetryModelSuffix accepted a model without a suffix")
	}
	if _, _, okSplit := splitRetryModelSuffix("(high)"); okSplit {
		t.Fatal("splitRetryModelSuffix accepted an empty base model")
	}
}

func TestParseByteSizeRoundTripsThroughFormat(t *testing.T) {
	cases := map[string]int64{
		"8MB":     8 << 20,
		"8MiB":    8 << 20,
		"512KB":   512 << 10,
		"1GiB":    1 << 30,
		"1048576": 1 << 20,
	}
	for raw, want := range cases {
		got, err := parseByteSize(raw)
		if err != nil {
			t.Fatalf("parseByteSize(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", raw, got, want)
		}
	}
	if rendered := formatByteSize(DefaultConfig().RetryMaxBytes); rendered != "8MB" {
		t.Fatalf("default byte budget renders as %q, want 8MB", rendered)
	}
	reparsed, err := parseByteSize(formatByteSize(DefaultConfig().RetryMaxBytes))
	if err != nil {
		t.Fatalf("reparse default byte budget: %v", err)
	}
	if reparsed != DefaultConfig().RetryMaxBytes {
		t.Fatalf("byte budget changed across a round trip: %d -> %d", DefaultConfig().RetryMaxBytes, reparsed)
	}
}

func TestRetrySettingsDurationRoundTrip(t *testing.T) {
	// The page documents "60s" style values, and it validates what it loads with
	// that same grammar, so the API must never hand it a Go-only form such as
	// "1m0s" without the page understanding it.
	cases := map[time.Duration]string{
		60 * time.Second:                      "60s",
		90 * time.Second:                      "90s",
		240 * time.Second:                     "240s",
		90*time.Second + 500*time.Millisecond: "1m30.5s",
	}
	for input, want := range cases {
		if got := formatRetryDuration(input); got != want {
			t.Fatalf("formatRetryDuration(%s) = %q, want %q", input, got, want)
		}
		parsed, err := time.ParseDuration(formatRetryDuration(input))
		if err != nil || parsed != input {
			t.Fatalf("round trip of %s = %s/%v", input, parsed, err)
		}
	}

	cfg := DefaultConfig()
	cfg.RetryEnabled = true
	cfg.RetryChain = []RetryChainRow{{Model: "gpt-5.6-sol", Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}}}}
	payload := SettingsFromConfig(cfg)
	for _, field := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"retry_stall_timeout", payload.RetryStallTimeout, cfg.RetryStallTimeout},
		{"retry_hold_timeout", payload.RetryHoldTimeout, cfg.RetryHoldTimeout},
		{"retry_chain_deadline", payload.RetryChainDeadline, cfg.RetryChainDeadline},
	} {
		if !strings.HasSuffix(field.raw, "s") || strings.ContainsAny(field.raw, "mh") {
			t.Fatalf("%s = %q, want a plain seconds value", field.name, field.raw)
		}
		parsed, err := time.ParseDuration(field.raw)
		if err != nil {
			t.Fatalf("%s = %q is not parseable: %v", field.name, field.raw, err)
		}
		if parsed != field.want {
			t.Fatalf("%s = %q, want %s", field.name, field.raw, field.want)
		}
	}

	restored, err := ConfigFromSettings(cfg, payload)
	if err != nil {
		t.Fatalf("ConfigFromSettings: %v", err)
	}
	if restored.RetryStallTimeout != cfg.RetryStallTimeout ||
		restored.RetryHoldTimeout != cfg.RetryHoldTimeout ||
		restored.RetryChainDeadline != cfg.RetryChainDeadline {
		t.Fatalf("budgets changed across the settings round trip: %+v", restored)
	}
}

func TestConfigFromSettingsAcceptsCompoundDurations(t *testing.T) {
	base := DefaultConfig()
	payload := SettingsFromConfig(base)
	payload.RetryStallTimeout = "1m0s"
	payload.RetryHoldTimeout = "1m30s"
	payload.RetryChainDeadline = "4m0s"
	cfg, err := ConfigFromSettings(base, payload)
	if err != nil {
		t.Fatalf("ConfigFromSettings rejected Go duration strings: %v", err)
	}
	if cfg.RetryStallTimeout != time.Minute ||
		cfg.RetryHoldTimeout != 90*time.Second ||
		cfg.RetryChainDeadline != 4*time.Minute {
		t.Fatalf("compound durations parsed as %+v", cfg)
	}
}
