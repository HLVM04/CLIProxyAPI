package auth

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Quota window identifiers produced by QuotaWindowsFromState.
const (
	QuotaWindowClaudeSession = "5h"
	QuotaWindowClaudeWeekly  = "7d"
	QuotaWindowCodexPrimary  = "primary"
	QuotaWindowCodexSecond   = "secondary"
)

// QuotaWindow is one normalized usage window derived from a passive quota
// observation. ResetAt is absolute; relative upstream values are resolved
// against the observation time.
type QuotaWindow struct {
	ID string `json:"id"`
	// UsedPercent is the consumed share of the window, 0..100.
	UsedPercent float64 `json:"used_percent"`
	// ResetAt is when the window refills. Zero when unknown.
	ResetAt time.Time `json:"reset_at,omitempty"`
	// WindowMinutes is the window length. Zero when unknown.
	WindowMinutes int `json:"window_minutes,omitempty"`
}

// Length returns the window length, or zero when unknown.
func (w QuotaWindow) Length() time.Duration {
	if w.WindowMinutes <= 0 {
		return 0
	}
	return time.Duration(w.WindowMinutes) * time.Minute
}

// RolledForward projects the window to now. A window whose reset already passed
// has refilled, so its usage drops to zero and, when the window length is known,
// the reset advances by whole periods; otherwise the reset becomes unknown.
func (w QuotaWindow) RolledForward(now time.Time) QuotaWindow {
	if w.ResetAt.IsZero() || w.ResetAt.After(now) {
		return w
	}
	w.UsedPercent = 0
	period := w.Length()
	if period <= 0 {
		w.ResetAt = time.Time{}
		return w
	}
	elapsed := now.Sub(w.ResetAt)
	w.ResetAt = w.ResetAt.Add((elapsed/period + 1) * period)
	return w
}

// QuotaWindowsFromState parses the passive quota observation for provider into
// normalized windows, ordered from the shortest to the longest window.
func QuotaWindowsFromState(provider string, quota QuotaState) []QuotaWindow {
	if len(quota.Signals) == 0 {
		return nil
	}
	var windows []QuotaWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows = claudeQuotaWindows(quota.Signals)
	case "codex":
		windows = codexQuotaWindows(quota.Signals, quota.ObservedAt)
	default:
		return nil
	}
	sort.SliceStable(windows, func(i, j int) bool {
		return windows[i].WindowMinutes < windows[j].WindowMinutes
	})
	return windows
}

// QuotaRoutingWindow returns the longest window with a known reset, rolled
// forward to now. The longest window is the one whose unused capacity is lost
// at reset, which is what soonest-reset-first routing optimizes for.
func QuotaRoutingWindow(provider string, quota QuotaState, now time.Time) (QuotaWindow, bool) {
	windows := QuotaWindowsFromState(provider, quota)
	for i := len(windows) - 1; i >= 0; i-- {
		window := windows[i].RolledForward(now)
		if !window.ResetAt.IsZero() {
			return window, true
		}
	}
	return QuotaWindow{}, false
}

func claudeQuotaWindows(signals map[string]string) []QuotaWindow {
	specs := []struct {
		id      string
		minutes int
	}{
		{QuotaWindowClaudeSession, 5 * 60},
		{QuotaWindowClaudeWeekly, 7 * 24 * 60},
	}
	windows := make([]QuotaWindow, 0, len(specs))
	for _, spec := range specs {
		prefix := "Anthropic-Ratelimit-Unified-" + spec.id + "-"
		utilization, okUtilization := quotaSignalFloat(signals, prefix+"Utilization")
		reset, okReset := quotaSignalUnix(signals, prefix+"Reset")
		if !okUtilization && !okReset {
			continue
		}
		window := QuotaWindow{ID: spec.id, WindowMinutes: spec.minutes}
		if okUtilization {
			window.UsedPercent = clampPercent(utilization * 100)
		}
		if okReset {
			window.ResetAt = reset
		}
		windows = append(windows, window)
	}
	return windows
}

func codexQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	windows := make([]QuotaWindow, 0, 2)
	for _, id := range []string{QuotaWindowCodexPrimary, QuotaWindowCodexSecond} {
		prefix := "X-Codex-" + strings.ToUpper(id[:1]) + id[1:] + "-"
		used, okUsed := quotaSignalFloat(signals, prefix+"Used-Percent")
		minutes, okMinutes := quotaSignalFloat(signals, prefix+"Window-Minutes")
		reset, okReset := quotaSignalUnix(signals, prefix+"Reset-At")
		if !okReset && !observedAt.IsZero() {
			if after, okAfter := quotaSignalFloat(signals, prefix+"Reset-After-Seconds"); okAfter && after >= 0 {
				reset = observedAt.Add(time.Duration(after * float64(time.Second))).Truncate(time.Second)
				okReset = true
			}
		}
		if !okUsed && !okReset {
			continue
		}
		window := QuotaWindow{ID: id, UsedPercent: clampPercent(used)}
		if okMinutes && minutes > 0 && minutes < math.MaxInt32 {
			window.WindowMinutes = int(minutes)
		}
		if okReset {
			window.ResetAt = reset
		}
		windows = append(windows, window)
	}
	return windows
}

func quotaSignalValue(signals map[string]string, name string) (string, bool) {
	if value, ok := signals[name]; ok {
		return strings.TrimSpace(value), true
	}
	canonical := http.CanonicalHeaderKey(name)
	if value, ok := signals[canonical]; ok {
		return strings.TrimSpace(value), true
	}
	return "", false
}

func quotaSignalFloat(signals map[string]string, name string) (float64, bool) {
	raw, ok := quotaSignalValue(signals, name)
	if !ok || raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// quotaSignalUnix parses a unix timestamp in seconds (or milliseconds when the
// magnitude makes that unambiguous).
func quotaSignalUnix(signals map[string]string, name string) (time.Time, bool) {
	value, ok := quotaSignalFloat(signals, name)
	if !ok || value <= 0 {
		return time.Time{}, false
	}
	if value > 1e12 {
		return time.UnixMilli(int64(value)), true
	}
	return time.Unix(int64(value), 0), true
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
