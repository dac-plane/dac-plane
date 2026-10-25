package adapterkit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxTokenFileBytes = 16 << 10

// FileToken returns a TokenSource that reads a JWT access token from path on
// every connection, so a sidecar or the platform can refresh the file. The
// expiry is read from the token's exp claim only to schedule renewal; the
// control plane verifies the token. Errors never include the token.
func FileToken(path string) TokenSource {
	return func(context.Context) (Token, error) {
		file, err := os.Open(path)
		if err != nil {
			return Token{}, errors.New("adapterkit: cannot open the token file")
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(io.LimitReader(file, maxTokenFileBytes+1))
		if err != nil || len(data) > maxTokenFileBytes {
			return Token{}, errors.New("adapterkit: cannot read the token file")
		}
		value := string(bytes.TrimSpace(data))
		expiry, err := tokenExpiry(value)
		if err != nil {
			return Token{}, err
		}
		return Token{Value: value, Expiry: expiry}, nil
	}
}

func tokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("adapterkit: the token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("adapterkit: the token payload is not base64url")
	}
	var claims struct {
		Expiry *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Expiry == nil {
		return time.Time{}, fmt.Errorf("adapterkit: the token carries no exp claim")
	}
	return time.Unix(*claims.Expiry, 0), nil
}
