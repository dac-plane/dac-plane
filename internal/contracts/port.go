package contracts

import "context"

// Role is the part a provider plays behind a port.
type Role string

const (
	RoleRuntime       Role = "runtime"
	RoleEnvironment   Role = "environment"
	RoleEnforcement   Role = "enforcement"
	RoleArtifactStore Role = "artifact_store"
)

// Roles lists every Role.
func Roles() []Role {
	return []Role{RoleRuntime, RoleEnvironment, RoleEnforcement, RoleArtifactStore}
}

// Method is an operation a provider in some role performs.
type Method string

const (
	MethodStartTurn       Method = "start_turn"
	MethodSteerTurn       Method = "steer_turn"
	MethodInterrupt       Method = "interrupt"
	MethodExportArtifacts Method = "export_artifacts"
	// Migration stages are explicit provider evidence. They never claim that a
	// native session was resumed; the only supported recovery is a new target
	// turn restarted from durable artifacts.
	MethodQuiesceTurn Method = "quiesce_turn"
	MethodRestartTurn Method = "restart_turn"

	MethodProvision Method = "provision"
	MethodStop      Method = "stop"
	MethodObserve   Method = "observe"
	MethodCleanup   Method = "cleanup"

	MethodPreAction     Method = "pre_action"
	MethodExecuteAction Method = "execute_action"
	MethodPostAction    Method = "post_action"
	MethodActionFailed  Method = "action_failed"

	MethodAuthorizeUpload Method = "authorize_upload"
	MethodStatObject      Method = "stat_object"
	MethodCommitManifest  Method = "commit_manifest"
)

// Methods is the catalogue of operations for a role; any other method is an
// unknown operation for that role.
func Methods(role Role) []Method {
	switch role {
	case RoleRuntime:
		return []Method{MethodStartTurn, MethodSteerTurn, MethodInterrupt, MethodExportArtifacts, MethodQuiesceTurn, MethodRestartTurn}
	case RoleEnvironment:
		return []Method{MethodProvision, MethodStop, MethodObserve, MethodCleanup}
	case RoleEnforcement:
		return []Method{MethodPreAction, MethodPostAction, MethodActionFailed, MethodExecuteAction}
	case RoleArtifactStore:
		return []Method{MethodAuthorizeUpload, MethodStatObject, MethodCommitManifest}
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
// is reconciled instead of resent. Cancel asks for the operation to stop;
// the stop is confirmed by observation, not by Cancel returning. Subscribe
// opens the provider's event stream from a cursor.
type Port interface {
	Role() Role
	Send(ctx context.Context, request Request) (Reply, error)
	Cancel(ctx context.Context, cancel Cancel) error
	Subscribe(ctx context.Context, cursor Cursor) (Stream, error)
}

// Stream yields stream frames until the context ends or the provider closes.
type Stream interface {
	Next(ctx context.Context) (StreamFrame, error)
	Close() error
}
