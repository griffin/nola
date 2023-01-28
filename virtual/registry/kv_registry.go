package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/richardartoul/nola/virtual/types"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"golang.org/x/sync/singleflight"
)

const (
	// HeartbeatTTL is the maximum amount of time between server heartbeats before
	// the registry will consider a server as dead.
	//
	// TODO: Should be configurable.
	HeartbeatTTL = 5 * time.Second
)

type kvRegistry struct {
	versionStampBatcher singleflight.Group

	// State.
	kv kv
}

func newKVRegistry(kv kv) Registry {
	return &kvRegistry{
		kv: kv,
	}
}

// TODO: Add compression?
func (k *kvRegistry) RegisterModule(
	ctx context.Context,
	namespace,
	moduleID string,
	moduleBytes []byte,
	opts ModuleOptions,
) (RegisterModuleResult, error) {
	r, err := k.kv.transact(func(tr transaction) (any, error) {
		_, ok, err := tr.get(ctx, getModulePartKey(namespace, moduleID, 0))
		if err != nil {
			return nil, err
		}
		if ok {
			if opts.AllowEmptyModuleBytes {
				// If empty module bytes are allowed then the fact that the
				// module already exists doesn't matter and we can just return
				// success. This makes it easier to register pure Go modules
				// on process startup each time without having to explicitly
				// handle "module already exists" errors.
				return RegisterModuleResult{}, nil
			}

			return RegisterModuleResult{}, fmt.Errorf(
				"error creating module: %s in namespace: %s, already exists",
				moduleID, namespace)
		}

		rm := registeredModule{
			Bytes: moduleBytes,
			Opts:  opts,
		}
		marshaled, err := json.Marshal(&rm)
		if err != nil {
			return nil, err
		}

		for i := 0; len(marshaled) > 0; i++ {
			// Maximum value size in FoundationDB is 100_000, so split anything larger
			// over multiple KV pairs.
			numBytes := 99_999
			if len(marshaled) < numBytes {
				numBytes = len(marshaled)
			}
			toWrite := marshaled[:numBytes]
			tr.put(ctx, getModulePartKey(namespace, moduleID, i), toWrite)
			marshaled = marshaled[numBytes:]
		}
		return RegisterModuleResult{}, err
	})
	if err != nil {
		return RegisterModuleResult{}, fmt.Errorf("RegisterModule: error: %w", err)
	}

	return r.(RegisterModuleResult), nil
}

// GetModule gets the bytes and options associated with the provided module.
func (k *kvRegistry) GetModule(
	ctx context.Context,
	namespace,
	moduleID string,
) ([]byte, ModuleOptions, error) {
	key := getModulePrefix(namespace, moduleID)
	r, err := k.kv.transact(func(tr transaction) (any, error) {
		var (
			moduleBytes []byte
			i           = 0
		)
		err := tr.iterPrefix(ctx, key, func(k, v []byte) error {
			moduleBytes = append(moduleBytes, v...)
			i++
			return nil
		})
		if err != nil {
			return ModuleOptions{}, err
		}
		if i == 0 {
			return ModuleOptions{}, fmt.Errorf(
				"error getting module: %s, does not exist in namespace: %s",
				moduleID, namespace)
		}

		rm := registeredModule{}
		if err := json.Unmarshal(moduleBytes, &rm); err != nil {
			return ModuleOptions{}, fmt.Errorf("error unmarshaling stored module: %w", err)
		}
		return rm, nil
	})
	if err != nil {
		return nil, ModuleOptions{}, fmt.Errorf("GetModule: error: %w", err)
	}

	result := r.(registeredModule)
	return result.Bytes, result.Opts, nil
}

func (k *kvRegistry) CreateActor(
	ctx context.Context,
	namespace,
	actorID,
	moduleID string,
	opts ActorOptions,
) (CreateActorResult, error) {
	var (
		actorKey  = getActorKey(namespace, actorID)
		moduleKey = getModulePartKey(namespace, moduleID, 0)
	)
	r, err := k.kv.transact(func(tr transaction) (any, error) {
		_, ok, err := tr.get(ctx, actorKey)
		if err != nil {
			return nil, err
		}
		if ok {
			return RegisterModuleResult{}, fmt.Errorf(
				"error creating actor with ID: %s, already exists in namespace: %s",
				actorID, namespace)
		}

		_, ok, err = tr.get(ctx, moduleKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			return RegisterModuleResult{}, fmt.Errorf(
				"error creating actor, module: %s does not exist in namespace: %s",
				moduleID, namespace)
		}

		ra := registeredActor{
			Opts:       opts,
			ModuleID:   moduleID,
			Generation: 1,
		}
		marshaled, err := json.Marshal(&ra)
		if err != nil {
			return nil, err
		}

		tr.put(ctx, actorKey, marshaled)
		return CreateActorResult{}, err
	})
	if err != nil {
		return CreateActorResult{}, fmt.Errorf("CreateActor: error: %w", err)
	}

	return r.(CreateActorResult), nil
}

func (k *kvRegistry) IncGeneration(
	ctx context.Context,
	namespace,
	actorID string,
) error {
	actorKey := getActorKey(namespace, actorID)
	_, err := k.kv.transact(func(tr transaction) (any, error) {
		actorBytes, ok, err := tr.get(ctx, actorKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			return RegisterModuleResult{}, fmt.Errorf(
				"error incrementing generation for actor with ID: %s, actor does not exist in namespace: %s",
				actorID, namespace)
		}

		var ra registeredActor
		if err := json.Unmarshal(actorBytes, &ra); err != nil {
			return nil, fmt.Errorf("error unmarshaling registered actor: %w", err)
		}

		ra.Generation++

		marshaled, err := json.Marshal(&ra)
		if err != nil {
			return nil, fmt.Errorf("error marshaling registered actor: %w", err)
		}

		tr.put(ctx, actorKey, marshaled)

		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("IncGeneration: error: %w", err)
	}

	return nil
}

func (k *kvRegistry) EnsureActivation(
	ctx context.Context,
	namespace,
	actorID string,
) ([]types.ActorReference, error) {
	actorKey := getActorKey(namespace, actorID)
	references, err := k.kv.transact(func(tr transaction) (any, error) {
		v, ok, err := tr.get(ctx, actorKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf(
				"error ensuring activation of actor with ID: %s, does not exist in namespace: %s",
				actorID, namespace)
		}

		var ra registeredActor
		if err := json.Unmarshal(v, &ra); err != nil {
			return nil, fmt.Errorf("error unmarsaling registered actor with ID: %s", actorID)
		}

		serverKey := getServerKey(ra.Activation.ServerID)
		v, ok, err = tr.get(ctx, serverKey)
		if err != nil {
			return nil, err
		}

		var (
			server       serverState
			serverExists bool
		)
		if ok {
			if err := json.Unmarshal(v, &server); err != nil {
				return nil, fmt.Errorf("error unmarsaling server state with ID: %s", actorID)
			}
			serverExists = true
		}

		vs, err := tr.getVersionStamp()
		if err != nil {
			return nil, fmt.Errorf("error getting versionstamp: %w", err)
		}

		var (
			currActivation, activationExists = ra.Activation, ra.Activation.ServerID != ""
			timeSinceLastHeartbeat           = versionSince(vs, server.LastHeartbeatedAt)
			serverID                         string
			serverAddress                    string
			serverVersion                    int64
		)
		if activationExists && serverExists && timeSinceLastHeartbeat < HeartbeatTTL {
			// We have an existing activation and the server is still alive, so just use that.

			// It is acceptable to look up the ServerVersion from the server discovery key directly,
			// as long as the activation is still active, it guarantees that the server's version
			// has not changed since the activation was first created.
			serverVersion = server.ServerVersion
			serverID = currActivation.ServerID
			serverAddress = server.HeartbeatState.Address
		} else {
			// We need to create a new activation.
			liveServers := []serverState{}
			err = tr.iterPrefix(ctx, getServersPrefix(), func(k, v []byte) error {
				var currServer serverState
				if err := json.Unmarshal(v, &currServer); err != nil {
					return fmt.Errorf("error unmarshaling server state: %w", err)
				}

				if versionSince(vs, currServer.LastHeartbeatedAt) < HeartbeatTTL {
					liveServers = append(liveServers, currServer)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if len(liveServers) == 0 {
				return nil, fmt.Errorf("0 live servers available for new activation")
			}

			// Pick the server with the lowest current number of activated actors to try and load-balance.
			// TODO: This is obviously insufficient and we should take other factors into account like
			//       memory / CPU usage.
			// TODO: We should also have some hard limits and just reject new activations at some point.
			sort.Slice(liveServers, func(i, j int) bool {
				return liveServers[i].HeartbeatState.NumActivatedActors < liveServers[j].HeartbeatState.NumActivatedActors
			})

			serverID = liveServers[0].ServerID
			serverAddress = liveServers[0].HeartbeatState.Address
			currActivation = activation{ServerID: serverID}
			serverVersion = liveServers[0].ServerVersion

			ra.Activation = currActivation
			marshaled, err := json.Marshal(&ra)
			if err != nil {
				return nil, fmt.Errorf("error marshaling activation: %w", err)
			}

			tr.put(ctx, actorKey, marshaled)
		}

		ref, err := types.NewActorReference(serverID, serverVersion, serverAddress, namespace, ra.ModuleID, actorID, ra.Generation)
		if err != nil {
			return nil, fmt.Errorf("error creating new actor reference: %w", err)
		}

		return []types.ActorReference{ref}, nil

	})
	if err != nil {
		return nil, fmt.Errorf("EnsureActivation: error: %w", err)
	}

	return references.([]types.ActorReference), nil
}

func (k *kvRegistry) GetVersionStamp(
	ctx context.Context,
) (int64, error) {
	// GetVersionStamp() is in the critical path of the entire system. It is
	// called extremely frequently. Caching it directly is unsafe and could lead
	// to correctness issues. Instead, we use a singleflight.Group to debounce/batch
	// calls to the underlying storage. This has the same effect as an extremely
	// short TTL cache, but with none of the correctness issues. In effect, the
	// system calls getVersionStamp() in a *single-threaded* loop as fast as it
	// can and each GetVersionStamp() call "gloms on" to the current outstanding
	// call (or initiates the next one if none is ongoing).
	//
	// We pass "" as the key because every call is the same.
	v, err, _ := k.versionStampBatcher.Do("", func() (any, error) {
		return k.kv.transact(func(tr transaction) (any, error) {
			return tr.getVersionStamp()
		})
	})
	if err != nil {
		return -1, fmt.Errorf("GetVersionStamp: error: %w", err)
	}

	return v.(int64), nil
}

func (k *kvRegistry) BeginTransaction(
	ctx context.Context,
	namespace string,
	actorID string,
) (ActorKVTransaction, error) {
	kvTr, err := k.kv.beginTransaction(ctx)
	if err != nil {
		return nil, fmt.Errorf("kvRegistry: beginTransaction: error beginning transaction: %w", err)
	}

	actorKey := getActorKey(namespace, actorID)
	// TODO: This is an expensive check to run each time, consider removing this if it becomes
	//       a bottleneck.
	_, ok, err := kvTr.get(ctx, actorKey)
	if err != nil {
		return nil, fmt.Errorf("kvRegistry: beginTransaction: error getting actor key: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("kvRegistry: beginTransaction: cannot perform KV Get for actor: %s that does not exist", actorID)
	}

	tr := newKVTransaction(ctx, namespace, actorID, kvTr)
	return tr, nil
}

func (k *kvRegistry) Heartbeat(
	ctx context.Context,
	serverID string,
	heartbeatState HeartbeatState,
) (HeartbeatResult, error) {
	key := getServerKey(serverID)
	var serverVersion int64
	versionStamp, err := k.kv.transact(func(tr transaction) (any, error) {
		v, ok, err := tr.get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("error getting server state: %w", err)
		}

		var state serverState
		if !ok {
			serverVersion = 1
			state = serverState{
				ServerID:      serverID,
				ServerVersion: serverVersion,
			}
			vs, err := tr.getVersionStamp()
			if err != nil {
				return nil, fmt.Errorf("error getting versionstamp: %w", err)
			}
			state.LastHeartbeatedAt = vs
		} else {
			if err := json.Unmarshal(v, &state); err != nil {
				return nil, fmt.Errorf("error unmarshaling server state: %w", err)
			}
		}

		vs, err := tr.getVersionStamp()
		if err != nil {
			return nil, fmt.Errorf("error getting versionstamp: %w", err)
		}
		timeSinceLastHeartbeat := versionSince(vs, state.LastHeartbeatedAt)
		if timeSinceLastHeartbeat >= HeartbeatTTL {
			state.ServerVersion++
		}

		serverVersion = state.ServerVersion
		state.LastHeartbeatedAt = vs
		state.HeartbeatState = heartbeatState

		marshaled, err := json.Marshal(&state)
		if err != nil {
			return nil, fmt.Errorf("error marshaling server state: %w", err)
		}

		tr.put(ctx, key, marshaled)

		return tr.getVersionStamp()
	})
	if err != nil {
		return HeartbeatResult{}, fmt.Errorf("Heartbeat: error: %w", err)
	}
	return HeartbeatResult{
		VersionStamp: versionStamp.(int64),
		// VersionStamp corresponds to ~ 1 million increments per second.
		HeartbeatTTL:  int64(HeartbeatTTL.Microseconds()),
		ServerVersion: serverVersion,
	}, nil
}

func (k *kvRegistry) Close(ctx context.Context) error {
	return k.kv.close(ctx)
}

func (k *kvRegistry) UnsafeWipeAll() error {
	return k.kv.unsafeWipeAll()
}

func getModulePrefix(namespace, moduleID string) []byte {
	return tuple.Tuple{namespace, "modules", moduleID}.Pack()
}

func getModulePartKey(namespace, moduleID string, part int) []byte {
	return tuple.Tuple{namespace, "modules", moduleID, part}.Pack()
}

func getActorKey(namespace, actorID string) []byte {
	return tuple.Tuple{namespace, "actors", actorID, "state"}.Pack()
}

func getActoKVKey(namespace, actorID string, key []byte) []byte {
	return tuple.Tuple{namespace, "actors", actorID, "kv", key}.Pack()
}

func getServerKey(serverID string) []byte {
	return tuple.Tuple{"servers", serverID}.Pack()
}

func getServersPrefix() []byte {
	return tuple.Tuple{"servers"}.Pack()
}

type registeredActor struct {
	Opts       ActorOptions
	ModuleID   string
	Generation uint64
	Activation activation
}

type registeredModule struct {
	Bytes []byte
	Opts  ModuleOptions
}

type serverState struct {
	ServerID          string
	LastHeartbeatedAt int64
	HeartbeatState    HeartbeatState
	ServerVersion     int64
}

type activation struct {
	ServerID string
}

func versionSince(curr, prev int64) time.Duration {
	since := curr - prev
	if since < 0 {
		panic(fmt.Sprintf(
			"prev: %d, curr: %d, versionstamp did not increase monotonically",
			prev, curr))
	}
	return time.Duration(since) * time.Microsecond
}

type kvTransaction struct {
	namespace string
	actorID   string
	tr        transaction
}

func newKVTransaction(
	ctx context.Context,
	namespace string,
	actorID string,
	tr transaction,
) *kvTransaction {
	return &kvTransaction{
		namespace: namespace,
		actorID:   actorID,
		tr:        tr,
	}
}

func (tr *kvTransaction) Get(
	ctx context.Context,
	key []byte,
) ([]byte, bool, error) {
	actorKVKey := getActoKVKey(tr.namespace, tr.actorID, key)
	return tr.tr.get(ctx, actorKVKey)
}

func (tr *kvTransaction) Put(
	ctx context.Context,
	key []byte,
	value []byte,
) error {
	actorKVKey := getActoKVKey(tr.namespace, tr.actorID, key)
	return tr.tr.put(ctx, actorKVKey, value)
}

func (tr *kvTransaction) Commit(ctx context.Context) error {
	return tr.tr.commit(ctx)
}

func (tr *kvTransaction) Cancel(ctx context.Context) error {
	return tr.tr.cancel(ctx)
}
