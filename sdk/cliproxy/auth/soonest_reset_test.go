package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaAuth(id string, weeklyUsed float64, weeklyReset time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: time.Now(),
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1",
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10),
				"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weeklyUsed, 'f', -1, 64),
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
		},
	}
}

func TestQuotaWindowsFromStateClaude(t *testing.T) {
	t.Parallel()

	reset := time.Unix(1787695200, 0)
	windows := QuotaWindowsFromState("claude", QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.53",
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1787695200",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1787296800",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":    "1787695200",
	}})
	if len(windows) != 2 {
		t.Fatalf("windows = %#v, want 5h and 7d", windows)
	}
	if windows[0].ID != QuotaWindowClaudeSession || windows[1].ID != QuotaWindowClaudeWeekly {
		t.Fatalf("window order = %q, %q", windows[0].ID, windows[1].ID)
	}
	if windows[1].UsedPercent != 53 || !windows[1].ResetAt.Equal(reset) || windows[1].WindowMinutes != 7*24*60 {
		t.Fatalf("weekly window = %#v", windows[1])
	}
}

func TestQuotaWindowsFromStateCodexResolvesRelativeReset(t *testing.T) {
	t.Parallel()

	observedAt := time.Unix(1_700_000_000, 0)
	windows := QuotaWindowsFromState("codex", QuotaState{ObservedAt: observedAt, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":           "40",
		"X-Codex-Primary-Window-Minutes":         "300",
		"X-Codex-Primary-Reset-After-Seconds":    "600",
		"X-Codex-Secondary-Used-Percent":         "12.5",
		"X-Codex-Secondary-Window-Minutes":       "10080",
		"X-Codex-Secondary-Reset-At":             "1700500000",
		"X-Codex-Bengalfox-Primary-Used-Percent": "99",
	}})
	if len(windows) != 2 {
		t.Fatalf("windows = %#v", windows)
	}
	if !windows[0].ResetAt.Equal(observedAt.Add(10*time.Minute)) || windows[0].UsedPercent != 40 {
		t.Fatalf("primary window = %#v", windows[0])
	}
	if windows[1].ID != QuotaWindowCodexSecond || windows[1].UsedPercent != 12.5 || !windows[1].ResetAt.Equal(time.Unix(1700500000, 0)) {
		t.Fatalf("secondary window = %#v", windows[1])
	}
}

func TestQuotaWindowRolledForwardAdvancesByPeriod(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	window := QuotaWindow{ID: "7d", UsedPercent: 80, ResetAt: now.Add(-25 * time.Hour), WindowMinutes: 24 * 60}
	rolled := window.RolledForward(now)
	if rolled.UsedPercent != 0 {
		t.Fatalf("rolled used = %v, want 0", rolled.UsedPercent)
	}
	if want := now.Add(23 * time.Hour); !rolled.ResetAt.Equal(want) {
		t.Fatalf("rolled reset = %v, want %v", rolled.ResetAt, want)
	}

	unknownPeriod := QuotaWindow{ID: "x", UsedPercent: 50, ResetAt: now.Add(-time.Minute)}.RolledForward(now)
	if !unknownPeriod.ResetAt.IsZero() || unknownPeriod.UsedPercent != 0 {
		t.Fatalf("rolled unknown-period window = %#v", unknownPeriod)
	}
}

func TestSoonestResetFirstSelectorPrefersEarliestReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		claudeQuotaAuth("a-late", 0.1, now.Add(96*time.Hour)),
		claudeQuotaAuth("b-soon", 0.6, now.Add(20*time.Hour)),
		{ID: "0-unknown", Provider: "claude"},
		claudeQuotaAuth("c-mid", 0.3, now.Add(48*time.Hour)),
	}
	got, errPick := (&SoonestResetFirstSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got.ID != "b-soon" {
		t.Fatalf("Pick() = %q, want b-soon", got.ID)
	}
}

func TestSoonestResetFirstSelectorRanksUnknownBeforeExhausted(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		claudeQuotaAuth("a-exhausted", 1.0, now.Add(2*time.Hour)),
		{ID: "b-unknown", Provider: "claude"},
	}
	got, errPick := (&SoonestResetFirstSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got.ID != "b-unknown" {
		t.Fatalf("Pick() = %q, want b-unknown", got.ID)
	}
}

func TestSoonestResetFirstSelectorKeepsPriorityTiers(t *testing.T) {
	t.Parallel()

	now := time.Now()
	soon := claudeQuotaAuth("a-soon-low", 0.2, now.Add(time.Hour))
	soon.Attributes = map[string]string{"priority": "0"}
	late := claudeQuotaAuth("b-late-high", 0.2, now.Add(100*time.Hour))
	late.Attributes = map[string]string{"priority": "5"}
	got, errPick := (&SoonestResetFirstSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{soon, late})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got.ID != "b-late-high" {
		t.Fatalf("Pick() = %q, want b-late-high", got.ID)
	}
}

func TestSoonestResetFirstSelectorRollsPastResetForward(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// The weekly window of "a" reset an hour ago, so its next reset is ~6 days out.
	rolled := claudeQuotaAuth("a-rolled", 0.9, now.Add(-time.Hour))
	fresh := claudeQuotaAuth("b-two-days", 0.2, now.Add(48*time.Hour))
	got, errPick := (&SoonestResetFirstSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{rolled, fresh})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got.ID != "b-two-days" {
		t.Fatalf("Pick() = %q, want b-two-days", got.ID)
	}
}

func TestSchedulerPick_SoonestResetFirst(t *testing.T) {
	t.Parallel()

	now := time.Now()
	scheduler := newSchedulerForTest(
		&SoonestResetFirstSelector{},
		claudeQuotaAuth("a", 0.1, now.Add(72*time.Hour)),
		claudeQuotaAuth("b", 0.1, now.Add(24*time.Hour)),
		&Auth{ID: "c", Provider: "claude"},
	)
	for index := 0; index < 3; index++ {
		got, errPick := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, errPick)
		}
		if got.ID != "b" {
			t.Fatalf("pickSingle() #%d = %q, want b", index, got.ID)
		}
	}

	tried := map[string]struct{}{"b": {}}
	got, errPick := scheduler.pickSingle(context.Background(), "claude", "", cliproxyexecutor.Options{}, tried)
	if errPick != nil {
		t.Fatalf("pickSingle() with b tried error = %v", errPick)
	}
	if got.ID != "a" {
		t.Fatalf("pickSingle() with b tried = %q, want a", got.ID)
	}
}

func TestSchedulerPick_SoonestResetFirstUsesProviderLevelObservation(t *testing.T) {
	now := time.Now()
	reg := registry.GetGlobalRegistry()
	for _, authID := range []string{"srf-a", "srf-b"} {
		reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "claude-model-x"}, {ID: "claude-model-y"}})
	}
	t.Cleanup(func() {
		reg.UnregisterClient("srf-a")
		reg.UnregisterClient("srf-b")
	})

	scheduler := newSchedulerForTest(
		&SoonestResetFirstSelector{},
		claudeQuotaAuth("srf-a", 0.1, now.Add(72*time.Hour)),
		claudeQuotaAuth("srf-b", 0.1, now.Add(48*time.Hour)),
	)
	// Materialize both model shards before the observation changes.
	for _, model := range []string{"claude-model-x", "claude-model-y"} {
		if got, errPick := scheduler.pickSingle(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil); errPick != nil || got.ID != "srf-b" {
			t.Fatalf("initial pickSingle(%s) = %v, %v; want srf-b", model, got, errPick)
		}
	}

	// A result for model x reveals that srf-a now resets first.
	updated := claudeQuotaAuth("srf-a", 0.1, now.Add(time.Hour))
	updated.Generation = 5
	updated.UpdatedAt = now
	scheduler.upsertAuthResult(updated, []string{"claude-model-x"}, false)

	got, errPick := scheduler.pickSingle(context.Background(), "claude", "claude-model-y", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickSingle(model y) error = %v", errPick)
	}
	if got.ID != "srf-a" {
		t.Fatalf("pickSingle(model y) = %q, want srf-a from the provider-level observation", got.ID)
	}
}

func TestSchedulerPickMixed_SoonestResetFirstComparesAcrossProviders(t *testing.T) {
	t.Parallel()

	now := time.Now()
	codex := &Auth{ID: "codex-a", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Used-Percent":   "10",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10),
	}}}
	scheduler := newSchedulerForTest(
		&SoonestResetFirstSelector{},
		claudeQuotaAuth("claude-a", 0.1, now.Add(30*time.Hour)),
		codex,
	)
	got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if got.ID != "codex-a" || provider != "codex" {
		t.Fatalf("pickMixed() = %q via %q, want codex-a via codex", got.ID, provider)
	}
}

func TestManagerObserveQuotaHeadersNotifiesHookAndIgnoresOlderSnapshots(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "obs", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	var notified []*Auth
	manager.SetQuotaObservationHook(func(auth *Auth) { notified = append(notified, auth) })

	observedAt := time.Now()
	reset := observedAt.Add(30 * time.Hour).Unix()
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.4")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(reset, 10))
	if !manager.ObserveQuotaHeaders("obs", headers, observedAt) {
		t.Fatal("ObserveQuotaHeaders() = false, want true")
	}
	if len(notified) != 1 || notified[0].Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] != strconv.FormatInt(reset, 10) {
		t.Fatalf("hook snapshots = %#v", notified)
	}

	if manager.RestoreQuotaObservation("obs", observedAt.Add(-time.Hour), map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9"}) {
		t.Fatal("RestoreQuotaObservation() replaced a newer observation")
	}
	stored, ok := manager.GetByID("obs")
	if !ok || stored.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.4" {
		t.Fatalf("stored signals = %#v", stored.Quota.Signals)
	}
}

func TestQuotaHeadersFromUsagePayload(t *testing.T) {
	t.Parallel()

	if got := QuotaUsageEndpointProvider("https://api.anthropic.com/api/oauth/usage"); got != "claude" {
		t.Fatalf("claude usage provider = %q", got)
	}
	if got := QuotaUsageEndpointProvider("https://chatgpt.com/backend-api/wham/usage?x=1"); got != "codex" {
		t.Fatalf("codex usage provider = %q", got)
	}
	if got := QuotaUsageEndpointProvider("https://example.com/api/oauth/usage"); got != "" {
		t.Fatalf("foreign usage provider = %q", got)
	}

	claude := QuotaHeadersFromUsagePayload("claude", []byte(`{
		"five_hour": {"utilization": 12.0, "resets_at": "2026-10-26T22:00:00.123+00:00"},
		"seven_day": {"utilization": 37.0, "resets_at": "2026-10-29T10:00:00+00:00"},
		"seven_day_opus": null
	}`))
	var claudeQuota QuotaState
	if !claudeQuota.ObserveResponseHeadersForProvider("claude", claude, time.Now()) {
		t.Fatalf("claude headers were not observed: %#v", claude)
	}
	window, ok := QuotaRoutingWindow("claude", claudeQuota, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	if !ok || window.ID != QuotaWindowClaudeWeekly || window.UsedPercent != 37 ||
		!window.ResetAt.Equal(time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("claude routing window = %#v, ok=%v", window, ok)
	}

	codex := QuotaHeadersFromUsagePayload("codex", []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 3, "limit_window_seconds": 18000, "reset_after_seconds": 100, "reset_at": 1787000000},
			"secondary_window": {"used_percent": 30, "limit_window_seconds": 604800, "reset_after_seconds": 9000, "reset_at": 1787500000}
		}
	}`))
	var codexQuota QuotaState
	if !codexQuota.ObserveResponseHeadersForProvider("codex", codex, time.Unix(1786990000, 0)) {
		t.Fatalf("codex headers were not observed: %#v", codex)
	}
	window, ok = QuotaRoutingWindow("codex", codexQuota, time.Unix(1786990000, 0))
	if !ok || window.ID != QuotaWindowCodexSecond || window.WindowMinutes != 10080 || window.UsedPercent != 30 {
		t.Fatalf("codex routing window = %#v, ok=%v", window, ok)
	}
	if codexQuota.Signals["X-Codex-Plan-Type"] != "pro" {
		t.Fatalf("codex plan type signal = %q", codexQuota.Signals["X-Codex-Plan-Type"])
	}

	if headers := QuotaHeadersFromUsagePayload("claude", []byte(`not json`)); headers != nil {
		t.Fatalf("invalid payload headers = %#v", headers)
	}
}
