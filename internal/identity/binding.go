package identity

import (
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

const maxIssuerLength = 2048

var (
	// subjectPattern is a subject identifier: 1 to 255 visible ASCII
	// characters, compared exactly. OpenID Connect guarantees only the pair
	// (issuer, subject) to be stable, so neither is ever normalized.
	subjectPattern = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)
	// issuerPattern is an https URL of visible ASCII: a host part without
	// user information, then an optional path, never a query or fragment.
	// contracts/jsonschema/identity/common.schema.json mirrors it.
	issuerPattern = regexp.MustCompile(`^https://[\x21\x22\x24-\x2e\x30-\x3e\x41-\x7e]+(/[\x21\x22\x24-\x3e\x40-\x7e]*)?$`)
)

// ValidIssuer reports whether issuer is an absolute https URL with a host and
// no user information, query or fragment, as an OpenID Connect issuer must be.
func ValidIssuer(issuer string) bool {
	if len(issuer) > maxIssuerLength || !issuerPattern.MatchString(issuer) {
		return false
	}
	parsed, err := url.Parse(issuer)
	return err == nil && parsed.Host != ""
}

// ValidSubject reports whether subject is a well-formed subject identifier.
func ValidSubject(subject string) bool {
	return subjectPattern.MatchString(subject)
}

// IdentityBinding maps an external identity, the pair (issuer, subject), to
// one principal of one tenant. It is the only way an authenticated identity
// becomes a principal: the tenant comes from the binding, never from a claim
// the identity provider asserts. A person who works in two tenants holds one
// binding per tenant. A disabled binding authenticates nobody.
type IdentityBinding struct {
	Tenant    ids.TenantID    `json:"tenant_id"`
	Principal ids.PrincipalID `json:"principal_id"`
	Issuer    string          `json:"issuer"`
	Subject   string          `json:"subject"`
	Enabled   bool            `json:"enabled"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Validate reports whether every field is set and well formed.
func (b IdentityBinding) Validate() error {
	if b.Tenant.IsZero() || b.Principal.IsZero() {
		return fmt.Errorf("%w: identity binding requires tenant and principal", ErrInvalid)
	}
	if !ValidIssuer(b.Issuer) {
		return fmt.Errorf("%w: identity binding issuer must be an https URL without query or fragment", ErrInvalid)
	}
	if !ValidSubject(b.Subject) {
		return fmt.Errorf("%w: identity binding subject must be 1 to 255 visible ASCII characters", ErrInvalid)
	}
	if b.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: identity binding requires its update time", ErrInvalid)
	}
	return nil
}

// TenantReader is the narrow permission to read every task of a tenant and
// its tenant-wide event stream. Without it a principal reads only the tasks it
// owns. It is set only through trusted provisioning.
type TenantReader struct {
	Tenant    ids.TenantID    `json:"tenant_id"`
	Principal ids.PrincipalID `json:"principal_id"`
	Enabled   bool            `json:"enabled"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Validate reports whether every field is set.
func (r TenantReader) Validate() error {
	if r.Tenant.IsZero() || r.Principal.IsZero() {
		return fmt.Errorf("%w: tenant reader requires tenant and principal", ErrInvalid)
	}
	if r.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: tenant reader requires its update time", ErrInvalid)
	}
	return nil
}
