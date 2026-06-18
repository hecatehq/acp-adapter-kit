package fakeruntime_test

import (
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	"github.com/hecatehq/acp-adapter-kit/fakeruntime"
)

func TestSessionLifecycleEmitsPromptNotificationsBeforeResponse(t *testing.T) {
	client := newClient(t)
	sessionID := createSession(t, client, "/work")

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
		},
	})

	if len(envelopes) != 4 {
		t.Fatalf("got %d envelopes, want 4", len(envelopes))
	}
	assertUpdate(t, envelopes[0], "agent_message_chunk", sessionID)
	assertUpdate(t, envelopes[1], "tool_call", sessionID)
	assertUpdate(t, envelopes[2], "tool_call_update", sessionID)

	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[3].ResultInto(t, &result)
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
	}
}

func TestCancelNotificationSettlesNextPromptAsCancelled(t *testing.T) {
	client := newClient(t)
	sessionID := createSession(t, client, "/work")

	client.Notify("session/cancel", map[string]string{"sessionId": sessionID})
	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]string{{"type": "text", "text": "still there?"}},
		},
	})

	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envelopes))
	}
	assertUpdate(t, envelopes[0], "agent_message_chunk", sessionID)
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[1].ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", result.StopReason)
	}
}

func TestCancelRequestEmitsUpdateBeforeResponse(t *testing.T) {
	client := newClient(t)
	sessionID := createSession(t, client, "/work")

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "cancel-1",
		"method":  "session/cancel",
		"params":  map[string]string{"sessionId": sessionID},
	})

	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envelopes))
	}
	assertUpdate(t, envelopes[0], "agent_message_chunk", sessionID)
	var result struct {
		Cancelled bool `json:"cancelled"`
	}
	envelopes[1].ResultInto(t, &result)
	if !result.Cancelled {
		t.Fatal("cancel response did not report cancelled=true")
	}
}

func TestCloseRemovesSession(t *testing.T) {
	client := newClient(t)
	sessionID := createSession(t, client, "/work")

	closeResponse := client.Request("session/close", map[string]string{"sessionId": sessionID})
	var closeResult struct {
		Closed bool `json:"closed"`
	}
	closeResponse.ResultInto(t, &closeResult)
	if !closeResult.Closed {
		t.Fatal("close response did not report closed=true")
	}

	response := client.Request("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
	})
	if response.Error == nil || response.Error.Code != -32001 {
		t.Fatalf("prompt after close error = %+v, want session not found", response.Error)
	}
}

func newClient(t testing.TB) *acptest.Client {
	t.Helper()
	runtime := fakeruntime.New()
	server := acp.NewServer(acp.AdapterInfo{Name: "test"}, runtime.Options()...)
	return acptest.NewClient(t, server)
}

func createSession(t testing.TB, client *acptest.Client, cwd string) string {
	t.Helper()
	response := client.Request("session/new", map[string]string{"cwd": cwd})
	var result struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	response.ResultInto(t, &result)
	if result.SessionID == "" {
		t.Fatal("sessionId is empty")
	}
	if result.CWD != cwd {
		t.Fatalf("cwd = %q, want %q", result.CWD, cwd)
	}
	return result.SessionID
}

func assertUpdate(t testing.TB, envelope acptest.Response, updateType string, sessionID string) {
	t.Helper()
	if envelope.Method != "session/update" {
		t.Fatalf("method = %q, want session/update", envelope.Method)
	}
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	envelope.ParamsInto(t, &params)
	if params.SessionID != sessionID {
		t.Fatalf("sessionId = %q, want %q", params.SessionID, sessionID)
	}
	if params.Update.SessionUpdate != updateType {
		t.Fatalf("sessionUpdate = %q, want %q", params.Update.SessionUpdate, updateType)
	}
}
