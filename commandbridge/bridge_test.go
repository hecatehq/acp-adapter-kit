package commandbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

	configUpdate, updated := setConfigOption(t, client, "session-1", "model", "smart")
	if configUpdate.SessionID != "session-1" ||
		configUpdate.Update.SessionUpdate != "config_option_update" ||
		len(configUpdate.Update.ConfigOptions) != 1 ||
		configUpdate.Update.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("config update = %#v, want smart config option notification", configUpdate)
	}
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
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want tool start + chunk + tool finish + prompt response: %#v", len(responses), responses)
	}
	start := decodeCommandToolUpdate(t, responses[0])
	if start.SessionID != "session-1" ||
		start.Update.SessionUpdate != "tool_call" ||
		start.Update.ToolCallID == "" ||
		start.Update.Title != "Run agent" ||
		start.Update.Kind != "execute" ||
		start.Update.Status != "in_progress" ||
		start.Update.RawInput["command"] != "agent --model smart hello\n\nfrom resource" ||
		start.Update.RawInput["cwd"] != "/tmp/work" {
		t.Fatalf("tool start = %#v, want prompt command metadata", start)
	}

	chunk := decodeAgentChunk(t, responses[1])
	if chunk.SessionID != "session-1" || chunk.Update.SessionUpdate != "agent_message_chunk" || chunk.Update.Content.Text != "assistant answer" {
		t.Fatalf("chunk = %#v, want assistant chunk", chunk)
	}

	finish := decodeCommandToolUpdate(t, responses[2])
	if finish.SessionID != "session-1" ||
		finish.Update.SessionUpdate != "tool_call_update" ||
		finish.Update.ToolCallID != start.Update.ToolCallID ||
		finish.Update.Title != "Run agent" ||
		finish.Update.Kind != "execute" ||
		finish.Update.Status != "completed" ||
		len(finish.Update.Content) != 1 ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "stdout:\nassistant answer") {
		t.Fatalf("tool finish = %#v, want completed command metadata", finish)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[3].ResultInto(t, &promptResult)
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
	if len(responses) != 5 {
		t.Fatalf("got %d responses, want tool start + two chunks + tool finish + prompt response: %#v", len(responses), responses)
	}
	start := decodeCommandToolUpdate(t, responses[0])
	if start.Update.SessionUpdate != "tool_call" || start.Update.ToolCallID == "" || start.Update.Status != "in_progress" {
		t.Fatalf("tool start = %#v, want running tool call", start)
	}
	for i, want := range []string{"hello ", "stream"} {
		update := decodeAgentChunk(t, responses[i+1])
		if update.SessionID != "session-1" ||
			update.Update.SessionUpdate != "agent_message_chunk" ||
			update.Update.Content.Type != "text" ||
			update.Update.Content.Text != want {
			t.Fatalf("chunk %d update = %#v, want %q", i, update, want)
		}
	}
	finish := decodeCommandToolUpdate(t, responses[3])
	if finish.Update.SessionUpdate != "tool_call_update" ||
		finish.Update.ToolCallID != start.Update.ToolCallID ||
		finish.Update.Status != "completed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "stdout:\nhello stream") {
		t.Fatalf("tool finish = %#v, want completed streamed command", finish)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[4].ResultInto(t, &promptResult)
	if promptResult.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", promptResult.StopReason)
	}
}

func TestBridgeParsesStructuredStreamIntoACPUpdates(t *testing.T) {
	var prompts []string
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				switch event["type"] {
				case "message":
					text, _ := event["text"].(string)
					return commandbridge.JSONLMapping{
						Events:         []commandbridge.StreamEvent{commandbridge.AgentMessageChunk(text)},
						TranscriptText: text,
					}, nil
				case "thought":
					text, _ := event["text"].(string)
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{commandbridge.AgentThoughtChunk("thought-1", text)}}, nil
				case "tool_start":
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{
						commandbridge.ToolCallStart("tool-1", "Run tests", "execute", "in_progress", map[string]any{"command": "go test ./..."}),
					}}, nil
				case "tool_finish":
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{
						commandbridge.ToolCallFinish("tool-1", "Run tests", "execute", "completed", "ok"),
					}}, nil
				case "usage":
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{commandbridge.UsageUpdate(12, 200)}}, nil
				default:
					return commandbridge.JSONLMapping{}, nil
				}
			})
		},
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			stream := strings.Join([]string{
				`{"type":"tool_start"}`,
				`{"type":"thought","text":"checking"}`,
				`{"type":"message","text":"done"}`,
				`{"type":"usage"}`,
				`{"type":"tool_finish"}`,
				"",
			}, "\n")
			if err := onStdout([]byte(stream[:len(stream)/2])); err != nil {
				return adapterprocess.Result{}, err
			}
			if err := onStdout([]byte(stream[len(stream)/2:])); err != nil {
				return adapterprocess.Result{}, err
			}
			return adapterprocess.Result{Stdout: []byte(stream)}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "first"))
	if len(responses) != 9 {
		t.Fatalf("got %d responses, want command start + 5 parsed updates + command finish + session info + prompt response: %#v", len(responses), responses)
	}
	start := decodeCommandToolUpdate(t, responses[0])
	if start.Update.SessionUpdate != "tool_call" || start.Update.Title != "Run agent" {
		t.Fatalf("start = %#v, want outer command tool start", start)
	}
	innerStart := decodeCommandToolUpdate(t, responses[1])
	if innerStart.Update.SessionUpdate != "tool_call" ||
		innerStart.Update.ToolCallID != "tool-1" ||
		innerStart.Update.Title != "Run tests" ||
		innerStart.Update.Kind != "execute" ||
		innerStart.Update.RawInput["command"] != "go test ./..." {
		t.Fatalf("inner start = %#v, want parsed tool start", innerStart)
	}
	thought := decodeAgentChunk(t, responses[2])
	if thought.Update.SessionUpdate != "agent_thought_chunk" || thought.Update.Content.Text != "checking" {
		t.Fatalf("thought = %#v, want parsed thought", thought)
	}
	message := decodeAgentChunk(t, responses[3])
	if message.Update.SessionUpdate != "agent_message_chunk" || message.Update.Content.Text != "done" {
		t.Fatalf("message = %#v, want parsed message", message)
	}
	usage := decodeUsageUpdate(t, responses[4])
	if usage.Update.Used != 12 || usage.Update.Size != 200 {
		t.Fatalf("usage = %#v, want parsed usage", usage)
	}
	innerFinish := decodeCommandToolUpdate(t, responses[5])
	if innerFinish.Update.SessionUpdate != "tool_call_update" ||
		innerFinish.Update.ToolCallID != "tool-1" ||
		innerFinish.Update.Status != "completed" ||
		innerFinish.Update.Content[0].Content.Text != "ok" {
		t.Fatalf("inner finish = %#v, want parsed tool finish", innerFinish)
	}
	finish := decodeCommandToolUpdate(t, responses[6])
	if finish.Update.SessionUpdate != "tool_call_update" || finish.Update.ToolCallID != start.Update.ToolCallID {
		t.Fatalf("finish = %#v, want outer command finish", finish)
	}
	info := decodeSessionInfoUpdate(t, responses[7])
	if info.SessionID != "session-1" ||
		info.Update.SessionUpdate != "session_info_update" ||
		info.Update.Title != "first" ||
		info.Update.UpdatedAt == "" {
		t.Fatalf("session info = %#v, want title + updated timestamp", info)
	}

	client.Send(promptRequest(3, "session-1", "second"))
	if len(prompts) != 2 ||
		!strings.Contains(prompts[1], "Assistant:\ndone") ||
		strings.Contains(prompts[1], `{"type":"message"`) {
		t.Fatalf("second prompt = %q, want cleaned transcript prelude", prompts[1])
	}
}

func TestBridgePublishesSessionInfoAfterTranscriptPrompt(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	ticks := 0
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		Now: func() time.Time {
			ticks++
			return base.Add(time.Duration(ticks) * time.Second)
		},
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("done")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "Use metadata updates so ACP hosts can show a useful command-backed session title"))
	if len(responses) != 5 {
		t.Fatalf("responses = %#v, want prompt lifecycle + session info", responses)
	}
	info := decodeSessionInfoUpdate(t, responses[3])
	if info.SessionID != "session-1" ||
		info.Update.SessionUpdate != "session_info_update" ||
		!strings.HasPrefix(info.Update.Title, "Use metadata updates so ACP hosts") ||
		info.Update.UpdatedAt != base.Add(2*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("session info = %#v, want transcript metadata", info)
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

func TestBridgeLoadSessionRebindsWorkspaceAndKeepsConfig(t *testing.T) {
	var ids []string
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string {
			ids = append(ids, "session-1")
			return "session-1"
		},
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
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
			return adapterprocess.Spec{Command: "agent", Args: []string{session.Config["model"], text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			if spec.Dir != "/tmp/rebound" || strings.Join(spec.Args, " ") != "smart hello" {
				t.Fatalf("process spec = %#v, want rebound cwd and kept config", spec)
			}
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/original"})
	setConfigOption(t, client, "session-1", "model", "smart")

	loaded := client.Request("session/load", map[string]any{
		"sessionId":             "session-1",
		"cwd":                   "/tmp/rebound",
		"additionalDirectories": []string{"/tmp/shared"},
	})
	var loadResult struct {
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	loaded.ResultInto(t, &loadResult)
	if len(loadResult.ConfigOptions) != 1 || loadResult.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("load result = %#v, want kept config", loadResult.ConfigOptions)
	}
	if len(ids) != 1 {
		t.Fatalf("generated ids = %#v, want no new session id on load", ids)
	}

	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		},
	})
	if len(responses) != 4 {
		t.Fatalf("responses = %#v, want prompt lifecycle", responses)
	}
}

func TestBridgeLoadSessionRejectsStaleID(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{}, nil
		},
	})
	client := acptest.NewClient(t, server(bridge))

	resp := client.Request("session/load", map[string]any{"sessionId": "missing", "cwd": "/tmp/work"})
	if resp.Error == nil || resp.Error.Code != -32001 || resp.Error.Message != "session not found" {
		t.Fatalf("load response error = %#v, want session not found", resp.Error)
	}
}

func TestBridgeLoadUnknownSessionAdoptsNativeIDWhenEnabled(t *testing.T) {
	const nativeID = "550e8400-e29b-41d4-a716-446655440000"
	generatedIDs := 0
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string {
			generatedIDs++
			return "generated-session"
		},
		LoadUnknownSessions: true,
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
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
			if session.ID != nativeID ||
				session.CWD != "/tmp/reloaded" ||
				session.Config["model"] != "default" ||
				len(session.AdditionalDirectories) != 1 ||
				session.AdditionalDirectories[0] != "/tmp/shared" {
				t.Fatalf("session = %#v, want adopted native id with loaded workspace state", session)
			}
			return adapterprocess.Spec{Command: "agent", Args: []string{session.ID, text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			if spec.Command != "agent" || spec.Dir != "/tmp/reloaded" || strings.Join(spec.Args, " ") != nativeID+" hello" {
				t.Fatalf("process spec = %#v, want adopted native session id", spec)
			}
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	loaded := client.Request("session/load", map[string]any{
		"sessionId":             nativeID,
		"cwd":                   "/tmp/reloaded",
		"additionalDirectories": []string{"/tmp/shared"},
	})
	var loadResult struct {
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	loaded.ResultInto(t, &loadResult)
	if len(loadResult.ConfigOptions) != 1 || loadResult.ConfigOptions[0].CurrentValue != "default" {
		t.Fatalf("load result = %#v, want default config for adopted session", loadResult.ConfigOptions)
	}
	if generatedIDs != 0 {
		t.Fatalf("generated ids = %d, want load to adopt requested id", generatedIDs)
	}

	listed := client.Request("session/list", map[string]any{"cwd": "/tmp/reloaded"})
	var listResult struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			CWD       string `json:"cwd"`
		} `json:"sessions"`
	}
	listed.ResultInto(t, &listResult)
	if len(listResult.Sessions) != 1 || listResult.Sessions[0].SessionID != nativeID || listResult.Sessions[0].CWD != "/tmp/reloaded" {
		t.Fatalf("listed sessions = %#v, want adopted native session", listResult.Sessions)
	}

	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": nativeID,
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		},
	})
	if len(responses) != 4 {
		t.Fatalf("responses = %#v, want prompt lifecycle", responses)
	}
}

func TestBridgeForkSessionClonesConfigAndSeparatesState(t *testing.T) {
	next := 0
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string {
			next++
			return []string{"source", "fork"}[next-1]
		},
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
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
			return adapterprocess.Spec{Command: "agent", Args: []string{session.ID, session.Config["model"], text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte(strings.Join(spec.Args, "|") + "@" + spec.Dir)}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/source"})
	setConfigOption(t, client, "source", "model", "smart")

	forked := client.Request("session/fork", map[string]any{
		"sessionId": "source",
		"cwd":       "/tmp/fork",
	})
	var forkResult struct {
		SessionID     string `json:"sessionId"`
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	forked.ResultInto(t, &forkResult)
	if forkResult.SessionID != "fork" || len(forkResult.ConfigOptions) != 1 || forkResult.ConfigOptions[0].CurrentValue != "smart" {
		t.Fatalf("fork result = %#v, want fork with cloned smart config", forkResult)
	}

	setConfigOption(t, client, "fork", "model", "default")
	sourceResponses := client.Send(promptRequest(5, "source", "hello"))
	sourceChunk := decodeAgentChunk(t, sourceResponses[1])
	if sourceChunk.Update.Content.Text != "source|smart|hello@/tmp/source" {
		t.Fatalf("source chunk = %#v, want source config unaffected", sourceChunk)
	}
	forkResponses := client.Send(promptRequest(6, "fork", "hello"))
	forkChunk := decodeAgentChunk(t, forkResponses[1])
	if forkChunk.Update.Content.Text != "fork|default|hello@/tmp/fork" {
		t.Fatalf("fork chunk = %#v, want fork state", forkChunk)
	}
}

func TestBridgePublishesAvailableCommandsOnSessionCreateAndLoad(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		Commands: []commandbridge.AvailableCommand{
			{Name: "web", Description: "Search the web", InputHint: "query"},
			{Name: "plan", Description: "Create a plan"},
		},
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{}, nil
		},
	})
	client := acptest.NewClient(t, server(bridge))

	createResponses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "session/new",
		"params":  map[string]any{"cwd": "/tmp/work"},
	})
	if len(createResponses) != 2 {
		t.Fatalf("create responses = %#v, want command update + session response", createResponses)
	}
	createCommands := decodeAvailableCommands(t, createResponses[0])
	if createCommands.SessionID != "session-1" ||
		len(createCommands.Update.AvailableCommands) != 2 ||
		createCommands.Update.AvailableCommands[0].Name != "web" ||
		createCommands.Update.AvailableCommands[0].Input.Unstructured.Hint != "query" {
		t.Fatalf("create commands = %#v, want web + plan commands", createCommands)
	}

	loadResponses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/load",
		"params":  map[string]any{"sessionId": "session-1", "cwd": "/tmp/work"},
	})
	if len(loadResponses) != 2 {
		t.Fatalf("load responses = %#v, want command update + load response", loadResponses)
	}
	loadCommands := decodeAvailableCommands(t, loadResponses[0])
	if loadCommands.SessionID != "session-1" || len(loadCommands.Update.AvailableCommands) != 2 {
		t.Fatalf("load commands = %#v, want command replay", loadCommands)
	}
}

func TestBridgeListSessionsReturnsMetadataAndFiltersByWorkspace(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	ticks := 0
	nextID := 0
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string {
			nextID++
			return []string{"session-a", "session-b"}[nextID-1]
		},
		Now: func() time.Time {
			ticks++
			return base.Add(time.Duration(ticks) * time.Minute)
		},
		IncludeTranscript: true,
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("done")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{
		"cwd":                   "/tmp/a",
		"additionalDirectories": []string{"/tmp/shared"},
	})
	client.Send(promptRequest(2, "session-a", "Implement the command-backed metadata contract with a long enough title to trim eventually"))
	client.Request("session/new", map[string]any{"cwd": "/tmp/b"})

	all := client.Request("session/list", map[string]any{})
	var allResult struct {
		Sessions []struct {
			SessionID             string   `json:"sessionId"`
			CWD                   string   `json:"cwd"`
			AdditionalDirectories []string `json:"additionalDirectories"`
			Title                 string   `json:"title"`
			UpdatedAt             string   `json:"updatedAt"`
		} `json:"sessions"`
	}
	all.ResultInto(t, &allResult)
	if len(allResult.Sessions) != 2 || allResult.Sessions[0].SessionID != "session-b" || allResult.Sessions[1].SessionID != "session-a" {
		t.Fatalf("all sessions = %#v, want newest first", allResult.Sessions)
	}

	filtered := client.Request("session/list", map[string]any{"cwd": "/tmp/a"})
	var filteredResult struct {
		Sessions []struct {
			SessionID             string   `json:"sessionId"`
			CWD                   string   `json:"cwd"`
			AdditionalDirectories []string `json:"additionalDirectories"`
			Title                 string   `json:"title"`
			UpdatedAt             string   `json:"updatedAt"`
		} `json:"sessions"`
	}
	filtered.ResultInto(t, &filteredResult)
	if len(filteredResult.Sessions) != 1 ||
		filteredResult.Sessions[0].SessionID != "session-a" ||
		filteredResult.Sessions[0].CWD != "/tmp/a" ||
		len(filteredResult.Sessions[0].AdditionalDirectories) != 1 ||
		filteredResult.Sessions[0].AdditionalDirectories[0] != "/tmp/shared" ||
		!strings.HasPrefix(filteredResult.Sessions[0].Title, "Implement the command-backed metadata contract") ||
		filteredResult.Sessions[0].UpdatedAt != base.Add(2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("filtered sessions = %#v, want session-a metadata", filteredResult.Sessions)
	}
}

func TestBridgeCanIncludeBoundedTranscriptInPromptCommand(t *testing.T) {
	var prompts []string
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:                  func() string { return "session-1" },
		IncludeTranscript:      true,
		MaxTranscriptExchanges: 1,
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			current := spec.Args[0]
			if marker := "Current user request:\n"; strings.Contains(current, marker) {
				current = current[strings.LastIndex(current, marker)+len(marker):]
			}
			return adapterprocess.Result{Stdout: []byte("answer to " + strings.TrimSpace(current))}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	client.Send(promptRequest(2, "session-1", "first"))
	client.Send(promptRequest(3, "session-1", "second"))
	client.Send(promptRequest(4, "session-1", "third"))

	if len(prompts) != 3 {
		t.Fatalf("prompts = %#v, want three prompt builds", prompts)
	}
	if prompts[0] != "first" {
		t.Fatalf("first prompt = %q, want raw first turn", prompts[0])
	}
	if !strings.Contains(prompts[1], "Previous conversation:") ||
		!strings.Contains(prompts[1], "User:\nfirst") ||
		!strings.Contains(prompts[1], "Assistant:\nanswer to first") ||
		!strings.Contains(prompts[1], "Current user request:\nsecond") {
		t.Fatalf("second prompt = %q, want first exchange prelude", prompts[1])
	}
	if strings.Contains(prompts[2], "User:\nfirst") ||
		!strings.Contains(prompts[2], "User:\nsecond") ||
		!strings.Contains(prompts[2], "Current user request:\nthird") {
		t.Fatalf("third prompt = %q, want bounded transcript with only second exchange", prompts[2])
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
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + tool finish + prompt response", len(responses))
	}
	start := decodeCommandToolUpdate(t, responses[0])
	if start.Update.SessionUpdate != "tool_call" || start.Update.Status != "in_progress" {
		t.Fatalf("tool start = %#v, want running command", start)
	}
	finish := decodeCommandToolUpdate(t, responses[1])
	if finish.Update.SessionUpdate != "tool_call_update" ||
		finish.Update.ToolCallID != start.Update.ToolCallID ||
		finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "cancelled: context canceled") {
		t.Fatalf("tool finish = %#v, want failed cancelled command", finish)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	responses[2].ResultInto(t, &result)
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

	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt":    []map[string]any{{"type": "text", "text": "fail"}},
		},
	})
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + tool finish + prompt error", len(responses))
	}
	start := decodeCommandToolUpdate(t, responses[0])
	finish := decodeCommandToolUpdate(t, responses[1])
	if finish.Update.SessionUpdate != "tool_call_update" ||
		finish.Update.ToolCallID != start.Update.ToolCallID ||
		finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "stderr:\nboom") {
		t.Fatalf("tool finish = %#v, want failed stderr preview", finish)
	}
	resp := responses[2]
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "prompt command failed" {
		t.Fatalf("response error = %#v, want prompt command failure", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte("boom")) {
		t.Fatalf("error data = %s, want stderr", raw)
	}
}

func TestBridgeRunsLogoutCommand(t *testing.T) {
	var saw adapterprocess.Spec
	bridge := commandbridge.New(commandbridge.Spec{
		BuildLogout: func() (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent", Args: []string{"logout"}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			saw = spec
			return adapterprocess.Result{Stdout: []byte("logged out\n")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	resp := client.Request("logout", map[string]any{})
	var result map[string]any
	resp.ResultInto(t, &result)
	if len(result) != 0 {
		t.Fatalf("logout result = %#v, want empty object", result)
	}
	if saw.Command != "agent" || strings.Join(saw.Args, " ") != "logout" {
		t.Fatalf("logout command = %#v, want agent logout", saw)
	}
}

func TestBridgeLogoutCommandErrorMapsToRPCError(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		BuildLogout: func() (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent", Args: []string{"logout"}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("not signed in")}, errors.New("exit 1")
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	resp := client.Request("logout", map[string]any{})
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "logout command failed" {
		t.Fatalf("response error = %#v, want logout command failure", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte("not signed in")) {
		t.Fatalf("error data = %s, want stderr", raw)
	}
}

func server(bridge *commandbridge.Bridge) *acp.Server {
	return acp.NewServer(acp.AdapterInfo{
		Name:  "test-command-adapter",
		Title: "Test Command Adapter",
	}, bridge.Options()...)
}

func promptRequest(id int, sessionID, prompt string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": prompt}},
		},
	}
}

func setConfigOption(t testing.TB, client *acptest.Client, sessionID, configID, value string) (configOptionsUpdate, acptest.Response) {
	t.Helper()
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      "set-config-" + configID,
		"method":  "session/set_config_option",
		"params": map[string]any{
			"sessionId": sessionID,
			"configId":  configID,
			"value":     value,
		},
	})
	if len(responses) != 2 {
		t.Fatalf("set_config_option responses = %#v, want config update + response", responses)
	}
	return decodeConfigOptionsUpdate(t, responses[0]), responses[1]
}

type streamingRunnerFunc func(context.Context, adapterprocess.Spec, func([]byte) error) (adapterprocess.Result, error)

func (f streamingRunnerFunc) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	return adapterprocess.Result{}, errors.New("buffered Run should not be called")
}

func (f streamingRunnerFunc) RunStream(ctx context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
	return f(ctx, spec, onStdout)
}

type commandToolUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string         `json:"sessionUpdate"`
		ToolCallID    string         `json:"toolCallId"`
		Title         string         `json:"title"`
		Kind          string         `json:"kind"`
		Status        string         `json:"status"`
		RawInput      map[string]any `json:"rawInput"`
		Content       []struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	} `json:"update"`
}

func decodeCommandToolUpdate(t testing.TB, response acptest.Response) commandToolUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update commandToolUpdate
	response.ParamsInto(t, &update)
	return update
}

type agentChunkUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"update"`
}

func decodeAgentChunk(t testing.TB, response acptest.Response) agentChunkUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update agentChunkUpdate
	response.ParamsInto(t, &update)
	return update
}

type usageUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Used          int    `json:"used"`
		Size          int    `json:"size"`
	} `json:"update"`
}

func decodeUsageUpdate(t testing.TB, response acptest.Response) usageUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update usageUpdate
	response.ParamsInto(t, &update)
	if update.Update.SessionUpdate != "usage_update" {
		t.Fatalf("session update = %q, want usage_update", update.Update.SessionUpdate)
	}
	return update
}

type availableCommandsUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate     string `json:"sessionUpdate"`
		AvailableCommands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Input       struct {
				Unstructured struct {
					Hint string `json:"hint"`
				} `json:"unstructured"`
			} `json:"input"`
		} `json:"availableCommands"`
	} `json:"update"`
}

type configOptionsUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	} `json:"update"`
}

type sessionInfoUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Title         string `json:"title"`
		UpdatedAt     string `json:"updatedAt"`
	} `json:"update"`
}

func decodeConfigOptionsUpdate(t testing.TB, response acptest.Response) configOptionsUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update configOptionsUpdate
	response.ParamsInto(t, &update)
	if update.Update.SessionUpdate != "config_option_update" {
		t.Fatalf("session update = %q, want config_option_update", update.Update.SessionUpdate)
	}
	return update
}

func decodeSessionInfoUpdate(t testing.TB, response acptest.Response) sessionInfoUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update sessionInfoUpdate
	response.ParamsInto(t, &update)
	if update.Update.SessionUpdate != "session_info_update" {
		t.Fatalf("session update = %q, want session_info_update", update.Update.SessionUpdate)
	}
	return update
}

func decodeAvailableCommands(t testing.TB, response acptest.Response) availableCommandsUpdate {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var update availableCommandsUpdate
	response.ParamsInto(t, &update)
	if update.Update.SessionUpdate != "available_commands_update" {
		t.Fatalf("session update = %q, want available_commands_update", update.Update.SessionUpdate)
	}
	return update
}
