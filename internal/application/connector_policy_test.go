package application

import (
	"github.com/dac-plane/dac-plane/internal/identity"
	"github.com/dac-plane/dac-plane/internal/ids"
	"testing"
)

func TestConnectorPolicyCannotRaiseExplicitTenantPermission(t *testing.T) {
	tenant, err := ids.New[ids.Tenant]()
	if err != nil {
		t.Fatal(err)
	}
	action := identity.ActionClass("connector.get_issue")
	for _, setting := range []identity.Permission{"", identity.PermissionNever, identity.PermissionAsk, identity.PermissionAlways} {
		for _, ceiling := range []identity.Permission{"", identity.PermissionNever, identity.PermissionAsk, identity.PermissionAlways} {
			for _, grant := range []identity.Permission{identity.PermissionAsk, identity.PermissionAlways} {
				p := identity.Policy{Tenant: tenant, Generation: 1, Settings: map[identity.ActionClass]identity.Permission{}, Ceiling: map[identity.ActionClass]identity.Permission{}}
				if setting != "" {
					p.Settings[action] = setting
				}
				if ceiling != "" {
					p.Ceiling[action] = ceiling
				}
				result := ConnectorPolicy(p, action, grant)
				want := grant
				if setting == identity.PermissionNever || ceiling == identity.PermissionNever {
					want = identity.PermissionNever
				} else if setting == identity.PermissionAsk || ceiling == identity.PermissionAsk {
					want = identity.PermissionAsk
				}
				if result.Effective(action) != want {
					t.Fatalf("%s/%s with %s = %s, want %s", setting, ceiling, grant, result.Effective(action), want)
				}
				if p.Settings[action] != setting || p.Ceiling[action] != ceiling {
					t.Fatal("mutated tenant policy")
				}
			}
		}
	}
}
