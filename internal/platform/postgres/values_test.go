package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

func TestIsUniqueViolationMatchesConstraint(t *testing.T) {
	conflict := &pgconn.PgError{Code: "23505", ConstraintName: "task_pkey"}
	if !isUniqueViolation(conflict) {
		t.Fatal("isUniqueViolation() = false for 23505")
	}
	if !isUniqueViolation(conflict, "task_pkey") {
		t.Fatal("isUniqueViolation() = false for the named constraint")
	}
	if isUniqueViolation(conflict, "other_constraint") {
		t.Fatal("isUniqueViolation() = true for a different constraint")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503"}) {
		t.Fatal("isUniqueViolation() = true for a foreign-key violation")
	}
	if isUniqueViolation(nil) {
		t.Fatal("isUniqueViolation() = true for nil")
	}
}

func TestParseIDRoundTrip(t *testing.T) {
	id, err := ids.New[ids.Task]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	parsed, err := parseID[ids.Task](`task.task_id`, id.String())
	if err != nil {
		t.Fatalf("parseID() error = %v", err)
	}
	if parsed != id {
		t.Fatalf("parseID() = %v, want %v", parsed, id)
	}
	if _, err := parseID[ids.Task](`task.task_id`, "not-a-uuid"); err == nil {
		t.Fatal("parseID() accepted garbage")
	}
	if _, err := parseOptionalID[ids.Task](`task.task_id`, nil); err != nil {
		t.Fatalf("parseOptionalID(nil) error = %v", err)
	}
}

func TestUUIDHelpers(t *testing.T) {
	id, err := ids.New[ids.Principal]()
	if err != nil {
		t.Fatalf("ids.New() error = %v", err)
	}
	if uuidOrNil(ids.PrincipalID{}) != nil {
		t.Fatal("uuidOrNil() non-nil for zero id")
	}
	if uuidOrNil(id) != id.String() {
		t.Fatal("uuidOrNil() did not render the id")
	}
	if textOrNil("") != nil || textOrNil("x") != "x" {
		t.Fatal("textOrNil() mishandled empty or set text")
	}
	arrayed := formatResponders([]ids.PrincipalID{id})
	parsed, err := parseUUIDArray[ids.Principal](`responders`, arrayed)
	if err != nil {
		t.Fatalf("parseUUIDArray() error = %v", err)
	}
	if len(parsed) != 1 || parsed[0] != id {
		t.Fatalf("parseUUIDArray() = %v, want [%v]", parsed, id)
	}
}
