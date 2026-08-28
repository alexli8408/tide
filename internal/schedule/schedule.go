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

	// Held is true when Replicas comes from the scale-down delay rather than
	// the instantaneous schedule: a higher recent value is being held while
	// it ages out of the lookback.
	Held bool

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

// windowOccurrence pairs a parsed window with one concrete occurrence.
type windowOccurrence struct {
	w   *window
	occ occurrence
}

// Evaluate returns the Decision for spec at the instant now. It returns an
// error only for specs that are invalid in ways CRD validation cannot catch
// statically (unknown timezone, duplicate window names, bad scaleDownDelay).
func Evaluate(spec *tidev1alpha1.ScalingScheduleSpec, now time.Time) (Decision, error) {
	loc, err := loadLocation(spec.TimeZone)
	if err != nil {
		return Decision{}, err
	}
	windows, err := parseWindows(spec.Windows)
	if err != nil {
		return Decision{}, err
	}
	delay, err := scaleDownDelay(spec)
	if err != nil {
		return Decision{}, err
	}

	decision := Decision{Replicas: spec.DefaultReplicas}
	if len(windows) == 0 {
		return decision, nil
	}

	nowLocal := now.In(loc)
	var occs []windowOccurrence
	for i := range windows {
		for _, occ := range windows[i].occurrences(nowLocal) {
			occs = append(occs, windowOccurrence{w: &windows[i], occ: occ})
		}
	}

	// rawAt is the undamped decision at t: the highest replica count among
	// windows active at t (ties keep spec order), or DefaultReplicas.
	rawAt := func(t time.Time) (int32, string) {
		replicas, name, active := spec.DefaultReplicas, "", false
		for _, wo := range occs {
			if !t.Before(wo.occ.start) && t.Before(wo.occ.end) {
				if !active || wo.w.replicas > replicas {
					replicas, name = wo.w.replicas, wo.w.name
				}
				active = true
			}
		}
		return replicas, name
	}

	decision.Replicas, decision.WindowName = rawAt(now)

	if delay > 0 {
		// Scale-down damping is a sliding-window maximum: the decision is
		// the highest raw value over [now-delay, now], so it may only fall
		// once it has been lower for the whole delay — while scale-ups pass
		// through immediately. The raw value is piecewise constant, so the
		// max is found by sampling the lookback start plus every window
		// boundary inside the lookback.
		lookbackStart := now.Add(-delay)
		samples := []time.Time{lookbackStart}
		for _, wo := range occs {
			for _, b := range []time.Time{wo.occ.start, wo.occ.end} {
				if b.After(lookbackStart) && !b.After(now) {
					samples = append(samples, b)
				}
			}
		}
		for _, s := range samples {
			if replicas, name := rawAt(s); replicas > decision.Replicas {
				decision.Replicas, decision.WindowName = replicas, name
				decision.Held = true
			}
		}
	}

	// The decision can change at any window boundary and, with damping, at
	// any boundary plus the delay: the raw value can fall at an END (a high
	// window closing) or at a START (a below-default window opening), and
	// either fall surfaces only when it ages out of the lookback. This
	// over-approximates — some candidates change nothing — and those
	// wakeups are cheap no-ops.
	var next time.Time
	for _, wo := range occs {
		boundaries := []time.Time{wo.occ.start, wo.occ.end}
		if delay > 0 {
			boundaries = append(boundaries, wo.occ.start.Add(delay), wo.occ.end.Add(delay))
		}
		for _, boundary := range boundaries {
			if boundary.After(now) && (next.IsZero() || boundary.Before(next)) {
				next = boundary
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
	if _, err := parseWindows(spec.Windows); err != nil {
		return err
	}
	_, err := scaleDownDelay(spec)
	return err
}

// scaleDownDelay validates and returns the spec's scale-down damping. The
// 24h cap keeps the occurrence lookback bounded (see occurrences).
func scaleDownDelay(spec *tidev1alpha1.ScalingScheduleSpec) (time.Duration, error) {
	if spec.ScaleDownDelay == nil {
		return 0, nil
	}
	d := spec.ScaleDownDelay.Duration
	if d < 0 {
		return 0, fmt.Errorf("scaleDownDelay must not be negative, got %s", d)
	}
	if d > 24*time.Hour {
		return 0, fmt.Errorf("scaleDownDelay must be at most 24h, got %s", d)
	}
	return d, nil
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

// parseHHMM accepts exactly "HH:MM" — two digits, a colon, two digits.
// Sscanf-style parsing is too lenient here (it tolerates signs, spaces, and
// trailing garbage), so the format is checked byte by byte.
func parseHHMM(s string) (int, int, error) {
	valid := len(s) == 5 && s[2] == ':' &&
		isDigit(s[0]) && isDigit(s[1]) && isDigit(s[3]) && isDigit(s[4])
	if !valid {
		return 0, 0, fmt.Errorf("invalid time %q, want 24-hour HH:MM", s)
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, 0, fmt.Errorf("invalid time %q, want 24-hour HH:MM", s)
	}
	return h, m, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// occurrences returns every concrete instance of w that could be active now,
// start within the transition horizon, or have ended within the last 24h
// (the scaleDownDelay maximum). Offsets start at -3: a wrapping window ends
// one civil day after it starts, the 24h lookback start can land another day
// back, and a 23-hour spring-forward day inside the lookback can push it one
// civil day further still.
//
// Instants are built with resolveCivil on civil dates rather than by adding
// 24-hour durations, so a window's start stays at its wall-clock time across
// daylight-saving transitions. An occurrence fully swallowed by a DST gap
// resolves to zero length and is dropped.
func (w *window) occurrences(nowLocal time.Time) []occurrence {
	year, month, day := nowLocal.Date()
	loc := nowLocal.Location()
	occs := make([]occurrence, 0, horizonDays)

	for offset := -3; offset <= horizonDays; offset++ {
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
		occ := occurrence{
			start: resolveCivil(year, month, day+offset, w.startH, w.startM, loc),
			end:   resolveCivil(year, month, day+endOffset, w.endH, w.endM, loc),
		}
		if occ.end.After(occ.start) {
			occs = append(occs, occ)
		}
	}
	return occs
}

// resolveCivil returns the instant of the wall-clock time hh:mm on the given
// civil date in loc. When a DST spring-forward gap makes that wall-clock
// time nonexistent, time.Date is free to resolve it to either side of the
// gap (Go resolves 02:30 on a 02:00->03:00 day to 01:30), which would make a
// window ending inside the gap shrink — or invert and never activate at all.
// This resolves such times forward to the first instant after the gap
// instead: a boundary inside the gap lands exactly on the transition.
func resolveCivil(year int, month time.Month, day, hh, mm int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, hh, mm, 0, 0, loc)
	if t.Hour() == hh && t.Minute() == mm {
		return t
	}
	return transitionAfter(t.Add(-24 * time.Hour))
}

// transitionAfter binary-searches the first instant within 48h of lo at
// which loc's UTC offset differs from lo's — the DST transition instant.
// The caller guarantees exactly one transition lies in that range.
func transitionAfter(lo time.Time) time.Time {
	hi := lo.Add(48 * time.Hour)
	_, offLo := lo.Zone()
	for hi.Sub(lo) > time.Minute {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.Zone(); off == offLo {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Real transitions happen on whole minutes; hi is within a minute past
	// the transition, so truncating lands exactly on it.
	return hi.Truncate(time.Minute)
}
