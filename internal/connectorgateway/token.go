// Package connectorgateway serves the attempt-scoped connector MCP endpoint.
package connectorgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/dac-plane/dac-plane/internal/ids"
)

// Tokens derives replay-stable gateway tokens. The key is shared by replicas,
// held outside PostgreSQL and distinct from adapter and external credentials.
type Tokens struct{ key []byte }

func NewTokens(key []byte) (*Tokens, error) {
	if len(key) < 32 {
		return nil, errors.New("gateway signing key must contain at least 32 bytes")
	}
	return &Tokens{key: append([]byte(nil), key...)}, nil
}
func (t *Tokens) mac(subject string) []byte {
	h := hmac.New(sha256.New, t.key)
	h.Write([]byte("dac-plane/connector-gateway/v1:" + subject))
	return h.Sum(nil)
}
func (t *Tokens) Issue(tenant ids.TenantID, attempt ids.AttemptID) string {
	subject := "dacg1." + tenant.String() + "." + attempt.String()
	return subject + "." + base64.RawURLEncoding.EncodeToString(t.mac(subject))
}
func (t *Tokens) Verify(token string) (ids.TenantID, ids.AttemptID, error) {
	var tenant ids.TenantID
	var attempt ids.AttemptID
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "dacg1" {
		return tenant, attempt, errors.New("invalid gateway token")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(mac, t.mac(strings.Join(parts[:3], "."))) {
		return tenant, attempt, errors.New("invalid gateway token")
	}
	tenant, err = ids.Parse[ids.Tenant](parts[1])
	if err != nil {
		return tenant, attempt, errors.New("invalid gateway token")
	}
	attempt, err = ids.Parse[ids.Attempt](parts[2])
	return tenant, attempt, err
}
