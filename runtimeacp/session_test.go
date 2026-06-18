package runtimeacp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestNewSessionSendsWorkspaceAndMCPServers(t *testing.T) {
	client := newSessionClient(t)

	result, err := runtimeacp.NewSession(context.Background(), client, runtimeacp.NewSessionParams{
		CWD:                   "/tmp/project",
		AdditionalDirectories: []string{"/tmp/shared"},
		MCPServers: []runtimeacp.MCPServer{{
			Name:    "filesystem",
			Command: "/bin/mcp",
			Args:    []string{"--stdio"},
			Env:     []runtimeacp.EnvVariable{{Name: "TOKEN", Value: "secret"}},
		}},
	})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if result.SessionID != "sess-test" {
		t.Fatalf("SessionID = %q, want sess-test", result.SessionID)
	}
	if len(result.ConfigOptions) != 1 {
		t.Fatalf("ConfigOptions len = %d, want 1", len(result.ConfigOptions))
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"configOptions"`) || !strings.Contains(string(raw), `"modes"`) {
		t.Fatalf("marshaled result = %s, want configOptions and modes preserved", raw)
	}
}

func TestNewSessionRawPreservesUnknownParams(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"sessionId":"sess-test"}`)}

	_, err := runtimeacp.NewSessionRaw(context.Background(), client, json.RawMessage(`{"cwd":"/tmp/project","x-extra":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("NewSessionRaw returned error: %v", err)
	}
	if client.method != "session/new" {
		t.Fatalf("method = %q, want session/new", client.method)
	}
	var params map[string]json.RawMessage
	mustJSONRoundTrip(t, client.params, &params)
	if string(params["x-extra"]) != `{"enabled":true}` {
		t.Fatalf("x-extra = %s, want preserved object", params["x-extra"])
	}
}

func TestPromptSendsTextContentAndParsesStopReason(t *testing.T) {
	client := newSessionClient(t)

	resultCh := make(chan error, 1)
	go func() {
		result, err := runtimeacp.Prompt(context.Background(), client, runtimeacp.PromptParams{
			SessionID: "sess-test",
			Prompt: []runtimeacp.ContentBlock{{
				Type: "text",
				Text: "hello",
			}},
		})
		if err != nil {
			resultCh <- err
			return
		}
		if result.StopReason != runtimeacp.StopReasonEndTurn {
			resultCh <- errors.New("unexpected stop reason: " + string(result.StopReason))
			return
		}
		resultCh <- nil
	}()

	event := nextRuntimeACPEvent(t, client)
	if event.Method != "session/update" {
		t.Fatalf("event method = %q, want session/update", event.Method)
	}
	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prompt result")
	}
}

func TestPromptRawPreservesUnknownParams(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"stopReason":"end_turn"}`)}

	_, err := runtimeacp.PromptRaw(context.Background(), client, json.RawMessage(`{"sessionId":"sess-test","prompt":[{"type":"text","text":"hello","x-block":1}],"x-prompt":true}`))
	if err != nil {
		t.Fatalf("PromptRaw returned error: %v", err)
	}
	if client.method != "session/prompt" {
		t.Fatalf("method = %q, want session/prompt", client.method)
	}
	var params map[string]json.RawMessage
	mustJSONRoundTrip(t, client.params, &params)
	if string(params["x-prompt"]) != `true` {
		t.Fatalf("x-prompt = %s, want true", params["x-prompt"])
	}
	if !strings.Contains(string(params["prompt"]), `"x-block":1`) {
		t.Fatalf("prompt = %s, want x-block preserved", params["prompt"])
	}
}

func TestPromptPreservesRawRuntimeResult(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"stopReason":"end_turn","x-result":{"kept":true}}`)}

	result, err := runtimeacp.Prompt(context.Background(), client, runtimeacp.PromptParams{
		SessionID: "sess-test",
		Prompt:    []runtimeacp.ContentBlock{{Type: "text", Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"x-result":{"kept":true}`) {
		t.Fatalf("marshaled result = %s, want x-result preserved", raw)
	}
}

func TestForkSessionSendsWorkspaceAndPreservesRuntimeResult(t *testing.T) {
	client := newSessionClient(t)

	result, err := runtimeacp.ForkSession(context.Background(), client, runtimeacp.ForkSessionParams{
		SessionID:             "sess-test",
		CWD:                   "/tmp/project",
		AdditionalDirectories: []string{"/tmp/shared"},
		MCPServers:            []runtimeacp.MCPServer{{Name: "filesystem", Command: "/bin/mcp"}},
	})
	if err != nil {
		t.Fatalf("ForkSession returned error: %v", err)
	}
	if result.SessionID != "forked-session" {
		t.Fatalf("SessionID = %q, want forked-session", result.SessionID)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"configOptions"`) ||
		!strings.Contains(string(raw), `"modes"`) ||
		!strings.Contains(string(raw), `"source":"fork"`) {
		t.Fatalf("marshaled result = %s, want configOptions, modes, and source preserved", raw)
	}
}

func TestLoadSessionSendsWorkspaceAndReplaysUpdates(t *testing.T) {
	client := newSessionClient(t)

	resultCh := make(chan error, 1)
	go func() {
		raw, err := runtimeacp.LoadSession(context.Background(), client, runtimeacp.LoadSessionParams{
			SessionID:             "sess-test",
			CWD:                   "/tmp/project",
			AdditionalDirectories: []string{"/tmp/shared"},
			MCPServers:            []runtimeacp.MCPServer{{Name: "filesystem", Command: "/bin/mcp"}},
		})
		if err != nil {
			resultCh <- err
			return
		}
		if string(raw) != "null" {
			resultCh <- errors.New("load result = " + string(raw) + ", want null")
			return
		}
		resultCh <- nil
	}()

	event := nextRuntimeACPEvent(t, client)
	if event.Method != "session/update" {
		t.Fatalf("event method = %q, want session/update", event.Method)
	}
	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("LoadSession returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for load result")
	}
}

func TestResumeSessionPreservesRuntimeResult(t *testing.T) {
	client := newSessionClient(t)

	raw, err := runtimeacp.ResumeSession(context.Background(), client, runtimeacp.ResumeSessionParams{
		SessionID: "sess-test",
		CWD:       "/tmp/project",
	})
	if err != nil {
		t.Fatalf("ResumeSession returned error: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode resume result: %v", err)
	}
	if result["mode"] != "default" {
		t.Fatalf("resume result = %#v, want mode default", result)
	}
}

func TestListSessionsParsesSessionsAndCursor(t *testing.T) {
	client := newSessionClient(t)

	result, err := runtimeacp.ListSessions(context.Background(), client, runtimeacp.ListSessionsParams{
		CWD:    "/tmp/project",
		Cursor: "page-1",
	})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(result.Sessions))
	}
	session := result.Sessions[0]
	if session.SessionID != "sess-test" || session.CWD != "/tmp/project" || session.Title != "Test session" {
		t.Fatalf("session = %#v, want sess-test /tmp/project Test session", session)
	}
	if len(session.AdditionalDirectories) != 1 || session.AdditionalDirectories[0] != "/tmp/shared" {
		t.Fatalf("additional dirs = %#v, want /tmp/shared", session.AdditionalDirectories)
	}
	if result.NextCursor != "page-2" {
		t.Fatalf("NextCursor = %q, want page-2", result.NextCursor)
	}
}

func TestListSessionsPreservesRawRuntimeResult(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"sessions":[],"nextCursor":"page-2","x-result":true}`)}

	result, err := runtimeacp.ListSessions(context.Background(), client, runtimeacp.ListSessionsParams{CWD: "/tmp/project"})
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"x-result":true`) {
		t.Fatalf("marshaled result = %s, want x-result preserved", raw)
	}
}

func TestCancelSendsNotification(t *testing.T) {
	client := newSessionClient(t)

	if err := runtimeacp.Cancel(context.Background(), client, runtimeacp.CancelParams{SessionID: "sess-test"}); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	event := nextRuntimeACPEvent(t, client)
	if event.Method != "runtime/cancel_seen" {
		t.Fatalf("event method = %q, want runtime/cancel_seen", event.Method)
	}
}

func TestCloseSessionSendsRequest(t *testing.T) {
	client := newSessionClient(t)

	if err := runtimeacp.CloseSession(context.Background(), client, runtimeacp.CloseSessionParams{SessionID: "sess-test"}); err != nil {
		t.Fatalf("CloseSession returned error: %v", err)
	}
}

func TestDeleteSessionSendsRequest(t *testing.T) {
	client := newSessionClient(t)

	if err := runtimeacp.DeleteSession(context.Background(), client, runtimeacp.DeleteSessionParams{SessionID: "sess-test"}); err != nil {
		t.Fatalf("DeleteSession returned error: %v", err)
	}
}

func TestSessionRPCErrorPropagates(t *testing.T) {
	client := newSessionClient(t)

	_, err := runtimeacp.NewSession(context.Background(), client, runtimeacp.NewSessionParams{CWD: "/tmp/fail"})
	var rpcErr *runtimejsonrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("NewSession error = %T %[1]v, want RPCError", err)
	}
	if rpcErr.Code != -32010 {
		t.Fatalf("RPCError.Code = %d, want -32010", rpcErr.Code)
	}
}

func newSessionClient(t testing.TB) *runtimejsonrpc.Client {
	t.Helper()
	t.Setenv("GO_WANT_RUNTIMEACP_SESSION_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeACPSessionHelper", "--"},
		InheritEnv: []string{"GO_WANT_RUNTIMEACP_SESSION_HELPER"},
	})
	client, err := runtimejsonrpc.Connect(context.Background(), runtimejsonrpc.ConnectSpec{
		Launcher: launcher,
		Launch: runtimeproc.LaunchSpec{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Kill()
		_ = client.Wait()
	})
	return client
}

func nextRuntimeACPEvent(t testing.TB, client *runtimejsonrpc.Client) runtimejsonrpc.Event {
	t.Helper()
	select {
	case event := <-client.Events():
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime event")
	}
	return runtimejsonrpc.Event{}
}

func TestRuntimeACPSessionHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIMEACP_SESSION_HELPER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := decoder.Decode(&req); err != nil {
			return
		}
		if len(req.ID) == 0 {
			if req.Method == "session/cancel" && string(req.Params) == `{"sessionId":"sess-test"}` {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "runtime/cancel_seen",
					"params":  map[string]any{"ok": true},
				})
			}
			continue
		}
		switch req.Method {
		case "session/new":
			var params runtimeacp.NewSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.CWD == "" ||
				(params.CWD != "/tmp/project" && params.CWD != "/tmp/fail") {
				writeSessionError(encoder, req.ID, -32602, "bad session/new params")
				continue
			}
			if params.CWD == "/tmp/fail" {
				writeSessionError(encoder, req.ID, -32010, "session failed")
				continue
			}
			if len(params.MCPServers) != 1 ||
				params.MCPServers[0].Name != "filesystem" ||
				len(params.AdditionalDirectories) != 1 {
				writeSessionError(encoder, req.ID, -32602, "missing mcp/additional dirs")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"sessionId": "sess-test",
					"configOptions": []map[string]any{{
						"id":           "model",
						"name":         "Model",
						"type":         "select",
						"currentValue": "fast",
						"options":      []map[string]any{{"value": "fast", "name": "Fast"}},
					}},
					"modes": map[string]any{
						"currentModeId": "ask",
					},
				},
			})
		case "session/prompt":
			var params runtimeacp.PromptParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.SessionID != "sess-test" ||
				len(params.Prompt) != 1 ||
				params.Prompt[0].Type != "text" ||
				params.Prompt[0].Text != "hello" {
				writeSessionError(encoder, req.ID, -32602, "bad prompt params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "sess-test",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "hi"},
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{"stopReason": "end_turn"},
			})
		case "session/load":
			var params runtimeacp.LoadSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.SessionID != "sess-test" ||
				params.CWD != "/tmp/project" ||
				len(params.MCPServers) != 1 ||
				len(params.AdditionalDirectories) != 1 {
				writeSessionError(encoder, req.ID, -32602, "bad load params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "sess-test",
					"update": map[string]any{
						"sessionUpdate": "user_message_chunk",
						"content":       map[string]any{"type": "text", "text": "replayed"},
					},
				},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  nil,
			})
		case "session/fork":
			var params runtimeacp.ForkSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.SessionID != "sess-test" ||
				params.CWD != "/tmp/project" ||
				len(params.MCPServers) != 1 ||
				len(params.AdditionalDirectories) != 1 {
				writeSessionError(encoder, req.ID, -32602, "bad fork params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"sessionId": "forked-session",
					"configOptions": []map[string]any{{
						"id":           "model",
						"name":         "Model",
						"type":         "select",
						"currentValue": "smart",
						"options":      []map[string]any{{"value": "smart", "name": "Smart"}},
					}},
					"modes":  map[string]any{"currentModeId": "code"},
					"source": "fork",
				},
			})
		case "session/resume":
			var params runtimeacp.ResumeSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.SessionID != "sess-test" ||
				params.CWD != "/tmp/project" {
				writeSessionError(encoder, req.ID, -32602, "bad resume params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{"mode": "default"},
			})
		case "session/list":
			var params runtimeacp.ListSessionsParams
			if err := json.Unmarshal(req.Params, &params); err != nil ||
				params.CWD != "/tmp/project" ||
				params.Cursor != "page-1" {
				writeSessionError(encoder, req.ID, -32602, "bad list params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"sessions": []map[string]any{{
						"sessionId":             "sess-test",
						"cwd":                   "/tmp/project",
						"additionalDirectories": []string{"/tmp/shared"},
						"title":                 "Test session",
						"updatedAt":             "2026-06-18T05:00:00Z",
						"_meta":                 map[string]any{"messageCount": 2},
					}},
					"nextCursor": "page-2",
				},
			})
		case "session/close":
			var params runtimeacp.CloseSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionID != "sess-test" {
				writeSessionError(encoder, req.ID, -32602, "bad close params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{},
			})
		case "session/delete":
			var params runtimeacp.DeleteSessionParams
			if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionID != "sess-test" {
				writeSessionError(encoder, req.ID, -32602, "bad delete params")
				continue
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{},
			})
		default:
			writeSessionError(encoder, req.ID, -32601, "method not found")
		}
	}
}

func writeSessionError(encoder *json.Encoder, id json.RawMessage, code int, message string) {
	_ = encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}
