package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestInitializeResponse(t *testing.T) {
	server := NewServer(AdapterInfo{
		Name:    "codex-acp-adapter",
		Title:   "Codex ACP Adapter",
		Version: "test",
		Capabilities: Capabilities{
			Images:                true,
			Audio:                 true,
			EmbeddedContext:       true,
			MCPHTTP:               true,
			LoadSession:           true,
			SessionList:           true,
			SessionResume:         true,
			SessionClose:          true,
			SessionDelete:         true,
			AdditionalDirectories: true,
		},
	})

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	result := got["result"].(map[string]any)
	info := result["agentInfo"].(map[string]any)
	if info["name"] != "codex-acp-adapter" {
		t.Fatalf("agent name = %v", info["name"])
	}
	caps := result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("loadSession = %v, want true", caps["loadSession"])
	}
	promptCaps := caps["promptCapabilities"].(map[string]any)
	if promptCaps["image"] != true || promptCaps["audio"] != true || promptCaps["embeddedContext"] != true {
		t.Fatalf("prompt capabilities = %#v", promptCaps)
	}
	sessionCaps := caps["sessionCapabilities"].(map[string]any)
	for _, key := range []string{"list", "resume", "close", "delete", "additionalDirectories"} {
		if _, ok := sessionCaps[key].(map[string]any); !ok {
			t.Fatalf("sessionCapabilities[%q] = %#v, want empty object", key, sessionCaps[key])
		}
	}
}

func TestInitializeOmitsUnsupportedSessionCapabilities(t *testing.T) {
	server := NewServer(AdapterInfo{
		Name:    "minimal-acp-adapter",
		Version: "test",
	})

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	result := got["result"].(map[string]any)
	caps := result["agentCapabilities"].(map[string]any)
	if _, ok := caps["sessionCapabilities"]; ok {
		t.Fatalf("sessionCapabilities = %#v, want omitted", caps["sessionCapabilities"])
	}
}

func TestInitializeCanUseRuntimeResult(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"}, WithInitializeResult(map[string]any{
		"protocolVersion": 1,
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"sessionCapabilities": map[string]any{
				"list": map[string]any{},
			},
		},
		"agentInfo": map[string]any{
			"name": "runtime-agent",
		},
		"authMethods": []any{map[string]any{"id": "runtime-auth"}},
	}))

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	result := got["result"].(map[string]any)
	info := result["agentInfo"].(map[string]any)
	if info["name"] != "runtime-agent" {
		t.Fatalf("agent name = %v, want runtime-agent", info["name"])
	}
	caps := result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("loadSession = %v, want true", caps["loadSession"])
	}
	if _, ok := caps["sessionCapabilities"].(map[string]any)["list"]; !ok {
		t.Fatalf("sessionCapabilities = %#v, want list", caps["sessionCapabilities"])
	}
	auth := result["authMethods"].([]any)
	if len(auth) != 1 {
		t.Fatalf("authMethods len = %d, want 1", len(auth))
	}
}

func TestInitializeAdvertisesAuthMethodsAndLogout(t *testing.T) {
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithAuthMethods([]AuthMethod{{
			ID:          "agent-login",
			Name:        "Agent login",
			Description: "Sign in using the agent CLI.",
		}}),
		WithAuthLogout(),
	)

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	var result struct {
		AgentCapabilities struct {
			Auth struct {
				Logout map[string]any `json:"logout"`
			} `json:"auth"`
		} `json:"agentCapabilities"`
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.AgentCapabilities.Auth.Logout == nil {
		t.Fatalf("auth.logout = nil, want advertised empty object")
	}
	if len(result.AuthMethods) != 1 ||
		result.AuthMethods[0].ID != "agent-login" ||
		result.AuthMethods[0].Name != "Agent login" {
		t.Fatalf("authMethods = %#v, want agent-login", result.AuthMethods)
	}
}

func TestInitializeHandlerReceivesClientParams(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"}, WithInitializeHandler(func(params json.RawMessage) (any, *RPCError) {
		var req struct {
			ClientCapabilities struct {
				Terminal bool `json:"terminal"`
			} `json:"clientCapabilities"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
		}
		return map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"terminalEcho": req.ClientCapabilities.Terminal,
			},
		}, nil
	}))

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientCapabilities":{"terminal":true}}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	envelopes := decodeServerEnvelopes(t, out.Bytes())
	var result map[string]any
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	caps := result["agentCapabilities"].(map[string]any)
	if caps["terminalEcho"] != true {
		t.Fatalf("terminalEcho = %v, want true", caps["terminalEcho"])
	}
}

func TestKnownMethodsReturnStructuredNotImplemented(t *testing.T) {
	methods := []string{
		"authenticate",
		"document/didChange",
		"document/didClose",
		"document/didFocus",
		"document/didOpen",
		"document/didSave",
		"logout",
		"mcp/message",
		"nes/accept",
		"nes/close",
		"nes/reject",
		"nes/start",
		"nes/suggest",
		"providers/disable",
		"providers/list",
		"providers/set",
		"session/new",
		"session/fork",
		"session/load",
		"session/resume",
		"session/list",
		"session/set_config_option",
		"session/set_mode",
		"session/prompt",
		"session/cancel",
		"session/close",
		"session/delete",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			server := NewServer(AdapterInfo{Name: "codex-acp-adapter"})

			var out bytes.Buffer
			input := fmt.Sprintf(`{"jsonrpc":"2.0","id":"p1","method":%q}`+"\n", method)
			err := server.Serve(strings.NewReader(input), &out)
			if err != nil {
				t.Fatalf("Serve returned error: %v", err)
			}

			var got response
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Error == nil {
				t.Fatal("expected error")
			}
			if got.Error.Code != -32004 {
				t.Fatalf("error code = %d, want -32004", got.Error.Code)
			}
		})
	}
}

func TestMethodContextRequestWaitsForClientResponse(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"}, WithMethod("test/needs_client", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
		result, rpcErr, err := ctx.Request("client/approve", map[string]any{"toolCallId": "tool-1"})
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: "request failed", Data: err.Error()}
		}
		if rpcErr != nil {
			return nil, rpcErr
		}
		var decoded map[string]any
		if err := json.Unmarshal(result, &decoded); err != nil {
			return nil, &RPCError{Code: -32000, Message: "decode failed", Data: err.Error()}
		}
		return map[string]any{"decision": decoded["decision"]}, nil
	}))

	client := newServerTestClient(t, server)
	client.write(t, `{"jsonrpc":"2.0","id":1,"method":"test/needs_client","params":{}}`)
	request := client.read(t)
	if request.Method != "client/approve" {
		t.Fatalf("first method = %q, want client/approve", request.Method)
	}
	if string(request.ID) != `"server-1"` {
		t.Fatalf("client request id = %s, want server-1", request.ID)
	}
	client.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"decision":"approved"}}`, request.ID))
	final := client.read(t)
	var result map[string]any
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode final result: %v", err)
	}
	if result["decision"] != "approved" {
		t.Fatalf("final result = %#v, want approved", result)
	}
	client.close(t)
}

func TestMethodContextRequestsUseDistinctOutboundIDs(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"}, WithMethod("test/needs_client_twice", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
		first, rpcErr, err := ctx.Request("client/first", nil)
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: "first request failed", Data: err.Error()}
		}
		if rpcErr != nil {
			return nil, rpcErr
		}
		second, rpcErr, err := ctx.Request("client/second", nil)
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: "second request failed", Data: err.Error()}
		}
		if rpcErr != nil {
			return nil, rpcErr
		}
		return map[string]any{"first": string(first), "second": string(second)}, nil
	}))

	client := newServerTestClient(t, server)
	client.write(t, `{"jsonrpc":"2.0","id":1,"method":"test/needs_client_twice","params":{}}`)
	first := client.read(t)
	if first.Method != "client/first" || string(first.ID) != `"server-1"` {
		t.Fatalf("first client request = method %q id %s, want client/first server-1", first.Method, first.ID)
	}
	client.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":"first"}}`, first.ID))
	second := client.read(t)
	if second.Method != "client/second" || string(second.ID) != `"server-2"` {
		t.Fatalf("second client request = method %q id %s, want client/second server-2", second.Method, second.ID)
	}
	if string(first.ID) == string(second.ID) {
		t.Fatalf("client request IDs are not distinct: %s", first.ID)
	}
	client.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":"second"}}`, second.ID))
	final := client.read(t)
	var result map[string]string
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode final result: %v", err)
	}
	if !strings.Contains(result["first"], "first") || !strings.Contains(result["second"], "second") {
		t.Fatalf("final result = %#v, want both client responses", result)
	}
	client.close(t)
}

func TestMethodContextRequestReturnsClientRPCError(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"}, WithMethod("test/needs_client", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
		_, rpcErr, err := ctx.Request("client/approve", nil)
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: "request failed", Data: err.Error()}
		}
		if rpcErr != nil {
			return nil, rpcErr
		}
		return map[string]any{"unexpected": true}, nil
	}))

	client := newServerTestClient(t, server)
	client.write(t, `{"jsonrpc":"2.0","id":1,"method":"test/needs_client","params":{}}`)
	request := client.read(t)
	client.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32060,"message":"denied","data":{"reason":"policy"}}}`, request.ID))
	final := client.read(t)
	if final.Error == nil {
		t.Fatalf("final response error is nil: %#v", final)
	}
	if final.Error.Code != -32060 || final.Error.Message != "denied" {
		t.Fatalf("final error = %+v, want denied -32060", final.Error)
	}
	client.close(t)
}

func TestNotificationDispatchesWhileMethodIsRunning(t *testing.T) {
	cancelled := make(chan struct{})
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithMethod("session/prompt", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			select {
			case <-cancelled:
				return map[string]any{"stopReason": "cancelled"}, nil
			case <-time.After(250 * time.Millisecond):
				return nil, &RPCError{Code: -32000, Message: "cancel was not dispatched"}
			}
		}),
		WithNotification("session/cancel", func(_ json.RawMessage) error {
			close(cancelled)
			return nil
		}),
	)

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1"}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`,
	}, "\n")+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want prompt response\n%s", len(envelopes), out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result["stopReason"] != "cancelled" {
		t.Fatalf("result = %#v, want cancelled", result)
	}
}

func TestConcurrentMethodDispatchesWhileMethodIsRunning(t *testing.T) {
	cancelled := make(chan struct{})
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithMethod("session/prompt", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			select {
			case <-cancelled:
				return map[string]any{"stopReason": "cancelled"}, nil
			case <-time.After(250 * time.Millisecond):
				return nil, &RPCError{Code: -32000, Message: "cancel was not dispatched"}
			}
		}),
		WithConcurrentMethod("session/cancel", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			close(cancelled)
			return map[string]any{"cancelled": true}, nil
		}),
	)

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt","method":"session/prompt","params":{"sessionId":"s1"}}`,
		`{"jsonrpc":"2.0","id":"cancel","method":"session/cancel","params":{"sessionId":"s1"}}`,
	}, "\n")+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want cancel + prompt responses\n%s", len(envelopes), out.String())
	}
	byID := map[string]serverEnvelope{}
	for _, envelope := range envelopes {
		byID[string(envelope.ID)] = envelope
	}
	var cancelResult map[string]any
	if err := json.Unmarshal(byID[`"cancel"`].Result, &cancelResult); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if cancelResult["cancelled"] != true {
		t.Fatalf("cancel result = %#v, want cancelled", cancelResult)
	}
	var promptResult map[string]any
	if err := json.Unmarshal(byID[`"prompt"`].Result, &promptResult); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if promptResult["stopReason"] != "cancelled" {
		t.Fatalf("prompt result = %#v, want cancelled", promptResult)
	}
}

func TestProtocolCancelRequestCancelsRunningMethod(t *testing.T) {
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithMethod("session/prompt", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
			_, _, err := ctx.Request("session/request_permission", map[string]string{"toolCallId": "tool-1"})
			if !errors.Is(err, context.Canceled) {
				message := "request was not cancelled"
				if err != nil {
					message = err.Error()
				}
				return nil, &RPCError{Code: -32000, Message: "unexpected request error", Data: message}
			}
			return map[string]any{"stopReason": "cancelled"}, nil
		}),
	)

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		serveDone <- err
	}()
	decoder := json.NewDecoder(outputReader)

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"prompt","method":"session/prompt","params":{"sessionId":"s1"}}`); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	var permission serverEnvelope
	if err := decoder.Decode(&permission); err != nil {
		t.Fatalf("decode permission request: %v", err)
	}
	if permission.Method != "session/request_permission" {
		t.Fatalf("first method = %q, want session/request_permission", permission.Method)
	}

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"prompt"}}`); err != nil {
		t.Fatalf("write cancel_request: %v", err)
	}
	var outboundCancellation serverEnvelope
	if err := decoder.Decode(&outboundCancellation); err != nil {
		t.Fatalf("decode outbound cancellation: %v", err)
	}
	if outboundCancellation.Method != "$/cancel_request" {
		t.Fatalf("outbound cancellation method = %q, want $/cancel_request", outboundCancellation.Method)
	}
	var outboundCancellationParams cancelRequestParams
	if err := json.Unmarshal(outboundCancellation.Params, &outboundCancellationParams); err != nil {
		t.Fatalf("decode outbound cancellation params: %v", err)
	}
	if string(outboundCancellationParams.RequestID) != `"server-1"` {
		t.Fatalf("outbound cancellation request id = %s, want server-1", outboundCancellationParams.RequestID)
	}
	var promptResponse serverEnvelope
	if err := decoder.Decode(&promptResponse); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	if string(promptResponse.ID) != `"prompt"` {
		t.Fatalf("prompt response id = %s, want prompt", promptResponse.ID)
	}
	var result map[string]any
	if err := json.Unmarshal(promptResponse.Result, &result); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if result["stopReason"] != "cancelled" {
		t.Fatalf("prompt result = %#v, want cancelled", result)
	}

	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
}

func TestMethodContextRequestContextDiscardsLateResponseAfterCancellation(t *testing.T) {
	cancelRequest := make(chan context.CancelFunc, 1)
	server := NewServer(
		AdapterInfo{Name: "test-adapter"},
		WithMethod("test/cancellable", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
			requestCtx, cancel := context.WithCancel(ctx.Context())
			cancelRequest <- cancel
			_, _, err := ctx.RequestContext(requestCtx, "session/request_permission", nil)
			if !errors.Is(err, context.Canceled) {
				return nil, &RPCError{Code: -32000, Message: "unexpected request error", Data: fmt.Sprint(err)}
			}
			ctx.conn.requestMu.Lock()
			defer ctx.conn.requestMu.Unlock()
			return map[string]any{
				"stopReason": "cancelled",
				"pending":    len(ctx.conn.pending),
			}, nil
		}),
		WithMethod("test/pending_state", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
			ctx.conn.requestMu.Lock()
			defer ctx.conn.requestMu.Unlock()
			return map[string]any{
				"pending": len(ctx.conn.pending),
			}, nil
		}),
	)

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		serveDone <- err
	}()
	decoder := json.NewDecoder(outputReader)

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"prompt","method":"test/cancellable","params":{}}`); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	var permission serverEnvelope
	if err := decoder.Decode(&permission); err != nil {
		t.Fatalf("decode permission request: %v", err)
	}
	if string(permission.ID) != `"server-1"` {
		t.Fatalf("permission request id = %s, want server-1", permission.ID)
	}

	select {
	case cancel := <-cancelRequest:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound request cancellation handle")
	}
	var cancelNotification serverEnvelope
	if err := decoder.Decode(&cancelNotification); err != nil {
		t.Fatalf("decode outbound cancellation notification: %v", err)
	}
	if cancelNotification.Method != "$/cancel_request" || len(cancelNotification.ID) != 0 {
		t.Fatalf("outbound cancellation envelope = %#v, want notification", cancelNotification)
	}
	var cancelParams cancelRequestParams
	if err := json.Unmarshal(cancelNotification.Params, &cancelParams); err != nil {
		t.Fatalf("decode outbound cancellation params: %v", err)
	}
	if string(cancelParams.RequestID) != `"server-1"` {
		t.Fatalf("outbound cancellation request id = %s, want server-1", cancelParams.RequestID)
	}
	var promptResponse serverEnvelope
	if err := decoder.Decode(&promptResponse); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	if string(promptResponse.ID) != `"prompt"` {
		t.Fatalf("prompt response id = %s, want prompt", promptResponse.ID)
	}
	var promptResult map[string]any
	if err := json.Unmarshal(promptResponse.Result, &promptResult); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if promptResult["pending"] != float64(0) {
		t.Fatalf("outbound request state without a late response = %#v, want empty", promptResult)
	}

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`); err != nil {
		t.Fatalf("write late permission response: %v", err)
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"state","method":"test/pending_state","params":{}}`); err != nil {
		t.Fatalf("write pending-state request: %v", err)
	}
	var stateResponse serverEnvelope
	if err := decoder.Decode(&stateResponse); err != nil {
		t.Fatalf("decode pending-state response: %v", err)
	}
	var state map[string]int
	if err := json.Unmarshal(stateResponse.Result, &state); err != nil {
		t.Fatalf("decode pending state: %v", err)
	}
	if state["pending"] != 0 {
		t.Fatalf("outbound request state after late response = %#v, want empty", state)
	}

	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
}

func TestMethodContextRequestContextKeepsCancellationAuthoritativeWhenNotificationFails(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	writer := &cancelThenFailWriter{cancel: cancel}
	conn := &connection{
		encoder: json.NewEncoder(writer),
		pending: map[string]chan clientResponse{},
	}
	ctx := &MethodContext{conn: conn, ctx: context.Background()}
	_, _, err := ctx.RequestContext(requestCtx, "session/request_permission", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v, want context canceled", err)
	}
	if writer.calls != 2 {
		t.Fatalf("writer calls = %d, want request plus failed cancellation notification", writer.calls)
	}
	if len(conn.pending) != 0 {
		t.Fatalf("pending requests = %d, want abandoned", len(conn.pending))
	}
}

func TestConnectionDiscardsUnsolicitedFutureResponse(t *testing.T) {
	conn := &connection{
		pending: map[string]chan clientResponse{},
	}
	id := json.RawMessage(`"server-1"`)
	conn.deliverResponse(message{ID: &id, Result: json.RawMessage(`{"spoofed":true}`)})
	registeredID, resultCh, err := conn.registerRequest()
	if err != nil {
		t.Fatal(err)
	}
	if string(registeredID) != `"server-1"` {
		t.Fatalf("registered id = %s, want server-1", registeredID)
	}
	select {
	case result := <-resultCh:
		t.Fatalf("future request consumed unsolicited response: %#v", result)
	default:
	}
}

func TestConnectionDiscardsCancelRequestsForUnknownInboundIDs(t *testing.T) {
	conn := &connection{
		queued:    map[string]struct{}{},
		active:    map[string]context.CancelFunc{},
		cancelled: map[string]struct{}{},
	}
	for i := 0; i < 10_000; i++ {
		conn.cancelInboundRequest(json.RawMessage(fmt.Sprintf(`{"requestId":"unknown-%d"}`, i)))
	}
	if got := len(conn.cancelled); got != 0 {
		t.Fatalf("retained %d cancellation records for unknown request IDs, want 0", got)
	}

	id := json.RawMessage(`"queued"`)
	conn.queueInboundRequest(&id)
	conn.cancelInboundRequest(json.RawMessage(`{"requestId":"queued"}`))
	if got := len(conn.cancelled); got != 1 {
		t.Fatalf("retained %d cancellation records for a queued request, want 1", got)
	}
	ctx, finish := conn.beginInboundRequest(&id)
	defer finish()
	if !errors.Is(ctx.Context().Err(), context.Canceled) {
		t.Fatalf("queued request context error = %v, want context canceled", ctx.Context().Err())
	}
	if got := len(conn.cancelled); got != 0 {
		t.Fatalf("retained %d cancellation records after request start, want 0", got)
	}
}

func TestProtocolCancelRequestCancelsQueuedMethodBeforeStart(t *testing.T) {
	unblockFirst := make(chan struct{})
	cancelConsumed := make(chan struct{})
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithMethod("test/block", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			<-unblockFirst
			return map[string]bool{"ok": true}, nil
		}),
		WithMethod("session/prompt", func(ctx *MethodContext, _ json.RawMessage) (any, *RPCError) {
			if !errors.Is(ctx.Context().Err(), context.Canceled) {
				return nil, &RPCError{Code: -32000, Message: "request was not cancelled before start"}
			}
			return map[string]any{"stopReason": "cancelled"}, nil
		}),
		WithNotification("test/cancel_consumed", func(_ json.RawMessage) error {
			close(cancelConsumed)
			return nil
		}),
	)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"block","method":"test/block","params":{}}`,
		`{"jsonrpc":"2.0","id":"prompt","method":"session/prompt","params":{"sessionId":"s1"}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"prompt"}}`,
		`{"jsonrpc":"2.0","method":"test/cancel_consumed","params":{}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(strings.NewReader(input), &out)
	}()

	select {
	case <-cancelConsumed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancel_request to be consumed")
	}
	close(unblockFirst)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want block + prompt responses\n%s", len(envelopes), out.String())
	}
	byID := map[string]serverEnvelope{}
	for _, envelope := range envelopes {
		byID[string(envelope.ID)] = envelope
	}
	var result map[string]any
	if err := json.Unmarshal(byID[`"prompt"`].Result, &result); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if result["stopReason"] != "cancelled" {
		t.Fatalf("prompt result = %#v, want cancelled", result)
	}
}

func TestNotificationDispatchesAfterBurstOfQueuedMethods(t *testing.T) {
	cancelled := make(chan struct{})
	server := NewServer(
		AdapterInfo{Name: "codex-acp-adapter"},
		WithMethod("session/prompt", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			select {
			case <-cancelled:
				return map[string]any{"stopReason": "cancelled"}, nil
			case <-time.After(250 * time.Millisecond):
				return nil, &RPCError{Code: -32000, Message: "cancel was not dispatched"}
			}
		}),
		WithMethod("test/noop", func(_ *MethodContext, _ json.RawMessage) (any, *RPCError) {
			return map[string]any{"ok": true}, nil
		}),
		WithNotification("session/cancel", func(_ json.RawMessage) error {
			close(cancelled)
			return nil
		}),
	)

	lines := []string{`{"jsonrpc":"2.0","id":"prompt","method":"session/prompt","params":{"sessionId":"s1"}}`}
	for i := 0; i < 160; i++ {
		lines = append(lines, `{"jsonrpc":"2.0","id":"noop","method":"test/noop","params":{}}`)
	}
	lines = append(lines, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 161 {
		t.Fatalf("got %d envelopes, want prompt response + noop responses\n%s", len(envelopes), out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if result["stopReason"] != "cancelled" {
		t.Fatalf("prompt result = %#v, want cancelled", result)
	}
}

type serverEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type cancelThenFailWriter struct {
	cancel context.CancelFunc
	calls  int
}

func (w *cancelThenFailWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		w.cancel()
		return len(data), nil
	}
	return 0, errors.New("write failed")
}

type serverTestClient struct {
	input   *io.PipeWriter
	output  *io.PipeReader
	decoder *json.Decoder
	done    <-chan error
}

func newServerTestClient(t *testing.T, server *Server) *serverTestClient {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := server.Serve(inputReader, outputWriter)
		_ = outputWriter.CloseWithError(err)
		_ = inputReader.Close()
		done <- err
	}()
	client := &serverTestClient{
		input:   inputWriter,
		output:  outputReader,
		decoder: json.NewDecoder(outputReader),
		done:    done,
	}
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = outputReader.Close()
	})
	return client
}

func (c *serverTestClient) write(t *testing.T, message string) {
	t.Helper()
	if _, err := fmt.Fprintln(c.input, message); err != nil {
		t.Fatalf("write client message: %v", err)
	}
}

func (c *serverTestClient) read(t *testing.T) serverEnvelope {
	t.Helper()
	var envelope serverEnvelope
	if err := c.decoder.Decode(&envelope); err != nil {
		t.Fatalf("read server message: %v", err)
	}
	return envelope
}

func (c *serverTestClient) close(t *testing.T) {
	t.Helper()
	if err := c.input.Close(); err != nil {
		t.Fatalf("close client input: %v", err)
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
}

func decodeServerEnvelopes(t testing.TB, raw []byte) []serverEnvelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	envelopes := make([]serverEnvelope, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var envelope serverEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode envelope %q: %v", line, err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
}
