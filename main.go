// CLIProxyAPI v8.1.2 (85225381) 独立 Antigravity 请求兼容模块 C ABI 动态插件入口。
// 本插件仅注册 RequestNormalizer（严格限定 ToFormat == "antigravity"）与可选只读 ManagementAPI 脱敏诊断接口，
// 绝不注册全局前置拦截器（RequestInterceptor），确保 Codex（含 gpt-6.2-sol / gpt-6.2-astra）、
// Gemini API、Claude 等非 Antigravity 渠道及跨渠道回退场景 100% 零污染。
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	__atomic_store_n(&stored_host, host, __ATOMIC_RELEASE);
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	const cliproxy_host_api* host = __atomic_load_n(&stored_host, __ATOMIC_ACQUIRE);
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static int call_host_log(const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return call_host_api("host.log", request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	const cliproxy_host_api* host = __atomic_load_n(&stored_host, __ATOMIC_ACQUIRE);
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/Bigheadh/cli-proxy-antigravity-compat/compat"
	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	lifecycleMu  sync.Mutex
	activeEngine atomic.Pointer[compat.Engine]
)

type rpcEnvelope struct {
	OK     bool             `json:"ok"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *pluginabi.Error `json:"error,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registrationResponse struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability 仅暴露 RequestNormalizer 与可选只读 ManagementAPI，绝不声明全局拦截器。
type registrationCapability struct {
	RequestNormalizer bool `json:"request_normalizer"`
	ManagementAPI     bool `json:"management_api,omitempty"`
}

type managementRouteWire struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementRegisterResponseWire struct {
	Routes []managementRouteWire `json:"routes,omitempty"`
}

type hostLogWireRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "调用方法名不能为空"))
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
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	C.store_host_api(nil)
}

func getOrInitEngine() *compat.Engine {
	if eng := activeEngine.Load(); eng != nil {
		return eng
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if eng := activeEngine.Load(); eng != nil {
		return eng
	}
	eng := compat.NewEngine(compat.DefaultConfig(), emitHostLog)
	activeEngine.Store(eng)
	return eng
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		cfg, errCfg := configurePlugin(request)
		if errCfg != nil {
			return nil, errCfg
		}
		return okEnvelope(buildRegistration(cfg))

	case pluginabi.MethodRequestNormalize:
		var req pluginapi.RequestTransformRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		resp := getOrInitEngine().NormalizeRequest(req)
		if len(resp.Body) == 0 && len(req.Body) > 0 {
			resp.Body = req.Body
		}
		return okEnvelope(resp)

	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		regResp := getOrInitEngine().RegisterManagement(req)
		wireRoutes := make([]managementRouteWire, 0, len(regResp.Routes))
		for _, r := range regResp.Routes {
			wireRoutes = append(wireRoutes, managementRouteWire{
				Method:      r.Method,
				Path:        r.Path,
				Description: r.Description,
			})
		}
		return okEnvelope(managementRegisterResponseWire{Routes: wireRoutes})

	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		resp := getOrInitEngine().HandleManagement(req)
		return okEnvelope(resp)

	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})

	default:
		return errorEnvelope("unknown_method", "不支持的插件方法: "+method), nil
	}
}

func configurePlugin(raw []byte) (compat.Config, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return compat.Config{}, errUnmarshal
		}
	}
	cfg, errParse := compat.ParseConfig(req.ConfigYAML)
	if errParse != nil {
		return compat.Config{}, errParse
	}

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if existing := activeEngine.Load(); existing != nil {
		existing.Reconfigure(cfg)
	} else {
		activeEngine.Store(compat.NewEngine(cfg, emitHostLog))
	}
	return cfg, nil
}

func buildRegistration(cfg compat.Config) registrationResponse {
	return registrationResponse{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             compat.PluginID,
			Version:          compat.PluginVersion,
			Author:           compat.PluginAuthor,
			GitHubRepository: compat.RepositoryURL,
			ConfigFields:     compat.ConfigFields(),
		},
		Capabilities: registrationCapability{
			RequestNormalizer: true,
			ManagementAPI:     cfg.DiagnosticsEndpointEnabled,
		},
	}
}

func emitHostLog(level, message string, fields map[string]any) {
	payload, errMarshal := json.Marshal(hostLogWireRequest{
		Level:   level,
		Message: message,
		Fields:  fields,
	})
	if errMarshal != nil || len(payload) == 0 {
		return
	}
	reqPtr := (*C.uint8_t)(C.CBytes(payload))
	if reqPtr == nil {
		return
	}
	defer C.free(unsafe.Pointer(reqPtr))

	var respBuf C.cliproxy_buffer
	if C.call_host_log(reqPtr, C.size_t(len(payload)), &respBuf) == 0 && respBuf.ptr != nil {
		C.free_host_buffer(respBuf.ptr, respBuf.len)
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(rpcEnvelope{
		OK:     true,
		Result: raw,
	})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(rpcEnvelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:    code,
			Message: message,
		},
	})
	return raw
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
