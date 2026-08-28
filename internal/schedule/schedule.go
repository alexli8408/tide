/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

// Package schedule evaluates ScalingSchedule specs against wall-clock time.
//
// It is deliberately pure: no Kubernetes clients, no clocks of its own. The
// caller passes "now" in, which makes every edge case — midnight wrap,
// timezone shifts, daylight-saving transitions — testable with plain table
// tests.
package schedule

import (
	"fmt"
	"time"

	tidev1alpha1 "github.com/alexli8408/tide/api/v1alpha1"
)

// Decision is the outcome of evaluating a schedule at one instant.
type Decision struct {
	// Replicas the target should have right now.
	Replicas int32

	// WindowName of the window that decided Replicas, or empty when
	// DefaultReplicas applies.
	WindowName string

	// NextTransition is the earliest instant after "now" at which the
	// decision could change. It is the zero time when the schedule has no
	// windows (the decision then never changes until the spec does).
	//
	// This is a safe over-approximation: it fires at every window boundary,
	// including boundaries where an overlapping window keeps the replica
	// count the same. Those reconciles are cheap no-ops.
	NextTransition time.Time
}

var dayOfWeek = map[tidev1alpha1.DayOfWeek]time.Weekday{
	tidev1alpha1.DayOfWeek("Sun"): time.Sunday,
	tidev1alpha1.DayOfWeek("Mon"): time.Monday,
	tidev1alpha1.DayOfWeek("Tue"): time.Tuesday,
	tidev1alpha1.DayOfWeek("Wed"): time.Wednesday,
	tidev1alpha1.DayOfWeek("Thu"): time.Thursday,
	tidev1alpha1.DayOfWeek("Fri"): time.Friday,
	tidev1alpha1.DayOfWeek("Sat"): time.Saturday,
}

// horizonDays bounds the search for the next transition. A window with at
// least one valid day recurs within 7 days; one extra day covers ends that
// wrap past midnight.
const horizonDays = 8

// window is a parsed, validated ScalingWindow.
type window struct {
	name         string
	days         map[time.Weekday]bool
	startH       int
	startM       int
	endH         int
	endM         int
	wraps        bool // end is on the day after start
	replicas     int32
}

// occurrence is one concrete [start, end) instance of a window.
type occurrence struct {
	start time.Time
	end   time.Time
}

// Evaluate returns the Decision for spec at the instant now. It returns an
// error only for specs that are invalid in ways CRD validation cannot catch
// statically (unknown timezone, duplicate window names).
func Evaluate(spec *tidev1alpha1.ScalingScheduleSpec, now time.Time) (Decision, error) {
	loc, err := loadLocation(spec.TimeZone)
	if err != nil {
		return Decision{}, err
	}

	windows, err := parseWindows(spec.Windows)
	if err != nil {
		return Decision{}, err
	}

	decision := Decision{Replicas: spec.DefaultReplicas}
	if len(windows) == 0 {
		return decision, nil
	}

	nowLocal := now.In(loc)
	var next time.Time
	active := false

	for _, w := range windows {
		for _, occ := range w.occurrences(nowLocal) {
			if !now.Before(occ.start) && now.Before(occ.end) {
				// Highest replica count wins among overlapping windows;
				// ties keep the first window in spec order.
				if !active || w.replicas > decision.Replicas {
					decision.Replicas = w.replicas
					decision.WindowName = w.name
				}
				active = true
			}
			for _, boundary := range []time.Time{occ.start, occ.end} {
				if boundary.After(now) && (next.IsZero() || boundary.Before(next)) {
					next = boundary
				}
			}
		}
	}

	decision.NextTransition = next
	return decision, nil
}

// Validate reports whether the spec is well-formed without evaluating it.
func Validate(spec *tidev1alpha1.ScalingScheduleSpec) error {
	if _, err := loadLocation(spec.TimeZone); err != nil {
		return err
	}
	_, err := parseWindows(spec.Windows)
	return err
}

func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", name, err)
	}
	return loc, nil
}

func parseWindows(in []tidev1alpha1.ScalingWindow) ([]window, error) {
	seen := make(map[string]bool, len(in))
	out := make([]window, 0, len(in))
	for _, sw := range in {
		if seen[sw.Name] {
			return nil, fmt.Errorf("duplicate window name %q", sw.Name)
		}
		seen[sw.Name] = true

		w := window{
			name:     sw.Name,
			days:     make(map[time.Weekday]bool, len(sw.Days)),
			replicas: sw.Replicas,
		}
		var err error
		if w.startH, w.startM, err = parseHHMM(sw.Start); err != nil {
			return nil, fmt.Errorf("window %q start: %w", sw.Name, err)
		}
		if w.endH, w.endM, err = parseHHMM(sw.End); err != nil {
			return nil, fmt.Errorf("window %q end: %w", sw.Name, err)
		}
		// End at or before start means the window runs into the next day.
		w.wraps = w.endH*60+w.endM <= w.startH*60+w.startM

		for _, d := range sw.Days {
			wd, ok := dayOfWeek[d]
			if !ok {
				return nil, fmt.Errorf("window %q: unknown day %q", sw.Name, d)
			}
			w.days[wd] = true
		}
		out = append(out, w)
	}
	return out, nil
}

func parseHHMM(s string) (int, int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%02d:%02d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(s) != 5 {
		return 0, 0, fmt.Errorf("invalid time %q, want 24-hour HH:MM", s)
	}
	return h, m, nil
}

// occurrences returns every concrete instance of w that could be active now
// or start within the transition horizon. Offsets start at -1 so a window
// that began yesterday and wraps past midnight is still considered.
//
// Instants are built with time.Date on civil dates rather than by adding
// 24-hour durations, so a window's start stays at its wall-clock time across
// daylight-saving transitions. When a DST jump makes a wall-clock time
// nonexistent (spring forward) or ambiguous (fall back), time.Date resolves
// it to a real instant; the window shifts by the size of the gap that day.
func (w *window) occurrences(nowLocal time.Time) []occurrence {
	year, month, day := nowLocal.Date()
	loc := nowLocal.Location()
	occs := make([]occurrence, 0, horizonDays)

	for offset := -1; offset <= horizonDays; offset++ {
		// Noon is used to determine the weekday: DST shifts never move noon
		// across a date boundary, while midnight arithmetic can.
		weekday := time.Date(year, month, day+offset, 12, 0, 0, 0, loc).Weekday()
		if !w.days[weekday] {
			continue
		}
		endOffset := offset
		if w.wraps {
			endOffset++
		}
		occs = append(occs, occurrence{
			start: time.Date(year, month, day+offset, w.startH, w.startM, 0, 0, loc),
			end:   time.Date(year, month, day+endOffset, w.endH, w.endM, 0, 0, loc),
		})
	}
	return occs
}
