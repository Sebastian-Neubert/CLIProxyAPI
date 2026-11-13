package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, input := range []string{"soonest-reset", "SoonestReset", "sr"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy(%q) = %q, want soonest-reset", input, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
		}
	}
}

func TestSoonestResetRoutingSelectorWrappedBySessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:                "soonest-reset",
			SessionAffinity:         true,
			SessionAffinityRebindOn: "quota",
		},
	})
	selector := newRoutingSelector(state)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", selector)
	}
	defer affinity.Stop()
	if state.sessionAffinityRebindOn != coreauth.SessionAffinityRebindOnQuota {
		t.Fatalf("rebind-on = %q, want quota", state.sessionAffinityRebindOn)
	}
}
