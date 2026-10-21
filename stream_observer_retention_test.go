package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:noinline
func completeOrdinaryObservedStream(t *testing.T, id string, outcome pluginapi.RequestCompletionOutcome) (weak.Pointer[byte], int) {
	t.Helper()
	body := append([]byte("event: response.output_text.delta\ndata: "), bytes.Repeat([]byte("x"), 256<<10)...)
	body = append(body, []byte("\n\n")...)
	observeStreamChunk(globalState, &pluginapi.StreamChunkInterceptRequest{RequestID: id, Body: body}, time.Now())
	o := globalStreamObservers.observers[id]
	if o == nil || cap(o.buffer) == 0 {
		t.Fatal("stream did not allocate observer buffer")
	}
	capacity := cap(o.buffer)
	ref := weak.Make(&o.buffer[:capacity][0])
	raw, err := json.Marshal(pluginapi.RequestCompletion{RequestID: id, Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = handleRequestComplete(raw); err != nil {
		t.Fatal(err)
	}
	return ref, capacity
}

func TestStreamObserverReleasedOnRequestCompletion(t *testing.T) {
	previousStore, previousObservers := globalState, globalStreamObservers
	t.Cleanup(func() { globalState, globalStreamObservers = previousStore, previousObservers })
	for _, outcome := range []pluginapi.RequestCompletionOutcome{pluginapi.RequestCompletionSucceeded, pluginapi.RequestCompletionFailed, pluginapi.RequestCompletionCanceled, pluginapi.RequestCompletionRejected} {
		t.Run(string(outcome), func(t *testing.T) {
			globalState = NewPluginState(DefaultConfig())
			globalStreamObservers = newStreamObserverRegistry()
			refs := make([]weak.Pointer[byte], 32)
			var capacity int
			for i := range refs {
				var n int
				refs[i], n = completeOrdinaryObservedStream(t, fmt.Sprintf("completed-%d", i), outcome)
				capacity += n
			}
			runtime.GC()
			runtime.GC()
			retained := 0
			for _, r := range refs {
				if r.Value() != nil {
					retained++
				}
			}
			t.Logf("completed=32 registry_entries=%d retained_buffers=%d capacity_bytes=%d", len(globalStreamObservers.observers), retained, capacity)
			if len(globalStreamObservers.observers) != 0 || retained != 0 {
				t.Errorf("completed streams still retained by global registry")
			}
			for i := range refs {
				globalStreamObservers.forget(fmt.Sprintf("completed-%d", i))
			}
			runtime.GC()
			runtime.GC()
			for _, r := range refs {
				if r.Value() != nil {
					t.Fatal("buffer remained reachable after removing registry references")
				}
			}
			t.Log("after removing registry references: all 32 buffers reclaimed")
		})
	}
}
