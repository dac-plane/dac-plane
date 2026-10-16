// Package ids defines the opaque, typed identifiers of product records.
//
// Every identifier is a UUIDv7 in canonical lowercase form. A distinct type per
// record kind prevents one kind of identifier from being used as another.
package ids

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrInvalid reports an identifier that is not a canonical, non-nil UUIDv7.
var ErrInvalid = errors.New("invalid identifier")

// Kind markers. They exist only to make ID types distinct.
type (
	Tenant      struct{}
	Principal   struct{}
	Attempt     struct{}
	Provider    struct{}
	Operation   struct{}
	Grant       struct{}
	Task        struct{}
	Attention   struct{}
	Decision    struct{}
	Audit       struct{}
	Migration   struct{}
	Claim       struct{}
	Budget      struct{}
	Reservation struct{}
	Spend       struct{}
	Artifact    struct{}
	Revision    struct{}
	Review      struct{}
	// Any marks an identifier whose record kind is carried alongside it, such
	// as the subject of an audit event.
	Any struct{}
)

// Typed identifiers for product records.
type (
	TenantID      = ID[Tenant]
	PrincipalID   = ID[Principal]
	AttemptID     = ID[Attempt]
	ProviderID    = ID[Provider]
	OperationID   = ID[Operation]
	GrantID       = ID[Grant]
	TaskID        = ID[Task]
	AttentionID   = ID[Attention]
	DecisionID    = ID[Decision]
	AuditID       = ID[Audit]
	MigrationID   = ID[Migration]
	ClaimID       = ID[Claim]
	BudgetID      = ID[Budget]
	ReservationID = ID[Reservation]
	SpendID       = ID[Spend]
	ArtifactID    = ID[Artifact]
	RevisionID    = ID[Revision]
	ReviewID      = ID[Review]
	AnyID         = ID[Any]
)

// ID is an opaque UUIDv7 identifier of kind K. The zero value is invalid.
type ID[K any] struct {
	value uuid.UUID
}

// New returns a new time-ordered identifier.
func New[K any]() (ID[K], error) {
	value, err := uuid.NewV7()
	if err != nil {
		return ID[K]{}, fmt.Errorf("generate identifier: %w", err)
	}
	return ID[K]{value: value}, nil
}

// Parse accepts only the canonical lowercase form of a non-nil UUIDv7.
func Parse[K any](text string) (ID[K], error) {
	value, err := uuid.Parse(text)
	if err != nil || value.Version() != 7 || value.String() != text {
		return ID[K]{}, ErrInvalid
	}
	return ID[K]{value: value}, nil
}

// IsZero reports whether the identifier is unset.
func (id ID[K]) IsZero() bool {
	return id.value == uuid.Nil
}

// String returns the canonical form, or the empty string for the zero value.
func (id ID[K]) String() string {
	if id.IsZero() {
		return ""
	}
	return id.value.String()
}

// MarshalText refuses the zero value so an unset identifier never serializes.
func (id ID[K]) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, ErrInvalid
	}
	return []byte(id.value.String()), nil
}

// Untyped erases the kind for contexts that record it separately.
func (id ID[K]) Untyped() AnyID {
	return AnyID{value: id.value}
}

// UnmarshalText applies the same rules as Parse.
func (id *ID[K]) UnmarshalText(text []byte) error {
	parsed, err := Parse[K](string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
