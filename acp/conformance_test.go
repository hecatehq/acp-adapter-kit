package acp_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
)

func TestConformancePreservesRequestID(t *testing.T) {
	client := acptest.NewClient(t, acp.NewServer(acp.AdapterInfo{Name: "test"}))

	response := client.RequestWithID("request-123", "initialize", map[string]any{})

	if got, want := string(response.ID), `"request-123"`; got != want {
		t.Fatalf("response id = %s, want %s", got, want)
	}
}

func TestConformanceParseErrorDoesNotStopNextRequest(t *testing.T) {
	client := acptest.NewClient(t, acp.NewServer(acp.AdapterInfo{Name: "test"}))

	responses := client.SendRaw("{bad json}\n" + `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")

	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}
	if responses[0].Error == nil || responses[0].Error.Code != -32700 {
		t.Fatalf("first response error = %+v, want parse error", responses[0].Error)
	}
	if responses[1].Error != nil {
		t.Fatalf("second response error = %+v", responses[1].Error)
	}
}

func TestConformanceInvalidJSONRPCVersion(t *testing.T) {
	client := acptest.NewClient(t, acp.NewServer(acp.AdapterInfo{Name: "test"}))

	response := client.Send(map[string]any{
		"jsonrpc": "1.0",
		"id":      1,
		"method":  "initialize",
	})[0]

	if response.Error == nil || response.Error.Code != -32600 {
		t.Fatalf("error = %+v, want invalid request", response.Error)
	}
}

func TestConformanceNotificationDispatchesWithoutResponse(t *testing.T) {
	var got map[string]string
	server := acp.NewServer(acp.AdapterInfo{Name: "test"}, acp.WithNotification("session/cancel", func(params json.RawMessage) error {
		return json.Unmarshal(params, &got)
	}))
	client := acptest.NewClient(t, server)

	client.Notify("session/cancel", map[string]string{"sessionId": "s1"})

	if got["sessionId"] != "s1" {
		t.Fatalf("notification params = %#v", got)
	}
}

func TestConformanceFakeRuntimeMethodDispatch(t *testing.T) {
	runtime := &fakeRuntime{}
	server := acp.NewServer(
		acp.AdapterInfo{Name: "test"},
		acp.WithMethod("session/new", runtime.newSession),
		acp.WithMethod("session/prompt", runtime.prompt),
	)
	client := acptest.NewClient(t, server)

	newSession := client.Request("session/new", map[string]string{"cwd": "/tmp/work"})
	var session struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	newSession.ResultInto(t, &session)
	if session.SessionID != "fake-session" || session.CWD != "/tmp/work" {
		t.Fatalf("session result = %#v", session)
	}

	prompt := client.Request("session/prompt", map[string]any{
		"sessionId": "fake-session",
		"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
	})
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	prompt.ResultInto(t, &promptResult)
	if promptResult.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q", promptResult.StopReason)
	}
	if runtime.promptText != "hello" {
		t.Fatalf("runtime prompt text = %q", runtime.promptText)
	}
}

func TestConformanceRuntimeErrorPropagates(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"}, acp.WithMethod("session/prompt", func(*acp.MethodContext, json.RawMessage) (any, *acp.RPCError) {
		return nil, &acp.RPCError{Code: -32042, Message: "runtime failed", Data: "boom"}
	}))
	client := acptest.NewClient(t, server)

	response := client.Request("session/prompt", map[string]any{"sessionId": "s1"})

	if response.Error == nil {
		t.Fatal("expected runtime error")
	}
	if response.Error.Code != -32042 || response.Error.Message != "runtime failed" || response.Error.Data != "boom" {
		t.Fatalf("runtime error = %+v", response.Error)
	}
}

func TestConformanceRejectsOversizedMessages(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"})

	var input bytes.Buffer
	input.Write(bytes.Repeat([]byte("x"), 1024*1024+1))
	input.WriteByte('\n')

	var output bytes.Buffer
	err := server.Serve(&input, &output)
	if err == nil {
		t.Fatal("expected oversized message error")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("error = %v, want token too long", err)
	}
}

type fakeRuntime struct {
	promptText string
}

func (f *fakeRuntime) newSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
	}
	return map[string]string{"sessionId": "fake-session", "cwd": req.CWD}, nil
}

func (f *fakeRuntime) prompt(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
	}
	if len(req.Prompt) > 0 {
		f.promptText = req.Prompt[0].Text
	}
	return map[string]string{"stopReason": "end_turn"}, nil
}
