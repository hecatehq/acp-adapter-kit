package commandbridge_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
	"github.com/hecatehq/acp-adapter-kit/commandbridge"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestBridgeDefaultSessionIDsDoNotCollideAcrossBridgeInstances(t *testing.T) {
	assertDefaultID := func(t *testing.T, id string) {
		t.Helper()
		encoded := strings.TrimPrefix(id, "session-")
		if encoded == id || len(encoded) != 32 {
			t.Fatalf("session id = %q, want session- plus 128 bits", id)
		}
		if _, err := hex.DecodeString(encoded); err != nil {
			t.Fatalf("session id = %q, want hexadecimal entropy: %v", id, err)
		}
	}
	newSession := func(t *testing.T) string {
		t.Helper()
		bridge := commandbridge.New(commandbridge.Spec{})
		client := acptest.NewClient(t, server(bridge))
		created := client.Request("session/new", map[string]any{"cwd": "/tmp/work"})
		var session struct {
			SessionID string `json:"sessionId"`
		}
		created.ResultInto(t, &session)
		return session.SessionID
	}
	type forkIDs struct {
		source string
		fork   string
	}
	newFork := func(t *testing.T) forkIDs {
		t.Helper()
		bridge := commandbridge.New(commandbridge.Spec{})
		client := acptest.NewClient(t, server(bridge))
		created := client.Request("session/new", map[string]any{"cwd": "/tmp/work"})
		var source struct {
			SessionID string `json:"sessionId"`
		}
		created.ResultInto(t, &source)
		forked := client.Request("session/fork", map[string]any{"sessionId": source.SessionID})
		var session struct {
			SessionID string `json:"sessionId"`
		}
		forked.ResultInto(t, &session)
		return forkIDs{source: source.SessionID, fork: session.SessionID}
	}

	t.Run("new", func(t *testing.T) {
		first := newSession(t)
		second := newSession(t)
		if first == second {
			t.Fatalf("independent bridges returned the same session id %q", first)
		}
		assertDefaultID(t, first)
		assertDefaultID(t, second)
	})
	t.Run("fork", func(t *testing.T) {
		first := newFork(t)
		second := newFork(t)
		for _, ids := range []forkIDs{first, second} {
			if ids.fork == ids.source {
				t.Fatalf("fork reused source session id %q", ids.source)
			}
			assertDefaultID(t, ids.source)
			assertDefaultID(t, ids.fork)
		}
		if first.fork == second.fork {
			t.Fatalf("independent bridges returned the same fork session id %q", first.fork)
		}
	})
}

func TestBridgeRejectsEmptyCustomSessionID(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		bridge := commandbridge.New(commandbridge.Spec{NewID: func() string { return " " }})
		client := acptest.NewClient(t, server(bridge))
		response := client.Request("session/new", map[string]any{"cwd": "/tmp/work"})
		if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != "session id generation failed" {
			t.Fatalf("session/new response = %#v, want session id generation error", response)
		}
	})
	t.Run("fork", func(t *testing.T) {
		ids := []string{"source", " "}
		bridge := commandbridge.New(commandbridge.Spec{NewID: func() string {
			id := ids[0]
			ids = ids[1:]
			return id
		}})
		client := acptest.NewClient(t, server(bridge))
		client.Request("session/new", map[string]any{"cwd": "/tmp/work"})
		response := client.Request("session/fork", map[string]any{"sessionId": "source"})
		if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != "session id generation failed" {
			t.Fatalf("session/fork response = %#v, want session id generation error", response)
		}
		listed := client.Request("session/list", map[string]any{})
		var list struct {
			Sessions []struct {
				SessionID string `json:"sessionId"`
			} `json:"sessions"`
		}
		listed.ResultInto(t, &list)
		if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "source" {
			t.Fatalf("sessions after failed fork = %#v, want only source", list.Sessions)
		}
	})
}

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
			if spec.Command != "agent" || len(spec.Args) != 3 || spec.Args[0] != "--model" || spec.Args[1] != "smart" ||
				!strings.Contains(spec.Args[2], "hello") ||
				!strings.Contains(spec.Args[2], `"kind":"embedded_text"`) ||
				!strings.Contains(spec.Args[2], `"name":"note.md"`) ||
				!strings.Contains(spec.Args[2], `"text":"from resource"`) ||
				spec.Dir != "/tmp/work" {
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
		start.Update.RawInput["command"] != "agent" ||
		start.Update.RawInput["arguments"] != adapterprocess.RedactedValue ||
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
	if saw.ID != "session-1" || saw.Config["model"] != "smart" ||
		!strings.Contains(sawPrompt, `"kind":"embedded_text"`) ||
		!strings.Contains(sawPrompt, `"text":"from resource"`) {
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

func TestBridgeRequestsPermissionForStructuredStreamTool(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				switch event["type"] {
				case "permission":
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{
						commandbridge.ToolCallPermissionRequest("tool-1", "Run tests", "execute", map[string]any{"command": "go test ./..."}, nil),
					}}, nil
				case "message":
					return commandbridge.JSONLMapping{
						Events:         []commandbridge.StreamEvent{commandbridge.AgentMessageChunk("allowed")},
						TranscriptText: "allowed",
					}, nil
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
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			stream := strings.Join([]string{
				`{"type":"permission"}`,
				`{"type":"message"}`,
				"",
			}, "\n")
			if err := onStdout([]byte(stream)); err != nil {
				return adapterprocess.Result{Stdout: []byte(stream)}, err
			}
			return adapterprocess.Result{Stdout: []byte(stream)}, nil
		}),
	})
	client := acptest.NewLiveClient(t, server(bridge), acptest.WithAutoAllowPermissions())
	client.Request("new", "session/new", map[string]any{"cwd": "/tmp/work"}, time.Second)
	responses := client.PromptText("prompt", "session-1", "first", time.Second)
	if len(responses) != 5 {
		t.Fatalf("got %d responses, want command start + permission request + message + command finish + prompt response: %#v", len(responses), responses)
	}
	start := decodeCommandToolUpdate(t, responses[0])
	if start.Update.SessionUpdate != "tool_call" || start.Update.Title != "Run agent" {
		t.Fatalf("start = %#v, want outer command tool start", start)
	}
	permission := decodePermissionRequest(t, responses[1])
	if string(responses[1].ID) != `"server-1"` ||
		permission.SessionID != "session-1" ||
		permission.ToolCall.ToolCallID != "tool-1" ||
		permission.ToolCall.Title != "Run tests" ||
		permission.ToolCall.Kind != "execute" ||
		permission.ToolCall.Status != "pending" ||
		permission.ToolCall.RawInput["command"] != "go test ./..." {
		t.Fatalf("permission = %#v, want pending tool permission request", permission)
	}
	if len(permission.Options) != 2 ||
		permission.Options[0].OptionID != "allow_once" ||
		permission.Options[0].Kind != "allow_once" ||
		permission.Options[1].OptionID != "reject_once" ||
		permission.Options[1].Kind != "reject_once" {
		t.Fatalf("permission options = %#v, want default allow/reject options", permission.Options)
	}
	message := decodeAgentChunk(t, responses[2])
	if message.Update.Content.Text != "allowed" {
		t.Fatalf("message = %#v, want allowed stream after permission", message)
	}
	finish := decodeCommandToolUpdate(t, responses[3])
	if finish.Update.Status != "completed" {
		t.Fatalf("finish = %#v, want completed command", finish)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[4].ResultInto(t, &promptResult)
	if promptResult.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", promptResult.StopReason)
	}
}

func TestBridgeRejectsPermissionForStructuredStreamTool(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				if event["type"] != "permission" {
					return commandbridge.JSONLMapping{}, nil
				}
				return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{
					commandbridge.ToolCallPermissionRequest("tool-1", "Run rm", "execute", map[string]any{"command": "rm -rf tmp"}, []commandbridge.PermissionOption{
						{OptionID: "allow", Name: "Allow", Kind: "allow_once"},
						{OptionID: "reject", Name: "Reject", Kind: "reject_once"},
					}),
				}}, nil
			})
		},
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			stream := `{"type":"permission"}` + "\n"
			if err := onStdout([]byte(stream)); err != nil {
				return adapterprocess.Result{Stdout: []byte(stream)}, err
			}
			return adapterprocess.Result{Stdout: []byte(stream)}, nil
		}),
	})
	client := acptest.NewLiveClient(t, server(bridge), acptest.WithAutoRejectPermissions())
	client.Request("new", "session/new", map[string]any{"cwd": "/tmp/work"}, time.Second)
	responses := client.PromptText("prompt", "session-1", "first", time.Second)
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want command start + permission request + command finish + prompt error: %#v", len(responses), responses)
	}
	permission := decodePermissionRequest(t, responses[1])
	if permission.ToolCall.ToolCallID != "tool-1" || permission.Options[1].OptionID != "reject" {
		t.Fatalf("permission = %#v, want custom reject option", permission)
	}
	finish := decodeCommandToolUpdate(t, responses[2])
	if finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "permission rejected for Run rm") {
		t.Fatalf("finish = %#v, want failed rejected command", finish)
	}
	if responses[3].Error == nil ||
		responses[3].Error.Code != -32000 ||
		responses[3].Error.Message != "prompt command failed" {
		t.Fatalf("prompt response error = %#v, want prompt command failed", responses[3].Error)
	}
	raw, _ := json.Marshal(responses[3].Error.Data)
	if !bytes.Contains(raw, []byte("permission rejected for Run rm")) {
		t.Fatalf("error data = %s, want permission rejection", raw)
	}
}

func newPermissionOutcomeTestBridge() *commandbridge.Bridge {
	return commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				switch event["type"] {
				case "permission":
					return commandbridge.JSONLMapping{Events: []commandbridge.StreamEvent{
						commandbridge.ToolCallPermissionRequest("tool-1", "Run tests", "execute", map[string]any{"command": "go test ./..."}, []commandbridge.PermissionOption{
							{OptionID: "allow-session", Name: "Allow for session", Kind: "allow_always"},
							{OptionID: "deny-once", Name: "Deny once", Kind: "reject_once"},
						}),
					}}, nil
				case "message":
					return commandbridge.JSONLMapping{
						Events:         []commandbridge.StreamEvent{commandbridge.AgentMessageChunk("allowed")},
						TranscriptText: "allowed",
					}, nil
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
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			stream := strings.Join([]string{
				`{"type":"permission"}`,
				`{"type":"message"}`,
				"",
			}, "\n")
			if err := onStdout([]byte(stream)); err != nil {
				return adapterprocess.Result{Stdout: []byte(stream)}, err
			}
			return adapterprocess.Result{Stdout: []byte(stream)}, nil
		}),
	})
}

func TestBridgePermissionOutcomeVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		clientResponse string
		wantAllowed    bool
		wantError      string
	}{
		{
			name:           "allow always",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"selected","optionId":"allow-session"}}}`,
			wantAllowed:    true,
		},
		{
			name:           "reject selected",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"selected","optionId":"deny-once"}}}`,
			wantError:      "permission rejected for Run tests",
		},
		{
			name:           "cancelled outcome",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"cancelled"}}}`,
			wantError:      "permission cancelled for Run tests",
		},
		{
			name:           "unknown selected option",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"selected","optionId":"allow-forever"}}}`,
			wantError:      `permission response selected unknown option "allow-forever" for Run tests`,
		},
		{
			name:           "missing selected option",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{"outcome":{"outcome":"selected"}}}`,
			wantError:      "permission response missing selected option for Run tests",
		},
		{
			name:           "missing outcome",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","result":{}}`,
			wantError:      "permission response missing outcome",
		},
		{
			name:           "client rpc error",
			clientResponse: `{"jsonrpc":"2.0","id":"server-1","error":{"code":-32060,"message":"operator unavailable"}}`,
			wantError:      "request permission: operator unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := newPermissionOutcomeTestBridge()
			client := acptest.NewLiveClient(t, server(bridge), acptest.WithLiveResponseHandler(func(client *acptest.LiveClient, response acptest.Response) {
				if response.Method == "session/request_permission" {
					client.Write(json.RawMessage(tt.clientResponse))
				}
			}))
			client.Request("new", "session/new", map[string]any{"cwd": "/tmp/work"}, time.Second)
			responses := client.PromptText("prompt", "session-1", "first", time.Second)
			if tt.wantAllowed {
				if len(responses) != 5 {
					t.Fatalf("got %d responses, want command start + permission + message + command finish + prompt response: %#v", len(responses), responses)
				}
				permission := decodePermissionRequest(t, responses[1])
				if len(permission.Options) != 2 ||
					permission.Options[0].OptionID != "allow-session" ||
					permission.Options[0].Kind != "allow_always" ||
					permission.Options[1].OptionID != "deny-once" ||
					permission.Options[1].Kind != "reject_once" {
					t.Fatalf("permission options = %#v, want allow_always/reject_once", permission.Options)
				}
				message := decodeAgentChunk(t, responses[2])
				if message.Update.Content.Text != "allowed" {
					t.Fatalf("message = %#v, want allowed stream after permission", message)
				}
				finish := decodeCommandToolUpdate(t, responses[3])
				if finish.Update.Status != "completed" {
					t.Fatalf("finish = %#v, want completed command", finish)
				}
				var result struct {
					StopReason string `json:"stopReason"`
				}
				responses[4].ResultInto(t, &result)
				if result.StopReason != "end_turn" {
					t.Fatalf("stop reason = %q, want end_turn", result.StopReason)
				}
				return
			}

			if len(responses) != 4 {
				t.Fatalf("got %d responses, want command start + permission + command finish + prompt error: %#v", len(responses), responses)
			}
			permission := decodePermissionRequest(t, responses[1])
			if permission.ToolCall.ToolCallID != "tool-1" || permission.ToolCall.Title != "Run tests" {
				t.Fatalf("permission = %#v, want Run tests request", permission)
			}
			finish := decodeCommandToolUpdate(t, responses[2])
			if finish.Update.Status != "failed" ||
				!strings.Contains(finish.Update.Content[0].Content.Text, tt.wantError) {
				t.Fatalf("finish = %#v, want failed command containing %q", finish, tt.wantError)
			}
			if responses[3].Error == nil ||
				responses[3].Error.Code != -32000 ||
				responses[3].Error.Message != "prompt command failed" {
				t.Fatalf("prompt response error = %#v, want prompt command failed", responses[3].Error)
			}
			raw, _ := json.Marshal(responses[3].Error.Data)
			if !strings.Contains(fmt.Sprint(responses[3].Error.Data), tt.wantError) {
				t.Fatalf("error data = %s, want %q", raw, tt.wantError)
			}
		})
	}
}

func TestBridgePermissionRequestCancelsWithPrompt(t *testing.T) {
	bridge := newPermissionOutcomeTestBridge()
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		err := server(bridge).Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		serveDone <- err
	}()
	decoder := json.NewDecoder(outputReader)

	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":"prompt","method":"session/prompt","params":{"sessionId":"session-1","prompt":[{"type":"text","text":"first"}]}}`); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	start := decodeCommandToolUpdate(t, decodeResponse(t, decoder))
	if start.Update.Status != "in_progress" {
		t.Fatalf("start = %#v, want running prompt command", start)
	}
	permissionResponse := decodeResponse(t, decoder)
	permission := decodePermissionRequest(t, permissionResponse)
	if permission.ToolCall.ToolCallID != "tool-1" {
		t.Fatalf("permission = %#v, want tool-1 request", permission)
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"prompt"}}`); err != nil {
		t.Fatalf("write cancel request: %v", err)
	}
	cancelNotification := decodeResponse(t, decoder)
	if cancelNotification.Method != "$/cancel_request" || len(cancelNotification.ID) != 0 {
		t.Fatalf("outbound cancellation = %#v, want notification", cancelNotification)
	}
	var cancelParams struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(cancelNotification.Params, &cancelParams); err != nil {
		t.Fatalf("decode outbound cancellation params: %v", err)
	}
	if string(cancelParams.RequestID) != string(permissionResponse.ID) {
		t.Fatalf("outbound cancellation id = %s, want permission id %s", cancelParams.RequestID, permissionResponse.ID)
	}
	finish := decodeCommandToolUpdate(t, decodeResponse(t, decoder))
	if finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "cancelled: context canceled") {
		t.Fatalf("finish = %#v, want cancelled failed command", finish)
	}
	var result struct {
		StopReason string `json:"stopReason"`
	}
	decodeResponse(t, decoder).ResultInto(t, &result)
	if result.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}
}

func TestBridgeUsesStructuredStreamStopReason(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				switch event["type"] {
				case "message":
					text, _ := event["text"].(string)
					return commandbridge.JSONLMapping{
						Events:         []commandbridge.StreamEvent{commandbridge.AgentMessageChunk(text)},
						TranscriptText: text,
					}, nil
				case "done":
					return commandbridge.JSONLMapping{StopReason: runtimeacp.StopReasonMaxTokens}, nil
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
			return adapterprocess.Spec{Command: "agent", Args: []string{text}, Dir: session.CWD}, nil
		},
		Runner: streamingRunnerFunc(func(_ context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			stream := strings.Join([]string{
				`{"type":"message","text":"partial"}`,
				`{"type":"done"}`,
				"",
			}, "\n")
			if err := onStdout([]byte(stream)); err != nil {
				return adapterprocess.Result{}, err
			}
			return adapterprocess.Result{Stdout: []byte(stream)}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "hello"))
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want command start + message + finish + prompt response: %#v", len(responses), responses)
	}
	message := decodeAgentChunk(t, responses[1])
	if message.Update.Content.Text != "partial" {
		t.Fatalf("message = %#v, want parsed partial text", message)
	}
	var promptResult struct {
		StopReason string `json:"stopReason"`
	}
	responses[3].ResultInto(t, &promptResult)
	if promptResult.StopReason != "max_tokens" {
		t.Fatalf("stop reason = %q, want max_tokens", promptResult.StopReason)
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
	runCount := 0
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
		Runner: commandbridge.RunnerFunc(func(_ context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			answers := []string{"answer to first", "answer to second", "answer to third"}
			answer := answers[runCount]
			runCount++
			return adapterprocess.Result{Stdout: []byte(answer)}, nil
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
		!strings.Contains(prompts[1], "Current user request:\n\nsecond") ||
		strings.Count(prompts[1], "second") != 1 {
		t.Fatalf("second prompt = %q, want first exchange prelude", prompts[1])
	}
	if strings.Contains(prompts[2], "User:\nfirst") ||
		!strings.Contains(prompts[2], "User:\nsecond") ||
		!strings.Contains(prompts[2], "Current user request:\n\nthird") ||
		strings.Count(prompts[2], "third") != 1 {
		t.Fatalf("third prompt = %q, want bounded transcript with only second exchange", prompts[2])
	}
}

func TestBridgeCancelStopsActivePrompt(t *testing.T) {
	const secret = "cancelled-runner-sensitive-output"
	started := make(chan struct{})
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(ctx context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			close(started)
			<-ctx.Done()
			return adapterprocess.Result{Stdout: []byte(secret), Stderr: []byte(secret)}, ctx.Err()
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
	if raw, _ := json.Marshal(responses); strings.Contains(string(raw), secret) {
		t.Fatalf("cancelled runner output leaked through notifications: %s", raw)
	}
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

func TestBridgeCancellationDiscardsBufferedStreamingParserTail(t *testing.T) {
	const secret = "cancelled-buffered-stream-tail"
	started := make(chan struct{})
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		NewStreamParser: func(commandbridge.Session, runtimeacp.PromptParams) commandbridge.StreamParser {
			return commandbridge.NewJSONLStreamParser(func(event map[string]any) (commandbridge.JSONLMapping, error) {
				text, _ := event["text"].(string)
				return commandbridge.JSONLMapping{
					Events:         []commandbridge.StreamEvent{commandbridge.AgentMessageChunk(text)},
					TranscriptText: text,
				}, nil
			})
		},
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: streamingRunnerFunc(func(ctx context.Context, _ adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			chunk := []byte(`{"type":"message","text":"` + secret + `"}`)
			if err := onStdout(chunk); err != nil {
				return adapterprocess.Result{}, err
			}
			close(started)
			<-ctx.Done()
			return adapterprocess.Result{Stdout: chunk, Stderr: []byte(secret)}, ctx.Err()
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	promptDone := make(chan []acptest.Response, 1)
	go func() { promptDone <- client.Send(promptRequest(2, "session-1", "stop buffered output")) }()
	<-started
	client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})
	responses := <-promptDone
	if raw, _ := json.Marshal(responses); strings.Contains(string(raw), secret) {
		t.Fatalf("cancelled parser tail leaked through notifications: %s", raw)
	}
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + redacted finish + cancelled result: %#v", len(responses), responses)
	}
	finish := decodeCommandToolUpdate(t, responses[1])
	if finish.Update.Status != "failed" || !strings.Contains(finish.Update.Content[0].Content.Text, "cancelled: context canceled") {
		t.Fatalf("finish = %#v, want redacted cancellation", finish)
	}
	var result runtimeacp.PromptResult
	responses[2].ResultInto(t, &result)
	if result.StopReason != runtimeacp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", result.StopReason)
	}
}

func TestBridgeCloseSessionRemovesSessionState(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	closeResp := client.Request("session/close", map[string]any{"sessionId": "session-1"})
	var closeResult map[string]any
	closeResp.ResultInto(t, &closeResult)

	listResp := client.Request("session/list", map[string]any{})
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	listResp.ResultInto(t, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("sessions after close = %#v, want closed session removed", list.Sessions)
	}

	promptResp := client.Request("session/prompt", map[string]any{
		"sessionId": "session-1",
		"prompt":    []map[string]any{{"type": "text", "text": "after close"}},
	})
	if promptResp.Error == nil || promptResp.Error.Code != -32001 || promptResp.Error.Message != "session not found" {
		t.Fatalf("prompt after close error = %#v, want session not found", promptResp.Error)
	}
}

func TestBridgeDeleteSessionRemovesSessionState(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	deleteResp := client.Request("session/delete", map[string]any{"sessionId": "session-1"})
	var deleteResult map[string]any
	deleteResp.ResultInto(t, &deleteResult)

	listResp := client.Request("session/list", map[string]any{})
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	listResp.ResultInto(t, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("sessions after delete = %#v, want deleted session removed", list.Sessions)
	}

	promptResp := client.Request("session/prompt", map[string]any{
		"sessionId": "session-1",
		"prompt":    []map[string]any{{"type": "text", "text": "after delete"}},
	})
	if promptResp.Error == nil || promptResp.Error.Code != -32001 || promptResp.Error.Message != "session not found" {
		t.Fatalf("prompt after delete error = %#v, want session not found", promptResp.Error)
	}
}

func TestBridgeCloseRequestCancelsActivePromptAndRemovesSession(t *testing.T) {
	started := make(chan struct{})
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(ctx context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			close(started)
			select {
			case <-ctx.Done():
				return adapterprocess.Result{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return adapterprocess.Result{}, errors.New("prompt was not cancelled")
			}
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	promptDone := make(chan []acptest.Response, 1)
	go func() {
		promptDone <- client.Send(promptRequest(2, "session-1", "close soon"))
	}()
	<-started
	closeResp := client.Request("session/close", map[string]any{"sessionId": "session-1"})
	var closeResult map[string]any
	closeResp.ResultInto(t, &closeResult)

	responses := <-promptDone
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + tool finish + prompt response", len(responses))
	}
	finish := decodeCommandToolUpdate(t, responses[1])
	if finish.Update.SessionUpdate != "tool_call_update" ||
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

	listResp := client.Request("session/list", map[string]any{})
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	listResp.ResultInto(t, &list)
	if len(list.Sessions) != 0 {
		t.Fatalf("sessions after close = %#v, want closed session removed", list.Sessions)
	}
}

func TestBridgePassesPromptLifecycleStateToCommandBuilder(t *testing.T) {
	ids := []string{"session-1", "session-2"}
	var nextID int
	var seen []commandbridge.Session
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string {
			id := ids[nextID]
			nextID++
			return id
		},
		IncludeTranscript: true,
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			if _, err := commandbridge.RequirePromptText(params); err != nil {
				return adapterprocess.Spec{}, err
			}
			seen = append(seen, session)
			return adapterprocess.Spec{Command: "agent", Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	client.Send(promptRequest(2, "session-1", "first"))
	client.Send(promptRequest(3, "session-1", "second"))
	client.Request("session/fork", map[string]any{"sessionId": "session-1", "cwd": "/tmp/fork"})
	client.Send(promptRequest(5, "session-2", "forked"))

	if len(seen) != 3 {
		t.Fatalf("seen sessions = %#v, want three prompt builds", seen)
	}
	if seen[0].ID != "session-1" || seen[0].PromptCount != 0 || seen[0].Adopted {
		t.Fatalf("first prompt session = %#v, want fresh session", seen[0])
	}
	if seen[1].ID != "session-1" || seen[1].PromptCount != 1 || seen[1].Adopted {
		t.Fatalf("second prompt session = %#v, want one successful prior prompt", seen[1])
	}
	if seen[2].ID != "session-2" || seen[2].PromptCount != 0 || seen[2].Adopted {
		t.Fatalf("fork prompt session = %#v, want new runtime session despite copied transcript", seen[2])
	}
}

func TestBridgePassesAdoptedStateForLoadedUnknownSession(t *testing.T) {
	const nativeID = "native-session"
	var seen commandbridge.Session
	bridge := commandbridge.New(commandbridge.Spec{
		LoadUnknownSessions: true,
		BuildPrompt: func(session commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			if _, err := commandbridge.RequirePromptText(params); err != nil {
				return adapterprocess.Spec{}, err
			}
			seen = session
			return adapterprocess.Spec{Command: "agent", Dir: session.CWD}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	client.Request("session/load", map[string]any{"sessionId": nativeID, "cwd": "/tmp/reloaded"})
	client.Send(promptRequest(2, nativeID, "loaded"))

	if seen.ID != nativeID || seen.PromptCount != 0 || !seen.Adopted {
		t.Fatalf("loaded prompt session = %#v, want adopted session with no local prompt history", seen)
	}
}

func TestPromptTextIncludesResourceLinkAttachments(t *testing.T) {
	t.Parallel()

	got := commandbridge.PromptText(runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{
		{Type: "text", Text: "Inspect the input."},
		{
			Type:     "resource_link",
			Name:     "screen.png",
			MimeType: "image/png",
			URI:      "file:///tmp/private/screen.png",
		},
	}})
	for _, want := range []string{"Inspect the input.", `Attached file "screen.png" (image/png)`, "file:///tmp/private/screen.png"} {
		if !strings.Contains(got, want) {
			t.Fatalf("PromptText() = %q, want %q", got, want)
		}
	}
}

func TestBridgeDoesNotReplayEphemeralResourceLinkURIInTranscript(t *testing.T) {
	t.Parallel()

	const attachmentURI = "file:///tmp/private-acp-input-0123456789abcdef0123456789abcdef/screen%20shot.png"
	const attachmentPath = "/tmp/private-acp-input-0123456789abcdef0123456789abcdef/screen shot.png"
	const attachmentDir = "/tmp/private-acp-input-0123456789abcdef0123456789abcdef"
	const attachmentDirURI = "file:///tmp/private-acp-input-0123456789abcdef0123456789abcdef"
	const attachmentDirBase = "private-acp-input-0123456789abcdef0123456789abcdef"
	var prompts []string
	var runCount int
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(_ commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent", Dir: "/tmp"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			runCount++
			if runCount == 1 {
				return adapterprocess.Result{Stdout: []byte("Inspected " + attachmentURI + " at " + attachmentPath + " in " + attachmentDir + " via " + attachmentDirURI + " named " + attachmentDirBase)}, nil
			}
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp"})

	client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt": []map[string]any{
				{"type": "text", "text": "Inspect the input."},
				{
					"type":     "resource_link",
					"name":     "screen.png",
					"mimeType": "image/png",
					"uri":      attachmentURI,
				},
			},
		},
	})
	client.Send(promptRequest(3, "session-1", "What did you find?"))

	if len(prompts) != 2 {
		t.Fatalf("prompt count = %d, want 2", len(prompts))
	}
	if !strings.Contains(prompts[0], attachmentURI) {
		t.Fatalf("first prompt = %q, want current-turn attachment URI", prompts[0])
	}
	for _, alias := range []string{attachmentURI, attachmentPath, attachmentDir, attachmentDirURI, attachmentDirBase} {
		if strings.Contains(prompts[1], alias) {
			t.Fatalf("second prompt = %q, must not replay ephemeral attachment alias %q", prompts[1], alias)
		}
	}
	if !strings.Contains(prompts[1], "Inspected [attachment path omitted] at [attachment path omitted] in [attachment path omitted] via [attachment path omitted] named [attachment path omitted]") {
		t.Fatalf("second prompt = %q, want non-sensitive assistant transcript with path aliases removed", prompts[1])
	}
	if !strings.Contains(prompts[1], `Attached file "screen.png" (image/png) was provided for this turn.`) {
		t.Fatalf("second prompt = %q, want retained attachment metadata", prompts[1])
	}
}

func TestBridgeDoesNotReplayWindowsResourceLinkPathAliases(t *testing.T) {
	t.Parallel()

	const attachmentURI = "file:///C:/Private/Stage-0123456789abcdef/screen.png"
	const nativePath = `c:\PRIVATE\stage-0123456789ABCDEF\screen.png`
	const escapedNativePath = `C:\\Private\\Stage-0123456789abcdef\\screen.png`
	const nativeDir = `C:\private\STAGE-0123456789abcdef`
	const nativeDirBase = "stage-0123456789ABCDEF"
	var prompts []string
	var runCount int
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(_ commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent", Dir: "/tmp"}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, _ adapterprocess.Spec) (adapterprocess.Result, error) {
			runCount++
			if runCount == 1 {
				return adapterprocess.Result{Stdout: []byte("Read " + nativePath + " escaped " + escapedNativePath + " in " + nativeDir + " named " + nativeDirBase)}, nil
			}
			return adapterprocess.Result{Stdout: []byte("ok")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp"})
	client.Send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1",
			"prompt": []map[string]any{
				{"type": "text", "text": "Inspect the input."},
				{"type": "resource_link", "name": "screen.png", "mimeType": "image/png", "uri": attachmentURI},
			},
		},
	})
	client.Send(promptRequest(3, "session-1", "What did you find?"))

	if len(prompts) != 2 {
		t.Fatalf("prompt count = %d, want 2", len(prompts))
	}
	for _, alias := range []string{nativePath, escapedNativePath, nativeDir, nativeDirBase} {
		if strings.Contains(strings.ToLower(prompts[1]), strings.ToLower(alias)) {
			t.Fatalf("second prompt = %q, must not replay Windows attachment alias %q", prompts[1], alias)
		}
	}
	if !strings.Contains(prompts[1], "Read [attachment path omitted] escaped [attachment path omitted] in [attachment path omitted] named [attachment path omitted]") {
		t.Fatalf("second prompt = %q, want Windows path aliases removed", prompts[1])
	}
}

func TestBridgeCancelStopsStreamingPromptWithoutRecordingTranscript(t *testing.T) {
	started := make(chan struct{})
	var prompts []string
	var runCount int
	bridge := commandbridge.New(commandbridge.Spec{
		NewID:             func() string { return "session-1" },
		IncludeTranscript: true,
		BuildPrompt: func(_ commandbridge.Session, params runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			text, err := commandbridge.RequirePromptText(params)
			if err != nil {
				return adapterprocess.Spec{}, err
			}
			prompts = append(prompts, text)
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: streamingRunnerFunc(func(ctx context.Context, _ adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			runCount++
			if runCount > 1 {
				if err := onStdout([]byte("second answer")); err != nil {
					return adapterprocess.Result{}, err
				}
				return adapterprocess.Result{Stdout: []byte("second answer")}, nil
			}
			if err := onStdout([]byte("partial answer")); err != nil {
				return adapterprocess.Result{}, err
			}
			close(started)
			<-ctx.Done()
			return adapterprocess.Result{Stdout: []byte("partial answer\nlate process output")}, ctx.Err()
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	promptDone := make(chan []acptest.Response, 1)
	go func() {
		promptDone <- client.Send(promptRequest(2, "session-1", "stop soon"))
	}()
	<-started
	client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})

	responses := <-promptDone
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want tool start + streamed chunk + tool finish + prompt response", len(responses))
	}
	chunk := decodeAgentChunk(t, responses[1])
	if chunk.Update.Content.Text != "partial answer" {
		t.Fatalf("chunk = %#v, want partial answer", chunk)
	}
	finish := decodeCommandToolUpdate(t, responses[2])
	if finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "cancelled: context canceled") {
		t.Fatalf("tool finish = %#v, want failed cancelled command", finish)
	}
	var cancelled struct {
		StopReason string `json:"stopReason"`
	}
	responses[3].ResultInto(t, &cancelled)
	if cancelled.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q, want cancelled", cancelled.StopReason)
	}

	followup := client.Send(promptRequest(3, "session-1", "after cancel"))
	if len(followup) != 5 {
		t.Fatalf("followup responses = %#v, want normal prompt lifecycle", followup)
	}
	var completed struct {
		StopReason string `json:"stopReason"`
	}
	followup[4].ResultInto(t, &completed)
	if completed.StopReason != "end_turn" {
		t.Fatalf("followup stop reason = %q, want end_turn", completed.StopReason)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %#v, want cancelled prompt plus followup", prompts)
	}
	if prompts[1] != "after cancel" {
		t.Fatalf("followup prompt = %q, want cancelled exchange omitted from transcript", prompts[1])
	}
}

func TestBridgeDropsStreamChunksAfterCancel(t *testing.T) {
	started := make(chan struct{})
	lateErr := make(chan error, 1)
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		Runner: streamingRunnerFunc(func(ctx context.Context, _ adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
			if err := onStdout([]byte("before cancel")); err != nil {
				return adapterprocess.Result{}, err
			}
			close(started)
			<-ctx.Done()
			lateErr <- onStdout([]byte("late after cancel"))
			return adapterprocess.Result{Stdout: []byte("before cancel\nlate after cancel")}, ctx.Err()
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	promptDone := make(chan []acptest.Response, 1)
	go func() {
		promptDone <- client.Send(promptRequest(2, "session-1", "stop soon"))
	}()
	<-started
	client.Notify("session/cancel", map[string]any{"sessionId": "session-1"})

	responses := <-promptDone
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want tool start + one chunk + tool finish + prompt response: %#v", len(responses), responses)
	}
	chunk := decodeAgentChunk(t, responses[1])
	if chunk.Update.Content.Text != "before cancel" {
		t.Fatalf("chunk = %#v, want only pre-cancel output", chunk)
	}
	if err := <-lateErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("late onStdout error = %v, want context canceled", err)
	}
	finish := decodeCommandToolUpdate(t, responses[2])
	if finish.Update.Status != "failed" ||
		!strings.Contains(finish.Update.Content[0].Content.Text, "cancelled: context canceled") ||
		strings.Contains(finish.Update.Content[0].Content.Text, "late after cancel") {
		t.Fatalf("tool finish = %#v, want cancelled command without late output", finish)
	}
	var cancelled struct {
		StopReason string `json:"stopReason"`
	}
	responses[3].ResultInto(t, &cancelled)
	if cancelled.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q, want cancelled", cancelled.StopReason)
	}
}

func TestBridgePromptCommandErrorMapsToRPCError(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		ClassifyPromptFailure: func(commandbridge.Session, adapterprocess.Spec, adapterprocess.Result, error) commandbridge.PromptFailureKind {
			return commandbridge.PromptFailureUnknown
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("boom")}, &adapterprocess.ExitError{Command: "agent", Code: 2}
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
	if bytes.Contains(raw, []byte("errorKind")) {
		t.Fatalf("error data = %s, did not expect a discriminator for unknown failure", raw)
	}
}

func TestBridgePromptCommandAuthFailureMapsToAuthRequired(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		AuthRequired: func(result adapterprocess.Result, err error) bool {
			return err != nil && strings.Contains(string(result.Stderr), "not signed in")
		},
		ClassifyPromptFailure: func(commandbridge.Session, adapterprocess.Spec, adapterprocess.Result, error) commandbridge.PromptFailureKind {
			t.Fatal("ClassifyPromptFailure called after AuthRequired matched")
			return commandbridge.PromptFailureNativeSessionMissing
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("agent is not signed in")}, &adapterprocess.ExitError{Command: "agent", Code: 1}
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "hello"))
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + tool finish + prompt error", len(responses))
	}
	resp := responses[2]
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "Authentication required" {
		t.Fatalf("response error = %#v, want auth required", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte("agent is not signed in")) {
		t.Fatalf("error data = %s, want stderr", raw)
	}
	if bytes.Contains(raw, []byte("errorKind")) {
		t.Fatalf("error data = %s, auth failure must not be classified as recoverable", raw)
	}
}

func TestBridgePromptFailureClassificationAddsWireDiscriminator(t *testing.T) {
	runs := 0
	var built []commandbridge.Session
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(session commandbridge.Session, _ runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			built = append(built, session)
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		ClassifyPromptFailure: func(session commandbridge.Session, command adapterprocess.Spec, result adapterprocess.Result, err error) commandbridge.PromptFailureKind {
			if session.ID == "session-1" && command.Command == "agent" && err != nil && strings.Contains(string(result.Stderr), "native history missing") {
				return commandbridge.PromptFailureNativeSessionMissing
			}
			return commandbridge.PromptFailureUnknown
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			runs++
			if runs > 1 {
				return adapterprocess.Result{Stdout: []byte("recovered")}, nil
			}
			return adapterprocess.Result{Stderr: []byte("native history missing")}, &adapterprocess.ExitError{Command: "agent", Code: 1}
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "hello"))
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want tool start + tool finish + prompt error", len(responses))
	}
	resp := responses[2]
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "prompt command failed" {
		t.Fatalf("response error = %#v, want classified prompt command failure", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte(`"errorKind":"native_session_missing"`)) || !bytes.Contains(raw, []byte("native history missing")) {
		t.Fatalf("error data = %s, want error kind and provider stderr", raw)
	}
	if runs != 1 {
		t.Fatalf("runner calls after classified failure = %d, want exactly one", runs)
	}
	listed := client.Request("session/list", map[string]any{})
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	listed.ResultInto(t, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "session-1" {
		t.Fatalf("sessions after classified failure = %#v, want original session", list.Sessions)
	}
	second := client.Send(promptRequest(3, "session-1", "retry under host control"))
	if len(second) != 4 || second[len(second)-1].Error != nil {
		t.Fatalf("second prompt responses = %#v, want original session to remain usable", second)
	}
	if len(built) != 2 || built[1].PromptCount != 0 {
		t.Fatalf("prompt build state = %#v, want failed prompt not counted", built)
	}
}

func TestBridgePromptFailureClassificationRequiresCommandExit(t *testing.T) {
	classifierCalled := false
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		ClassifyPromptFailure: func(commandbridge.Session, adapterprocess.Spec, adapterprocess.Result, error) commandbridge.PromptFailureKind {
			classifierCalled = true
			return commandbridge.PromptFailureNativeSessionMissing
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("native history missing")}, errors.New("stream parser failed")
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "hello"))
	if len(responses) != 3 || responses[len(responses)-1].Error == nil {
		t.Fatalf("responses = %#v, want tool start + tool finish + prompt error", responses)
	}
	if classifierCalled {
		t.Fatal("ClassifyPromptFailure called for a non-exit failure")
	}
	resp := responses[len(responses)-1]
	raw, _ := json.Marshal(resp.Error.Data)
	if bytes.Contains(raw, []byte("errorKind")) {
		t.Fatalf("error data = %s, non-exit failure must not be classified", raw)
	}
}

func TestBridgePromptFailureClassificationRequiresNonzeroCommandExit(t *testing.T) {
	classifierCalled := false
	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-1" },
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent"}, nil
		},
		ClassifyPromptFailure: func(commandbridge.Session, adapterprocess.Spec, adapterprocess.Result, error) commandbridge.PromptFailureKind {
			classifierCalled = true
			return commandbridge.PromptFailureNativeSessionMissing
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("native history missing")}, &adapterprocess.ExitError{Command: "agent", Code: 0}
		}),
	})
	client := acptest.NewClient(t, server(bridge))
	client.Request("session/new", map[string]any{"cwd": "/tmp/work"})

	responses := client.Send(promptRequest(2, "session-1", "hello"))
	if len(responses) != 3 || responses[len(responses)-1].Error == nil {
		t.Fatalf("responses = %#v, want tool start + tool finish + prompt error", responses)
	}
	if classifierCalled {
		t.Fatal("ClassifyPromptFailure called for a zero exit code")
	}
	raw, _ := json.Marshal(responses[len(responses)-1].Error.Data)
	if bytes.Contains(raw, []byte("errorKind")) {
		t.Fatalf("error data = %s, zero exit code must not be classified", raw)
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

func TestBridgeAdvertisesAndRunsAuthenticateCommand(t *testing.T) {
	var saw adapterprocess.Spec
	var sawMethodID string
	bridge := commandbridge.New(commandbridge.Spec{
		AuthMethods: []acp.AuthMethod{{
			ID:          "agent-login",
			Name:        "Agent login",
			Description: "Sign in using the agent CLI.",
		}},
		BuildAuthenticate: func(methodID string) (adapterprocess.Spec, error) {
			sawMethodID = methodID
			return adapterprocess.Spec{Command: "agent", Args: []string{"login"}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(_ context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
			saw = spec
			return adapterprocess.Result{Stdout: []byte("logged in\n")}, nil
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	initialize := client.Request("initialize", map[string]any{})
	var initResult struct {
		AuthMethods []acp.AuthMethod `json:"authMethods"`
	}
	initialize.ResultInto(t, &initResult)
	if len(initResult.AuthMethods) != 1 || initResult.AuthMethods[0].ID != "agent-login" {
		t.Fatalf("authMethods = %#v, want agent-login", initResult.AuthMethods)
	}

	resp := client.Request("authenticate", map[string]any{"methodId": "agent-login"})
	var result map[string]any
	resp.ResultInto(t, &result)
	if len(result) != 0 {
		t.Fatalf("authenticate result = %#v, want empty object", result)
	}
	if sawMethodID != "agent-login" {
		t.Fatalf("methodID = %q, want agent-login", sawMethodID)
	}
	if saw.Command != "agent" || strings.Join(saw.Args, " ") != "login" {
		t.Fatalf("authenticate command = %#v, want agent login", saw)
	}
}

func TestBridgeAuthenticateRejectsUnknownMethod(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		AuthMethods: []acp.AuthMethod{{ID: "agent-login"}},
		BuildAuthenticate: func(string) (adapterprocess.Spec, error) {
			t.Fatal("BuildAuthenticate should not be called for unknown method")
			return adapterprocess.Spec{}, nil
		},
	})
	client := acptest.NewClient(t, server(bridge))

	resp := client.Request("authenticate", map[string]any{"methodId": "browser-login"})
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "unknown auth method" {
		t.Fatalf("response error = %#v, want unknown auth method", resp.Error)
	}
}

func TestBridgeAuthenticateCommandErrorMapsToRPCError(t *testing.T) {
	bridge := commandbridge.New(commandbridge.Spec{
		AuthMethods: []acp.AuthMethod{{ID: "agent-login"}},
		BuildAuthenticate: func(string) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent", Args: []string{"login"}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{Stderr: []byte("login refused")}, errors.New("exit 1")
		}),
	})
	client := acptest.NewClient(t, server(bridge))

	resp := client.Request("authenticate", map[string]any{"methodId": "agent-login"})
	if resp.Error == nil || resp.Error.Code != -32000 || resp.Error.Message != "authenticate command failed" {
		t.Fatalf("response error = %#v, want authenticate command failure", resp.Error)
	}
	raw, _ := json.Marshal(resp.Error.Data)
	if !bytes.Contains(raw, []byte("login refused")) {
		t.Fatalf("error data = %s, want stderr", raw)
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

func decodeResponse(t testing.TB, decoder *json.Decoder) acptest.Response {
	t.Helper()
	var response acptest.Response
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
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

type permissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string         `json:"toolCallId"`
		Title      string         `json:"title"`
		Kind       string         `json:"kind"`
		Status     string         `json:"status"`
		RawInput   map[string]any `json:"rawInput"`
	} `json:"toolCall"`
	Options []struct {
		OptionID string `json:"optionId"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

func decodePermissionRequest(t testing.TB, response acptest.Response) permissionRequest {
	t.Helper()
	if response.Method != "session/request_permission" {
		t.Fatalf("response method = %q, want session/request_permission", response.Method)
	}
	var req permissionRequest
	response.ParamsInto(t, &req)
	return req
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
