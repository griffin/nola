package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richardartoul/nola/virtual/registry"
	"github.com/richardartoul/nola/virtual/types"
	"github.com/richardartoul/nola/wapcutils"

	"github.com/stretchr/testify/require"
)

var (
	utilWasmBytes   []byte
	defaultOptsWASM = EnvironmentOptions{
		CustomHostFns: map[string]func([]byte) ([]byte, error){
			"testCustomFn": func([]byte) ([]byte, error) {
				return []byte("ok"), nil
			},
		},
	}
	defaultOptsGo = EnvironmentOptions{
		CustomHostFns: map[string]func([]byte) ([]byte, error){
			"testCustomFn": func([]byte) ([]byte, error) {
				return []byte("ok"), nil
			},
		},
		GoModules: map[types.NamespacedIDNoType]Module{
			{Namespace: "ns-1", ID: "test-module"}: testModule{},
			{Namespace: "ns-2", ID: "test-module"}: testModule{},
		},
	}
)

func init() {
	fBytes, err := ioutil.ReadFile("../testdata/tinygo/util/main.wasm")
	if err != nil {
		panic(err)
	}
	utilWasmBytes = fBytes
}

// TODO: Need a good concurrency test that spans a bunch of goroutine and
//       spams registry operations + invocations.

// TestSimpleActor is a basic sanity test that verifies the most basic flow for actors.
func TestSimpleActor(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			// Can't invoke because actor doesn't exist yet.
			_, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.Error(t, err)

			// Create actor.
			_, err = reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			for i := 0; i < 100; i++ {
				// Invoke should work now.
				result, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
				require.NoError(t, err)
				require.Equal(t, int64(i+1), getCount(t, result))

				if i == 0 {
					result, err = env.InvokeActor(ctx, ns, "a", "getStartupWasCalled", nil, types.CreateIfNotExist{})
					require.NoError(t, err)
					require.Equal(t, []byte("true"), result)
				}
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestCreateIfNotExist tests that the CreateIfNotExist argument can be used to invoke an actor and
// create it automatically if it does not already exist.
func TestCreateIfNotExist(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			// No create actor call before invoking.
			for i := 0; i < 100; i++ {
				if i == 0 {
					// Invoke should fail if CreateIfNotExist is not set.
					_, err := env.InvokeActor(
						ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
					require.Error(t, err)
					require.True(t, registry.IsActorDoesNotExistErr(err))
				}

				// Should succeed with it set.
				result, err := env.InvokeActor(
					ctx, ns, "a", "inc", nil, types.CreateIfNotExist{ModuleID: "test-module"})
				require.NoError(t, err)
				require.Equal(t, int64(i+1), getCount(t, result))

				if i == 0 {
					result, err = env.InvokeActor(ctx, ns, "a", "getStartupWasCalled", nil, types.CreateIfNotExist{})
					require.NoError(t, err)
					require.Equal(t, []byte("true"), result)
				}
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestSimpleWorker is a basic sanity test that verifies the most basic flow for workers.
func TestSimpleWorker(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			// Can invoke immediately once module exists, no need to "create" a worker or anything
			// like we do for actors.
			_, err := env.InvokeWorker(ctx, ns, "test-module", "inc", nil)
			require.NoError(t, err)

			// Workers can still "accumulate" in-memory state like actors do, but the state may vary
			// depending on which server/environment the call is executed on (unlike actors where the
			// request is always routed to the single active "global" instance).
			for i := 0; i < 100; i++ {
				result, err := env.InvokeWorker(ctx, ns, "test-module", "inc", nil)
				require.NoError(t, err)
				require.Equal(t, int64(i+2), getCount(t, result))

				if i == 0 {
					result, err = env.InvokeWorker(ctx, ns, "test-module", "getStartupWasCalled", nil)
					require.NoError(t, err)
					require.Equal(t, []byte("true"), result)
				}
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestGenerationCountIncInvalidatesActivation ensures that the registry returning a higher
// generation count will cause the environment to invalidate existing activations and recreate
// them as needed.
func TestGenerationCountIncInvalidatesActivation(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			// Build some state.
			for i := 0; i < 100; i++ {
				result, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
				require.NoError(t, err)
				require.Equal(t, int64(i+1), getCount(t, result))
			}

			// Increment the generation which should cause the next invocation to recreate the actor
			// activation from scratch and reset the internal counter back to 0.
			reg.IncGeneration(ctx, ns, "a")

			for i := 0; i < 100; i++ {
				if i == 0 {
					for {
						// Wait for cache to expire.
						result, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
						require.NoError(t, err)
						if getCount(t, result) == 1 {
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
					continue
				}
				result, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
				require.NoError(t, err)
				require.Equal(t, int64(i+1), getCount(t, result))
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestKVHostFunctions tests whether the KV interfaces from the registry can be used properly as host functions
// in the actor WASM module.
func TestKVHostFunctions(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		count := 0
		defer func() {
			count++
		}()

		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			if count == 0 {
				_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
				require.NoError(t, err)

				for i := 0; i < 100; i++ {
					_, err := env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
					require.NoError(t, err)

					// Write the current count to a key.
					key := []byte(fmt.Sprintf("key-%d", i))
					_, err = env.InvokeActor(ctx, ns, "a", "kvPutCount", key, types.CreateIfNotExist{})
					require.NoError(t, err)

					// Read the key back and make sure the value is == the count
					payload, err := env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
					require.NoError(t, err)
					val := getCount(t, payload)
					require.Equal(t, int64(i+1), val)

					if i > 0 {
						key := []byte(fmt.Sprintf("key-%d", i-1))
						payload, err := env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
						require.NoError(t, err)
						val := getCount(t, payload)
						require.Equal(t, int64(i), val)
					}
				}
			}

			// Ensure all previous KV are still readable.
			for i := 0; i < 100; i++ {
				key := []byte(fmt.Sprintf("key-%d", i))
				payload, err := env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
				require.NoError(t, err)
				val := getCount(t, payload)
				require.Equal(t, int64(i+1), val)
			}
		}
	}

	// Run the test twice with two different environments, but the same registry
	// to simulate a node restarting and being re-initialized with the same registry
	// to ensure the KV operations are durable if the KV itself is.
	runWithDifferentConfigs(t, testFn)
}

// TestKVTransactions tests whether the KV interfaces from the registry can be used
// properly within transactions, and that transactions are automatically rolled back
// if the actor returns an error.
func TestKVTransactions(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1"} {
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			_, err = env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Write the current count to a key.
			key := []byte("key")
			_, err = env.InvokeActor(ctx, ns, "a", "kvPutCount", key, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Read the key back and make sure the value is == the count
			payload, err := env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
			require.NoError(t, err)
			val := getCount(t, payload)
			require.Equal(t, int64(1), val)

			// Increment the count and write to KV again, but ensure the actor
			// returns an error so the updated counter will not be committed to
			// KV storage. This tests that implicit KV transactions are rolled
			// back if the actor returns an error.
			_, err = env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)

			_, err = env.InvokeActor(ctx, ns, "a", "kvPutCountError", key, types.CreateIfNotExist{})
			require.True(t, strings.Contains(err.Error(), "some fake error"), err.Error())

			// Count should still be 1.
			payload, err = env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
			require.NoError(t, err)
			val = getCount(t, payload)
			require.Equal(t, int64(1), val)
		}
	}

	// Run the test twice with two different environments, but the same registry
	// to simulate a node restarting and being re-initialized with the same registry
	// to ensure the KV operations are durable if the KV itself is.
	runWithDifferentConfigs(t, testFn)
}

// TestKVHostFunctionsActorsSeparatedRegression is a regression test that ensures each
// actor gets its own dedicated KV storage, even if another exists exists created from
// the same module.
func TestKVHostFunctionsActorsSeparatedRegression(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			_, err = reg.CreateActor(ctx, ns, "b", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			// Inc a twice, but b once.
			_, err = env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "b", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)

			key := []byte("key")

			// Persist each actor's count.
			_, err = env.InvokeActor(ctx, ns, "a", "kvPutCount", key, types.CreateIfNotExist{})
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "b", "kvPutCount", key, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Make sure we can read back the independent counts.
			payload, err := env.InvokeActor(ctx, ns, "a", "kvGet", key, types.CreateIfNotExist{})
			require.NoError(t, err)
			val := getCount(t, payload)
			require.Equal(t, int64(2), val)

			payload, err = env.InvokeActor(ctx, ns, "b", "kvGet", key, types.CreateIfNotExist{})
			require.NoError(t, err)
			val = getCount(t, payload)
			require.Equal(t, int64(1), val)
		}
	}
	runWithDifferentConfigs(t, testFn)
}

// TestCreateActorHostFunction tests whether the create actor host function can be used
// by the WASM module to create new actors on demand. In other words, this test ensures
// that actors can create new actors.
func TestCreateActorHostFunction(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			// Succeeds because actor exists.
			_, err = env.InvokeActor(ctx, ns, "a", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Fails because actor does not exist.
			_, err = env.InvokeActor(ctx, ns, "b", "inc", nil, types.CreateIfNotExist{})
			require.Error(t, err)

			// Create a new actor b by calling fork() on a, not by creating it ourselves.
			_, err = env.InvokeActor(ctx, ns, "a", "fork", []byte("b"), types.CreateIfNotExist{})
			require.NoError(t, err)

			// Should succeed now that actor a has created actor b.
			_, err = env.InvokeActor(ctx, ns, "b", "inc", nil, types.CreateIfNotExist{})
			require.NoError(t, err)

			for _, actor := range []string{"a", "b"} {
				for i := 0; i < 100; i++ {
					_, err := env.InvokeActor(ctx, ns, actor, "inc", nil, types.CreateIfNotExist{})
					require.NoError(t, err)

					// Write the current count to a key.
					key := []byte(fmt.Sprintf("key-%d", i))
					_, err = env.InvokeActor(ctx, ns, actor, "kvPutCount", key, types.CreateIfNotExist{})
					require.NoError(t, err)

					// Read the key back and make sure the value is == the count
					payload, err := env.InvokeActor(ctx, ns, actor, "kvGet", key, types.CreateIfNotExist{})
					require.NoError(t, err)
					val := getCount(t, payload)
					require.Equal(t, int64(i+2), val)
				}
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestInvokeActorHostFunction tests whether the invoke actor host function can be used
// by the WASM module to invoke operations on other actors on demand. In other words, this
// test ensures that actors can communicate with other actors.
func TestInvokeActorHostFunction(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			// Create an actor, then immediately fork it so we have two actors.
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			_, err = env.InvokeActor(ctx, ns, "a", "fork", []byte("b"), types.CreateIfNotExist{})
			require.NoError(t, err)

			// Ensure actor a can communicate with actor b.
			invokeReq := types.InvokeActorRequest{
				ActorID:   "b",
				Operation: "inc",
				Payload:   nil,
			}
			marshaled, err := json.Marshal(invokeReq)
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "a", "invokeActor", marshaled, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Ensure actor b can communicate with actor a.
			invokeReq = types.InvokeActorRequest{
				ActorID:   "a",
				Operation: "inc",
				Payload:   nil,
			}
			marshaled, err = json.Marshal(invokeReq)
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "b", "invokeActor", marshaled, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Ensure both actor's state was actually updated and they can request
			// each other's state.
			invokeReq = types.InvokeActorRequest{
				ActorID:   "b",
				Operation: "getCount",
				Payload:   nil,
			}
			marshaled, err = json.Marshal(invokeReq)
			require.NoError(t, err)
			result, err := env.InvokeActor(ctx, ns, "a", "invokeActor", marshaled, types.CreateIfNotExist{})
			require.NoError(t, err)
			require.Equal(t, int64(1), getCount(t, result))

			invokeReq = types.InvokeActorRequest{
				ActorID:   "a",
				Operation: "getCount",
				Payload:   nil,
			}
			marshaled, err = json.Marshal(invokeReq)
			require.NoError(t, err)
			result, err = env.InvokeActor(ctx, ns, "b", "invokeActor", marshaled, types.CreateIfNotExist{})
			require.NoError(t, err)
			require.Equal(t, int64(1), getCount(t, result))
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestScheduleInvocationHostFunction tests whether actors can schedule invocations to run
// sometime in the future as a way to implement timers.
func TestScheduleInvocationHostFunction(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		for _, ns := range []string{"ns-1", "ns-2"} {
			// Create an actor, then immediately fork it so we have two actors.
			_, err := reg.CreateActor(ctx, ns, "a", "test-module", types.ActorOptions{})
			require.NoError(t, err)

			_, err = env.InvokeActor(ctx, ns, "a", "fork", []byte("b"), types.CreateIfNotExist{})
			require.NoError(t, err)

			// A bit meta, but tell a to schedule an invocation on b to schedule an invocation
			// back on a. This ensures that actor's can schedule invocations on other actors.
			bScheduleA := wapcutils.ScheduleInvocationRequest{
				Invoke: types.InvokeActorRequest{
					ActorID:   "a",
					Operation: "inc",
					Payload:   nil,
				},
				AfterMillis: 1000,
			}
			marshaledBScheduleA, err := json.Marshal(bScheduleA)
			require.NoError(t, err)
			aScheduleB := wapcutils.ScheduleInvocationRequest{
				Invoke: types.InvokeActorRequest{
					ActorID:   "b",
					Operation: "scheduleInvocation",
					Payload:   marshaledBScheduleA,
				},
				AfterMillis: 1000,
			}
			marshaledAScheduleB, err := json.Marshal(aScheduleB)
			require.NoError(t, err)

			// In addition, tell a to schedule an invocation on itself to ensure we
			// can support "self timers".
			aScheduleA := wapcutils.ScheduleInvocationRequest{
				Invoke: types.InvokeActorRequest{
					ActorID:   "a",
					Operation: "inc",
					Payload:   nil,
				},
				AfterMillis: 1000,
			}
			marshaledAScheduleA, err := json.Marshal(aScheduleA)
			require.NoError(t, err)

			// Schedule both the a::a invocation and the a::b::a invocation.
			_, err = env.InvokeActor(ctx, ns, "a", "scheduleInvocation", marshaledAScheduleB, types.CreateIfNotExist{})
			require.NoError(t, err)
			_, err = env.InvokeActor(ctx, ns, "a", "scheduleInvocation", marshaledAScheduleA, types.CreateIfNotExist{})
			require.NoError(t, err)

			// Make sure a is 0 immediately after scheduling.
			result, err := env.InvokeActor(ctx, ns, "a", "getCount", nil, types.CreateIfNotExist{})
			require.NoError(t, err)
			require.Equal(t, int64(0), getCount(t, result))

			// Wait for both the a::a and a::b::a invocations to run.
			for {
				result, err := env.InvokeActor(ctx, ns, "a", "getCount", nil, types.CreateIfNotExist{})
				require.NoError(t, err)
				if getCount(t, result) != int64(2) {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				// We didn't ever schedule an inc for b so it should remain zero.
				result, err = env.InvokeActor(ctx, ns, "b", "getCount", nil, types.CreateIfNotExist{})
				require.NoError(t, err)
				require.Equal(t, int64(0), getCount(t, result))
				break
			}
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestInvokeActorHostFunctionDeadlockRegression is a regression test to ensure that an actor can invoke
// another actor that is not yet activated without introducing a deadlock.
func TestInvokeActorHostFunctionDeadlockRegression(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		_, err := reg.CreateActor(ctx, "ns-1", "a", "test-module", types.ActorOptions{})
		require.NoError(t, err)
		_, err = reg.CreateActor(ctx, "ns-1", "b", "test-module", types.ActorOptions{})
		require.NoError(t, err)

		invokeReq := types.InvokeActorRequest{
			ActorID:   "b",
			Operation: "inc",
			Payload:   nil,
		}
		marshaled, err := json.Marshal(invokeReq)
		require.NoError(t, err)

		_, err = env.InvokeActor(ctx, "ns-1", "a", "invokeActor", marshaled, types.CreateIfNotExist{})
		require.NoError(t, err)
	}

	runWithDifferentConfigs(t, testFn)
}

// TestHeartbeatAndSelfHealing tests the interaction between the service discovery / heartbeating system
// and the registry. It ensures that every "server" (environment) is constantly heartbeating the registry,
// that the registry will detect server's that are no longer heartbeating and reactivate the actors elsewhere,
// and that the activation/routing system can accomodate all of this.
func TestHeartbeatAndSelfHealing(t *testing.T) {
	var (
		reg = registry.NewLocalRegistry()
		ctx = context.Background()
	)
	// Create 3 environments backed by the same registry to simulate 3 different servers. Each environment
	// needs its own port so it looks unique.
	opts1 := defaultOptsWASM
	opts1.Discovery.Port = 1
	env1, err := NewEnvironment(ctx, "serverID1", reg, nil, opts1)
	require.NoError(t, err)
	opts2 := defaultOptsWASM
	opts2.Discovery.Port = 2
	env2, err := NewEnvironment(ctx, "serverID2", reg, nil, opts2)
	require.NoError(t, err)
	opts3 := defaultOptsWASM
	opts3.Discovery.Port = 3
	env3, err := NewEnvironment(ctx, "serverID3", reg, nil, opts3)
	require.NoError(t, err)

	_, err = reg.RegisterModule(ctx, "ns-1", "test-module", utilWasmBytes, registry.ModuleOptions{})
	require.NoError(t, err)

	// Create 3 different actors because we want to end up with at least one actor on each
	// server to test the ability to "migrate" the actor's activation from one server to
	// another.
	_, err = reg.CreateActor(ctx, "ns-1", "a", "test-module", types.ActorOptions{})
	require.NoError(t, err)
	_, err = reg.CreateActor(ctx, "ns-1", "b", "test-module", types.ActorOptions{})
	require.NoError(t, err)
	_, err = reg.CreateActor(ctx, "ns-1", "c", "test-module", types.ActorOptions{})
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		// Ensure we can invoke each actor from each environment. Note that just because
		// we invoke an actor on env1 first does not mean that the actor will be activated
		// on env1. The actor will be activated on whichever environment/server the Registry
		// decides and if we send the invocation to the "wrong" environment it will get
		// re-routed automatically.
		//
		// Also note that we force each environment to heartbeat manually. This is important
		// because the Registry load-balancing mechanism relies on the state provided to the
		// Registry about the server from the server heartbeats. Therefore we need to
		// heartbeat at least once after every actor is activated if we want to ensure the
		// registry is able to actually load-balance the activations evenly.
		_, err = env1.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env2.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env3.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env1.heartbeat())
		require.NoError(t, env2.heartbeat())
		require.NoError(t, env3.heartbeat())
		_, err = env1.InvokeActor(ctx, "ns-1", "b", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env2.InvokeActor(ctx, "ns-1", "b", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env3.InvokeActor(ctx, "ns-1", "b", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env1.heartbeat())
		require.NoError(t, env2.heartbeat())
		require.NoError(t, env3.heartbeat())
		_, err = env1.InvokeActor(ctx, "ns-1", "c", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env2.InvokeActor(ctx, "ns-1", "c", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		_, err = env3.InvokeActor(ctx, "ns-1", "c", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env1.heartbeat())
		require.NoError(t, env2.heartbeat())
		require.NoError(t, env3.heartbeat())
	}

	// Registry load-balancing should ensure that we ended up with 1 actor in each environment
	// I.E "on each server".
	require.Equal(t, 1, env1.numActivatedActors())
	require.Equal(t, 1, env2.numActivatedActors())
	require.Equal(t, 1, env3.numActivatedActors())

	// TODO: Sleeps in tests are bad, but I'm lazy to inject a clock right now and deal
	//       with all of that.
	require.NoError(t, env1.Close())
	require.NoError(t, env2.Close())
	time.Sleep(registry.HeartbeatTTL + time.Second)

	// env1 and env2 have been closed (and not heartbeating) for longer than the maximum
	// heartbeat delay which means that the registry should view them as "dead". Therefore, we
	// expect that we should still be able to invoke all 3 of our actors, however, all of them
	// should end up being activated on server3 now since it is the only remaining live actor.

	for i := 0; i < 100; i++ {
		if i == 0 {
			for {
				// Spin loop until there are no more errors as function calls will fail for
				// a bit until heartbeat + activation cache expire.
				_, err = env3.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				break
			}
			continue
		}

		_, err = env3.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env3.heartbeat())
		_, err = env3.InvokeActor(ctx, "ns-1", "b", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env3.heartbeat())
		_, err = env3.InvokeActor(ctx, "ns-1", "c", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)
		require.NoError(t, env3.heartbeat())
	}

	// Ensure that all of our invocations above were actually served by environment3.
	require.Equal(t, 3, env3.numActivatedActors())

	// Finally, make sure environment 3 is closed.
	require.NoError(t, env3.Close())
}

// TestVersionStampIsHonored ensures that the interaction between the client and server
// around versionstamp coordination works by preventing the server from updating its
// internal versionstamp and ensuring that eventually RPCs start to fail because the
// server can no longer be sure it "owns" the actor and is allowed to run it.
func TestVersionStampIsHonored(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		_, err := reg.CreateActor(ctx, "ns-1", "a", "test-module", types.ActorOptions{})
		require.NoError(t, err)

		_, err = env.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
		require.NoError(t, err)

		env.freezeHeartbeatState()

		for {
			// Eventually RPCs should start to fail because the server's versionstamp will become
			// stale and it will no longer be confident that it's allowed to run RPCs for the
			// actor.
			_, err = env.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
			if err != nil && strings.Contains(err.Error(), "server heartbeat") {
				break
			}
			require.NoError(t, err)
			time.Sleep(100 * time.Millisecond)
		}
	}

	runWithDifferentConfigs(t, testFn)
}

// TestCustomHostFns tests the ability for users to provide custom host functions that
// can be invoked by actors.
func TestCustomHostFns(t *testing.T) {
	testFn := func(t *testing.T, reg registry.Registry, env Environment) {
		ctx := context.Background()
		_, err := reg.CreateActor(ctx, "ns-1", "a", "test-module", types.ActorOptions{})
		require.NoError(t, err)

		result, err := env.InvokeActor(ctx, "ns-1", "a", "invokeCustomHostFn", []byte("testCustomFn"), types.CreateIfNotExist{})
		require.NoError(t, err)
		require.Equal(t, []byte("ok"), result)
	}

	runWithDifferentConfigs(t, testFn)
}

// TestGoModulesRegisterTwice ensures that writing modules in pure Go and registering
// them works repeatedly and doesn't fail due to "module already exists" errors from
// the registry.
func TestGoModulesRegisterTwice(t *testing.T) {
	// Create environment and register modules.
	reg := registry.NewLocalRegistry()
	env, err := NewEnvironment(context.Background(), "serverID1", reg, nil, defaultOptsGo)
	require.NoError(t, err)
	require.NoError(t, env.Close())

	// Recreate with same registry should not fail.
	env, err = NewEnvironment(context.Background(), "serverID1", reg, nil, defaultOptsGo)
	require.NoError(t, err)
	require.NoError(t, env.Close())
}

func getCount(t *testing.T, v []byte) int64 {
	x, err := strconv.Atoi(string(v))
	require.NoError(t, err)
	return int64(x)
}

func runWithDifferentConfigs(
	t *testing.T,
	testFn func(t *testing.T, reg registry.Registry, env Environment),
) {
	t.Run("wasm", func(t *testing.T) {
		reg := registry.NewLocalRegistry()
		env, err := NewEnvironment(context.Background(), "serverID1", reg, nil, defaultOptsWASM)
		require.NoError(t, err)
		defer env.Close()

		_, err = reg.RegisterModule(context.Background(), "ns-1", "test-module", utilWasmBytes, registry.ModuleOptions{})
		require.NoError(t, err)
		_, err = reg.RegisterModule(context.Background(), "ns-2", "test-module", utilWasmBytes, registry.ModuleOptions{})
		require.NoError(t, err)

		testFn(t, reg, env)
	})

	t.Run("go", func(t *testing.T) {
		reg := registry.NewLocalRegistry()
		env, err := NewEnvironment(context.Background(), "serverID1", reg, nil, defaultOptsGo)
		require.NoError(t, err)
		defer env.Close()

		testFn(t, reg, env)
	})
}

type testModule struct {
}

func (tm testModule) Instantiate(
	ctx context.Context,
	id string,
	host HostCapabilities,
) (Actor, error) {
	return &testActor{
		host: host,
	}, nil
}

func (tm testModule) Close(ctx context.Context) error {
	return nil
}

type testActor struct {
	host HostCapabilities

	count            int
	startupWasCalled bool
}

func (ta *testActor) Invoke(
	ctx context.Context,
	operation string,
	payload []byte,
	transaction registry.ActorKVTransaction,
) ([]byte, error) {
	switch operation {
	case wapcutils.StartupOperationName:
		ta.startupWasCalled = true
		return nil, nil
	case wapcutils.ShutdownOperationName:
		return nil, nil
	case "inc":
		ta.count++
		return []byte(strconv.Itoa(ta.count)), nil
	case "getCount":
		return []byte(strconv.Itoa(ta.count)), nil
	case "getStartupWasCalled":
		if ta.startupWasCalled {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case "kvPutCount":
		value := []byte(fmt.Sprintf("%d", ta.count))
		return nil, transaction.Put(ctx, payload, value)
	case "kvPutCountError":
		value := []byte(fmt.Sprintf("%d", ta.count))
		err := transaction.Put(ctx, payload, value)
		if err == nil {
			return nil, errors.New("some fake error")
		}
		return nil, err
	case "kvGet":
		v, _, err := transaction.Get(ctx, payload)
		if err != nil {
			return nil, err
		}
		return v, nil
	case "fork":
		_, err := ta.host.CreateActor(ctx, wapcutils.CreateActorRequest{
			ActorID:  string(payload),
			ModuleID: "",
		})
		return nil, err
	case "invokeActor":
		var req types.InvokeActorRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return ta.host.InvokeActor(ctx, req)
	case "scheduleInvocation":
		var req wapcutils.ScheduleInvocationRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		err := ta.host.ScheduleInvokeActor(ctx, req)
		return nil, err
	case "invokeCustomHostFn":
		return ta.host.CustomFn(ctx, string(payload), payload)
	default:
		return nil, fmt.Errorf("testActor: unhandled operation: %s", operation)
	}
}

// TestServerVersionIsHonored ensures client-server coordination around server versions by blocking actor invocations if versions don't match,
// indicating a missed heartbeat by the server and loss of ownership of the actor.
// This reproduces the bug identified in https://github.com/richardartoul/nola/blob/master/proofs/stateright/activation-cache/README.md
func TestServerVersionIsHonored(t *testing.T) {
	var (
		reg = registry.NewLocalRegistry()
		ctx = context.Background()
	)

	env1, err := NewEnvironment(ctx, "serverID1", reg, nil, EnvironmentOptions{
		ActivationCacheTTL: time.Second * 15,
	})
	require.NoError(t, err)

	_, err = reg.RegisterModule(ctx, "ns-1", "test-module", utilWasmBytes, registry.ModuleOptions{})
	require.NoError(t, err)

	_, err = reg.CreateActor(ctx, "ns-1", "a", "test-module", types.ActorOptions{})
	require.NoError(t, err)

	_, err = env1.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
	require.NoError(t, err)

	env1.pauseHeartbeat()

	time.Sleep(registry.HeartbeatTTL + time.Second)

	env1.resumeHeartbeat()

	require.NoError(t, env1.heartbeat())

	_, err = env1.InvokeActor(ctx, "ns-1", "a", "inc", nil, types.CreateIfNotExist{})
	require.EqualErrorf(t, err, "InvokeLocal: server version(2) != server version from reference(1)", "Error should be: %v, got: %v", "InvokeLocal: server version(1) != server version from reference(0)", err)
}

func (ta testActor) Close(ctx context.Context) error {
	return nil
}
