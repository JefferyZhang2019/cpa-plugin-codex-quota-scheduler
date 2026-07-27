package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestProbeClassifierOrderedRules(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	prev := now.Add(-time.Hour)
	usage := 80.0
	cases := []struct {
		name string
		base ProbeBaseline
		snap QuotaSnapshot
		want ProbeClassificationKind
	}{
		{"backwards", ResetProbeBaseline(prev, usage, 5*time.Hour), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(-3 * time.Minute)), Usage: ptrFloat(usage)}, ProbeAnomaly},
		{"large known jump", ResetProbeBaseline(prev, usage, 5*time.Hour), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(11 * time.Hour)), Usage: ptrFloat(usage)}, ProbeAnomaly},
		{"new reset", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(time.Hour)), Usage: ptrFloat(usage)}, ProbeActivatedNew},
		{"not due", ResetProbeBaseline(now.Add(30*time.Second), usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(now.Add(30 * time.Second)), Usage: ptrFloat(usage)}, ProbeNotDueYet},
		{"missing reset cleared", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, Usage: ptrFloat(0)}, ProbeActivatedInferred},
		{"same reset lazy", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev), Usage: ptrFloat(usage)}, ProbeStillLazy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyProbeWindow(tc.base, tc.snap, now).Kind; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestUsageActivatedRequiresConsumedBaseline(t *testing.T) {
	if usageActivated(0, 0) {
		t.Fatal("0 -> 0 is not activation evidence")
	}
	if !usageActivated(80, 0) {
		t.Fatal("positive -> 0 should remain activation evidence")
	}
}

func TestUnusedLazyResetRequiresAuthoritativeEvidence(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	reset := now.Add(5 * time.Hour)
	zero := 0.0
	base := ResetProbeBaseline(reset, 0, 5*time.Hour)
	snapshot := QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: &zero}
	if !looksLikeUnusedLazyReset(base, snapshot, now) {
		t.Fatal("valid zero-usage lazy reset was not detected")
	}

	invalid := snapshot
	invalid.Valid = false
	missingUsage := snapshot
	missingUsage.Usage = nil
	missingReset := snapshot
	missingReset.ResetAt = nil
	zeroResetBase := base
	zeroResetBase.ResetAt = time.Time{}
	unknownLength := base
	unknownLength.WindowLength = 0
	if looksLikeUnusedLazyReset(base, invalid, now) ||
		looksLikeUnusedLazyReset(base, missingUsage, now) ||
		looksLikeUnusedLazyReset(base, missingReset, now) ||
		looksLikeUnusedLazyReset(zeroResetBase, snapshot, now) ||
		looksLikeUnusedLazyReset(unknownLength, snapshot, now) {
		t.Fatal("lazy reset accepted incomplete or invalid evidence")
	}
}

func TestUsageOnlyNeverEntersResetRules(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	base := UsageOnlyProbeBaseline(75, now.Add(30*time.Minute))
	got := ClassifyProbeWindow(base, QuotaSnapshot{Valid: true, Usage: ptrFloat(75)}, now)
	if got.Kind != ProbeNotDueYet || got.Baseline.Kind != ProbeBaselineUsageOnly {
		t.Fatalf("classification=%#v", got)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
func ptrFloat(v float64) *float64    { return &v }

type probeGoldenRow struct {
	Baseline string `json:"baseline"`
	Offset   int    `json:"offset"`
	Length   string `json:"length"`
	Usage    string `json:"usage"`
	Delay    string `json:"delay"`
	Expected string `json:"expected"`
}

func generateProbeGolden() []probeGoldenRow {
	baselines := []string{"none", "reset", "usage_only"}
	offsets := []int{0, 1, 2, 3, 4, 5, 6, 7}
	lengths := []string{"5h", "7d", "unknown"}
	usages := []string{"cleared", "refilled", "same", "decreased"}
	delays := []string{"before", "edge", "after"}
	rows := make([]probeGoldenRow, 0, 864)
	for _, b := range baselines {
		for _, o := range offsets {
			for _, l := range lengths {
				for _, u := range usages {
					for _, d := range delays {
						rows = append(rows, probeGoldenRow{b, o, l, u, d, goldenProbeOracle(b, o, l, u, d)})
					}
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ka, kb := a.Baseline+a.Length+a.Usage+a.Delay, b.Baseline+b.Length+b.Usage+b.Delay
		return ka < kb || (ka == kb && a.Offset < b.Offset)
	})
	return rows
}
func goldenProbeOracle(b string, o int, l, u, d string) string {
	base, snap, now := materializeGolden(b, o, l, u, d)
	return string(independentProbeOracle(base, snap, now))
}
func TestProbeClassifyGolden(t *testing.T) {
	want := generateProbeGolden()
	if len(want) != 864 {
		t.Fatalf("rows=%d", len(want))
	}
	if os.Getenv("UPDATE_PROBE_GOLDEN") == "1" {
		raw, _ := json.MarshalIndent(want, "", "  ")
		if err := os.WriteFile("testdata/probe_classify_golden.json", append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/probe_classify_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []probeGoldenRow
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("golden diff; regenerate testdata/probe_classify_golden.json")
	}
	for _, row := range got {
		base, snap, now := materializeGolden(row.Baseline, row.Offset, row.Length, row.Usage, row.Delay)
		wantKind := independentProbeOracle(base, snap, now)
		if actual := ClassifyProbeWindow(base, snap, now).Kind; actual != wantKind {
			t.Fatalf("row=%#v actual=%s oracle=%s", row, actual, wantKind)
		}
	}
}

func materializeGolden(kind string, offset int, length, usage, delay string) (ProbeBaseline, QuotaSnapshot, time.Time) {
	prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := prev.Add(time.Minute)
	if delay == "before" {
		now = prev.Add(59 * time.Second)
	} else if delay == "after" {
		now = prev.Add(61 * time.Second)
	}
	old := 80.0
	observed := 80.0
	if usage == "cleared" || usage == "refilled" {
		observed = 0
	} else if usage == "decreased" {
		observed = 40
	}
	window := time.Duration(0)
	if length == "5h" {
		window = 5 * time.Hour
	} else if length == "7d" {
		window = 7 * 24 * time.Hour
	}
	base := ProbeBaseline{}
	if kind == "reset" {
		base = ResetProbeBaseline(prev, old, window)
	} else if kind == "usage_only" {
		base = UsageOnlyProbeBaseline(old, prev.Add(time.Minute))
	}
	snap := QuotaSnapshot{Valid: true, Usage: &observed}
	if offset > 0 {
		v := prev
		switch offset {
		case 1:
			v = prev.Add(-121 * time.Second)
		case 2:
			v = prev.Add(-120 * time.Second)
		case 3:
			v = prev
		case 4:
			v = prev.Add(120 * time.Second)
		case 5:
			v = prev.Add(121 * time.Second)
		case 6:
			if window > 0 {
				v = prev.Add(2 * window)
			} else {
				v = prev.Add(120 * 24 * time.Hour)
			}
		case 7:
			if window > 0 {
				v = prev.Add(2*window + time.Second)
			} else {
				v = prev.Add(120*24*time.Hour + time.Second)
			}
		}
		snap.ResetAt = &v
	}
	return base, snap, now
}
func independentProbeOracle(base ProbeBaseline, snap QuotaSnapshot, now time.Time) ProbeClassificationKind {
	if !snap.Valid || snap.Usage == nil {
		return ProbeInvalid
	}
	if base.Kind == ProbeBaselineNone {
		if snap.ResetAt == nil {
			return ProbeNotDueYet
		}
		if snap.ResetAt.After(now) {
			return ProbeNotDueYet
		}
		return ProbeStillLazy
	}
	if base.Kind == ProbeBaselineUsageOnly {
		if snap.ResetAt != nil {
			if snap.ResetAt.After(now) {
				return ProbeNotDueYet
			}
			return ProbeStillLazy
		}
		if *snap.Usage == 0 {
			return ProbeActivatedInferred
		}
		if now.Before(base.NextRecheckAt) {
			return ProbeNotDueYet
		}
		return ProbeStillLazy
	}
	if snap.ResetAt != nil && snap.ResetAt.Before(base.ResetAt.Add(-probeSkewTolerance)) {
		return ProbeAnomaly
	}
	if snap.ResetAt != nil {
		delta := snap.ResetAt.Sub(base.ResetAt)
		if base.WindowLength > 0 && delta > 2*base.WindowLength {
			return ProbeAnomaly
		}
		if base.WindowLength == 0 && delta > probeMaxPlausibleWindow {
			return ProbeAnomaly
		}
		if snap.ResetAt.After(base.ResetAt.Add(probeSkewTolerance)) {
			return ProbeActivatedNew
		}
	}
	if now.Before(base.ResetAt.Add(probeRefreshAfterResetDelay)) {
		return ProbeNotDueYet
	}
	if snap.ResetAt == nil {
		if *snap.Usage == 0 {
			return ProbeActivatedInferred
		}
		return ProbeAmbiguous
	}
	if absDuration(snap.ResetAt.Sub(base.ResetAt)) <= probeSkewTolerance && *snap.Usage != 0 {
		return ProbeStillLazy
	}
	return ProbeAmbiguous
}
