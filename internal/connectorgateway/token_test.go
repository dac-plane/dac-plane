package connectorgateway

import (
	"bytes"
	"github.com/dac-plane/dac-plane/internal/ids"
	"strings"
	"testing"
)

func TestTokensBindAudienceTenantAndAttempt(t *testing.T) {
	tokens, err := NewTokens(bytes.Repeat([]byte("a"), 32))
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := ids.New[ids.Tenant]()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := ids.New[ids.Attempt]()
	if err != nil {
		t.Fatal(err)
	}
	token := tokens.Issue(tenant, attempt)
	if tokens.Issue(tenant, attempt) != token {
		t.Fatal("token is not replay stable")
	}
	gotTenant, gotAttempt, err := tokens.Verify(token)
	if err != nil || gotTenant != tenant || gotAttempt != attempt {
		t.Fatal("token did not verify")
	}
	other, err := NewTokens(bytes.Repeat([]byte("b"), 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Verify(token); err == nil {
		t.Fatal("foreign key accepted")
	}
	for _, bad := range []string{"", token + "x", strings.Replace(token, "dacg1", "other", 1), strings.Replace(token, attempt.String(), tenant.String(), 1)} {
		if _, _, err := tokens.Verify(bad); err == nil {
			t.Fatal("tampered token accepted")
		}
	}
	if _, err := NewTokens([]byte("short")); err == nil {
		t.Fatal("short signing key accepted")
	}
}
