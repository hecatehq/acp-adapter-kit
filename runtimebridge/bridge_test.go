package runtimebridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	"github.com/hecatehq/acp-adapter-kit/runtimebridge"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
)

func TestNewSessionProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/new", map[string]any{
		"cwd": "/tmp/project",
	})

	var result struct {
		SessionID string `json:"sessionId"`
	}
	response.ResultInto(t, &result)
	if result.SessionID != "sess-bridge" {
		t.Fatalf("sessionId = %q, want sess-bridge", result.SessionID)
	}
	var fullResult map[string]any
	response.ResultInto(t, &fullResult)
	if _, ok := fullResult["configOptions"]; !ok {
		t.Fatalf("result = %#v, want configOptions preserved", fullResult)
	}
	if runtime.called("session/new") != 1 {
		t.Fatalf("session/new calls = %d, want 1", runtime.called("session/new"))
	}
}

func TestNewSessionForwardsRawParamsToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/new", map[string]any{
		"cwd":     "/tmp/project",
		"x-extra": map[string]any{"enabled": true},
	})
	var result struct {
		SessionID string `json:"sessionId"`
	}
	response.ResultInto(t, &result)
	if result.SessionID != "sess-bridge" {
		t.Fatalf("sessionId = %q, want sess-bridge", result.SessionID)
	}
	var params map[string]json.RawMessage
	runtime.lastRequestParamsInto(t, "session/new", &params)
	if string(params["x-extra"]) != `{"enabled":true}` {
		t.Fatalf("x-extra = %s, want preserved object", params["x-extra"])
	}
}

func TestNewSessionForwardsRuntimeUpdatesBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.newSessionUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"plan","description":"Create a plan"}]}}`),
	}
	client := newBridgeClient(t, runtime)

	responsesCh := make(chan []acptest.Response, 1)
	go func() {
		responsesCh <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      "session-1",
			"method":  "session/new",
			"params":  map[string]any{"cwd": "/tmp/project"},
		})
	}()

	var envelopes []acptest.Response
	select {
	case envelopes = <-responsesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session/new response; bridge did not drain runtime update")
	}
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want available commands update + session/new response", len(envelopes))
	}
	var commands struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate     string `json:"sessionUpdate"`
			AvailableCommands []struct {
				Name string `json:"name"`
			} `json:"availableCommands"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &commands)
	if commands.SessionID != "sess-bridge" ||
		commands.Update.SessionUpdate != "available_commands_update" ||
		len(commands.Update.AvailableCommands) != 1 ||
		commands.Update.AvailableCommands[0].Name != "plan" {
		t.Fatalf("available commands update = %#v", commands)
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	envelopes[1].ResultInto(t, &result)
	if result.SessionID != "sess-bridge" {
		t.Fatalf("sessionId = %q, want sess-bridge", result.SessionID)
	}
}

func TestPromptForwardsRuntimeUpdatesBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "sess-bridge",
			"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
		},
	})
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want notification + response", len(envelopes))
	}
	if envelopes[0].Method != "session/update" {
		t.Fatalf("first envelope method = %q, want session/update", envelopes[0].Method)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[1].ResultInto(t, &result)
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
	}
}

func TestPromptForwardsRawParamsToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "sess-bridge",
			"prompt": []map[string]any{{
				"type":    "text",
				"text":    "hello",
				"x-block": float64(1),
			}},
			"x-prompt": true,
		},
	})
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want notification + response", len(envelopes))
	}
	var params map[string]json.RawMessage
	runtime.lastRequestParamsInto(t, "session/prompt", &params)
	if string(params["x-prompt"]) != `true` {
		t.Fatalf("x-prompt = %s, want true", params["x-prompt"])
	}
	if !strings.Contains(string(params["prompt"]), `"x-block":1`) {
		t.Fatalf("prompt = %s, want x-block preserved", params["prompt"])
	}
}

func TestPromptPreservesRawRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.promptResult = json.RawMessage(`{"stopReason":"end_turn","x-result":{"kept":true}}`)
	client := newBridgeClient(t, runtime)

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "sess-bridge",
			"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
		},
	})
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want prompt response", len(envelopes))
	}
	var result map[string]json.RawMessage
	envelopes[0].ResultInto(t, &result)
	if string(result["x-result"]) != `{"kept":true}` {
		t.Fatalf("x-result = %s, want preserved object", result["x-result"])
	}
}

func TestPromptForwardsDynamicSessionUpdates(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.promptUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"web","description":"Search the web","input":{"hint":"query"}}]}}`),
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"smart","options":[{"value":"smart","name":"Smart"}]}]}}`),
	}
	client := newBridgeClient(t, runtime)

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "prompt-1",
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "sess-bridge",
			"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
		},
	})
	if len(envelopes) != 3 {
		t.Fatalf("got %d envelopes, want command update + config update + response", len(envelopes))
	}
	var commands struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate     string `json:"sessionUpdate"`
			AvailableCommands []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Input       struct {
					Hint string `json:"hint"`
				} `json:"input"`
			} `json:"availableCommands"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &commands)
	if commands.SessionID != "sess-bridge" ||
		commands.Update.SessionUpdate != "available_commands_update" ||
		len(commands.Update.AvailableCommands) != 1 ||
		commands.Update.AvailableCommands[0].Name != "web" ||
		commands.Update.AvailableCommands[0].Input.Hint != "query" {
		t.Fatalf("available commands update = %#v", commands)
	}

	var config struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			ConfigOptions []struct {
				ID           string `json:"id"`
				CurrentValue string `json:"currentValue"`
			} `json:"configOptions"`
		} `json:"update"`
	}
	envelopes[1].ParamsInto(t, &config)
	if config.SessionID != "sess-bridge" ||
		config.Update.SessionUpdate != "config_option_update" ||
		len(config.Update.ConfigOptions) != 1 ||
		config.Update.ConfigOptions[0].ID != "model" ||
		config.Update.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("config update = %#v", config)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[2].ResultInto(t, &result)
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
	}
}

func TestPromptForwardsRuntimeChildRequestAndReturnsClientResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.childRequest = true
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"needs permission"}]}}`,
		`{"jsonrpc":"2.0","id":"server-1","result":{"outcome":"approved"}}`,
	}, "\n") + "\n")
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want child request + prompt response", len(envelopes))
	}
	if envelopes[0].Method != "session/request_permission" {
		t.Fatalf("child request method = %q, want session/request_permission", envelopes[0].Method)
	}
	if string(envelopes[0].ID) != `"server-1"` {
		t.Fatalf("child request id = %s, want server-1", envelopes[0].ID)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[1].ResultInto(t, &result)
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
	}

	response := runtime.nextResponse(t)
	if string(response.id) != `"runtime-child-1"` {
		t.Fatalf("runtime response id = %s, want runtime-child-1", response.id)
	}
	if !strings.Contains(string(response.result), "approved") {
		t.Fatalf("runtime response result = %s, want approved", response.result)
	}
}

func TestPromptForwardsRuntimeMCPChildRequestAndReturnsClientResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.childRequest = true
	runtime.childRequestMethod = "mcp/message"
	runtime.childRequestParams = json.RawMessage(`{"connectionId":"mcp-conn","method":"tools/list","params":{"cursor":"next"}}`)
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"needs tools"}]}}`,
		`{"jsonrpc":"2.0","id":"server-1","result":{"tools":[{"name":"read"}]}}`,
	}, "\n") + "\n")
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want MCP child request + prompt response", len(envelopes))
	}
	if envelopes[0].Method != "mcp/message" {
		t.Fatalf("child request method = %q, want mcp/message", envelopes[0].Method)
	}
	var params struct {
		ConnectionID string `json:"connectionId"`
		Method       string `json:"method"`
		Params       struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	envelopes[0].ParamsInto(t, &params)
	if params.ConnectionID != "mcp-conn" || params.Method != "tools/list" || params.Params.Cursor != "next" {
		t.Fatalf("MCP child request params = %#v", params)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[1].ResultInto(t, &result)
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
	}

	response := runtime.nextResponse(t)
	if string(response.id) != `"runtime-child-1"` {
		t.Fatalf("runtime response id = %s, want runtime-child-1", response.id)
	}
	if !strings.Contains(string(response.result), "tools") {
		t.Fatalf("runtime response result = %s, want tools result", response.result)
	}
}

func TestPromptForwardsRuntimeTerminalChildRequests(t *testing.T) {
	tests := []struct {
		method       string
		params       json.RawMessage
		clientResult string
		wantResult   string
	}{
		{
			method:       "terminal/create",
			params:       json.RawMessage(`{"sessionId":"sess-bridge","command":"sh","args":["-c","printf ok"],"cwd":"/tmp/project","outputByteLimit":128}`),
			clientResult: `{"terminalId":"term-1"}`,
			wantResult:   `"terminalId":"term-1"`,
		},
		{
			method:       "terminal/output",
			params:       json.RawMessage(`{"sessionId":"sess-bridge","terminalId":"term-1"}`),
			clientResult: `{"output":"ok\n","truncated":false,"exitStatus":{"exitCode":0}}`,
			wantResult:   `"output":"ok\n"`,
		},
		{
			method:       "terminal/wait_for_exit",
			params:       json.RawMessage(`{"sessionId":"sess-bridge","terminalId":"term-1"}`),
			clientResult: `{"exitCode":0}`,
			wantResult:   `"exitCode":0`,
		},
		{
			method:       "terminal/kill",
			params:       json.RawMessage(`{"sessionId":"sess-bridge","terminalId":"term-1"}`),
			clientResult: `{}`,
			wantResult:   `{}`,
		},
		{
			method:       "terminal/release",
			params:       json.RawMessage(`{"sessionId":"sess-bridge","terminalId":"term-1"}`),
			clientResult: `{}`,
			wantResult:   `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			runtime := newFakeRuntimeClient()
			runtime.childRequest = true
			runtime.childRequestMethod = tt.method
			runtime.childRequestParams = tt.params
			client := newBridgeClient(t, runtime)

			envelopes := client.SendRaw(strings.Join([]string{
				`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"needs terminal"}]}}`,
				`{"jsonrpc":"2.0","id":"server-1","result":` + tt.clientResult + `}`,
			}, "\n") + "\n")
			if len(envelopes) != 2 {
				t.Fatalf("got %d envelopes, want terminal child request + prompt response", len(envelopes))
			}
			if envelopes[0].Method != tt.method {
				t.Fatalf("child request method = %q, want %s", envelopes[0].Method, tt.method)
			}
			if string(envelopes[0].Params) != string(tt.params) {
				t.Fatalf("child request params = %s, want %s", envelopes[0].Params, tt.params)
			}
			var result struct {
				StopReason string `json:"stopReason"`
			}
			envelopes[1].ResultInto(t, &result)
			if result.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, want end_turn", result.StopReason)
			}

			response := runtime.nextResponse(t)
			if string(response.id) != `"runtime-child-1"` {
				t.Fatalf("runtime response id = %s, want runtime-child-1", response.id)
			}
			if !strings.Contains(string(response.result), tt.wantResult) {
				t.Fatalf("runtime response result = %s, want %s", response.result, tt.wantResult)
			}
		})
	}
}

func TestPromptCancelWhileAwaitingRuntimeChildRequest(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.childRequest = true
	runtime.childRequestReturnsOnCancel = true

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	server := acp.NewServer(acp.AdapterInfo{Name: "test"}, runtimebridge.New(runtime).Options()...)
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		serveDone <- err
	}()
	reader := bufio.NewReader(outputReader)

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"needs permission"}]}}`); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	childRequest := readBridgeResponseLine(t, reader)
	if childRequest.Method != "session/request_permission" {
		t.Fatalf("child request method = %q, want session/request_permission", childRequest.Method)
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-bridge"}}`); err != nil {
		t.Fatalf("write cancel: %v", err)
	}
	promptResponse := readBridgeResponseLine(t, reader)
	var result struct {
		StopReason string `json:"stopReason"`
	}
	promptResponse.ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", result.StopReason)
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

func TestPromptCanBeCancelledByClientNotification(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.blockPromptUntilCancel = true
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"keep going"}]}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-bridge"}}`,
	}, "\n") + "\n")
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want cancelled prompt response", len(envelopes))
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[0].ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", result.StopReason)
	}
	if runtime.notified("session/cancel") != 1 {
		t.Fatalf("session/cancel notifications = %d, want 1", runtime.notified("session/cancel"))
	}
}

func TestPromptCanBeCancelledByClientRequest(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.blockPromptUntilCancel = true
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"keep going"}]}}`,
		`{"jsonrpc":"2.0","id":"cancel-1","method":"session/cancel","params":{"sessionId":"sess-bridge"}}`,
	}, "\n") + "\n")
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want cancel response + cancelled prompt response", len(envelopes))
	}
	byID := map[string]acptest.Response{}
	for _, envelope := range envelopes {
		byID[string(envelope.ID)] = envelope
	}
	var cancelResult struct {
		Cancelled bool `json:"cancelled"`
	}
	byID[`"cancel-1"`].ResultInto(t, &cancelResult)
	if !cancelResult.Cancelled {
		t.Fatal("cancelled = false, want true")
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	byID[`"prompt-1"`].ResultInto(t, &promptResult)
	if promptResult.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", promptResult.StopReason)
	}
	if runtime.notified("session/cancel") != 1 {
		t.Fatalf("session/cancel notifications = %d, want 1", runtime.notified("session/cancel"))
	}
}

func TestPromptProtocolCancelCancelsRuntimeRequestContext(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.blockPromptUntilContextCancel = true
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"keep going"}]}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"prompt-1"}}`,
	}, "\n") + "\n")
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want cancelled prompt response", len(envelopes))
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	envelopes[0].ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", result.StopReason)
	}
}

func TestLoadSessionForwardsReplayBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	envelopes := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "load-1",
		"method":  "session/load",
		"params": map[string]any{
			"sessionId": "sess-bridge",
			"cwd":       "/tmp/project",
		},
	})
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want replay notification + response", len(envelopes))
	}
	if envelopes[0].Method != "session/update" {
		t.Fatalf("first envelope method = %q, want session/update", envelopes[0].Method)
	}
	if string(envelopes[1].Result) != "null" {
		t.Fatalf("load result = %s, want null", envelopes[1].Result)
	}
}

func TestForkSessionProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/fork", map[string]any{
		"sessionId":             "sess-bridge",
		"cwd":                   "/tmp/project",
		"additionalDirectories": []string{"/tmp/shared"},
		"mcpServers":            []map[string]any{{"name": "filesystem", "command": "/bin/mcp"}},
	})
	var result map[string]any
	response.ResultInto(t, &result)
	if result["sessionId"] != "forked-bridge" || result["source"] != "fork" {
		t.Fatalf("fork result = %#v, want forked-bridge source fork", result)
	}
	if _, ok := result["configOptions"]; !ok {
		t.Fatalf("fork result = %#v, want configOptions preserved", result)
	}
	if runtime.called("session/fork") != 1 {
		t.Fatalf("session/fork calls = %d, want 1", runtime.called("session/fork"))
	}
}

func TestResumeSessionProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/resume", map[string]any{
		"sessionId": "sess-bridge",
		"cwd":       "/tmp/project",
	})
	var result map[string]any
	response.ResultInto(t, &result)
	if result["mode"] != "default" {
		t.Fatalf("resume result = %#v, want mode default", result)
	}
}

func TestResumeSessionForwardsReplayBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.resumeUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"resumed replay"}}}`),
	}
	client := newBridgeClient(t, runtime)

	responsesCh := make(chan []acptest.Response, 1)
	go func() {
		responsesCh <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      "resume-1",
			"method":  "session/resume",
			"params": map[string]any{
				"sessionId": "sess-bridge",
				"cwd":       "/tmp/project",
			},
		})
	}()

	var envelopes []acptest.Response
	select {
	case envelopes = <-responsesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session/resume response; bridge did not drain runtime update")
	}
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want replay notification + resume response", len(envelopes))
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &update)
	if update.SessionID != "sess-bridge" ||
		update.Update.SessionUpdate != "user_message_chunk" ||
		update.Update.Content.Text != "resumed replay" {
		t.Fatalf("resume replay update = %#v", update)
	}
	var result map[string]any
	envelopes[1].ResultInto(t, &result)
	if result["mode"] != "default" {
		t.Fatalf("resume result = %#v, want mode default", result)
	}
}

func TestListSessionsProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/list", map[string]any{"cwd": "/tmp/project"})
	var result struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			CWD       string `json:"cwd"`
			Title     string `json:"title"`
		} `json:"sessions"`
		NextCursor string `json:"nextCursor"`
	}
	response.ResultInto(t, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].SessionID != "sess-bridge" {
		t.Fatalf("list result = %#v, want sess-bridge", result)
	}
	if result.NextCursor != "next" {
		t.Fatalf("NextCursor = %q, want next", result.NextCursor)
	}
}

func TestListSessionsPreservesRawRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.listSessionsResult = json.RawMessage(`{"sessions":[],"nextCursor":"next","x-result":true}`)
	client := newBridgeClient(t, runtime)

	response := client.Request("session/list", map[string]any{"cwd": "/tmp/project"})
	var result map[string]json.RawMessage
	response.ResultInto(t, &result)
	if string(result["x-result"]) != `true` {
		t.Fatalf("x-result = %s, want true", result["x-result"])
	}
}

func TestMCPMessageProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("mcp/message", map[string]any{
		"connectionId": "mcp-conn",
		"method":       "tools/list",
		"params":       map[string]any{"cursor": "next"},
	})
	var result struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			OK bool `json:"ok"`
		} `json:"result"`
	}
	response.ResultInto(t, &result)
	if result.JSONRPC != "2.0" || result.ID != 1 || !result.Result.OK {
		t.Fatalf("mcp/message result = %#v, want raw MCP response", result)
	}
	if runtime.called("mcp/message") != 1 {
		t.Fatalf("mcp/message calls = %d, want 1", runtime.called("mcp/message"))
	}
}

func TestMCPMessageNotificationProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	client.Notify("mcp/message", map[string]any{
		"connectionId": "mcp-conn",
		"method":       "notifications/initialized",
		"params":       map[string]any{"ok": true},
	})

	if runtime.notified("mcp/message") != 1 {
		t.Fatalf("mcp/message notifications = %d, want 1", runtime.notified("mcp/message"))
	}
	var params struct {
		ConnectionID string `json:"connectionId"`
		Method       string `json:"method"`
		Params       struct {
			OK bool `json:"ok"`
		} `json:"params"`
	}
	runtime.lastNotificationParamsInto(t, "mcp/message", &params)
	if params.ConnectionID != "mcp-conn" || params.Method != "notifications/initialized" || !params.Params.OK {
		t.Fatalf("mcp/message notification params = %#v", params)
	}
}

func TestCancelNotificationProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	client.Notify("session/cancel", map[string]string{"sessionId": "sess-bridge"})

	if runtime.notified("session/cancel") != 1 {
		t.Fatalf("session/cancel notifications = %d, want 1", runtime.notified("session/cancel"))
	}
}

func TestCloseSessionProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/close", map[string]string{"sessionId": "sess-bridge"})
	if response.Error != nil {
		t.Fatalf("close error = %+v", response.Error)
	}
	if runtime.called("session/close") != 1 {
		t.Fatalf("session/close calls = %d, want 1", runtime.called("session/close"))
	}
}

func TestCloseSessionRequestCancelsActiveRuntimePrompt(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.blockPromptUntilCancel = true
	runtime.closeCancelsPrompt = true
	client := newBridgeClient(t, runtime)

	envelopes := client.SendRaw(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"prompt-1","method":"session/prompt","params":{"sessionId":"sess-bridge","prompt":[{"type":"text","text":"keep going"}]}}`,
		`{"jsonrpc":"2.0","id":"close-1","method":"session/close","params":{"sessionId":"sess-bridge"}}`,
	}, "\n") + "\n")
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want close response + cancelled prompt response", len(envelopes))
	}
	byID := map[string]acptest.Response{}
	for _, envelope := range envelopes {
		byID[string(envelope.ID)] = envelope
	}
	var closeResult map[string]any
	byID[`"close-1"`].ResultInto(t, &closeResult)

	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	byID[`"prompt-1"`].ResultInto(t, &promptResult)
	if promptResult.StopReason != "cancelled" {
		t.Fatalf("stopReason = %q, want cancelled", promptResult.StopReason)
	}
	if runtime.called("session/close") != 1 {
		t.Fatalf("session/close calls = %d, want 1", runtime.called("session/close"))
	}
}

func TestCloseSessionForwardsRuntimeUpdatesBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.closeUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"closing"}}}`),
	}
	client := newBridgeClient(t, runtime)

	responsesCh := make(chan []acptest.Response, 1)
	go func() {
		responsesCh <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      "close-1",
			"method":  "session/close",
			"params":  map[string]any{"sessionId": "sess-bridge"},
		})
	}()

	var envelopes []acptest.Response
	select {
	case envelopes = <-responsesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session/close response; bridge did not drain runtime update")
	}
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want close update + response", len(envelopes))
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &update)
	if update.SessionID != "sess-bridge" ||
		update.Update.SessionUpdate != "agent_message_chunk" ||
		update.Update.Content.Text != "closing" {
		t.Fatalf("close update = %#v", update)
	}
	var result map[string]any
	envelopes[1].ResultInto(t, &result)
}

func TestDeleteSessionProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/delete", map[string]string{"sessionId": "sess-bridge"})
	if response.Error != nil {
		t.Fatalf("delete error = %+v", response.Error)
	}
	if runtime.called("session/delete") != 1 {
		t.Fatalf("session/delete calls = %d, want 1", runtime.called("session/delete"))
	}
}

func TestSetConfigOptionProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/set_config_option", map[string]string{
		"sessionId": "sess-bridge",
		"configId":  "model",
		"value":     "smart",
	})
	var result map[string]any
	response.ResultInto(t, &result)
	if _, ok := result["configOptions"]; !ok {
		t.Fatalf("set_config_option result = %#v, want configOptions", result)
	}
}

func TestSetConfigOptionForwardsRuntimeUpdatesBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.configOptionUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","currentValue":"smart"}]}}`),
	}
	client := newBridgeClient(t, runtime)

	responsesCh := make(chan []acptest.Response, 1)
	go func() {
		responsesCh <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      "config-1",
			"method":  "session/set_config_option",
			"params": map[string]any{
				"sessionId": "sess-bridge",
				"configId":  "model",
				"value":     "smart",
			},
		})
	}()

	var envelopes []acptest.Response
	select {
	case envelopes = <-responsesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session/set_config_option response; bridge did not drain runtime update")
	}
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want config update + response", len(envelopes))
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			ConfigOptions []struct {
				ID           string `json:"id"`
				CurrentValue string `json:"currentValue"`
			} `json:"configOptions"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &update)
	if update.SessionID != "sess-bridge" ||
		update.Update.SessionUpdate != "config_option_update" ||
		len(update.Update.ConfigOptions) != 1 ||
		update.Update.ConfigOptions[0].ID != "model" ||
		update.Update.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("config option update = %#v", update)
	}
	var result map[string]any
	envelopes[1].ResultInto(t, &result)
	if _, ok := result["configOptions"]; !ok {
		t.Fatalf("set_config_option result = %#v, want configOptions", result)
	}
}

func TestSetModeProxiesRuntimeResult(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/set_mode", map[string]string{
		"sessionId": "sess-bridge",
		"modeId":    "code",
	})
	var result map[string]any
	response.ResultInto(t, &result)
	if _, ok := result["modes"]; !ok {
		t.Fatalf("set_mode result = %#v, want modes", result)
	}
}

func TestSetModeForwardsRuntimeUpdatesBeforeResponse(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.events = make(chan runtimejsonrpc.Event)
	runtime.modeUpdates = []json.RawMessage{
		json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"current_mode_update","currentModeId":"code"}}`),
	}
	client := newBridgeClient(t, runtime)

	responsesCh := make(chan []acptest.Response, 1)
	go func() {
		responsesCh <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      "mode-1",
			"method":  "session/set_mode",
			"params": map[string]any{
				"sessionId": "sess-bridge",
				"modeId":    "code",
			},
		})
	}()

	var envelopes []acptest.Response
	select {
	case envelopes = <-responsesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session/set_mode response; bridge did not drain runtime update")
	}
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want mode update + response", len(envelopes))
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			CurrentModeID string `json:"currentModeId"`
		} `json:"update"`
	}
	envelopes[0].ParamsInto(t, &update)
	if update.SessionID != "sess-bridge" ||
		update.Update.SessionUpdate != "current_mode_update" ||
		update.Update.CurrentModeID != "code" {
		t.Fatalf("mode update = %#v", update)
	}
	var result map[string]any
	envelopes[1].ResultInto(t, &result)
	if _, ok := result["modes"]; !ok {
		t.Fatalf("set_mode result = %#v, want modes", result)
	}
}

func TestAuthenticateProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("authenticate", map[string]string{"methodId": "agent-login"})
	if response.Error != nil {
		t.Fatalf("authenticate error = %+v", response.Error)
	}
	if runtime.called("authenticate") != 1 {
		t.Fatalf("authenticate calls = %d, want 1", runtime.called("authenticate"))
	}
}

func TestLogoutProxiesToRuntime(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("logout", map[string]any{})
	if response.Error != nil {
		t.Fatalf("logout error = %+v", response.Error)
	}
	if runtime.called("logout") != 1 {
		t.Fatalf("logout calls = %d, want 1", runtime.called("logout"))
	}
}

func TestRuntimeRPCErrorMapsToACPError(t *testing.T) {
	runtime := newFakeRuntimeClient()
	runtime.err = &runtimejsonrpc.RPCError{Code: -32010, Message: "runtime exploded"}
	client := newBridgeClient(t, runtime)

	response := client.Request("session/new", map[string]any{"cwd": "/tmp/project"})
	if response.Error == nil {
		t.Fatal("response error is nil, want runtime error")
	}
	if response.Error.Code != -32010 || response.Error.Message != "runtime exploded" {
		t.Fatalf("response error = %+v, want runtime exploded", response.Error)
	}
}

func TestInvalidParamsReturnInvalidParamsError(t *testing.T) {
	runtime := newFakeRuntimeClient()
	client := newBridgeClient(t, runtime)

	response := client.Request("session/new", "not-an-object")
	if response.Error == nil {
		t.Fatal("response error is nil, want invalid params")
	}
	if response.Error.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", response.Error.Code)
	}
	if runtime.called("session/new") != 0 {
		t.Fatalf("session/new calls = %d, want 0", runtime.called("session/new"))
	}
}

func newBridgeClient(t testing.TB, runtime *fakeRuntimeClient) *acptest.Client {
	t.Helper()
	server := acp.NewServer(acp.AdapterInfo{Name: "test"}, runtimebridge.New(runtime).Options()...)
	return acptest.NewClient(t, server)
}

func readBridgeResponseLine(t testing.TB, reader *bufio.Reader) acptest.Response {
	t.Helper()
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		var response acptest.Response
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		return response
	case err := <-errCh:
		t.Fatalf("read response: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
	return acptest.Response{}
}

type fakeRuntimeClient struct {
	mu                            sync.Mutex
	calls                         map[string]int
	requestParams                 map[string]json.RawMessage
	notifications                 map[string]int
	notificationParams            map[string]json.RawMessage
	events                        chan runtimejsonrpc.Event
	err                           error
	childRequest                  bool
	childRequestMethod            string
	childRequestParams            json.RawMessage
	childRequestReturnsOnCancel   bool
	blockPromptUntilCancel        bool
	blockPromptUntilContextCancel bool
	closeCancelsPrompt            bool
	promptResult                  json.RawMessage
	listSessionsResult            json.RawMessage
	cancelled                     chan struct{}
	newSessionUpdates             []json.RawMessage
	promptUpdates                 []json.RawMessage
	resumeUpdates                 []json.RawMessage
	closeUpdates                  []json.RawMessage
	configOptionUpdates           []json.RawMessage
	modeUpdates                   []json.RawMessage
	responses                     chan runtimeChildResponse
	lastResponse                  *runtimeChildResponse
}

func newFakeRuntimeClient() *fakeRuntimeClient {
	return &fakeRuntimeClient{
		calls:              map[string]int{},
		requestParams:      map[string]json.RawMessage{},
		notifications:      map[string]int{},
		notificationParams: map[string]json.RawMessage{},
		events:             make(chan runtimejsonrpc.Event, 8),
		cancelled:          make(chan struct{}),
		responses:          make(chan runtimeChildResponse, 8),
	}
}

func (f *fakeRuntimeClient) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls[method]++
	f.requestParams[method] = append(json.RawMessage(nil), raw...)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	switch method {
	case "session/new":
		for _, params := range f.newSessionUpdates {
			f.events <- runtimejsonrpc.Event{
				Method: "session/update",
				Params: append(json.RawMessage(nil), params...),
			}
		}
		return json.RawMessage(`{"sessionId":"sess-bridge","configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"fast","options":[{"value":"fast","name":"Fast"}]}],"modes":{"currentModeId":"ask"}}`), nil
	case "session/prompt":
		if len(f.promptResult) != 0 {
			return append(json.RawMessage(nil), f.promptResult...), nil
		}
		if f.blockPromptUntilContextCancel {
			select {
			case <-ctx.Done():
				return json.RawMessage(`{"stopReason":"cancelled"}`), nil
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for request context cancellation")
			}
		}
		if f.blockPromptUntilCancel {
			select {
			case <-f.cancelled:
				return json.RawMessage(`{"stopReason":"cancelled"}`), nil
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for cancel notification")
			}
		}
		if f.childRequest {
			method := f.childRequestMethod
			if method == "" {
				method = "session/request_permission"
			}
			params := f.childRequestParams
			if len(params) == 0 {
				params = json.RawMessage(`{"sessionId":"sess-bridge","toolCallId":"tool-1"}`)
			}
			f.events <- runtimejsonrpc.Event{
				ID:     json.RawMessage(`"runtime-child-1"`),
				Method: method,
				Params: append(json.RawMessage(nil), params...),
			}
			if f.childRequestReturnsOnCancel {
				select {
				case <-f.responses:
				case <-f.cancelled:
					return json.RawMessage(`{"stopReason":"cancelled"}`), nil
				case <-time.After(2 * time.Second):
					return nil, errors.New("timed out waiting for cancel or permission response")
				}
			} else {
				<-f.responses
			}
			return json.RawMessage(`{"stopReason":"end_turn"}`), nil
		}
		if len(f.promptUpdates) != 0 {
			for _, params := range f.promptUpdates {
				f.events <- runtimejsonrpc.Event{
					Method: "session/update",
					Params: append(json.RawMessage(nil), params...),
				}
			}
			return json.RawMessage(`{"stopReason":"end_turn"}`), nil
		}
		f.events <- runtimejsonrpc.Event{
			Method: "session/update",
			Params: json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}`),
		}
		return json.RawMessage(`{"stopReason":"end_turn"}`), nil
	case "session/load":
		f.events <- runtimejsonrpc.Event{
			Method: "session/update",
			Params: json.RawMessage(`{"sessionId":"sess-bridge","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"replay"}}}`),
		}
		return json.RawMessage(`null`), nil
	case "session/fork":
		return json.RawMessage(`{"sessionId":"forked-bridge","configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"smart","options":[{"value":"smart","name":"Smart"}]}],"modes":{"currentModeId":"code"},"source":"fork"}`), nil
	case "session/resume":
		for _, params := range f.resumeUpdates {
			f.events <- runtimejsonrpc.Event{
				Method: "session/update",
				Params: append(json.RawMessage(nil), params...),
			}
		}
		return json.RawMessage(`{"mode":"default"}`), nil
	case "session/list":
		if len(f.listSessionsResult) != 0 {
			return append(json.RawMessage(nil), f.listSessionsResult...), nil
		}
		return json.RawMessage(`{"sessions":[{"sessionId":"sess-bridge","cwd":"/tmp/project","title":"Bridge session"}],"nextCursor":"next"}`), nil
	case "session/close":
		if f.closeCancelsPrompt {
			f.mu.Lock()
			select {
			case <-f.cancelled:
			default:
				close(f.cancelled)
			}
			f.mu.Unlock()
		}
		for _, params := range f.closeUpdates {
			f.events <- runtimejsonrpc.Event{
				Method: "session/update",
				Params: append(json.RawMessage(nil), params...),
			}
		}
		return json.RawMessage(`{}`), nil
	case "session/delete":
		return json.RawMessage(`{}`), nil
	case "session/set_config_option":
		for _, params := range f.configOptionUpdates {
			f.events <- runtimejsonrpc.Event{
				Method: "session/update",
				Params: append(json.RawMessage(nil), params...),
			}
		}
		return json.RawMessage(`{"configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"smart","options":[{"value":"smart","name":"Smart"}]}]}`), nil
	case "session/set_mode":
		for _, params := range f.modeUpdates {
			f.events <- runtimejsonrpc.Event{
				Method: "session/update",
				Params: append(json.RawMessage(nil), params...),
			}
		}
		return json.RawMessage(`{"modes":{"currentModeId":"code","availableModes":[{"id":"code","name":"Code"}]}}`), nil
	case "mcp/message":
		return json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), nil
	case "authenticate":
		return json.RawMessage(`{}`), nil
	case "logout":
		return json.RawMessage(`{}`), nil
	default:
		return nil, &runtimejsonrpc.RPCError{Code: -32601, Message: "method not found"}
	}
}

func (f *fakeRuntimeClient) Notify(_ctx context.Context, method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.notifications[method]++
	if raw, err := json.Marshal(params); err == nil {
		f.notificationParams[method] = raw
	}
	if method == "session/cancel" {
		select {
		case <-f.cancelled:
		default:
			close(f.cancelled)
		}
	}
	return nil
}

func (f *fakeRuntimeClient) Respond(_ctx context.Context, id json.RawMessage, result any, rpcErr *runtimejsonrpc.RPCError) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	response := runtimeChildResponse{
		id:     append(json.RawMessage(nil), id...),
		result: raw,
		err:    rpcErr,
	}
	f.mu.Lock()
	f.lastResponse = &response
	f.mu.Unlock()
	f.responses <- response
	return nil
}

func (f *fakeRuntimeClient) Events() <-chan runtimejsonrpc.Event {
	return f.events
}

func (f *fakeRuntimeClient) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeRuntimeClient) notified(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifications[method]
}

func (f *fakeRuntimeClient) lastNotificationParamsInto(t testing.TB, method string, target any) {
	t.Helper()
	f.mu.Lock()
	raw := append(json.RawMessage(nil), f.notificationParams[method]...)
	f.mu.Unlock()
	if len(raw) == 0 {
		t.Fatalf("notification params for %s were not recorded", method)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s notification params: %v\n%s", method, err, string(raw))
	}
}

func (f *fakeRuntimeClient) lastRequestParamsInto(t testing.TB, method string, target any) {
	t.Helper()
	f.mu.Lock()
	raw := append(json.RawMessage(nil), f.requestParams[method]...)
	f.mu.Unlock()
	if len(raw) == 0 {
		t.Fatalf("request params for %s were not recorded", method)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s request params: %v\n%s", method, err, string(raw))
	}
}

func (f *fakeRuntimeClient) nextResponse(t testing.TB) runtimeChildResponse {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastResponse == nil {
		t.Fatal("runtime response was not recorded")
	}
	return *f.lastResponse
}

type runtimeChildResponse struct {
	id     json.RawMessage
	result json.RawMessage
	err    *runtimejsonrpc.RPCError
}

var _ runtimebridge.RuntimeClient = (*fakeRuntimeClient)(nil)
