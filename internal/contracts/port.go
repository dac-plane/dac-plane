package contracts

import (
	"context"
	"errors"
)

// ErrNotDelivered reports that a transport wrote nothing of a request or
// cancel: no connection could carry it. Nothing can have taken effect, so the
// caller may release the work and send it again later. Any other Send error
// leaves the outcome uncertain.
var ErrNotDelivered = errors.New("not delivered to the provider")

// Role is the part a provider plays behind a port.
type Role string

const (
	RoleRuntime       Role = "runtime"
	RoleEnvironment   Role = "environment"
	RoleEnforcement   Role = "enforcement"
	RoleArtifactStore Role = "artifact_store"
	RoleConnector     Role = "connector"
)

// Roles lists every Role.
func Roles() []Role {
	return []Role{RoleRuntime, RoleEnvironment, RoleEnforcement, RoleArtifactStore, RoleConnector}
}

// Method is an operation a provider in some role performs.
type Method string

const (
	MethodStartTurn       Method = "start_turn"
	MethodSteerTurn       Method = "steer_turn"
	MethodInterrupt       Method = "interrupt"
	MethodExportArtifacts Method = "export_artifacts"

	MethodProvision Method = "provision"
	MethodStop      Method = "stop"
	MethodObserve   Method = "observe"

	MethodPreAction     Method = "pre_action"
	MethodExecuteAction Method = "execute_action"
	MethodPostAction    Method = "post_action"
	MethodActionFailed  Method = "action_failed"

	MethodAuthorizeUpload Method = "authorize_upload"
	MethodStatObject      Method = "stat_object"
	MethodCommitManifest  Method = "commit_manifest"
	// MethodAuthorizeDownload presigns a read of one committed artifact
	// version (ADR-0025). It is sent only to an adapter that selected
	// CapabilityArtifactDownload.
	MethodAuthorizeDownload Method = "authorize_download"

	MethodListTools             Method = "list_tools"
	MethodBeginAuthorization    Method = "begin_authorization"
	MethodCompleteAuthorization Method = "complete_authorization"
	MethodRevokeConnection      Method = "revoke_connection"
	MethodCallTool              Method = "call_tool"
	MethodReadCall              Method = "read_call"
)

// Methods is the catalogue of operations for a role; any other method is an
// unknown operation for that role.
func Methods(role Role) []Method {
	switch role {
	case RoleRuntime:
		return []Method{MethodStartTurn, MethodSteerTurn, MethodInterrupt, MethodExportArtifacts}
	case RoleEnvironment:
		return []Method{MethodProvision, MethodStop, MethodObserve}
	case RoleEnforcement:
		return []Method{MethodPreAction, MethodPostAction, MethodActionFailed, MethodExecuteAction}
	case RoleArtifactStore:
		return []Method{MethodAuthorizeUpload, MethodStatObject, MethodCommitManifest, MethodAuthorizeDownload}
	case RoleConnector:
		return []Method{MethodListTools, MethodBeginAuthorization, MethodCompleteAuthorization, MethodRevokeConnection, MethodCallTool, MethodReadCall}
	}
	return nil
}

// AllMethods lists every method of every role, in role order.
func AllMethods() []Method {
	var all []Method
	for _, role := range Roles() {
		all = append(all, Methods(role)...)
	}
	return all
}

// Port is the transport-neutral boundary to one provider. Send delivers a
// request and returns its reply; a transport that loses the reply after
// delivery must return an error, never a guessed outcome, so the operation
// is reconciled instead of resent. Only a transport that wrote nothing may
// return ErrNotDelivered. Cancel asks for the operation to stop;
// the stop is confirmed by observation, not by Cancel returning. Subscribe
// opens the provider's event stream from a cursor.
type Port interface {
	Role() Role
	Send(ctx context.Context, request Request) (Reply, error)
	Cancel(ctx context.Context, cancel Cancel) error
	Subscribe(ctx context.Context, cursor Cursor) (Stream, error)
}

// StableOperationReplayer is implemented only by a negotiated provider port
// whose adapter selected CapabilityOperationReplay. Replay delivers the same
// stable operation and payload under a fresh delivery identity after worker
// takeover; the adapter is responsible for operation-keyed idempotency.
type StableOperationReplayer interface {
	Port
	Replay(ctx context.Context, request Request) (Reply, error)
}

// CapabilityScoped is implemented by a negotiated provider port whose adapter
// connections each select their own optional capabilities. Requiring returns
// the port restricted to connections that selected capability, or nil when no
// open connection did. The restricted port is a StableOperationReplayer only
// while one of those connections also selected CapabilityOperationReplay.
type CapabilityScoped interface {
	Port
	Requiring(capability Capability) Port
}

// Stream yields stream frames until the context ends or the provider closes.
type Stream interface {
	Next(ctx context.Context) (StreamFrame, error)
	Close() error
}
