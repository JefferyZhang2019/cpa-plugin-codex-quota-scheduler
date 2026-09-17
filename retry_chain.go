package main

import (
	"fmt"
	"strings"
)

// RetryTarget is one execution attempt target. Provider is optional: when it is
// empty CPA resolves the provider for the model the way it would for any other
// request, which keeps a chain entry valid across routing changes. When it is
// set it is forwarded as the nested execution's forced provider.
type RetryTarget struct {
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string `json:"model" yaml:"model"`
}

// RetryChainRow maps one requested model to its ordered fallback attempts. The
// key is the model string the client sends; the value is the ordered list of
// targets to try after the original attempt fails retryably.
type RetryChainRow struct {
	Model     string        `json:"model" yaml:"model"`
	Fallbacks []RetryTarget `json:"fallbacks" yaml:"fallbacks"`
}

// RetryPlan is the resolved attempt list for one request: the original attempt
// followed by the configured fallbacks, truncated to the attempt budget.
type RetryPlan struct {
	Key         string
	Requested   RetryTarget
	Fallbacks   []RetryTarget
	MaxAttempts int
}

// TargetCount is the number of attempts the plan may run, including the first.
func (p RetryPlan) TargetCount() int {
	count := 1 + len(p.Fallbacks)
	if p.MaxAttempts > 0 && count > p.MaxAttempts {
		return p.MaxAttempts
	}
	return count
}

// Target returns the attempt at index, where index 0 is the original request
// target. It reports false when the index is outside the attempt budget.
func (p RetryPlan) Target(index int) (RetryTarget, bool) {
	if index < 0 || index >= p.TargetCount() {
		return RetryTarget{}, false
	}
	if index == 0 {
		return p.Requested, true
	}
	return p.Fallbacks[index-1], true
}

// Remaining returns the fallback targets the plan has not used yet.
func (p RetryPlan) Remaining(attemptIndex int) []RetryTarget {
	if p.TargetCount()-1 <= attemptIndex {
		return nil
	}
	out := make([]RetryTarget, 0, p.TargetCount()-1-attemptIndex)
	for index := attemptIndex + 1; index < p.TargetCount(); index++ {
		target, ok := p.Target(index)
		if !ok {
			continue
		}
		out = append(out, target)
	}
	return out
}

// NormalizeRetryTarget trims a target and lowercases its provider.
func NormalizeRetryTarget(target RetryTarget) RetryTarget {
	return RetryTarget{
		Provider: strings.ToLower(strings.TrimSpace(target.Provider)),
		Model:    strings.TrimSpace(target.Model),
	}
}

// RetryTargetKey identifies a target for duplicate detection and logging. Model
// and provider names are compared case-insensitively because CPA resolves them
// that way, so "gpt-5.5" and "GPT-5.5" are the same target.
func RetryTargetKey(target RetryTarget) string {
	target = NormalizeRetryTarget(target)
	target.Model = strings.ToLower(target.Model)
	if target.Provider == "" {
		return target.Model
	}
	return target.Provider + "/" + target.Model
}

// FormatRetryTarget renders a target the way the settings page shows it.
func FormatRetryTarget(target RetryTarget) string {
	target = NormalizeRetryTarget(target)
	if target.Provider == "" {
		return target.Model
	}
	return target.Provider + "/" + target.Model
}

// ParseRetryTarget accepts either "model" or "provider/model".
func ParseRetryTarget(raw string) (RetryTarget, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return RetryTarget{}, fmt.Errorf("retry target must not be empty")
	}
	parts := strings.Split(trimmed, "/")
	switch len(parts) {
	case 1:
		return NormalizeRetryTarget(RetryTarget{Model: parts[0]}), nil
	case 2:
		target := NormalizeRetryTarget(RetryTarget{Provider: parts[0], Model: parts[1]})
		if target.Model == "" {
			return RetryTarget{}, fmt.Errorf("retry target %q is missing a model", trimmed)
		}
		return target, nil
	default:
		return RetryTarget{}, fmt.Errorf("retry target %q must be \"model\" or \"provider/model\"", trimmed)
	}
}

// NormalizeRetryChain trims every row and target and drops rows that carry no
// model. It does not judge whether a row is usable; validateRetryChain does.
func NormalizeRetryChain(rows []RetryChainRow) []RetryChainRow {
	if len(rows) == 0 {
		return nil
	}
	normalized := make([]RetryChainRow, 0, len(rows))
	for _, row := range rows {
		model := strings.TrimSpace(row.Model)
		if model == "" {
			continue
		}
		entry := RetryChainRow{Model: model}
		for _, fallback := range row.Fallbacks {
			target := NormalizeRetryTarget(fallback)
			if target.Model == "" {
				continue
			}
			entry.Fallbacks = append(entry.Fallbacks, target)
		}
		normalized = append(normalized, entry)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// validateRetryChain rejects chains that cannot be executed deterministically.
// A model may appear as a key only once, fallbacks must not repeat inside a row,
// and a row must not fall back to the model it was requested as.
func validateRetryChain(rows []RetryChainRow) error {
	seenKeys := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		model := strings.TrimSpace(row.Model)
		if model == "" {
			return fmt.Errorf("retry chain entry is missing a model")
		}
		lowerModel := strings.ToLower(model)
		if _, exists := seenKeys[lowerModel]; exists {
			return fmt.Errorf("retry chain has more than one entry for model %q", model)
		}
		seenKeys[lowerModel] = struct{}{}
		if len(row.Fallbacks) == 0 {
			return fmt.Errorf("retry chain entry %q needs at least one fallback", model)
		}
		seenTargets := make(map[string]struct{}, len(row.Fallbacks))
		for _, fallback := range row.Fallbacks {
			target := NormalizeRetryTarget(fallback)
			if target.Model == "" {
				return fmt.Errorf("retry chain entry %q has a fallback without a model", model)
			}
			if target.Provider != "" && strings.ContainsAny(target.Provider, " \t") {
				return fmt.Errorf("retry chain entry %q has an invalid provider %q", model, target.Provider)
			}
			if strings.EqualFold(target.Model, model) && target.Provider == "" {
				return fmt.Errorf("retry chain entry %q falls back to itself", model)
			}
			key := RetryTargetKey(target)
			if _, exists := seenTargets[key]; exists {
				return fmt.Errorf("retry chain entry %q lists %q twice", model, key)
			}
			seenTargets[key] = struct{}{}
		}
	}
	return nil
}

// retryChainRowFor finds the configured row for a requested model. Matching is
// exact first, then case-insensitive, mirroring how CPA resolves model aliases.
func retryChainRowFor(rows []RetryChainRow, model string) (RetryChainRow, bool) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return RetryChainRow{}, false
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Model) == trimmed {
			return row, true
		}
	}
	lower := strings.ToLower(trimmed)
	for _, row := range rows {
		if strings.ToLower(strings.TrimSpace(row.Model)) == lower {
			return row, true
		}
	}
	return RetryChainRow{}, false
}

// RetryPlanFor resolves the attempt plan for a requested model. It returns false
// when no chain row applies, which keeps the request on today's behavior.
func (cfg Config) RetryPlanFor(model string) (RetryPlan, bool) {
	row, ok := retryChainRowFor(cfg.RetryChain, model)
	if !ok {
		return RetryPlan{}, false
	}
	maxAttempts := cfg.RetryMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultConfig().RetryMaxAttempts
	}
	fallbacks := make([]RetryTarget, 0, len(row.Fallbacks))
	for _, fallback := range row.Fallbacks {
		fallbacks = append(fallbacks, NormalizeRetryTarget(fallback))
	}
	return RetryPlan{
		Key:         strings.TrimSpace(row.Model),
		Requested:   NormalizeRetryTarget(RetryTarget{Model: strings.TrimSpace(row.Model)}),
		Fallbacks:   fallbacks,
		MaxAttempts: maxAttempts,
	}, true
}

// RetryActive reports whether the chain should claim requests at all. The kill
// switch wins over every other setting, and an empty chain is inert.
func (cfg Config) RetryActive() bool {
	return cfg.RetryEnabled && len(cfg.RetryChain) > 0
}
