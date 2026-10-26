package auth

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// QuotaUsageEndpointProvider reports which provider's usage endpoint rawURL
// targets, so a management usage probe can double as a quota observation.
// It returns an empty string for any other URL.
func QuotaUsageEndpointProvider(rawURL string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	switch {
	case host == "api.anthropic.com" && path == "/api/oauth/usage":
		return "claude"
	case host == "chatgpt.com" && path == "/backend-api/wham/usage":
		return "codex"
	default:
		return ""
	}
}

// QuotaHeadersFromUsagePayload converts a provider usage-endpoint response body
// into the response-header form understood by passive quota observation.
// It returns nil when the payload carries no recognizable window.
func QuotaHeadersFromUsagePayload(provider string, payload []byte) http.Header {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil
	}
	root := gjson.ParseBytes(payload)
	var headers http.Header
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		headers = claudeUsageHeaders(root)
	case "codex":
		headers = codexUsageHeaders(root)
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func claudeUsageHeaders(root gjson.Result) http.Header {
	headers := make(http.Header)
	for _, spec := range []struct{ key, id string }{
		{"five_hour", QuotaWindowClaudeSession},
		{"seven_day", QuotaWindowClaudeWeekly},
	} {
		window := root.Get(spec.key)
		if !window.IsObject() {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + spec.id + "-"
		// The usage endpoint reports utilization in percent; the response
		// headers use a 0..1 fraction.
		if utilization := window.Get("utilization"); utilization.Type == gjson.Number {
			headers.Set(prefix+"Utilization", strconv.FormatFloat(clampPercent(utilization.Float())/100, 'f', -1, 64))
		}
		if reset, ok := parseUsageResetTime(window.Get("resets_at")); ok {
			headers.Set(prefix+"Reset", strconv.FormatInt(reset.Unix(), 10))
		}
	}
	return headers
}

func codexUsageHeaders(root gjson.Result) http.Header {
	rateLimit := root.Get("rate_limit")
	if !rateLimit.IsObject() {
		return nil
	}
	headers := make(http.Header)
	for _, spec := range []struct{ key, prefix string }{
		{"primary_window", "X-Codex-Primary-"},
		{"secondary_window", "X-Codex-Secondary-"},
	} {
		window := rateLimit.Get(spec.key)
		if !window.IsObject() {
			continue
		}
		used := window.Get("used_percent")
		if used.Type != gjson.Number {
			continue
		}
		headers.Set(spec.prefix+"Used-Percent", strconv.FormatFloat(clampPercent(used.Float()), 'f', -1, 64))
		if seconds := window.Get("limit_window_seconds"); seconds.Type == gjson.Number && seconds.Int() > 0 {
			headers.Set(spec.prefix+"Window-Minutes", strconv.FormatInt(seconds.Int()/60, 10))
		}
		if resetAt := window.Get("reset_at"); resetAt.Type == gjson.Number && resetAt.Int() > 0 {
			headers.Set(spec.prefix+"Reset-At", strconv.FormatInt(resetAt.Int(), 10))
		}
		if resetAfter := window.Get("reset_after_seconds"); resetAfter.Type == gjson.Number && resetAfter.Int() >= 0 {
			headers.Set(spec.prefix+"Reset-After-Seconds", strconv.FormatInt(resetAfter.Int(), 10))
		}
	}
	if len(headers) == 0 {
		return nil
	}
	if allowed := rateLimit.Get("allowed"); allowed.IsBool() {
		headers.Set("X-Codex-Allowed", strconv.FormatBool(allowed.Bool()))
	}
	if reached := rateLimit.Get("limit_reached"); reached.IsBool() {
		headers.Set("X-Codex-Limit-Reached", strconv.FormatBool(reached.Bool()))
	}
	if plan := strings.TrimSpace(root.Get("plan_type").String()); plan != "" && validQuotaSignalValue(plan) {
		headers.Set("X-Codex-Plan-Type", plan)
	}
	return headers
}

func parseUsageResetTime(value gjson.Result) (time.Time, bool) {
	switch value.Type {
	case gjson.String:
		raw := strings.TrimSpace(value.String())
		if raw == "" {
			return time.Time{}, false
		}
		if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
			return parsed, true
		}
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
			return time.Unix(seconds, 0), true
		}
	case gjson.Number:
		if seconds := value.Int(); seconds > 0 {
			return time.Unix(seconds, 0), true
		}
	}
	return time.Time{}, false
}
