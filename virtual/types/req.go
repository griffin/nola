package types

// InvokeActorRequest is the JSON struct that represents a request from an existing
// actor to invoke an operation on another one.
type InvokeActorRequest struct {
	// ActorID is the ID of the target actor. Omit when being used inside of
	// ScheduleInvocationRequest to target self.
	ActorID string `json:"actor_id"`
	// Operation is the name of the operation to invoke on the target actor.
	Operation string `json:"operation"`
	// Payload is the []byte payload to provide to the invoked function on the
	// target actor.
	Payload []byte `json:"payload"`
	// CreateIfNotExist provides the arguments for InvokeActorRequest to construct the
	// actor if it doesn't already exist. This field is optional.
	CreateIfNotExist CreateIfNotExist `json:"create_if_not_exist"`
}

// CreateIfNotExist provides the arguments for InvokeActorRequest to construct the
// actor if it doesn't already exist.
type CreateIfNotExist struct {
	ModuleID string       `json:"module_id"`
	Options  ActorOptions `json:"actor_options"`
}

// ActorOptions contains the options for a given actor.
type ActorOptions struct {
}
