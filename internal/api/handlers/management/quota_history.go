package management

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const defaultQuotaHistoryLookback = 7 * 24 * time.Hour

type quotaHistoryWindow struct {
	ID            string     `json:"id"`
	UsedPercent   float64    `json:"used_percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
}

type quotaHistoryRouting struct {
	// WindowID is the window soonest-reset-first routing orders by.
	WindowID string     `json:"window_id,omitempty"`
	Tier     string     `json:"tier"`
	ResetAt  *time.Time `json:"reset_at,omitempty"`
	// Rank is the 1-based preference within the provider, ignoring cooldowns.
	Rank int `json:"rank"`
}

type quotaHistoryCredential struct {
	AuthIndex   string                `json:"auth_index"`
	AuthID      string                `json:"auth_id"`
	Name        string                `json:"name"`
	Provider    string                `json:"provider"`
	Label       string                `json:"label,omitempty"`
	Email       string                `json:"email,omitempty"`
	PlanType    string                `json:"plan_type,omitempty"`
	Priority    int                   `json:"priority"`
	Disabled    bool                  `json:"disabled"`
	Unavailable bool                  `json:"unavailable"`
	ObservedAt  *time.Time            `json:"observed_at,omitempty"`
	Windows     []quotaHistoryWindow  `json:"windows"`
	Routing     quotaHistoryRouting   `json:"routing"`
	Samples     []quotahistory.Sample `json:"samples"`
	sortTier    int
	sortReset   time.Time
}

// GetQuotaHistory returns recorded quota usage and current windows for every
// credential whose provider reports passive quota windows (Claude, Codex).
// Query parameter "since" (RFC3339 or unix milliseconds) bounds the samples;
// it defaults to seven days ago.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	now := time.Now()
	since, ok := parseQuotaHistorySince(c.Query("since"), now)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since"})
		return
	}

	store := quotahistory.Default()
	history := store.Snapshot(since)
	credentials := make([]*quotaHistoryCredential, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil || !quotaHistoryProvider(auth.Provider) {
			continue
		}
		auth.EnsureIndex()
		credentials = append(credentials, buildQuotaHistoryCredential(auth, history[auth.ID], now))
	}
	rankQuotaHistoryCredentials(credentials)

	strategy, _ := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	retentionHours := 0
	if store != nil {
		retentionHours = int(store.Retention() / time.Hour)
	}
	c.JSON(http.StatusOK, gin.H{
		"generated_at":     now.UTC(),
		"since":            since.UTC(),
		"routing_strategy": strategy,
		"recording":        store != nil,
		"retention_hours":  retentionHours,
		"credentials":      credentials,
	})
}

func quotaHistoryProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex":
		return true
	default:
		return false
	}
}

func parseQuotaHistorySince(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now.Add(-defaultQuotaHistoryLookback), true
	}
	if millis, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
		if millis < 0 {
			return time.Time{}, false
		}
		return time.UnixMilli(millis), true
	}
	parsed, errParse := time.Parse(time.RFC3339, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func buildQuotaHistoryCredential(auth *coreauth.Auth, history quotahistory.Credential, now time.Time) *quotaHistoryCredential {
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	credential := &quotaHistoryCredential{
		AuthIndex:   auth.Index,
		AuthID:      auth.ID,
		Name:        name,
		Provider:    strings.ToLower(strings.TrimSpace(auth.Provider)),
		Label:       strings.TrimSpace(auth.Label),
		Email:       authEmail(auth),
		Priority:    quotaHistoryPriority(auth),
		Disabled:    auth.Disabled || auth.Status == coreauth.StatusDisabled,
		Unavailable: auth.Unavailable,
		Windows:     make([]quotaHistoryWindow, 0, 2),
		Samples:     history.Samples,
	}
	if credential.Samples == nil {
		credential.Samples = []quotahistory.Sample{}
	}
	if plan := strings.TrimSpace(auth.Quota.Signals["X-Codex-Plan-Type"]); plan != "" {
		credential.PlanType = plan
	}
	if !auth.Quota.ObservedAt.IsZero() {
		observedAt := auth.Quota.ObservedAt.UTC()
		credential.ObservedAt = &observedAt
	}
	for _, window := range coreauth.QuotaWindowsFromState(auth.Provider, auth.Quota) {
		window = window.RolledForward(now)
		entry := quotaHistoryWindow{
			ID:            window.ID,
			UsedPercent:   window.UsedPercent,
			WindowMinutes: window.WindowMinutes,
		}
		if !window.ResetAt.IsZero() {
			resetAt := window.ResetAt.UTC()
			entry.ResetAt = &resetAt
		}
		credential.Windows = append(credential.Windows, entry)
	}

	tier, resetAt := coreauth.SoonestResetFirstRank(auth, now)
	credential.Routing.Tier = tier
	credential.sortReset = resetAt
	if !resetAt.IsZero() {
		reset := resetAt.UTC()
		credential.Routing.ResetAt = &reset
	}
	if window, okWindow := coreauth.QuotaRoutingWindow(auth.Provider, auth.Quota, now); okWindow {
		credential.Routing.WindowID = window.ID
	}
	switch tier {
	case "known":
		credential.sortTier = 0
	case "exhausted":
		credential.sortTier = 2
	default:
		credential.sortTier = 1
	}
	return credential
}

// rankQuotaHistoryCredentials assigns the soonest-reset-first preference within
// each provider: priority first, then tier, reset, and ID, matching the router.
func rankQuotaHistoryCredentials(credentials []*quotaHistoryCredential) {
	sort.SliceStable(credentials, func(i, j int) bool {
		left, right := credentials[i], credentials[j]
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		if left.Disabled != right.Disabled {
			return !left.Disabled
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if left.sortTier != right.sortTier {
			return left.sortTier < right.sortTier
		}
		if !left.sortReset.Equal(right.sortReset) {
			return left.sortReset.Before(right.sortReset)
		}
		return left.AuthID < right.AuthID
	})
	rank := 0
	provider := ""
	for _, credential := range credentials {
		if credential.Provider != provider {
			provider = credential.Provider
			rank = 0
		}
		if credential.Disabled {
			continue
		}
		rank++
		credential.Routing.Rank = rank
	}
}

func quotaHistoryPriority(auth *coreauth.Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	priority, errParse := strconv.Atoi(raw)
	if errParse != nil {
		return 0
	}
	return priority
}

// observeQuotaUsageProbe feeds a successful provider usage-endpoint response
// fetched through the management API back into the credential's quota
// observation, so routing and history learn resets of idle credentials too.
func (h *Handler) observeQuotaUsageProbe(auth *coreauth.Auth, method, rawURL string, statusCode int, body []byte) {
	if h == nil || h.authManager == nil || auth == nil {
		return
	}
	if method != http.MethodGet || statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return
	}
	provider := coreauth.QuotaUsageEndpointProvider(rawURL)
	if provider == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
		return
	}
	headers := coreauth.QuotaHeadersFromUsagePayload(provider, body)
	if len(headers) == 0 {
		return
	}
	if h.authManager.ObserveQuotaHeaders(auth.ID, headers, time.Now()) {
		log.Debugf("quota usage probe updated observation for auth %s", auth.Index)
	}
}
