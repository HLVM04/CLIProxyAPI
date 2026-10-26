package auth

import (
	"net/http"
	"strings"
	"time"
)

// QuotaObservationHook receives a snapshot of an auth whose passive quota
// observation changed. It runs outside the manager lock and must not block.
type QuotaObservationHook func(auth *Auth)

type quotaObservationHookHolder struct {
	hook QuotaObservationHook
}

// SetQuotaObservationHook installs the callback notified after every quota
// observation change. Passing nil removes it.
func (m *Manager) SetQuotaObservationHook(hook QuotaObservationHook) {
	if m == nil {
		return
	}
	if hook == nil {
		m.quotaObservationHook.Store(nil)
		return
	}
	m.quotaObservationHook.Store(&quotaObservationHookHolder{hook: hook})
}

func (m *Manager) notifyQuotaObservation(snapshot *Auth) {
	if m == nil || snapshot == nil {
		return
	}
	holder := m.quotaObservationHook.Load()
	if holder == nil || holder.hook == nil {
		return
	}
	holder.hook(snapshot)
}

// ObserveQuotaHeaders records a quota observation taken outside request
// execution, such as a management usage probe. Observations older than the
// current snapshot are ignored. It reports whether the snapshot changed.
func (m *Manager) ObserveQuotaHeaders(authID string, headers http.Header, observedAt time.Time) bool {
	return m.applyQuotaObservation(authID, observedAt, func(auth *Auth) bool {
		return auth.Quota.ObserveResponseHeadersForProvider(auth.Provider, headers, observedAt)
	})
}

// RestoreQuotaObservation seeds a previously persisted observation snapshot so
// quota-aware routing survives restarts. It only applies when the auth has no
// newer observation of its own.
func (m *Manager) RestoreQuotaObservation(authID string, observedAt time.Time, signals map[string]string) bool {
	if observedAt.IsZero() || len(signals) == 0 {
		return false
	}
	return m.applyQuotaObservation(authID, observedAt, func(auth *Auth) bool {
		if !ProviderSupportsQuotaObservation(auth.Provider) {
			return false
		}
		restored := make(map[string]string, len(signals))
		for key, value := range signals {
			if validQuotaSignalValue(value) {
				restored[key] = value
			}
		}
		if len(restored) == 0 {
			return false
		}
		auth.Quota.Signals = restored
		auth.Quota.ObservedAt = observedAt
		return true
	})
}

func (m *Manager) applyQuotaObservation(authID string, observedAt time.Time, apply func(*Auth) bool) bool {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" || observedAt.IsZero() {
		return false
	}
	releaseMutation := m.lockAuthMutation(authID)
	defer releaseMutation()

	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil || observedAt.Before(auth.Quota.ObservedAt) || !apply(auth) {
		m.mu.Unlock()
		return false
	}
	auth.Generation++
	auth.UpdatedAt = time.Now()
	snapshot := auth.Clone()
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.upsertAuthResult(snapshot, nil, true)
	}
	m.notifyQuotaObservation(snapshot)
	return true
}
