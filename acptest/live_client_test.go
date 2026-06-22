package acptest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
)

func TestLiveClientCollectsNotificationsAndFinalResponse(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"},
		acp.WithMethod("test/stream", func(ctx *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
			if err := ctx.Notify("test/update", map[string]string{"value": "chunk"}); err != nil {
				return nil, &acp.RPCError{Code: -32000, Message: err.Error()}
			}
			return map[string]string{"ok": "true"}, nil
		}),
	)
	client := acptest.NewLiveClient(t, server)

	responses := client.Request("req-1", "test/stream", nil, time.Second)
	if len(responses) != 2 {
		t.Fatalf("responses = %#v, want notification + final response", responses)
	}
	if responses[0].Method != "test/update" {
		t.Fatalf("first response method = %q, want test/update", responses[0].Method)
	}
	if !acptest.ResponseIDEquals(responses[1].ID, "req-1") {
		t.Fatalf("final response id = %s, want req-1", responses[1].ID)
	}
}

func TestLiveClientAutoAllowsPermissionRequests(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"},
		acp.WithMethod("test/permission", func(ctx *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
			result, rpcErr, err := ctx.Request("session/request_permission", map[string]any{
				"options": []map[string]string{
					{"optionId": "deny", "kind": "deny"},
					{"optionId": "allow_once", "kind": "allow"},
				},
			})
			if err != nil {
				return nil, &acp.RPCError{Code: -32000, Message: err.Error()}
			}
			if rpcErr != nil {
				return nil, rpcErr
			}
			return json.RawMessage(result), nil
		}),
	)
	client := acptest.NewLiveClient(t, server, acptest.WithAutoAllowPermissions())

	responses := client.Request("req-2", "test/permission", nil, time.Second)
	if len(responses) != 2 {
		t.Fatalf("responses = %#v, want permission request + final response", responses)
	}
	var final struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	responses[1].ResultInto(t, &final)
	if final.Outcome.Outcome != "selected" || final.Outcome.OptionID != "allow_once" {
		t.Fatalf("permission result = %#v, want selected allow_once", final.Outcome)
	}
}

func TestLiveClientAutoRejectsPermissionRequests(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"},
		acp.WithMethod("test/permission", func(ctx *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
			result, rpcErr, err := ctx.Request("session/request_permission", map[string]any{
				"options": []map[string]string{
					{"optionId": "allow_once", "kind": "allow"},
					{"optionId": "reject_once", "kind": "reject_once"},
				},
			})
			if err != nil {
				return nil, &acp.RPCError{Code: -32000, Message: err.Error()}
			}
			if rpcErr != nil {
				return nil, rpcErr
			}
			return json.RawMessage(result), nil
		}),
	)
	client := acptest.NewLiveClient(t, server, acptest.WithAutoRejectPermissions())

	responses := client.Request("req-reject", "test/permission", nil, time.Second)
	if len(responses) != 2 {
		t.Fatalf("responses = %#v, want permission request + final response", responses)
	}
	var final struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	responses[1].ResultInto(t, &final)
	if final.Outcome.Outcome != "selected" || final.Outcome.OptionID != "reject_once" {
		t.Fatalf("permission result = %#v, want selected reject_once", final.Outcome)
	}
}

func TestLiveClientAutoCancelsPermissionRequests(t *testing.T) {
	server := acp.NewServer(acp.AdapterInfo{Name: "test"},
		acp.WithMethod("test/permission", func(ctx *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
			result, rpcErr, err := ctx.Request("session/request_permission", map[string]any{
				"options": []map[string]string{
					{"optionId": "allow_once", "kind": "allow"},
					{"optionId": "reject_once", "kind": "reject_once"},
				},
			})
			if err != nil {
				return nil, &acp.RPCError{Code: -32000, Message: err.Error()}
			}
			if rpcErr != nil {
				return nil, rpcErr
			}
			return json.RawMessage(result), nil
		}),
	)
	client := acptest.NewLiveClient(t, server, acptest.WithAutoCancelPermissions())

	responses := client.Request("req-cancel", "test/permission", nil, time.Second)
	if len(responses) != 2 {
		t.Fatalf("responses = %#v, want permission request + final response", responses)
	}
	var final struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	responses[1].ResultInto(t, &final)
	if final.Outcome.Outcome != "cancelled" || final.Outcome.OptionID != "" {
		t.Fatalf("permission result = %#v, want cancelled without option", final.Outcome)
	}
}

func TestLiveClientPromptTextAndCancel(t *testing.T) {
	cancelled := make(chan struct{})
	server := acp.NewServer(acp.AdapterInfo{Name: "test"},
		acp.WithConcurrentMethod("session/prompt", func(_ *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
			select {
			case <-cancelled:
				return map[string]string{"stopReason": "cancelled"}, nil
			case <-time.After(time.Second):
				return nil, &acp.RPCError{Code: -32000, Message: "cancel was not dispatched"}
			}
		}),
		acp.WithNotification("session/cancel", func(_ json.RawMessage) error {
			select {
			case <-cancelled:
			default:
				close(cancelled)
			}
			return nil
		}),
	)
	client := acptest.NewLiveClient(t, server)

	responses := client.PromptTextAndCancel("prompt-1", "session-1", "stop soon", 10*time.Millisecond, time.Second)
	if len(responses) != 1 {
		t.Fatalf("responses = %#v, want final prompt response", responses)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	responses[0].ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
}
