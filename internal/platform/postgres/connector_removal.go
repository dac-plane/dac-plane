package postgres

import (
	"context"

	"github.com/dac-plane/dac-plane/internal/ids"
)

// NextConnectionRemovals rotates a bounded batch before contacting adapters,
// so an unavailable adapter cannot indefinitely hide later destruction intents.
// Scheduling metadata does not change the connection's lifecycle version.
func NextConnectionRemovals(ctx context.Context, q Querier, tenant ids.TenantID) ([]ids.ConnectionID, error) {
	rows, err := q.Query(ctx, `with next as (
  select tenant_id, connection_id from connection
  where tenant_id=$1 and state='removing'
  order by revoke_attempted_at, updated_at, connection_id
  limit 10 for update skip locked
 )
 update connection c set revoke_attempted_at=statement_timestamp()
 from next n where c.tenant_id=n.tenant_id and c.connection_id=n.connection_id
 returning c.connection_id::text`, tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ids.ConnectionID
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		id, err := parseID[ids.Connection]("connection.connection_id", text)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
