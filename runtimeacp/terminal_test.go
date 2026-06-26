package runtimeacp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestTerminalCreateSendsCallbackRequest(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"terminalId":"term-1","x-extra":true}`)}

	result, err := runtimeacp.TerminalCreate(context.Background(), client, runtimeacp.TerminalCreateParams{
		SessionID:       "sess-test",
		Command:         "sh",
		Args:            []string{"-c", "printf ok"},
		CWD:             "/tmp/project",
		OutputByteLimit: 128,
	})
	if err != nil {
		t.Fatalf("TerminalCreate returned error: %v", err)
	}
	if client.method != "terminal/create" {
		t.Fatalf("method = %q, want terminal/create", client.method)
	}
	var params runtimeacp.TerminalCreateParams
	mustJSONRoundTrip(t, client.params, &params)
	if params.SessionID != "sess-test" ||
		params.Command != "sh" ||
		len(params.Args) != 2 ||
		params.Args[1] != "printf ok" ||
		params.CWD != "/tmp/project" ||
		params.OutputByteLimit != 128 {
		t.Fatalf("params = %#v, want terminal create payload", params)
	}
	if result.TerminalID != "term-1" {
		t.Fatalf("TerminalID = %q, want term-1", result.TerminalID)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"x-extra":true`) {
		t.Fatalf("marshaled result = %s, want raw result fields preserved", raw)
	}
}

func TestTerminalOutputPreservesExitStatusAndRawResult(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"output":"ok\n","truncated":true,"exitStatus":{"exitCode":7},"x-extra":"kept"}`)}

	result, err := runtimeacp.TerminalOutput(context.Background(), client, runtimeacp.TerminalOutputParams{
		SessionID:  "sess-test",
		TerminalID: "term-1",
	})
	if err != nil {
		t.Fatalf("TerminalOutput returned error: %v", err)
	}
	if client.method != "terminal/output" {
		t.Fatalf("method = %q, want terminal/output", client.method)
	}
	var params runtimeacp.TerminalOutputParams
	mustJSONRoundTrip(t, client.params, &params)
	if params.SessionID != "sess-test" || params.TerminalID != "term-1" {
		t.Fatalf("params = %#v, want terminal output payload", params)
	}
	if result.Output != "ok\n" || !result.Truncated {
		t.Fatalf("result = %#v, want output and truncated flag", result)
	}
	if result.ExitStatus == nil || result.ExitStatus.ExitCode == nil || *result.ExitStatus.ExitCode != 7 {
		t.Fatalf("exit status = %#v, want exit code 7", result.ExitStatus)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"x-extra":"kept"`) {
		t.Fatalf("marshaled result = %s, want raw result fields preserved", raw)
	}
}

func TestTerminalWaitForExitReturnsRawResult(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"exitCode":0,"signal":"none"}`)}

	result, err := runtimeacp.TerminalWaitForExit(context.Background(), client, runtimeacp.TerminalWaitForExitParams{
		SessionID:  "sess-test",
		TerminalID: "term-1",
	})
	if err != nil {
		t.Fatalf("TerminalWaitForExit returned error: %v", err)
	}
	if client.method != "terminal/wait_for_exit" {
		t.Fatalf("method = %q, want terminal/wait_for_exit", client.method)
	}
	var params runtimeacp.TerminalWaitForExitParams
	mustJSONRoundTrip(t, client.params, &params)
	if params.SessionID != "sess-test" || params.TerminalID != "term-1" {
		t.Fatalf("params = %#v, want terminal wait payload", params)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("ExitCode = %#v, want 0", result.ExitCode)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"signal":"none"`) {
		t.Fatalf("marshaled result = %s, want raw result fields preserved", raw)
	}
}

func TestTerminalKillAndReleaseSendCallbackRequests(t *testing.T) {
	tests := []struct {
		name   string
		call   func(context.Context, runtimeacp.JSONRPCClient) error
		method string
	}{
		{
			name: "kill",
			call: func(ctx context.Context, client runtimeacp.JSONRPCClient) error {
				return runtimeacp.TerminalKill(ctx, client, runtimeacp.TerminalKillParams{SessionID: "sess-test", TerminalID: "term-1"})
			},
			method: "terminal/kill",
		},
		{
			name: "release",
			call: func(ctx context.Context, client runtimeacp.JSONRPCClient) error {
				return runtimeacp.TerminalRelease(ctx, client, runtimeacp.TerminalReleaseParams{SessionID: "sess-test", TerminalID: "term-1"})
			},
			method: "terminal/release",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingACPClient{result: json.RawMessage(`{}`)}

			if err := tt.call(context.Background(), client); err != nil {
				t.Fatalf("%s returned error: %v", tt.name, err)
			}
			if client.method != tt.method {
				t.Fatalf("method = %q, want %s", client.method, tt.method)
			}
			var params struct {
				SessionID  string `json:"sessionId"`
				TerminalID string `json:"terminalId"`
			}
			mustJSONRoundTrip(t, client.params, &params)
			if params.SessionID != "sess-test" || params.TerminalID != "term-1" {
				t.Fatalf("params = %#v, want terminal id", params)
			}
		})
	}
}

func TestTerminalCreateRequiresTerminalID(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{}`)}

	_, err := runtimeacp.TerminalCreate(context.Background(), client, runtimeacp.TerminalCreateParams{Command: "sh"})
	if err == nil || !strings.Contains(err.Error(), "missing terminalId") {
		t.Fatalf("TerminalCreate error = %v, want missing terminalId", err)
	}
}

func TestTerminalHelpersRequireClient(t *testing.T) {
	if _, err := runtimeacp.TerminalCreate(context.Background(), nil, runtimeacp.TerminalCreateParams{}); !isRuntimeACPClientRequired(err) {
		t.Fatalf("TerminalCreate error = %v, want client required", err)
	}
	if _, err := runtimeacp.TerminalOutput(context.Background(), nil, runtimeacp.TerminalOutputParams{}); !isRuntimeACPClientRequired(err) {
		t.Fatalf("TerminalOutput error = %v, want client required", err)
	}
	if _, err := runtimeacp.TerminalWaitForExit(context.Background(), nil, runtimeacp.TerminalWaitForExitParams{}); !isRuntimeACPClientRequired(err) {
		t.Fatalf("TerminalWaitForExit error = %v, want client required", err)
	}
	if err := runtimeacp.TerminalKill(context.Background(), nil, runtimeacp.TerminalKillParams{}); !isRuntimeACPClientRequired(err) {
		t.Fatalf("TerminalKill error = %v, want client required", err)
	}
	if err := runtimeacp.TerminalRelease(context.Background(), nil, runtimeacp.TerminalReleaseParams{}); !isRuntimeACPClientRequired(err) {
		t.Fatalf("TerminalRelease error = %v, want client required", err)
	}
}

func isRuntimeACPClientRequired(err error) bool {
	return err != nil && strings.Contains(err.Error(), "runtime ACP client is required")
}
