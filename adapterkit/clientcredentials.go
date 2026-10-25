package adapterkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const maxSecretFileBytes = 16 << 10

// ClientCredentialsConfig names an OAuth 2.0 client-credentials grant
// (RFC 6749 §4.4) at the identity provider ADR-0008 trusts for adapters.
type ClientCredentialsConfig struct {
	// TokenURL is the provider's token endpoint.
	TokenURL string
	// ClientID identifies the adapter; it is the subject its binding names.
	ClientID string
	// SecretFile holds the client secret. It is read on every fetch, so a
	// rotated secret applies to the next token without a restart.
	SecretFile string
	// Scopes are requested with the token; providers commonly derive the
	// audience, the adapter endpoint's identifier, from them.
	Scopes []string
	// HTTPClient reaches the token endpoint; nil uses a client with a
	// timeout and the system roots.
	HTTPClient *http.Client
}

// ClientCredentials returns a TokenSource that fetches a fresh access token
// for every connection and renewal, the way an adapter authenticates as a
// scoped service with short-lived, rotated credentials. Errors never include
// the secret or the token.
func ClientCredentials(config ClientCredentialsConfig) (TokenSource, error) {
	if config.TokenURL == "" || config.ClientID == "" || config.SecretFile == "" {
		return nil, errors.New("adapterkit: token URL, client ID and secret file are required")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return func(ctx context.Context) (Token, error) {
		secret, err := readSecret(config.SecretFile)
		if err != nil {
			return Token{}, err
		}
		grant := clientcredentials.Config{
			ClientID: config.ClientID, ClientSecret: secret, TokenURL: config.TokenURL,
			Scopes: config.Scopes, AuthStyle: oauth2.AuthStyleInHeader,
		}
		token, err := grant.Token(context.WithValue(ctx, oauth2.HTTPClient, client))
		if err != nil {
			return Token{}, errors.New("adapterkit: the token endpoint refused the client credentials")
		}
		if token.AccessToken == "" || token.Expiry.IsZero() {
			return Token{}, errors.New("adapterkit: the token endpoint returned no access token with an expiry")
		}
		return Token{Value: token.AccessToken, Expiry: token.Expiry}, nil
	}, nil
}

func readSecret(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("adapterkit: cannot open the client secret file")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil || len(data) > maxSecretFileBytes {
		return "", errors.New("adapterkit: cannot read the client secret file")
	}
	secret := string(bytes.TrimRight(data, "\r\n"))
	if secret == "" {
		return "", errors.New("adapterkit: the client secret file is empty")
	}
	return secret, nil
}
