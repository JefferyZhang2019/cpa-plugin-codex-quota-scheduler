package main

import "time"

const (
	probeSkewTolerance          = 120 * time.Second
	probeMaxPlausibleWindow     = 120 * 24 * time.Hour
	probeUnknownResetRecheck    = 30 * time.Minute
	probeRefreshAfterResetDelay = time.Minute
)

type ProbeBaselineKind string

const (
	ProbeBaselineNone      ProbeBaselineKind = ""
	ProbeBaselineReset     ProbeBaselineKind = "reset"
	ProbeBaselineUsageOnly ProbeBaselineKind = "usage_only"
)

type ProbeBaseline struct {
	Kind            ProbeBaselineKind `json:"kind"`
	ResetAt         time.Time         `json:"reset_at,omitempty"`
	Usage           float64           `json:"usage"`
	NextRecheckAt   time.Time         `json:"next_recheck_at,omitempty"`
	WindowLength    time.Duration     `json:"window_length,omitempty"`
	CandidateLength time.Duration     `json:"candidate_length,omitempty"`
	StableIntervals int               `json:"stable_intervals,omitempty"`
}

func ResetProbeBaseline(reset time.Time, usage float64, length time.Duration) ProbeBaseline {
	return ProbeBaseline{Kind: ProbeBaselineReset, ResetAt: reset, Usage: usage, WindowLength: length}
}
func UsageOnlyProbeBaseline(usage float64, next time.Time) ProbeBaseline {
	return ProbeBaseline{Kind: ProbeBaselineUsageOnly, Usage: usage, NextRecheckAt: next}
}

type QuotaSnapshot struct {
	Valid   bool
	ResetAt *time.Time
	Usage   *float64
}
type ProbeClassificationKind string

const (
	ProbeInvalid           ProbeClassificationKind = "invalid"
	ProbeActivatedNew      ProbeClassificationKind = "activated_new"
	ProbeActivatedInferred ProbeClassificationKind = "activated_inferred"
	ProbeNotDueYet         ProbeClassificationKind = "not_due_yet"
	ProbeStillLazy         ProbeClassificationKind = "still_lazy"
	ProbeAmbiguous         ProbeClassificationKind = "ambiguous"
	ProbeAnomaly           ProbeClassificationKind = "anomaly"
)

type ProbeClassification struct {
	Kind          ProbeClassificationKind
	Baseline      ProbeBaseline
	LengthUnknown bool
}

func ClassifyProbeWindow(base ProbeBaseline, snap QuotaSnapshot, now time.Time) ProbeClassification {
	if !snap.Valid || snap.Usage == nil {
		return ProbeClassification{Kind: ProbeInvalid, Baseline: base}
	}
	if base.Kind == ProbeBaselineNone {
		if snap.ResetAt == nil {
			return ProbeClassification{Kind: ProbeNotDueYet, Baseline: UsageOnlyProbeBaseline(*snap.Usage, now.Add(probeUnknownResetRecheck))}
		}
		n := ResetProbeBaseline(*snap.ResetAt, *snap.Usage, 0)
		if snap.ResetAt.After(now) {
			return ProbeClassification{Kind: ProbeNotDueYet, Baseline: n}
		}
		return ProbeClassification{Kind: ProbeStillLazy, Baseline: n}
	}
	if base.Kind == ProbeBaselineUsageOnly {
		if snap.ResetAt != nil {
			n := ResetProbeBaseline(*snap.ResetAt, *snap.Usage, 0)
			if snap.ResetAt.After(now) {
				return ProbeClassification{Kind: ProbeNotDueYet, Baseline: n}
			}
			return ProbeClassification{Kind: ProbeStillLazy, Baseline: n}
		}
		if usageActivated(base.Usage, *snap.Usage) {
			base.Usage = *snap.Usage
			base.NextRecheckAt = now.Add(probeUnknownResetRecheck)
			return ProbeClassification{Kind: ProbeActivatedInferred, Baseline: base}
		}
		if now.Before(base.NextRecheckAt) {
			return ProbeClassification{Kind: ProbeNotDueYet, Baseline: base}
		}
		base.NextRecheckAt = now.Add(probeUnknownResetRecheck)
		return ProbeClassification{Kind: ProbeStillLazy, Baseline: base}
	}
	if snap.ResetAt != nil && snap.ResetAt.Before(base.ResetAt.Add(-probeSkewTolerance)) {
		return ProbeClassification{Kind: ProbeAnomaly, Baseline: base}
	}
	if snap.ResetAt != nil {
		delta := snap.ResetAt.Sub(base.ResetAt)
		if base.WindowLength > 0 && delta > 2*base.WindowLength {
			return ProbeClassification{Kind: ProbeAnomaly, Baseline: base}
		}
		if base.WindowLength == 0 && delta > probeMaxPlausibleWindow {
			return ProbeClassification{Kind: ProbeAnomaly, Baseline: base}
		}
		if snap.ResetAt.After(base.ResetAt.Add(probeSkewTolerance)) {
			n := base
			n.ResetAt = *snap.ResetAt
			n.Usage = *snap.Usage
			if n.WindowLength == 0 {
				if n.CandidateLength > 0 && absDuration(n.CandidateLength-delta) <= probeSkewTolerance {
					n.StableIntervals++
					if n.StableIntervals >= 2 {
						n.WindowLength = delta
					}
				} else {
					n.CandidateLength = delta
					n.StableIntervals = 1
				}
			}
			return ProbeClassification{Kind: ProbeActivatedNew, Baseline: n, LengthUnknown: n.WindowLength == 0}
		}
	}
	if now.Before(base.ResetAt.Add(probeRefreshAfterResetDelay)) {
		return ProbeClassification{Kind: ProbeNotDueYet, Baseline: base}
	}
	if snap.ResetAt == nil {
		if usageActivated(base.Usage, *snap.Usage) {
			base.Usage = *snap.Usage
			return ProbeClassification{Kind: ProbeActivatedInferred, Baseline: base}
		}
		if *snap.Usage == base.Usage {
			return ProbeClassification{Kind: ProbeAmbiguous, Baseline: base}
		}
		return ProbeClassification{Kind: ProbeAmbiguous, Baseline: base}
	}
	if absDuration(snap.ResetAt.Sub(base.ResetAt)) <= probeSkewTolerance && !usageActivated(base.Usage, *snap.Usage) {
		return ProbeClassification{Kind: ProbeStillLazy, Baseline: base}
	}
	return ProbeClassification{Kind: ProbeAmbiguous, Baseline: base}
}

// A zero-usage snapshot is activation evidence only when the previous window
// had actually consumed quota. Treating 0 -> 0 as activation makes a never-used
// lazy window look healthy forever.
func usageActivated(old, new float64) bool { return old > 0 && new == 0 }

func looksLikeUnusedLazyReset(base ProbeBaseline, snap QuotaSnapshot, now time.Time) bool {
	if !snap.Valid || base.Kind != ProbeBaselineReset || base.ResetAt.IsZero() || base.WindowLength <= 0 || snap.ResetAt == nil || snap.Usage == nil || *snap.Usage != 0 {
		return false
	}
	return absDuration(snap.ResetAt.Sub(now.Add(base.WindowLength))) <= resetProbeCloseThreshold
}
