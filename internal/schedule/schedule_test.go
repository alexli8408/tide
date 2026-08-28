/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

package schedule

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tidev1alpha1 "github.com/alexli8408/tide/api/v1alpha1"
)

// Fixed reference dates (see comments for weekdays):
//   2026-08-24 Mon, 2026-08-28 Fri, 2026-08-29 Sat, 2026-08-30 Sun
//   2026-03-08 Sun — America/Toronto springs forward 02:00 -> 03:00
//   2026-11-01 Sun — America/Toronto falls back   02:00 -> 01:00

func toronto(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatalf("load America/Toronto: %v", err)
	}
	return loc
}

func days(ds ...string) []tidev1alpha1.DayOfWeek {
	out := make([]tidev1alpha1.DayOfWeek, len(ds))
	for i, d := range ds {
		out[i] = tidev1alpha1.DayOfWeek(d)
	}
	return out
}

func businessHours(replicas int32) tidev1alpha1.ScalingWindow {
	return tidev1alpha1.ScalingWindow{
		Name:     "business-hours",
		Days:     days("Mon", "Tue", "Wed", "Thu", "Fri"),
		Start:    "09:00",
		End:      "17:00",
		Replicas: replicas,
	}
}

func TestEvaluateNoWindows(t *testing.T) {
	spec := &tidev1alpha1.ScalingScheduleSpec{DefaultReplicas: 3}
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 3 || d.WindowName != "" || !d.NextTransition.IsZero() {
		t.Fatalf("want default decision {3, \"\", zero}, got %+v", d)
	}
}

func TestEvaluateBasicWindow(t *testing.T) {
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		Windows:         []tidev1alpha1.ScalingWindow{businessHours(5)},
	}

	cases := []struct {
		name       string
		now        time.Time
		replicas   int32
		window     string
		transition time.Time
	}{
		{
			name:       "monday midday is inside the window",
			now:        time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
			replicas:   5,
			window:     "business-hours",
			transition: time.Date(2026, 8, 24, 17, 0, 0, 0, time.UTC),
		},
		{
			name:       "monday early morning is before the window",
			now:        time.Date(2026, 8, 24, 6, 30, 0, 0, time.UTC),
			replicas:   1,
			window:     "",
			transition: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
		},
		{
			name:       "window start is inclusive",
			now:        time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
			replicas:   5,
			window:     "business-hours",
			transition: time.Date(2026, 8, 24, 17, 0, 0, 0, time.UTC),
		},
		{
			name:       "window end is exclusive",
			now:        time.Date(2026, 8, 24, 17, 0, 0, 0, time.UTC),
			replicas:   1,
			window:     "",
			transition: time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC),
		},
		{
			name:       "saturday is not a scheduled day",
			now:        time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			replicas:   1,
			window:     "",
			transition: time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Evaluate(spec, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			if d.Replicas != tc.replicas || d.WindowName != tc.window {
				t.Fatalf("want (%d, %q), got (%d, %q)", tc.replicas, tc.window, d.Replicas, d.WindowName)
			}
			if !d.NextTransition.Equal(tc.transition) {
				t.Fatalf("want next transition %v, got %v", tc.transition, d.NextTransition)
			}
		})
	}
}

func TestEvaluateOverlapHighestWins(t *testing.T) {
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			businessHours(5),
			{Name: "lunch-rush", Days: days("Mon"), Start: "11:00", End: "14:00", Replicas: 8},
			{Name: "background", Days: days("Mon"), Start: "10:00", End: "16:00", Replicas: 2},
		},
	}
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 8 || d.WindowName != "lunch-rush" {
		t.Fatalf("want lunch-rush at 8 replicas, got %d from %q", d.Replicas, d.WindowName)
	}
	// The next boundary is 14:00 (lunch-rush end), even though the decision
	// afterwards comes from business-hours.
	want := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	if !d.NextTransition.Equal(want) {
		t.Fatalf("want next transition %v, got %v", want, d.NextTransition)
	}
}

func TestEvaluateOverlapTieKeepsSpecOrder(t *testing.T) {
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "first", Days: days("Mon"), Start: "09:00", End: "17:00", Replicas: 4},
			{Name: "second", Days: days("Mon"), Start: "10:00", End: "18:00", Replicas: 4},
		},
	}
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.WindowName != "first" {
		t.Fatalf("tie should keep spec order, got %q", d.WindowName)
	}
}

func TestEvaluateMidnightWrap(t *testing.T) {
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "friday-night-batch", Days: days("Fri"), Start: "22:00", End: "02:00", Replicas: 6},
		},
	}

	// Saturday 01:00: the Friday window is still running.
	d, err := Evaluate(spec, time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 6 || d.WindowName != "friday-night-batch" {
		t.Fatalf("want wrap window active at Sat 01:00, got %d from %q", d.Replicas, d.WindowName)
	}
	if want := time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("want next transition %v, got %v", want, d.NextTransition)
	}

	// Saturday 22:30: Saturday is not a start day, so nothing is active.
	d, err = Evaluate(spec, time.Date(2026, 8, 29, 22, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 1 || d.WindowName != "" {
		t.Fatalf("wrap window must belong to its start day, got %d from %q", d.Replicas, d.WindowName)
	}
}

func TestEvaluateFullDayWrap(t *testing.T) {
	// start == end wraps a full 24 hours.
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 0,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "all-monday", Days: days("Mon"), Start: "00:00", End: "00:00", Replicas: 2},
		},
	}
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 23, 59, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 2 {
		t.Fatalf("want 24h window active late Monday, got %+v", d)
	}
	if want := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("want next transition %v, got %v", want, d.NextTransition)
	}
}

func TestEvaluateTimezone(t *testing.T) {
	loc := toronto(t)
	spec := &tidev1alpha1.ScalingScheduleSpec{
		TimeZone:        "America/Toronto",
		DefaultReplicas: 1,
		Windows:         []tidev1alpha1.ScalingWindow{businessHours(5)},
	}

	// Monday 13:00 UTC is 09:00 in Toronto (EDT, UTC-4): window just opened.
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 5 {
		t.Fatalf("want window open at 09:00 Toronto, got %+v", d)
	}
	if want := time.Date(2026, 8, 24, 17, 0, 0, 0, loc); !d.NextTransition.Equal(want) {
		t.Fatalf("want next transition %v, got %v", want, d.NextTransition)
	}

	// Monday 12:59 UTC is 08:59 in Toronto: still closed.
	d, err = Evaluate(spec, time.Date(2026, 8, 24, 12, 59, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 1 {
		t.Fatalf("want window closed at 08:59 Toronto, got %+v", d)
	}
}

func TestEvaluateDSTSpringForwardKeepsWallClock(t *testing.T) {
	loc := toronto(t)
	spec := &tidev1alpha1.ScalingScheduleSpec{
		TimeZone:        "America/Toronto",
		DefaultReplicas: 1,
		Windows:         []tidev1alpha1.ScalingWindow{businessHours(5)},
	}

	// Sunday 2026-03-08 20:00 Toronto, hours after the spring-forward jump.
	// The next window start must still be Monday 09:00 *wall clock*, which is
	// now 13:00 UTC (EDT) — not 14:00 UTC as it was under EST.
	d, err := Evaluate(spec, time.Date(2026, 3, 8, 20, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 9, 13, 0, 0, 0, time.UTC)
	if !d.NextTransition.Equal(want) {
		t.Fatalf("window start must stay at 09:00 wall clock across DST: want %v, got %v",
			want, d.NextTransition)
	}
}

func TestEvaluateDSTWindowSpanningTransition(t *testing.T) {
	loc := toronto(t)
	spec := &tidev1alpha1.ScalingScheduleSpec{
		TimeZone:        "America/Toronto",
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "overnight", Days: days("Sun"), Start: "01:00", End: "05:00", Replicas: 4},
		},
	}

	// 03:30 EDT on spring-forward day: the 01:00–05:00 window brackets the
	// nonexistent 02:00–03:00 hour and must still be active.
	d, err := Evaluate(spec, time.Date(2026, 3, 8, 3, 30, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 4 || d.WindowName != "overnight" {
		t.Fatalf("window spanning DST gap must stay active, got %+v", d)
	}

	// Fall-back day: same window, 03:30 EST (after the repeated hour).
	d, err = Evaluate(spec, time.Date(2026, 11, 1, 3, 30, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 4 {
		t.Fatalf("window spanning fall-back must stay active, got %+v", d)
	}
}

func TestEvaluateWindowEndingInsideDSTGap(t *testing.T) {
	loc := toronto(t)
	// On 2026-03-08 the 02:00–02:59 hour does not exist in Toronto. A window
	// ending at 02:30 must resolve its end forward to the first instant
	// after the gap (03:00 EDT = 07:00 UTC), not shrink to 01:30 EST.
	spec := &tidev1alpha1.ScalingScheduleSpec{
		TimeZone:        "America/Toronto",
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "late-night", Days: days("Sun"), Start: "01:00", End: "02:30", Replicas: 4},
		},
	}

	d, err := Evaluate(spec, time.Date(2026, 3, 8, 1, 45, 0, 0, loc)) // 01:45 EST
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 4 || d.WindowName != "late-night" {
		t.Fatalf("window ending in DST gap must stay active until the gap, got %+v", d)
	}
	if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("gap end must resolve to the transition instant %v, got %v", want, d.NextTransition)
	}
}

func TestEvaluateWindowSwallowedByDSTGap(t *testing.T) {
	loc := toronto(t)
	// A window lying entirely inside the skipped hour has no real duration
	// that day: it must not activate, and the next transition must be next
	// week's occurrence, not an inverted boundary.
	spec := &tidev1alpha1.ScalingScheduleSpec{
		TimeZone:        "America/Toronto",
		DefaultReplicas: 1,
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "gap-only", Days: days("Sun"), Start: "02:15", End: "02:45", Replicas: 9},
		},
	}

	for _, now := range []time.Time{
		time.Date(2026, 3, 8, 1, 0, 0, 0, loc),  // before the gap (EST)
		time.Date(2026, 3, 8, 3, 10, 0, 0, loc), // after the gap (EDT)
	} {
		d, err := Evaluate(spec, now)
		if err != nil {
			t.Fatal(err)
		}
		if d.Replicas != 1 || d.WindowName != "" {
			t.Fatalf("swallowed window must not activate at %v, got %+v", now, d)
		}
		// Next Sunday 02:15 EDT = 06:15 UTC.
		if want := time.Date(2026, 3, 15, 6, 15, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
			t.Fatalf("next transition must be next week's occurrence %v, got %v", want, d.NextTransition)
		}
	}
}

func TestEvaluateScaleDownDelayHoldsAfterWindow(t *testing.T) {
	delay := &metav1.Duration{Duration: 30 * time.Minute}
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		ScaleDownDelay:  delay,
		Windows:         []tidev1alpha1.ScalingWindow{businessHours(5)},
	}

	// 17:15 Monday: the window ended at 17:00 but the delay holds 5.
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 17, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 5 || d.WindowName != "business-hours" {
		t.Fatalf("delay must hold window replicas after end, got %+v", d)
	}
	if want := time.Date(2026, 8, 24, 17, 30, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("next transition must be end+delay %v, got %v", want, d.NextTransition)
	}

	// 17:45: the hold has aged out; back to default.
	d, err = Evaluate(spec, time.Date(2026, 8, 24, 17, 45, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 1 || d.WindowName != "" {
		t.Fatalf("hold must expire after the delay, got %+v", d)
	}
	if want := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("want next transition at Tuesday's start %v, got %v", want, d.NextTransition)
	}
}

func TestEvaluateScaleDownDelayNeverDelaysScaleUp(t *testing.T) {
	// A low-replica window ends and the schedule returns to a HIGHER
	// default: that is a scale-up and must not be held back by the delay.
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 3,
		ScaleDownDelay:  &metav1.Duration{Duration: time.Hour},
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "quiet-night", Days: days("Mon"), Start: "00:00", End: "06:00", Replicas: 0},
		},
	}
	d, err := Evaluate(spec, time.Date(2026, 8, 24, 6, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 3 || d.WindowName != "" {
		t.Fatalf("scale-up must be immediate despite delay, got %+v", d)
	}
}

func TestEvaluateScaleDownDelayAcrossMidnightWrap(t *testing.T) {
	// Wrap window Sun 22:00–Mon 02:00 with a 24h delay, evaluated Tuesday
	// 00:30: the occurrence starts two civil days back, ends 22.5h before
	// now, and must still be held by the lookback.
	spec := &tidev1alpha1.ScalingScheduleSpec{
		DefaultReplicas: 1,
		ScaleDownDelay:  &metav1.Duration{Duration: 24 * time.Hour},
		Windows: []tidev1alpha1.ScalingWindow{
			{Name: "sunday-batch", Days: days("Sun"), Start: "22:00", End: "02:00", Replicas: 6},
		},
	}
	d, err := Evaluate(spec, time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)) // Tue
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 6 || d.WindowName != "sunday-batch" {
		t.Fatalf("24h delay must hold a wrap window that ended Monday 02:00, got %+v", d)
	}
	// The hold ages out at Mon 02:00 + 24h = Tue 02:00.
	if want := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC); !d.NextTransition.Equal(want) {
		t.Fatalf("want hold expiry %v, got %v", want, d.NextTransition)
	}
}

func TestScaleDownDelayValidation(t *testing.T) {
	for _, tc := range []struct {
		delay   time.Duration
		wantErr bool
	}{
		{-time.Minute, true},
		{25 * time.Hour, true},
		{24 * time.Hour, false},
		{0, false},
	} {
		spec := &tidev1alpha1.ScalingScheduleSpec{
			DefaultReplicas: 1,
			ScaleDownDelay:  &metav1.Duration{Duration: tc.delay},
			Windows:         []tidev1alpha1.ScalingWindow{businessHours(5)},
		}
		err := Validate(spec)
		if (err != nil) != tc.wantErr {
			t.Errorf("Validate with delay %v: wantErr=%v, got %v", tc.delay, tc.wantErr, err)
		}
	}
}

func TestParseHHMMStrict(t *testing.T) {
	for _, bad := range []string{"09:5a", " 9:00", "+9:00", "1:234", "12:60", "24:00", "1200", "09-00", "９:00"} {
		if _, _, err := parseHHMM(bad); err == nil {
			t.Errorf("parseHHMM(%q) must be rejected", bad)
		}
	}
	for _, good := range []string{"00:00", "09:05", "23:59"} {
		if _, _, err := parseHHMM(good); err != nil {
			t.Errorf("parseHHMM(%q) must be accepted: %v", good, err)
		}
	}
}

func TestEvaluateErrors(t *testing.T) {
	cases := []struct {
		name    string
		spec    tidev1alpha1.ScalingScheduleSpec
		wantErr string
	}{
		{
			name:    "unknown timezone",
			spec:    tidev1alpha1.ScalingScheduleSpec{TimeZone: "Mars/Olympus"},
			wantErr: "unknown timezone",
		},
		{
			name: "duplicate window names",
			spec: tidev1alpha1.ScalingScheduleSpec{Windows: []tidev1alpha1.ScalingWindow{
				{Name: "w", Days: days("Mon"), Start: "09:00", End: "10:00", Replicas: 1},
				{Name: "w", Days: days("Tue"), Start: "09:00", End: "10:00", Replicas: 1},
			}},
			wantErr: "duplicate window name",
		},
		{
			name: "malformed start time",
			spec: tidev1alpha1.ScalingScheduleSpec{Windows: []tidev1alpha1.ScalingWindow{
				{Name: "w", Days: days("Mon"), Start: "9am", End: "10:00", Replicas: 1},
			}},
			wantErr: "invalid time",
		},
		{
			name: "out of range hour",
			spec: tidev1alpha1.ScalingScheduleSpec{Windows: []tidev1alpha1.ScalingWindow{
				{Name: "w", Days: days("Mon"), Start: "24:00", End: "10:00", Replicas: 1},
			}},
			wantErr: "invalid time",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(&tc.spec, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if verr := Validate(&tc.spec); verr == nil {
				t.Fatal("Validate must reject what Evaluate rejects")
			}
		})
	}
}
