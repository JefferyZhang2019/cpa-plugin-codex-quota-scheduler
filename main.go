package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static int call_host_api(cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var (
	hostAPI          atomic.Pointer[C.cliproxy_host_api]
	hostRosterLatest atomic.Pointer[HostRosterSnapshot]
)

const hostRosterDetectionTimeout = 5 * time.Second

func main() {}

// ABIHostAuthLister normalizes raw host.auth.list JSON before the typed ABI
// can erase whether Priority was absent or explicitly zero.
type ABIHostAuthLister struct {
	call func(string, any) (json.RawMessage, error)
}

func (l ABIHostAuthLister) ListHostAuths(ctx context.Context) ([]RosterEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := l.call
	if call == nil {
		call = callHostCallback
	}
	result, err := call(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
			Provider  string `json:"provider"`
			Priority  *int   `json:"priority"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	entries := make([]RosterEntry, 0, len(response.Files))
	for _, file := range response.Files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") {
			continue
		}
		priority := file.Priority
		if priority == nil {
			defaultPriority := 0
			priority = &defaultPriority
		}
		entries = append(entries, RosterEntry{
			ID:        file.ID,
			AuthIndex: file.AuthIndex,
			Provider:  "codex",
			Priority:  priority,
		})
	}
	return entries, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI.Store(host)
	refresherMu.Lock()
	callHostCallback = callHostCallbackABI
	callHostModel = callHostCallbackStatus
	roster := HostRosterSnapshot{Capability: CapabilityB}
	if latest := hostRosterLatest.Load(); latest != nil {
		roster = *latest
	}
	credentialHost := &rosterCredentialHost{host: ABIHostClient{}, roster: roster}
	production, err := NewProductionQuotaRefresher(ABIHostClient{}, globalState, credentialHost, roster, defaultStatePath(), time.Now)
	if err == nil {
		globalRefresher = production
		credentialHost.bindings = production.bindings
	} else {
		// Runtime persistence is an authorization prerequisite. Keep background
		// work disabled rather than falling back to the legacy external-call path.
		globalRefresher = nil
	}
	globalRosterController = NewRosterController(RosterControllerOptions{
		Host: ABIHostAuthLister{},
		Provisional: func() *ActiveRoster {
			if production == nil {
				return nil
			}
			return production.ProvisionalRoster()
		}(),
		Publish: func(ctx context.Context, active ActiveRoster) (ActiveRoster, error) {
			snapshot := hostRosterSnapshotFromActive(active)
			hostRosterLatest.Store(&snapshot)
			if production == nil {
				return active, ErrCapabilityB
			}
			if err := production.PublishAuthoritativeRoster(ctx, snapshot); err != nil {
				return active, err
			}
			active.Generation = production.runtimeRoster().Generation
			return active, nil
		},
		Observe: func(active ActiveRoster) {
			snapshot := hostRosterSnapshotFromActive(active)
			if production != nil {
				production.ObserveRosterLifecycle(active)
				snapshot = production.runtimeRoster()
			}
			hostRosterLatest.Store(&snapshot)
		},
		ProbeOnProvisional: true,
		VerifyProvisional: func(ctx context.Context, active ActiveRoster) bool {
			return production != nil && production.VerifyConfiguredProvisionalRoster(ctx, active)
		},
		CommitProvisional: func(commit func()) bool {
			globalState.mu.RLock()
			defer globalState.mu.RUnlock()
			if !globalState.cfg.ProbeOnProvisionalRoster {
				return false
			}
			commit()
			return true
		},
	})
	if production != nil {
		production.SetRosterLifecycleAuthority(globalRosterController.EnforceLifecycle)
	}
	replaceGlobalPickActivityPump(globalRosterController, production)
	managementProvisionalRiskChanged = func(enabled bool) {
		refresherMu.Lock()
		controller := globalRosterController
		refresher := globalRefresher
		refresherMu.Unlock()
		if controller != nil {
			_, _ = controller.WakeForProbe(context.Background())
		}
		if enabled && refresher != nil {
			refresher.Start()
		}
	}
	managementRefreshSoon = refreshGlobalRefresherSoon
	managementRefreshOneSoon = refreshGlobalRefresherOneSoon
	refresherMu.Unlock()
	// The controller is the sole roster synchronization owner. Host callback
	// readiness during init is not guaranteed, so startup remains asynchronous.
	go func() {
		refresherMu.Lock()
		controller := globalRosterController
		refresherMu.Unlock()
		if controller != nil {
			_, _ = controller.Startup(context.Background())
		}
	}()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

func detectStartupHostRoster(
	ctx context.Context,
	host HostAuthLister,
	now time.Time,
	after func(time.Duration) <-chan time.Time,
) HostRosterSnapshot {
	detectCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan HostRosterSnapshot, 1)
	go func() {
		result <- DetectHostRoster(detectCtx, host, now)
	}()
	select {
	case snapshot := <-result:
		return snapshot
	case <-ctx.Done():
		return HostRosterSnapshot{Capability: CapabilityB}
	case <-after(hostRosterDetectionTimeout):
		return HostRosterSnapshot{Capability: CapabilityB}
	}
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	stopGlobalPickActivityPump()
	refresherMu.Lock()
	refresher := globalRefresher
	globalRefresher = nil
	globalRosterController = nil
	refresherMu.Unlock()
	if refresher != nil {
		refresher.Stop()
	}
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// hostCallRaw performs one plugin-to-host ABI call and returns the raw response
// bytes together with the ABI call code. Both the flattened and the
// status-aware wrappers share this transport.
func hostCallRaw(method string, payload any) ([]byte, int, error) {
	host := hostAPI.Load()
	if host == nil {
		return nil, 0, fmt.Errorf("host callback %s unavailable", method)
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, 0, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	callCode := C.call_host_api(host, cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(host, response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, int(callCode), fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	return rawResponse, int(callCode), nil
}

func callHostCallbackABI(method string, payload any) (json.RawMessage, error) {
	rawResponse, callCode, errCall := hostCallRaw(method, payload)
	if errCall != nil {
		return nil, errCall
	}

	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, callCode)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// callHostCallbackStatus mirrors callHostCallbackABI but keeps the http_status
// the ABI error envelope carries. The retry chain needs it: a 429 or 503 from a
// nested attempt must stay distinguishable from a client-side error.
func callHostCallbackStatus(method string, payload any) (json.RawMessage, error) {
	rawResponse, callCode, errCall := hostCallRaw(method, payload)
	if errCall != nil {
		return nil, errCall
	}
	var env abiErrorEnvelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostCallError{
				Code:    strings.TrimSpace(env.Error.Code),
				Message: strings.TrimSpace(env.Error.Message),
				Status:  env.Error.HTTPStatus,
			}
		}
		return nil, &hostCallError{Code: "host_call_failed", Message: "host callback " + method + " failed", Status: callCode}
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, callCode)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
