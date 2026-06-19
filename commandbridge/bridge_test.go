package commandbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	"github.com/hecatehq/acp-adapter-kit/commandbridge"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestBridgeRunsPromptCommandAndStreamsOutput(t *testing.T) {
	var saw commandbridge.Session
	var sawPrompt string
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			DefaultValue: "default",
			Options: []commandbridge.SelectValue{
				{Value: "default", Name: "Default"},
				{Value: "smart", Name: "Smart"},
			},
		}},
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			saw = session
			sawPrompt = text
			return adapterprocess.Spec{Command: "agent", Args: []string{"--model", session.Config["model"], text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			if spec.Command != "agent" || strings.Join(spec.Args, " ") != "--model smart hello\n\nfrom resource" || spec.Dir != "/tmp/work" {
				t.Fatalf("process spec = %#v, want model + prompt + cwd", spec)
			}
			return adapterprocess.Result{Stdout: []byte("assistant answer\n")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	created := client.Request("session/new", map[string]any{"cwd": "/tmp/work"})
	var session struct {
		SessionID     string `json:"sessionId"`
		ConfigOptions []struct {
			Type         string `json:"type"`
			ID           string `json:"id"`
			Category     string `json:"category"`
			CurrentValue string `json:"currentValue"`
			Options      []struct {
				Value string `json:"value"`
				Name  string `json:"name"`
			} `json:"options"`
		} `json:"configOptions"`
	}
	created.ResultInto(t, &session)
	if session.SessionID != "session-1" {
		t.Fatalf("session id = %q, want session-1", session.SessionID)
	}
	if len(session.ConfigOptions) != 1 || session.ConfigOptions[0].Type != "select" || session.ConfigOptions[0].ID != "model" || session.ConfigOptions[0].Category != "model" || session.ConfigOptions[0].CurrentValue != "default" {
		t.Fatalf("config options = %#v, want default model selector", session.ConfigOptions)
	}

	updated := client.Request("session/set_config_option", map[string]any{
		"sessionId": "session-1",
		"configId":  "model",
		"value":     "smart",
	})
	var setResult struct {
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	updated.ResultInto(t, &setResult)
	if len(setResult.ConfigOptions) != 1 || setResult.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("set config result = %#v, want smart", setResult.ConfigOptions)
	}

	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt": []map[string]any{
				{"type": "text", "text": "hello"},
				{"type": "resource", "resource": map[string]any{"uri": "file:///note.md", "text": "from resource"}},
			},
		},
	})
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want update notification + prompt response: %#v", len(responses), responses)
	}
	if responses[0].Method != "session/update" {
		t.Fatalf("first response method = %q, want session/update", responses[0].Method)
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	responses[0].ParamsInto(t, &update)
	if update.SessionID != "session-1" || update.Update.SessionUpdate != "agent_message_chunk" || update.Update.Content.Text != "assistant answer" {
		t.Fatalf("update = %#v, want assistant chunk", update)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[1].ResultInto(t, &promptResult)
	if promptResult.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", promptResult.StopReason)
	}
	if saw.ID != "session-1" || saw.Config["model"] != "smart" || sawPrompt != "hello\n\nfrom resource" {
		t.Fatalf("builder saw session=%#v prompt=%q, want configured session", saw, sawPrompt)
	}
}

func TestBridgeStreamsPromptCommandOutputChunks(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			if spec.Command != "agent" || strings.Join(spec.Args, " ") != "hello" || spec.Dir != "/tmp/work" {
				t.Fatalf("process spec = %#v, want prompt command", spec)
			}
			if err := onStdout([]byte("hello ")); err != nil {
				return adapterprocess.Result{}, err
			}
			if err := onStdout([]byte("stream")); err != nil {
				return adapterprocess.Result{}, err
			}
			return adapterprocess.Result{Stdout: []byte("hello stream")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		},
	})
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want two chunks + prompt response: %#v", len(responses), responses)
	}
	for i, want := range []string{"hello ", "stream"} {
		if responses[i].Method != "session/update" {
			t.Fatalf("response %d method = %q, want session/update", i, responses[i].Method)
		}
		var update struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		responses[i].ParamsInto(t, &update)
		if update.SessionID != "session-1" ||
			update.Update.SessionUpdate != "agent_message_chunk" ||
			update.Update.Content.Type != "text" ||
			update.Update.Content.Text != want {
			t.Fatalf("chunk %d update = %#v, want %q", i, update, want)
		}
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[2].ResultInto(t, &promptResult)
	if promptResult.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", promptResult.StopReason)
	}
}

func TestBridgeRejectsUnsupportedConfigValue(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
			DefaultValue: "default",
			Options:      []commandbridge.SelectValue{{Value: "default", Name: "Default"}},
		}},
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{}, nil
		},
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	resp := client.Request("session/set_config_option", map[string]any{
		"sessionId": "session-1",
		"configId":  "model",
		"value":     "missing",
	})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("response error = %#v, want invalid params", resp.Error)
	}
}

func TestBridgeCancelStopsActivePrompt(t *testing.T) {
	started := make(chan struct{})
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(ctx context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			close(started)
			<-ctx.Done()
			return adapterprocess.Result{}, ctx.Err()
		}),
	})
	srv := server(bridge)
	client := acptest.NewClient(t, srv)
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	promptDone := make(chan []acptest.Response, 1)
	go func() {
		promptDone <- client.Send(map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "session/prompt",
			"params": map[string]any{
				"sessionId": "session-1",
				"prompt":    []map[string]any{{"type": "text", "text": "stop soon"}},
			},
		})
	}()
	<-started
	client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})

	responses := <-promptDone
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want prompt response", len(responses))
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	responses[0].ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
}

func TestBridgePromptCommandErrorMapsToRPCError(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("boom")}, errors.New("exit 2")
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	resp := client.Request("session/prompt", map[string]any{
		"sessionId": "session-1",
		"prompt":    []map[string]any{{"type": "text", "text": "fail"}},
	})
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "prompt command failed" {
		t.Fatalf("response error = %#v, want prompt command failure", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte("boom")) {
		t.Fatalf("error data = %s, want stderr", raw)
	}
}

func server(bridge *commandbridge.Bridge) *acp.Server {
	return acp.NewServer(acp.AdapterInfo{
		Name:  "test-command-adapter",
		Title: "Test Command Adapter",
	}, bridge.Options()...)
}

type streamingRunnerFunc func(context.Context, adapterprocess.Spec, func([]byte) error) (adapterprocess.Result, error)

func (f streamingRunnerFunc) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	return adapterprocess.Result{}, errors.New("buffered Run should not be called")
}

func (f streamingRunnerFunc) RunStream(ctx context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
	return f(ctx, spec, onStdout)
}
