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

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"test/needs_client","params":{}}`,
		`{"jsonrpc":"2.0","id":"server-1","result":{"decision":"approved"}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want client request + final response\n%s", len(envelopes), out.String())
	}
	if envelopes[0].Method != "client/approve" {
		t.Fatalf("first method = %q, want client/approve", envelopes[0].Method)
	}
	if string(envelopes[0].ID) != `"server-1"` {
		t.Fatalf("client request id = %s, want server-1", envelopes[0].ID)
	}
	var result map[string]any
	if err := json.Unmarshal(envelopes[1].Result, &result); err != nil {
		t.Fatalf("decode final result: %v", err)
	}
	if result["decision"] != "approved" {
		t.Fatalf("final result = %#v, want approved", result)
	}
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

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"test/needs_client_twice","params":{}}`,
		`{"jsonrpc":"2.0","id":"server-1","result":{"ok":"first"}}`,
		`{"jsonrpc":"2.0","id":"server-2","result":{"ok":"second"}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 3 {
		t.Fatalf("got %d envelopes, want two client requests + final response\n%s", len(envelopes), out.String())
	}
	if envelopes[0].Method != "client/first" || string(envelopes[0].ID) != `"server-1"` {
		t.Fatalf("first client request = method %q id %s, want client/first server-1", envelopes[0].Method, envelopes[0].ID)
	}
	if envelopes[1].Method != "client/second" || string(envelopes[1].ID) != `"server-2"` {
		t.Fatalf("second client request = method %q id %s, want client/second server-2", envelopes[1].Method, envelopes[1].ID)
	}
	if string(envelopes[0].ID) == string(envelopes[1].ID) {
		t.Fatalf("client request IDs are not distinct: %s", envelopes[0].ID)
	}
	var result map[string]string
	if err := json.Unmarshal(envelopes[2].Result, &result); err != nil {
		t.Fatalf("decode final result: %v", err)
	}
	if !strings.Contains(result["first"], "first") || !strings.Contains(result["second"], "second") {
		t.Fatalf("final result = %#v, want both client responses", result)
	}
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

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"test/needs_client","params":{}}`,
		`{"jsonrpc":"2.0","id":"server-1","error":{"code":-32060,"message":"denied","data":{"reason":"policy"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want client request + final response", len(envelopes))
	}
	if envelopes[1].Error == nil {
		t.Fatalf("final response error is nil: %#v", envelopes[1])
	}
	if envelopes[1].Error.Code != -32060 || envelopes[1].Error.Message != "denied" {
		t.Fatalf("final error = %+v, want denied -32060", envelopes[1].Error)
	}
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
