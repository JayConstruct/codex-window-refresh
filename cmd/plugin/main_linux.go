//go:build cgo && linux

// Native ABI declarations follow the MIT-licensed CPA plugin examples. See
// NOTICE for upstream attribution. Domain/lifecycle implementation is local.
package main

/*
#cgo LDFLAGS: -ldl
#cgo CFLAGS: -D_GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
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
extern int cliproxyPluginCall(char*,uint8_t*,size_t,cliproxy_buffer*);
extern void cliproxyPluginFree(void*,size_t);
extern void cliproxyPluginShutdown(void);
static int copy_host_api(const cliproxy_host_api* host,cliproxy_host_api* copy) {
 if(!host || !copy || host->abi_version != 1 || !host->host_ctx || !host->call || !host->free_buffer) return 1;
 *copy=*host; return 0;
}
// Go's c-shared runtime may retain native threads after plugin shutdown. Keep
// executable mappings resident when CPA retires/dlcloses this version. Versioned
// paths still get isolated plugin instances; process exit reclaims the mappings.
static int retain_plugin_mapping(void) {
 Dl_info info;
 if(!dladdr((void*)&copy_host_api,&info) || !info.dli_fname) return 1;
 void* handle=dlopen(info.dli_fname,RTLD_NOW|RTLD_LOCAL|RTLD_NODELETE);
 if(!handle) return 1;
 dlclose(handle);
 return 0;
}
static int call_host(cliproxy_host_api* host,const char* method,const uint8_t* request,size_t len,cliproxy_buffer* response) {
 return host->call(host->host_ctx,method,request,len,response);
}
static void free_host(cliproxy_host_api* host,void* ptr,size_t len) { if(ptr) host->free_buffer(ptr,len); }
static size_t method_length(const char* method) { return strnlen(method,257); }
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"unsafe"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	pluginservice "codex-window-refresh/internal/plugin"
	scheduler "codex-window-refresh/internal/runtime"
)

const maxRPCBytes = 64 << 20

var nativeMu sync.Mutex
var service *pluginservice.Service
var callback *nativeCallback

func main() {}

type runtimeEngine struct{ *scheduler.Engine }

func (e runtimeEngine) Diagnostics() (core.HostCapabilities, string, int) {
	s := e.Status()
	return s.Capabilities, s.LastError, s.EffectiveConcurrency
}
func engineFactory(cfg config.Config, host core.Host, store core.Store, clock core.Clock, random core.Random) (pluginservice.Engine, error) {
	e, err := scheduler.New(cfg, host, store, clock, random)
	if err != nil {
		return nil, err
	}
	return runtimeEngine{e}, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) (code C.int) {
	code = 1
	defer func() {
		if recover() != nil {
			code = 1
		}
	}()
	if api == nil {
		return
	}
	*api = C.cliproxy_plugin_api{}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if service != nil {
		return
	}
	var copied C.cliproxy_host_api
	if C.copy_host_api(host, &copied) != 0 {
		return
	}
	if C.retain_plugin_mapping() != 0 {
		return
	}
	callback = &nativeCallback{api: copied}
	service = pluginservice.New(callback, pluginservice.Options{EngineFactory: engineFactory})
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	code = 0
	return
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (code C.int) {
	code = 1
	if response == nil {
		return
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, pluginservice.ErrorResponse("plugin_panic", "native operation failed"))
			code = 1
		}
	}()
	if method == nil || C.method_length(method) == 0 || C.method_length(method) > 256 {
		writeResponse(response, pluginservice.ErrorResponse("invalid_method", "method required and bounded"))
		return
	}
	if uint64(length) > maxRPCBytes || (length > 0 && request == nil) {
		writeResponse(response, pluginservice.ErrorResponse("invalid_payload", "invalid request buffer"))
		return
	}
	var payload []byte
	if length > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	nativeMu.Lock()
	current := service
	nativeMu.Unlock()
	if current == nil {
		writeResponse(response, pluginservice.ErrorResponse("not_initialized", "plugin not initialized"))
		return
	}
	raw := current.Handle(context.Background(), C.GoString(method), payload)
	if !writeResponse(response, raw) {
		return
	}
	code = 0
	return
}
func writeResponse(response *C.cliproxy_buffer, raw []byte) bool {
	if response == nil || len(raw) == 0 || len(raw) > maxRPCBytes {
		return false
	}
	if response.ptr != nil {
		C.free(response.ptr)
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return false
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return true
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	defer func() { _ = recover() }()
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	nativeMu.Lock()
	defer nativeMu.Unlock()
	current, cb := service, callback
	if cb != nil {
		cb.stopped.Store(true)
	}
	if current != nil {
		if err := current.Shutdown(context.Background()); err != nil {
			current.WaitForShutdown()
		}
	}
	service, callback = nil, nil
	// The host may free its context and dlclose only after this function
	// returns; all engine callbacks have drained. New callbacks are stopped.
}

type nativeCallback struct {
	api     C.cliproxy_host_api
	stopped atomic.Bool
}

func (cb *nativeCallback) Call(ctx context.Context, method string, request, response any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanup := method == "host.model.stream_close" || method == "host.http.stream_close" || method == "host.http.cancel"
	if cb.stopped.Load() && !cleanup {
		return errors.New("native callback stopped")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("host request encoding failed")
	}
	if len(raw) > maxRPCBytes {
		return errors.New("host request too large")
	}
	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))
	var req *C.uint8_t
	if len(raw) > 0 {
		req = (*C.uint8_t)(C.CBytes(raw))
		if req == nil {
			return errors.New("host request allocation failed")
		}
		defer C.free(unsafe.Pointer(req))
	}
	// Copy the entire API; never retain the host's temporary API struct pointer.
	api := cb.api
	var result C.cliproxy_buffer
	status := C.call_host(&api, cm, req, C.size_t(len(raw)), &result)
	if result.ptr != nil {
		defer C.free_host(&api, result.ptr, result.len)
	}
	if uint64(result.len) > maxRPCBytes || (result.len > 0 && result.ptr == nil) {
		return errors.New("invalid host response buffer")
	}
	if status != 0 {
		return errors.New("host callback failed")
	}
	if result.len == 0 {
		return errors.New("empty host response")
	}
	data := C.GoBytes(result.ptr, C.int(result.len))
	return decodeHostResult(ctx, data, response)
}
