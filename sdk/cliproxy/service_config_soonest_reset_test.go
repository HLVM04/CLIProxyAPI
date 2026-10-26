package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetFirstRoutingSelector(t *testing.T) {
	for _, input := range []string{"soonest-reset-first", "Soonest-Reset", "srf", "soonestresetfirst"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "soonest-reset-first" {
			t.Fatalf("strategy(%q) = %q, want soonest-reset-first", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetFirstSelector); !ok {
			t.Fatalf("selector type(%q) = %T, want *auth.SoonestResetFirstSelector", input, newRoutingSelector(state))
		}
	}
}
