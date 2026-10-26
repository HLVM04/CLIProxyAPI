package quotahistory

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func claudeAuth(id string, observedAt time.Time, weeklyUsed float64, weeklyReset time.Time) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		Quota: coreauth.QuotaState{
			ObservedAt: observedAt,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(weeklyUsed, 'f', -1, 64),
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(weeklyReset.Unix(), 10),
			},
		},
	}
}

func newTestStore(path string, now time.Time) *Store {
	store := New(path)
	store.nowFunc = func() time.Time { return now }
	return store
}

func TestRecordThrottlesUnchangedAndBurstSamples(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	reset := base.Add(72 * time.Hour)
	store := newTestStore("", base.Add(2*time.Hour))

	store.Record(claudeAuth("a", base, 0.10, reset))
	// Unchanged usage inside the heartbeat interval is not re-sampled.
	store.Record(claudeAuth("a", base.Add(10*time.Minute), 0.10, reset))
	// A change is sampled.
	store.Record(claudeAuth("a", base.Add(11*time.Minute), 0.12, reset))
	// A burst within minSampleInterval of the previous two collapses into the last sample.
	store.Record(claudeAuth("a", base.Add(12*time.Minute), 0.13, reset))
	store.Record(claudeAuth("a", base.Add(13*time.Minute), 0.14, reset))

	samples := store.Snapshot(time.Time{})["a"].Samples
	if len(samples) != 3 {
		t.Fatalf("samples = %#v, want 3", samples)
	}
	if got := samples[2].Windows["7d"].Used; got != 14 {
		t.Fatalf("last sample used = %v, want 14", got)
	}
	if samples[2].At != base.Add(13*time.Minute).UnixMilli() {
		t.Fatalf("last sample at = %d", samples[2].At)
	}
}

func TestRecordKeepsWindowRolloverAndIgnoresOlderObservations(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	store := newTestStore("", base.Add(time.Hour))

	store.Record(claudeAuth("a", base, 0.90, base.Add(time.Minute)))
	store.Record(claudeAuth("a", base.Add(2*time.Minute), 0.0, base.Add(7*24*time.Hour)))
	store.Record(claudeAuth("a", base.Add(time.Minute), 0.5, base.Add(time.Minute)))

	credential := store.Snapshot(time.Time{})["a"]
	if len(credential.Samples) != 2 {
		t.Fatalf("samples = %#v, want rollover kept and older observation ignored", credential.Samples)
	}
	if credential.Samples[1].Windows["7d"].Used != 0 {
		t.Fatalf("rollover sample = %#v", credential.Samples[1])
	}
	if !credential.ObservedAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("observed at = %v", credential.ObservedAt)
	}
}

func TestSnapshotKeepsBaselineBeforeSince(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	store := newTestStore("", base.Add(10*time.Hour))
	reset := base.Add(100 * time.Hour)
	for i := 0; i < 4; i++ {
		store.Record(claudeAuth("a", base.Add(time.Duration(i)*2*time.Hour), 0.1*float64(i+1), reset))
	}
	samples := store.Snapshot(base.Add(3 * time.Hour))["a"].Samples
	if len(samples) != 3 || samples[0].At != base.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("samples since = %#v, want baseline at +2h and two later samples", samples)
	}
}

func TestTrimSamplesKeepsNewestBeyondRetention(t *testing.T) {
	samples := []Sample{{At: 1}, {At: 2}, {At: 3}}
	trimmed := trimSamples(samples, time.UnixMilli(10))
	if len(trimmed) != 1 || trimmed[0].At != 3 {
		t.Fatalf("trimmed = %#v, want newest sample only", trimmed)
	}
}

func TestFlushLoadRoundTripAndRestore(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), FileName)
	store := newTestStore(path, now)
	reset := now.Add(40 * time.Hour)
	store.Record(claudeAuth("persisted", now.Add(-time.Minute), 0.25, reset))
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	loaded := newTestStore(path, now)
	if errLoad := loaded.Load(); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	credential := loaded.Snapshot(time.Time{})["persisted"]
	if len(credential.Samples) != 1 || credential.Samples[0].Windows["7d"].Used != 25 {
		t.Fatalf("loaded credential = %#v", credential)
	}

	manager := coreauth.NewManager(nil, &coreauth.SoonestResetFirstSelector{}, nil)
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "persisted", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if restored := loaded.RestoreInto(manager); restored != 1 {
		t.Fatalf("RestoreInto() = %d, want 1", restored)
	}
	auth, ok := manager.GetByID("persisted")
	if !ok {
		t.Fatal("restored auth missing")
	}
	window, okWindow := coreauth.QuotaRoutingWindow(auth.Provider, auth.Quota, now)
	if !okWindow || window.ResetAt.Unix() != reset.Unix() {
		t.Fatalf("restored routing window = %#v, ok=%v", window, okWindow)
	}
}

func TestFlushPrunesCredentialsSilentForRetention(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), FileName)
	store := newTestStore(path, now)
	store.Record(claudeAuth("stale", now.Add(-DefaultRetention-time.Hour), 0.1, now.Add(time.Hour)))
	store.Record(claudeAuth("fresh", now.Add(-time.Hour), 0.1, now.Add(time.Hour)))
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	snapshot := store.Snapshot(time.Time{})
	if _, ok := snapshot["stale"]; ok {
		t.Fatal("stale credential survived the flush")
	}
	if _, ok := snapshot["fresh"]; !ok {
		t.Fatal("fresh credential was pruned")
	}
}
