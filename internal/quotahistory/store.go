// Package quotahistory records per-credential quota usage over time so the
// management panel can chart pool drain and project resets, and so the last
// observation survives restarts for quota-aware routing.
package quotahistory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the history file kept in the auth directory. The extension
	// is deliberately not ".json" so auth-file watchers ignore it.
	FileName = "quota-history.jsonl"

	// DefaultRetention bounds how far back samples are kept.
	DefaultRetention = 14 * 24 * time.Hour

	minSampleInterval   = 5 * time.Minute
	heartbeatInterval   = time.Hour
	minUsedDelta        = 0.5
	resetShiftTolerance = 10 * time.Minute
	maxSamples          = 6000
	flushInterval       = 5 * time.Minute
	maxLineBytes        = 16 << 20
)

// WindowSample is one window's usage at a sample instant.
type WindowSample struct {
	// Used is the consumed share of the window, 0..100.
	Used float64 `json:"u"`
	// Reset is the window's reset as unix seconds, zero when unknown.
	Reset int64 `json:"r,omitempty"`
}

// Sample is the usage of every observed window at one instant.
type Sample struct {
	// At is the observation time as unix milliseconds.
	At      int64                   `json:"t"`
	Windows map[string]WindowSample `json:"w"`
}

// Credential is the recorded history of one credential.
type Credential struct {
	AuthID     string            `json:"auth_id"`
	Provider   string            `json:"provider"`
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals,omitempty"`
	Samples    []Sample          `json:"samples"`
}

// Store keeps quota history in memory and persists it to a single file.
type Store struct {
	mu          sync.Mutex
	path        string
	retention   time.Duration
	credentials map[string]*Credential
	dirty       bool
	nowFunc     func() time.Time
}

var defaultStore atomic.Pointer[Store]

// SetDefault installs the process-wide store read by the management API.
func SetDefault(store *Store) {
	defaultStore.Store(store)
}

// Default returns the process-wide store, or nil when history is disabled.
func Default() *Store {
	return defaultStore.Load()
}

// New creates a store persisted at path. An empty path keeps history in memory.
func New(path string) *Store {
	return &Store{
		path:        strings.TrimSpace(path),
		retention:   DefaultRetention,
		credentials: make(map[string]*Credential),
		nowFunc:     time.Now,
	}
}

// Retention reports how long samples are kept.
func (s *Store) Retention() time.Duration {
	if s == nil {
		return 0
	}
	return s.retention
}

// Record appends a sample for auth when its quota observation carries windows.
// It is safe to use as a coreauth.QuotaObservationHook.
func (s *Store) Record(auth *coreauth.Auth) {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	windows := coreauth.QuotaWindowsFromState(auth.Provider, auth.Quota)
	if len(windows) == 0 {
		return
	}
	observedAt := auth.Quota.ObservedAt
	if observedAt.IsZero() {
		observedAt = s.nowFunc()
	}
	sample := Sample{At: observedAt.UnixMilli(), Windows: make(map[string]WindowSample, len(windows))}
	for _, window := range windows {
		entry := WindowSample{Used: math.Round(window.UsedPercent*100) / 100}
		if !window.ResetAt.IsZero() {
			entry.Reset = window.ResetAt.Unix()
		}
		sample.Windows[window.ID] = entry
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	credential := s.credentials[auth.ID]
	if credential == nil {
		credential = &Credential{AuthID: auth.ID}
		s.credentials[auth.ID] = credential
	}
	if observedAt.Before(credential.ObservedAt) {
		return
	}
	credential.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	credential.ObservedAt = observedAt
	credential.Signals = cloneSignals(auth.Quota.Signals)
	credential.Samples = appendSample(credential.Samples, sample)
	credential.Samples = trimSamples(credential.Samples, s.nowFunc().Add(-s.retention))
	s.dirty = true
}

// appendSample adds next to samples while keeping density bounded: unchanged
// usage is only re-sampled hourly, and bursts collapse into one sample per
// minSampleInterval. A moved reset always starts a new sample so window
// rollovers stay visible.
func appendSample(samples []Sample, next Sample) []Sample {
	n := len(samples)
	if n == 0 {
		return append(samples, next)
	}
	last := samples[n-1]
	if next.At <= last.At {
		return samples
	}
	if resetMoved(last, next) {
		return append(samples, next)
	}
	elapsed := time.Duration(next.At-last.At) * time.Millisecond
	if !usageChanged(last, next) && elapsed < heartbeatInterval {
		return samples
	}
	if n >= 2 && elapsed < minSampleInterval &&
		time.Duration(last.At-samples[n-2].At)*time.Millisecond < minSampleInterval {
		samples[n-1] = next
		return samples
	}
	return append(samples, next)
}

func resetMoved(last, next Sample) bool {
	for id, window := range next.Windows {
		previous, ok := last.Windows[id]
		if !ok {
			if window.Reset > 0 {
				return true
			}
			continue
		}
		delta := time.Duration(window.Reset-previous.Reset) * time.Second
		if delta < 0 {
			delta = -delta
		}
		if delta > resetShiftTolerance {
			return true
		}
	}
	return false
}

func usageChanged(last, next Sample) bool {
	if len(last.Windows) != len(next.Windows) {
		return true
	}
	for id, window := range next.Windows {
		previous, ok := last.Windows[id]
		if !ok || math.Abs(window.Used-previous.Used) >= minUsedDelta {
			return true
		}
	}
	return false
}

// trimSamples drops samples older than cutoff, always keeping the newest one
// so the last known state is never lost, and caps the series length.
func trimSamples(samples []Sample, cutoff time.Time) []Sample {
	cutoffMs := cutoff.UnixMilli()
	drop := 0
	for drop < len(samples)-1 && samples[drop].At < cutoffMs {
		drop++
	}
	if over := len(samples) - drop - maxSamples; over > 0 {
		drop += over
	}
	if drop == 0 {
		return samples
	}
	return append(samples[:0], samples[drop:]...)
}

// Snapshot returns deep copies of all credentials with samples at or after
// since. The newest sample before since is kept as a baseline so charts can
// draw the window's starting level.
func (s *Store) Snapshot(since time.Time) map[string]Credential {
	if s == nil {
		return nil
	}
	sinceMs := since.UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Credential, len(s.credentials))
	for id, credential := range s.credentials {
		if credential == nil {
			continue
		}
		start := 0
		for start < len(credential.Samples)-1 && credential.Samples[start+1].At <= sinceMs {
			start++
		}
		samples := make([]Sample, 0, len(credential.Samples)-start)
		for _, sample := range credential.Samples[start:] {
			samples = append(samples, cloneSample(sample))
		}
		out[id] = Credential{
			AuthID:     credential.AuthID,
			Provider:   credential.Provider,
			ObservedAt: credential.ObservedAt,
			Signals:    cloneSignals(credential.Signals),
			Samples:    samples,
		}
	}
	return out
}

// RestoreInto seeds manager with the last persisted observation of every
// credential so quota-aware routing does not start blind after a restart.
func (s *Store) RestoreInto(manager *coreauth.Manager) int {
	if s == nil || manager == nil {
		return 0
	}
	s.mu.Lock()
	type seed struct {
		authID     string
		observedAt time.Time
		signals    map[string]string
	}
	seeds := make([]seed, 0, len(s.credentials))
	for id, credential := range s.credentials {
		if credential == nil || credential.ObservedAt.IsZero() || len(credential.Signals) == 0 {
			continue
		}
		seeds = append(seeds, seed{authID: id, observedAt: credential.ObservedAt, signals: cloneSignals(credential.Signals)})
	}
	s.mu.Unlock()

	restored := 0
	for _, item := range seeds {
		if manager.RestoreQuotaObservation(item.authID, item.observedAt, item.signals) {
			restored++
		}
	}
	return restored
}

// Load reads the history file. A missing file is treated as empty history.
func (s *Store) Load() error {
	if s == nil || s.path == "" {
		return nil
	}
	file, errOpen := os.Open(s.path)
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open quota history: %w", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("quota history: close file: %v", errClose)
		}
	}()

	loaded := make(map[string]*Credential)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	cutoff := s.nowFunc().Add(-s.retention)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var credential Credential
		if errDecode := json.Unmarshal(line, &credential); errDecode != nil || strings.TrimSpace(credential.AuthID) == "" {
			continue
		}
		credential.Samples = trimSamples(credential.Samples, cutoff)
		loaded[credential.AuthID] = &credential
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("read quota history: %w", errScan)
	}

	s.mu.Lock()
	s.credentials = loaded
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// Flush writes the history file when it changed since the last write.
func (s *Store) Flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	// Credentials silent for a whole retention period (usually deleted ones)
	// carry nothing a chart or the router could still use.
	cutoff := s.nowFunc().Add(-s.retention)
	for id, credential := range s.credentials {
		if credential == nil || credential.ObservedAt.Before(cutoff) {
			delete(s.credentials, id)
		}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, credential := range s.credentials {
		if credential == nil {
			continue
		}
		if errEncode := encoder.Encode(credential); errEncode != nil {
			s.mu.Unlock()
			return fmt.Errorf("encode quota history: %w", errEncode)
		}
	}
	s.dirty = false
	s.mu.Unlock()

	if errWrite := writeFileAtomic(s.path, buffer.Bytes()); errWrite != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return errWrite
	}
	return nil
}

// Run flushes periodically until ctx is done, then flushes once more.
func (s *Store) Run(ctx context.Context) {
	if s == nil || s.path == "" {
		return
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if errFlush := s.Flush(); errFlush != nil {
				log.Warnf("quota history: final flush failed: %v", errFlush)
			}
			return
		case <-ticker.C:
			if errFlush := s.Flush(); errFlush != nil {
				log.Warnf("quota history: flush failed: %v", errFlush)
			}
		}
	}
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create quota history directory: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(dir, ".quota-history-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create quota history temp file: %w", errCreate)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if errRemove := os.Remove(tmpPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.Debugf("quota history: remove temp file: %v", errRemove)
		}
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write quota history: %w", errWrite)
	}
	if errClose := tmp.Close(); errClose != nil {
		cleanup()
		return fmt.Errorf("close quota history: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, path); errRename != nil {
		cleanup()
		return fmt.Errorf("replace quota history: %w", errRename)
	}
	return nil
}

func cloneSignals(signals map[string]string) map[string]string {
	if len(signals) == 0 {
		return nil
	}
	out := make(map[string]string, len(signals))
	for key, value := range signals {
		out[key] = value
	}
	return out
}

func cloneSample(sample Sample) Sample {
	windows := make(map[string]WindowSample, len(sample.Windows))
	for id, window := range sample.Windows {
		windows[id] = window
	}
	return Sample{At: sample.At, Windows: windows}
}
