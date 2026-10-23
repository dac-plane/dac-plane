package provision

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunRefusesInvalidArgumentsBeforeOpeningTheDatabase(t *testing.T) {
	const tenant = "01a167fd-51d9-76ba-b563-496e2bd89646"
	const principal = "01a167fd-51d9-76be-8ef5-39a7e14723c0"
	for name, args := range map[string][]string{
		"no operation":            nil,
		"unknown operation":       {"delete-tenant"},
		"tenant with a stray":     {"tenant", "extra"},
		"tenant malformed id":     {"tenant", "-id", "not-a-uuid"},
		"tenant non-v7 id":        {"tenant", "-id", "01928f6e-7c3a-4b1e-9f00-0123456789ab"},
		"principal without kind":  {"principal", "-tenant", tenant},
		"principal stray issuer":  {"principal", "-tenant", tenant, "-kind", "human", "-issuer", "https://a.example"},
		"bind without subject":    {"bind", "-tenant", tenant, "-principal", principal, "-issuer", "https://a.example"},
		"unbind with principal":   {"unbind", "-tenant", tenant, "-principal", principal, "-issuer", "https://a.example", "-subject", "s"},
		"grant without tenant":    {"grant-reader", "-principal", principal},
		"unknown flag":            {"tenant", "-password", "x"},
		"principal unknown kind":  {"principal", "-tenant", tenant, "-kind", "admin"},
		"bind plaintext issuer":   {"bind", "-tenant", tenant, "-principal", principal, "-issuer", "http://a.example", "-subject", "s"},
		"bind issuer with query":  {"bind", "-tenant", tenant, "-principal", principal, "-issuer", "https://a.example/?x", "-subject", "s"},
		"bind subject with space": {"bind", "-tenant", tenant, "-principal", principal, "-issuer", "https://a.example", "-subject", "a b"},
		"unbind empty subject":    {"unbind", "-tenant", tenant, "-issuer", "https://a.example", "-subject", ""},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			opened := false
			open := func(context.Context) (*Provisioner, func(), error) {
				opened = true
				return nil, nil, errors.New("must not open")
			}
			if code := Run(context.Background(), args, &stdout, &stderr, open); code != ExitUsage {
				t.Fatalf("Run() = %d, want %d; stderr %s", code, ExitUsage, stderr.String())
			}
			if opened {
				t.Fatal("database opened for invalid arguments")
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: dac-plane provision") {
				t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunReportsOpenFailures(t *testing.T) {
	var stdout, stderr bytes.Buffer
	open := func(context.Context) (*Provisioner, func(), error) {
		return nil, nil, errors.New("DAC_DATABASE_URL is required")
	}
	if code := Run(context.Background(), []string{"tenant"}, &stdout, &stderr, open); code != ExitError {
		t.Fatalf("Run() = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "DAC_DATABASE_URL is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
