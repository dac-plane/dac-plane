package provision

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
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
  register-provider  -tenant TENANT -role ROLE [-id ID] [-artifact-store ID]
                 roles: runtime, environment, enforcement, artifact_store
  bind-adapter   -tenant TENANT -provider ID -role ROLE -issuer URL -subject SUB
  unbind-adapter -issuer URL -subject SUB

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
	var tenantText, principalText, providerText, storeText, idText, kind, role, issuer, subject string
	flags.StringVar(&tenantText, "tenant", "", "tenant identifier")
	flags.StringVar(&principalText, "principal", "", "principal identifier")
	flags.StringVar(&idText, "id", "", "identifier for the record to create")
	flags.StringVar(&kind, "kind", "", "principal kind: human or service")
	flags.StringVar(&providerText, "provider", "", "provider identifier")
	flags.StringVar(&role, "role", "", "provider role")
	flags.StringVar(&storeText, "artifact-store", "", "artifact store provider a runtime exports to")
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
	// Values are checked here, like identifiers, so a typo is a usage error
	// reported before any database is opened.
	if set["kind"] && !slices.Contains(identity.PrincipalKinds(), identity.PrincipalKind(kind)) {
		return nil, fmt.Errorf("%w: -kind must be human or service", errUsage)
	}
	if set["role"] && !slices.Contains(contracts.Roles(), contracts.Role(role)) {
		return nil, fmt.Errorf("%w: -role must be runtime, environment, enforcement or artifact_store", errUsage)
	}
	if set["artifact-store"] && role != string(contracts.RoleRuntime) {
		return nil, fmt.Errorf("%w: -artifact-store applies only to a runtime", errUsage)
	}
	if set["issuer"] && !identity.ValidIssuer(issuer) {
		return nil, fmt.Errorf("%w: -issuer must be an https URL without query or fragment", errUsage)
	}
	if set["subject"] && !identity.ValidSubject(subject) {
		return nil, fmt.Errorf("%w: -subject must be 1 to 255 visible ASCII characters", errUsage)
	}
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
	case "register-provider":
		if err := need([]string{"tenant", "role"}, "id", "artifact-store"); err != nil {
			return nil, err
		}
		tenant, err := requiredID[ids.Tenant]("tenant", tenantText)
		if err != nil {
			return nil, err
		}
		id, err := optionalID[ids.Provider]("id", idText)
		if err != nil {
			return nil, err
		}
		var artifactStore *ids.ProviderID
		if set["artifact-store"] {
			store, err := requiredID[ids.Provider]("artifact-store", storeText)
			if err != nil {
				return nil, err
			}
			artifactStore = &store
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			provider, err := p.RegisterProvider(ctx, tenant, id, contracts.Role(role), artifactStore)
			result := map[string]string{"tenant_id": tenant.String(), "provider_id": provider.String(), "role": role}
			if artifactStore != nil {
				result["artifact_store_id"] = artifactStore.String()
			}
			return result, err
		}, nil
	case "bind-adapter":
		if err := need([]string{"tenant", "provider", "role", "issuer", "subject"}); err != nil {
			return nil, err
		}
		tenant, err := requiredID[ids.Tenant]("tenant", tenantText)
		if err != nil {
			return nil, err
		}
		provider, err := requiredID[ids.Provider]("provider", providerText)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			err := p.BindAdapter(ctx, tenant, provider, contracts.Role(role), issuer, subject)
			return map[string]string{"tenant_id": tenant.String(), "provider_id": provider.String(), "role": role, "issuer": issuer, "subject": subject}, err
		}, nil
	case "unbind-adapter":
		if err := need([]string{"issuer", "subject"}); err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *Provisioner) (map[string]string, error) {
			binding, err := p.UnbindAdapter(ctx, issuer, subject)
			return map[string]string{"tenant_id": binding.Tenant.String(), "provider_id": binding.Provider.String(), "role": string(binding.Role), "issuer": issuer, "subject": subject}, err
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
