package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsInvalidConfigurationWithoutExposingCredentials(t *testing.T) {
	const secret = "database-password-that-must-not-leak"

	pool, err := Open(context.Background(), "postgres://user:"+secret+"@%")
	if pool != nil {
		pool.Close()
		t.Fatal("Open() pool is non-nil for invalid configuration")
	}
	if err == nil {
		t.Fatal("Open() error = nil, want configuration error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Open() error exposed credential: %v", err)
	}
}

func TestOpenReportsConnectionFailureWithoutExposingCredentials(t *testing.T) {
	const secret = "database-password-that-must-not-leak"

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	pool, err := Open(ctx, "postgres://user:"+secret+"@127.0.0.1:1/database?sslmode=disable")
	if pool != nil {
		pool.Close()
		t.Fatal("Open() pool is non-nil for connection failure")
	}
	if err == nil {
		t.Fatal("Open() error = nil, want connection error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Open() error exposed credential: %v", err)
	}
}
