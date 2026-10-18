package simulator

import (
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// Comparison is the parity between a primary and a shadow effect log: pairs
// that agree, pairs that disagree, and effects only one side applied.
type Comparison struct {
	Matched    int
	Mismatched int
	Missing    int
	Extra      int
}

// Balanced reports whether the shadow applied exactly the external effects
// the primary did. The comparison is a promotion gate: a shadow that diverges
// has not proven it may take traffic.
func (c Comparison) Balanced() bool {
	return c.Mismatched == 0 && c.Missing == 0 && c.Extra == 0
}

// Summary is the canonical one-line description of the comparison.
func (c Comparison) Summary() string {
	return fmt.Sprintf("matched=%d mismatched=%d missing=%d extra=%d", c.Matched, c.Mismatched, c.Missing, c.Extra)
}

// Digest is the canonical digest of the summary, so the same evidence always
// records the same value.
func (c Comparison) Digest() identity.Digest {
	return identity.DigestOf([]byte(c.Summary()))
}

// Compare pairs two effect logs by operation: within one operation the nth
// primary effect pairs with the nth shadow effect, where the same kind is a
// match and a different kind is a mismatch; effects without a counterpart are
// missing from the shadow or extra in it. Operations are walked in the order
// they first appear in either log, so the comparison of two logs is
// deterministic.
func Compare(primary, shadow []Effect) Comparison {
	primaryByOperation := groupEffects(primary)
	shadowByOperation := groupEffects(shadow)
	var comparison Comparison
	for _, operation := range operationsInOrder(primary, shadow) {
		primaryEffects := primaryByOperation[operation]
		shadowEffects := shadowByOperation[operation]
		pairs := min(len(primaryEffects), len(shadowEffects))
		for i := range pairs {
			if primaryEffects[i].Kind == shadowEffects[i].Kind {
				comparison.Matched++
			} else {
				comparison.Mismatched++
			}
		}
		comparison.Missing += len(primaryEffects) - pairs
		comparison.Extra += len(shadowEffects) - pairs
	}
	return comparison
}

// groupEffects indexes an effect log by operation, preserving each
// operation's arrival order.
func groupEffects(effects []Effect) map[ids.OperationID][]Effect {
	grouped := make(map[ids.OperationID][]Effect, len(effects))
	for _, effect := range effects {
		grouped[effect.Operation] = append(grouped[effect.Operation], effect)
	}
	return grouped
}

// operationsInOrder returns every operation in the logs, first appearance
// first.
func operationsInOrder(primary, shadow []Effect) []ids.OperationID {
	seen := make(map[ids.OperationID]bool, len(primary)+len(shadow))
	order := make([]ids.OperationID, 0, len(primary)+len(shadow))
	for _, effects := range [][]Effect{primary, shadow} {
		for _, effect := range effects {
			if !seen[effect.Operation] {
				seen[effect.Operation] = true
				order = append(order, effect.Operation)
			}
		}
	}
	return order
}
