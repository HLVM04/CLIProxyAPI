package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetQuotaHistory_RanksBySoonestReset(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, &coreauth.SoonestResetFirstSelector{}, nil)
	now := time.Now()
	register := func(id string, reset time.Time) {
		auth := &coreauth.Auth{ID: id, FileName: id + ".json", Provider: "claude", Metadata: map[string]any{"email": id + "@example.com"}}
		if !reset.IsZero() {
			auth.Quota = coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.3",
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset.Unix(), 10),
			}}
		}
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	register("late", now.Add(90*time.Hour))
	register("soon", now.Add(10*time.Hour))
	register("unknown", time.Time{})
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "gemini", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini) error = %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Routing: config.RoutingConfig{Strategy: "srf"}}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/quota", nil)
	h.GetQuotaHistory(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		RoutingStrategy string `json:"routing_strategy"`
		Credentials     []struct {
			AuthID  string `json:"auth_id"`
			Email   string `json:"email"`
			Windows []struct {
				ID          string  `json:"id"`
				UsedPercent float64 `json:"used_percent"`
			} `json:"windows"`
			Routing struct {
				Tier string `json:"tier"`
				Rank int    `json:"rank"`
			} `json:"routing"`
		} `json:"credentials"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if payload.RoutingStrategy != "soonest-reset-first" {
		t.Fatalf("routing_strategy = %q", payload.RoutingStrategy)
	}
	if len(payload.Credentials) != 3 {
		t.Fatalf("credentials = %#v, want the three Claude credentials", payload.Credentials)
	}
	wantOrder := []string{"soon", "late", "unknown"}
	for index, want := range wantOrder {
		got := payload.Credentials[index]
		if got.AuthID != want || got.Routing.Rank != index+1 {
			t.Fatalf("credential #%d = %s rank %d, want %s rank %d", index, got.AuthID, got.Routing.Rank, want, index+1)
		}
	}
	if payload.Credentials[0].Email != "soon@example.com" || len(payload.Credentials[0].Windows) != 1 ||
		payload.Credentials[0].Windows[0].UsedPercent != 30 {
		t.Fatalf("first credential = %#v", payload.Credentials[0])
	}
	if payload.Credentials[2].Routing.Tier != "unknown" {
		t.Fatalf("unknown credential tier = %q", payload.Credentials[2].Routing.Tier)
	}
}

func TestObserveQuotaUsageProbe_FeedsManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "probe", Provider: "codex"}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := []byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1900000000}}}`)

	h.observeQuotaUsageProbe(auth, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", http.StatusInternalServerError, body)
	if stored, _ := manager.GetByID("probe"); len(stored.Quota.Signals) != 0 {
		t.Fatalf("failed probe was observed: %#v", stored.Quota.Signals)
	}
	h.observeQuotaUsageProbe(auth, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", http.StatusOK, body)
	if stored, _ := manager.GetByID("probe"); len(stored.Quota.Signals) != 0 {
		t.Fatalf("probe for another provider was observed: %#v", stored.Quota.Signals)
	}

	h.observeQuotaUsageProbe(auth, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", http.StatusOK, body)
	stored, _ := manager.GetByID("probe")
	window, ok := coreauth.QuotaRoutingWindow(stored.Provider, stored.Quota, time.Unix(1_899_000_000, 0))
	if !ok || window.UsedPercent != 20 || window.ResetAt.Unix() != 1900000000 {
		t.Fatalf("probe window = %#v, ok=%v", window, ok)
	}
}
