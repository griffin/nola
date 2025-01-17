package registry

import (
	"context"

	"github.com/richardartoul/nola/virtual/types"
)

// Registry is the interface that is implemented by the virtual actor registry.
type Registry interface {
	ActorStorage
	ServiceDiscovery

	// RegisterModule registers the provided module []byte and options with the
	// provided module ID for subsequent calls to CreateActor().
	RegisterModule(
		ctx context.Context,
		namespace,
		moduleID string,
		moduleBytes []byte,
		opts ModuleOptions,
	) (RegisterModuleResult, error)

	// GetModule gets the bytes and options associated with the provided module.
	GetModule(
		ctx context.Context,
		namespace,
		moduleID string,
	) ([]byte, ModuleOptions, error)

	// CreateActor creates a new actor in the given namespace from the provided module
	// ID.
	CreateActor(
		ctx context.Context,
		namespace,
		actorID,
		moduleID string,
		opts types.ActorOptions,
	) (CreateActorResult, error)

	// IncGeneration increments the actor's generation count. This is useful for ensuring
	// that all actor activations are invalidated and recreated.
	IncGeneration(
		ctx context.Context,
		namespace,
		actorID string,
	) error

	// EnsureActivation checks the registry to see if the provided actor is already
	// activated, and if so it returns an ActorReference that points to its activated
	// location. Otherwise, the registry will pick a location to activate the actor at
	// and then return an ActorReference that points to the newly selected location.
	//
	// Note that when this method returns it is guaranteed that a location will have
	// been selected for the actor to be activated at, but the actor may not necessarily
	// have been activated. In general, actor activation is handled "lazily" when a
	// location (server) receives its first invocation for an actor ID that it doesn't
	// currently have activated.
	EnsureActivation(
		ctx context.Context,
		namespace,
		actorID string,
	) ([]types.ActorReference, error)

	// GetVersionStamp() returns a monotonically increasing integer that should increase
	// at a rate of ~ 1 million/s.
	GetVersionStamp(ctx context.Context) (int64, error)

	// Close closes the registry and releases any resources associated (DB connections, etc).
	Close(ctx context.Context) error

	// UnsafeWipeAll wipes the entire registry. Only used for tests. Do not call it anywhere
	// in production code.
	UnsafeWipeAll() error
}

// ActorStorage contains the methods for interacting with per-actor durable storage.
type ActorStorage interface {
	// BeginTransaction eagerly begins a transaction that allows the Actor to read/write
	// its KV storage in a transactional manner.
	BeginTransaction(
		ctx context.Context,
		namespace string,
		actorID string,

		serverID string,
		serverVersion int64,
	) (ActorKVTransaction, error)
}

// ActorKVTransaction is the interface exposed by the Registry to Actors so they can perform
// transactions against the actor-local KV storage.
type ActorKVTransaction interface {
	// Put stores the value at the provided key in the actor's KV storage.
	Put(ctx context.Context, key []byte, value []byte) error
	// Get is the inverse of Put.
	Get(ctx context.Context, key []byte) ([]byte, bool, error)
	// Commit commits the transaction, persisting all Put/Get operations.
	Commit(ctx context.Context) error
	// Cancel cancels the transaction, rolling back all Put/Get operations.
	Cancel(ctx context.Context) error
}

// ServiceDiscovery contains the methods for interacting with the Registry's service
// discovery mechanism.
type ServiceDiscovery interface {
	// Heartbeat updates the "lastHeartbeatedAt" value for the provided server ID. Server's
	// must heartbeat regularly to be considered alive and eligible for hosting actor
	// activations.
	Heartbeat(
		ctx context.Context,
		serverID string,
		state HeartbeatState,
	) (HeartbeatResult, error)
}

// CreateActorResult is the result of a call to CreateActor().
type CreateActorResult struct{}

// ModuleOptions contains the options for a given module.
type ModuleOptions struct {
	// AllowEmptyModuleBytes allows a module to be created with empty WASM bytes. This is
	// useful in the scenario where NOLA is being used as a library and the Actor's are
	// implemented in Go instead of WASM.
	AllowEmptyModuleBytes bool
}

// RegisterModuleResult is the result of a call to RegisterModule().
type RegisterModuleResult struct{}

// HeartbeatState contains information that accompanies a server's heartbeat. It contains
// various information about the current state of the server that might be useful to the
// registry. For example, the number of currently activated actors on the server is useful
// to the registry so it can load-balance future actor activations around the cluster to
// achieve uniformity.
//
// TODO: This should include things like how many CPU seconds and memory the actors are
//
//	using, etc for hotspot detection.
type HeartbeatState struct {
	// NumActivatedActors is the number of actors currently activated on the server.
	NumActivatedActors int
	// Address is the address at which the server can be reached.
	Address string
}

// HeartbeatResult is the result returned by the Heartbeat() method.
type HeartbeatResult struct {
	// VersionStamp associated with the successful heartbeat.
	VersionStamp int64
	// TTL of the successful heartbeat in the same unit as the
	// VerisionStamp.
	HeartbeatTTL int64
	// ServerVersion is incremented every time a server's heartbeat expires and resumes,
	// guaranteeing the server's ability to identify periods of inactivity/death for correctness purposes.
	ServerVersion int64
}
