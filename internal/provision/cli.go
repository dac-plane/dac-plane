package provision

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// Usage describes the provision subcommand.
const Usage = `usage: dac-plane provision <operation> [flags]

operations:
  tenant         [-id TENANT]                                  create a tenant
  principal      -tenant TENANT -kind human|service [-id ID]   create a principal
  bind           -tenant TENANT -principal ID -issuer URL -subject SUB
  unbind         -tenant TENANT -issuer URL -subject SUB
  grant-reader   -tenant TENANT -principal ID
  revoke-reader  -tenant TENANT -principal ID

Operations are idempotent; pass -id to make creation repeatable. Output is one
JSON object naming the affected records.`

// Exit codes of Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Run parses one provisioning operation from args (the arguments after
// "provision"), performs it with the provisioner open returns, and writes the
// result as JSON to stdout. Database access is opened only after the
// arguments are valid.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, open func(context.Context) (*Provisioner, func(), error)) int {
	operation, err := parse(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane provision: %v\n\n%s\n", err, Usage)
		return ExitUsage
	}
	provisioner, closeProvisioner, err := open(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane provision: %v\n", err)
		return ExitError
	}
	defer closeProvisioner()
	result, err := operation(ctx, provisioner)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane provision: %v\n", err)
		return ExitError
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "dac-plane provision: %v\n", err)
		return ExitError
	}
	return ExitOK
}

type operation func(context.Context, *Provisioner) (map[string]string, error)

var errUsage = errors.New("invalid arguments")

func parse(args []string) (operation, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: an operation is required", errUsage)
	}
	flags := flag.NewFlagSet("provision "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var tenantText, principalText, idText, kind, issuer, subject string
	flags.StringVar(&tenantText, "tenant", "", "tenant identifier")
	flags.StringVar(&principalText, "principal", "", "principal identifier")
	flags.StringVar(&idText, "id", "", "identifier for the record to create")
	flags.StringVar(&kind, "kind", "", "principal kind: human or service")
	flags.StringVar(&issuer, "issuer", "", "OpenID Connect issuer URL")
	flags.StringVar(&subject, "subject", "", "subject identifier within the issuer")
	if err := flags.Parse(args[1:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("%w: unexpected argument %q", errUsage, flags.Arg(0))
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	need := func(required []string, allowed ...string) error {
		for _, name := range required {
			if !set[name] {
				return fmt.Errorf("%w: -%s is required", errUsage, name)
			}
		}
		for name := range set {
			if !slices.Contains(required, name) && !slices.Contains(allowed, name) {
				return fmt.Errorf("%w: -%s does not apply to %s", errUsage, name, args[0])
			}
		}
		return nil
	}

	switch args[0] {
	case "tenant":
		if err := need(nil, "id"); err != nil {
			return nil, err
		}
		id, err := optionalID[ids.Tenant]("id", idText)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			tenant, err := p.CreateTenant(ctx, id)
			return map[string]string{"tenant_id": tenant.String()}, err
		}, nil
	case "principal":
		if err := need([]string{"tenant", "kind"}, "id"); err != nil {
			return nil, err
		}
		tenant, err := requiredID[ids.Tenant]("tenant", tenantText)
		if err != nil {
			return nil, err
		}
		id, err := optionalID[ids.Principal]("id", idText)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			principal, err := p.CreatePrincipal(ctx, tenant, id, identity.PrincipalKind(kind))
			return map[string]string{"tenant_id": tenant.String(), "principal_id": principal.String()}, err
		}, nil
	case "bind":
		if err := need([]string{"tenant", "principal", "issuer", "subject"}); err != nil {
			return nil, err
		}
		tenant, principal, err := tenantAndPrincipal(tenantText, principalText)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			err := p.BindIdentity(ctx, tenant, principal, issuer, subject)
			return map[string]string{"tenant_id": tenant.String(), "principal_id": principal.String(), "issuer": issuer, "subject": subject}, err
		}, nil
	case "unbind":
		if err := need([]string{"tenant", "issuer", "subject"}); err != nil {
			return nil, err
		}
		tenant, err := requiredID[ids.Tenant]("tenant", tenantText)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			err := p.UnbindIdentity(ctx, tenant, issuer, subject)
			return map[string]string{"tenant_id": tenant.String(), "issuer": issuer, "subject": subject}, err
		}, nil
	case "grant-reader", "revoke-reader":
		if err := need([]string{"tenant", "principal"}); err != nil {
			return nil, err
		}
		tenant, principal, err := tenantAndPrincipal(tenantText, principalText)
		if err != nil {
			return nil, err
		}
		enabled := args[0] == "grant-reader"
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			err := p.SetTenantReader(ctx, tenant, principal, enabled)
			return map[string]string{"tenant_id": tenant.String(), "principal_id": principal.String()}, err
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown operation %q", errUsage, args[0])
}

func tenantAndPrincipal(tenantText, principalText string) (ids.TenantID, ids.PrincipalID, error) {
	tenant, err := requiredID[ids.Tenant]("tenant", tenantText)
	if err != nil {
		return ids.TenantID{}, ids.PrincipalID{}, err
	}
	principal, err := requiredID[ids.Principal]("principal", principalText)
	return tenant, principal, err
}

func requiredID[K any](name, text string) (ids.ID[K], error) {
	id, err := ids.Parse[K](text)
	if err != nil {
		return ids.ID[K]{}, fmt.Errorf("%w: -%s must be a canonical UUIDv7", errUsage, name)
	}
	return id, nil
}

func optionalID[K any](name, text string) (ids.ID[K], error) {
	if text == "" {
		return ids.ID[K]{}, nil
	}
	return requiredID[K](name, text)
}
