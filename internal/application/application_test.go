package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

var unitNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func unitActor(t *testing.T) identity.AuthorizationContext {
	t.Helper()
	tenant, _ := ids.New[ids.Tenant]()
	principal, _ := ids.New[ids.Principal]()
	actor, err := identity.NewAuthorizationContext(identity.Principal{Tenant: tenant, ID: principal, Kind: identity.PrincipalHuman})
	if err != nil {
		t.Fatalf("NewAuthorizationContext() error = %v", err)
	}
	return actor
}

func validSubmit(t *testing.T) SubmitTask {
	t.Helper()
	provider, _ := ids.New[ids.Provider]()
	return SubmitTask{
		Command:  Command{Actor: unitActor(t), Nonce: "n-1"},
		At:       unitNow,
		Provider: provider,
		Demand:   contracts.DemandPerson,
		Deadline: unitNow.Add(time.Hour),
		Unit:     "tokens",
		Ceiling:  1000,
		Amount:   100,
	}
}

func TestSubmitValidation(t *testing.T) {
	if err := validSubmit(t).normalize().validate(); err != nil {
		t.Fatalf("valid submission: %v", err)
	}
	for name, mutate := range map[string]func(c *SubmitTask){
		"zero actor":          func(c *SubmitTask) { c.Actor = identity.AuthorizationContext{} },
		"empty nonce":         func(c *SubmitTask) { c.Nonce = "" },
		"over-long nonce":     func(c *SubmitTask) { c.Nonce = strings.Repeat("n", maxNonceLength+1) },
		"expected version":    func(c *SubmitTask) { c.ExpectedVersion = 1 },
		"no deadline":         func(c *SubmitTask) { c.Deadline = time.Time{} },
		"no command time":     func(c *SubmitTask) { c.At = time.Time{} },
		"no provider":         func(c *SubmitTask) { c.Provider = ids.ProviderID{} },
		"unknown demand":      func(c *SubmitTask) { c.Demand = "urgent" },
		"malformed unit":      func(c *SubmitTask) { c.Unit = "Tokens" },
		"zero amount":         func(c *SubmitTask) { c.Amount = 0 },
		"zero ceiling":        func(c *SubmitTask) { c.Ceiling = 0 },
		"amount beyond bound": func(c *SubmitTask) { c.Amount = 1 << 60 },
	} {
		c := validSubmit(t)
		mutate(&c)
		if err := c.normalize().validate(); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: validate() = %v, want ErrInvalidCommand", name, err)
		}
	}
}

func TestDeadlineIsCheckedOnlyForNewWork(t *testing.T) {
	late := validSubmit(t)
	late.At = late.Deadline
	// Envelope validation accepts a late retry, so it can replay a committed
	// submission; only creating new work compares the deadline.
	if err := late.normalize().validate(); err != nil {
		t.Fatalf("validate() rejected a late retry: %v", err)
	}
	if err := late.normalize().checkDeadline(); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("checkDeadline() = %v, want ErrInvalidCommand", err)
	}
	if err := validSubmit(t).normalize().checkDeadline(); err != nil {
		t.Fatalf("checkDeadline() on time = %v", err)
	}
}

func TestAssertValidationRequiresExpectedVersion(t *testing.T) {
	spend, _ := ids.New[ids.Spend]()
	cmd := AssertSpend{Command: Command{Actor: unitActor(t), Nonce: "a-1", ExpectedVersion: 1}, SubmissionNonce: "n-1"}
	cmd.Assertion.Challenge, cmd.Assertion.AssertedAt = spend, unitNow
	if err := cmd.validate(); err != nil {
		t.Fatalf("valid assertion command: %v", err)
	}
	for name, mutate := range map[string]func(c *AssertSpend){
		"no expected version": func(c *AssertSpend) { c.ExpectedVersion = 0 },
		"no submission":       func(c *AssertSpend) { c.SubmissionNonce = "" },
		"no challenge":        func(c *AssertSpend) { c.Assertion.Challenge = ids.SpendID{} },
		"no assertion time":   func(c *AssertSpend) { c.Assertion.AssertedAt = time.Time{} },
	} {
		c := cmd
		mutate(&c)
		if err := c.validate(); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: validate() = %v, want ErrInvalidCommand", name, err)
		}
	}
}

func submitDigest(t *testing.T, c SubmitTask) identity.Digest {
	t.Helper()
	c = c.normalize()
	digest, err := fingerprint(commandSubmitTask, submitPayload{
		Provider: c.Provider.String(), Demand: string(c.Demand), Deadline: c.Deadline.Format(time.RFC3339Nano),
		Unit: string(c.Unit), Ceiling: c.Ceiling, Amount: c.Amount, Spend: c.Spend,
	})
	if err != nil {
		t.Fatalf("fingerprint() error = %v", err)
	}
	return digest
}

func TestFingerprintIsCanonical(t *testing.T) {
	base := validSubmit(t)
	base.Demand = contracts.DemandBackground
	want := submitDigest(t, base)

	later := base
	later.At = base.At.Add(time.Minute)
	defaulted := base
	defaulted.Demand = ""
	zoned := base
	zoned.Deadline = base.Deadline.In(time.FixedZone("UTC+2", 2*60*60))
	for name, c := range map[string]SubmitTask{"later command time": later, "defaulted demand": defaulted, "other time zone": zoned} {
		if got := submitDigest(t, c); got != want {
			t.Errorf("%s: fingerprint changed", name)
		}
	}

	changed := base
	changed.Amount++
	governed := base
	governed.Spend = true
	for name, c := range map[string]SubmitTask{"amount": changed, "spend": governed} {
		if got := submitDigest(t, c); got == want {
			t.Errorf("%s: fingerprint did not change", name)
		}
	}
}
