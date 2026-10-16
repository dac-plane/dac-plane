package identity

import (
	"errors"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

func TestPrincipalValidate(t *testing.T) {
	valid := testPrincipal(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for name, mutate := range map[string]func(*Principal){
		"missing tenant":    func(p *Principal) { p.Tenant = ids.TenantID{} },
		"missing principal": func(p *Principal) { p.ID = ids.PrincipalID{} },
		"unknown kind":      func(p *Principal) { p.Kind = "provider" },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if err := p.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewAuthorizationContextRejectsInvalidPrincipal(t *testing.T) {
	if _, err := NewAuthorizationContext(Principal{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewAuthorizationContext() error = %v, want ErrInvalid", err)
	}
}

func TestRequireTenant(t *testing.T) {
	principal := testPrincipal(t)
	ctx, err := NewAuthorizationContext(principal)
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}

	if err := ctx.RequireTenant(principal.Tenant); err != nil {
		t.Fatalf("RequireTenant(own tenant) error = %v", err)
	}
	if err := ctx.RequireTenant(newID[ids.Tenant](t)); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("RequireTenant(other tenant) error = %v, want ErrCrossTenant", err)
	}
	if err := ctx.RequireTenant(ids.TenantID{}); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("RequireTenant(zero) error = %v, want ErrCrossTenant", err)
	}
	if err := (AuthorizationContext{}).RequireTenant(principal.Tenant); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("zero context RequireTenant() error = %v, want ErrCrossTenant", err)
	}
}

func testPrincipal(t *testing.T) Principal {
	t.Helper()
	return Principal{
		Tenant: newID[ids.Tenant](t),
		ID:     newID[ids.Principal](t),
		Kind:   PrincipalHuman,
	}
}

func newID[K any](t *testing.T) ids.ID[K] {
	t.Helper()
	id, err := ids.New[K]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	return id
}
