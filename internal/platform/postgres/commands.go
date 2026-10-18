package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// CommandRecord binds one client nonce, scoped to its tenant, principal and
// command kind, to the fingerprint of the request it first carried and to the
// facts of the outcome. A retry with the same fingerprint replays Result; a
// different fingerprint is a payload mismatch.
type CommandRecord struct {
	Tenant        ids.TenantID
	Principal     ids.PrincipalID
	Command       string
	Nonce         string
	RequestDigest identity.Digest
	Result        json.RawMessage
	CreatedAt     time.Time
}

// InsertCommandRecord writes a placeholder record with an empty result at the
// start of a command. It reports inserted=false when the key already exists:
// a committed record, or a concurrent duplicate that committed while this
// insert waited on the primary key. The caller then rolls back and replays.
func InsertCommandRecord(ctx context.Context, u *UnitOfWork, record CommandRecord) (bool, error) {
	if err := record.RequestDigest.Validate(); err != nil {
		return false, err
	}
	if record.Command == "" || record.Nonce == "" || record.CreatedAt.IsZero() {
		return false, errors.New("command record requires a command, a nonce and a time")
	}
	return insertReturning(ctx, u, `
		insert into command_record (tenant_id, principal_id, command, nonce, request_digest, result, created_at)
		values ($1, $2, $3, $4, $5, '{}'::jsonb, $6)`,
		record.Tenant.String(), record.Principal.String(), record.Command, record.Nonce,
		string(record.RequestDigest), record.CreatedAt,
	)
}

// SetCommandResult fills the outcome of a command inserted in the same unit
// of work. It runs once, before commit; no code path changes a committed
// record.
func SetCommandResult(ctx context.Context, u *UnitOfWork, tenant ids.TenantID, principal ids.PrincipalID, command, nonce string, result json.RawMessage) error {
	tag, err := u.Exec(ctx, `
		update command_record set result = $5::jsonb
		where tenant_id = $1 and principal_id = $2 and command = $3 and nonce = $4`,
		tenant.String(), principal.String(), command, nonce, []byte(result),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// GetCommandRecord loads a committed record. Records of other tenants,
// principals or command kinds read as not found.
func GetCommandRecord(ctx context.Context, q Querier, tenant ids.TenantID, principal ids.PrincipalID, command, nonce string) (CommandRecord, error) {
	record := CommandRecord{Tenant: tenant, Principal: principal, Command: command, Nonce: nonce}
	var digest string
	var result []byte
	err := q.QueryRow(ctx, `
		select request_digest, result, created_at from command_record
		where tenant_id = $1 and principal_id = $2 and command = $3 and nonce = $4`,
		tenant.String(), principal.String(), command, nonce,
	).Scan(&digest, &result, &record.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return CommandRecord{}, ErrNotFound
		}
		return CommandRecord{}, err
	}
	record.RequestDigest = identity.Digest(digest)
	if err := record.RequestDigest.Validate(); err != nil {
		return CommandRecord{}, corruptRow(`command_record.request_digest`, err)
	}
	record.Result = result
	return record, nil
}
