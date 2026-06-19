package commandbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hecatehq/acp-adapter-kit/acp"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

type Runner interface {
	Run(context.Context, adapterprocess.Spec) (adapterprocess.Result, error)
}

type StreamRunner interface {
	RunStream(context.Context, adapterprocess.Spec, func([]byte) error) (adapterprocess.Result, error)
}

type RunnerFunc func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error)

func (f RunnerFunc) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	return f(ctx, spec)
}

type ProcessRunner struct{}

func (ProcessRunner) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	return adapterprocess.Run(ctx, spec)
}

func (ProcessRunner) RunStream(ctx context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
	return adapterprocess.RunStream(ctx, spec, onStdout)
}

type PromptCommandBuilder func(Session, runtimeacp.PromptParams) (adapterprocess.Spec, error)

type Spec struct {
	Runner      Runner
	NewID       func() string
	Options     []SelectConfigOption
	BuildPrompt PromptCommandBuilder
}

type Bridge struct {
	spec Spec

	nextID atomic.Int64

	mu       sync.Mutex
	sessions map[string]*sessionState
	active   map[string]context.CancelFunc
}

type Session struct {
	ID                    string
	CWD                   string
	AdditionalDirectories []string
	MCPServers            []runtimeacp.MCPServer
	Config                map[string]string
	ModeID                string
}

type SelectConfigOption struct {
	ID           string
	Name         string
	Description  string
	Category     string
	DefaultValue string
	Options      []SelectValue
}

type SelectValue struct {
	Value       string
	Name        string
	Description string
}

type sessionState struct {
	Session
}

func New(spec Spec) *Bridge {
	if spec.Runner == nil {
		spec.Runner = ProcessRunner{}
	}
	return &Bridge{
		spec:     spec,
		sessions: map[string]*sessionState{},
		active:   map[string]context.CancelFunc{},
	}
}

func (b *Bridge) Options() []acp.Option {
	if b == nil {
		return nil
	}
	return []acp.Option{
		acp.WithMethod("session/new", b.newSession),
		acp.WithMethod("session/list", b.listSessions),
		acp.WithMethod("session/set_config_option", b.setConfigOption),
		acp.WithMethod("session/set_mode", b.setMode),
		acp.WithMethod("session/prompt", b.prompt),
		acp.WithConcurrentMethod("session/cancel", b.cancelMethod),
		acp.WithMethod("session/close", b.closeSession),
		acp.WithMethod("session/delete", b.deleteSession),
		acp.WithNotification("session/cancel", b.cancelNotification),
	}
}

func (b *Bridge) newSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.NewSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	id := b.newID()
	state := &sessionState{Session: Session{
		ID:                    id,
		CWD:                   strings.TrimSpace(req.CWD),
		AdditionalDirectories: append([]string(nil), req.AdditionalDirectories...),
		MCPServers:            append([]runtimeacp.MCPServer(nil), req.MCPServers...),
		Config:                defaultConfig(b.spec.Options),
	}}
	b.mu.Lock()
	b.sessions[id] = state
	b.mu.Unlock()
	return map[string]any{
		"sessionId":     id,
		"configOptions": b.configOptions(state),
	}, nil
}

func (b *Bridge) listSessions(_ *acp.MethodContext, _ json.RawMessage) (any, *acp.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sessions := make([]map[string]any, 0, len(b.sessions))
	for _, state := range b.sessions {
		sessions = append(sessions, map[string]any{
			"sessionId": state.ID,
			"cwd":       state.CWD,
		})
	}
	return map[string]any{"sessions": sessions}, nil
}

func (b *Bridge) setConfigOption(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	req, rpcErr := decodeSetConfigOption(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[req.SessionID]
	if state == nil {
		return nil, notFound("session not found", req.SessionID)
	}
	option, ok := b.selectOption(req.ConfigID)
	if !ok {
		return nil, invalidParams("unknown config option", req.ConfigID)
	}
	if !selectOptionAllows(option, req.Value) {
		return nil, invalidParams("unsupported config value", req.Value)
	}
	state.Config[req.ConfigID] = req.Value
	return map[string]any{"configOptions": b.configOptions(state)}, nil
}

func (b *Bridge) setMode(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.SetModeParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[req.SessionID]
	if state == nil {
		return nil, notFound("session not found", req.SessionID)
	}
	state.ModeID = req.ModeID
	return map[string]any{}, nil
}

func (b *Bridge) prompt(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.PromptParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	if b.spec.BuildPrompt == nil {
		return nil, &acp.RPCError{Code: -32004, Message: "not implemented", Data: "prompt command builder is not configured"}
	}
	state, rpcErr := b.session(req.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	runCtx, cancel := context.WithCancel(methodContext(ctx))
	if rpcErr := b.beginPrompt(req.SessionID, cancel); rpcErr != nil {
		cancel()
		return nil, rpcErr
	}
	defer b.endPrompt(req.SessionID)
	defer cancel()

	command, err := b.spec.BuildPrompt(state.Session, req)
	if err != nil {
		return nil, invalidParams("build prompt command", err.Error())
	}
	result, err := b.runPromptCommand(runCtx, ctx, req.SessionID, command)
	if runCtx.Err() != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "prompt command failed", Data: commandErrorData(result, err)}
	}
	return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonEndTurn}, nil
}

func (b *Bridge) runPromptCommand(runCtx context.Context, methodCtx *acp.MethodContext, sessionID string, command adapterprocess.Spec) (adapterprocess.Result, error) {
	if runner, ok := b.spec.Runner.(StreamRunner); ok {
		return runner.RunStream(runCtx, command, func(chunk []byte) error {
			return notifyAgentMessageChunk(methodCtx, sessionID, string(chunk))
		})
	}
	result, err := b.spec.Runner.Run(runCtx, command)
	text := strings.TrimSpace(string(result.Stdout))
	if text != "" {
		if notifyErr := notifyAgentMessageChunk(methodCtx, sessionID, text); notifyErr != nil && err == nil {
			err = fmt.Errorf("notification failed: %w", notifyErr)
		}
	}
	return result, err
}

func notifyAgentMessageChunk(ctx *acp.MethodContext, sessionID string, text string) error {
	if text == "" {
		return nil
	}
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{
				"type": "text",
				"text": text,
			},
		},
	})
}

func (b *Bridge) cancelMethod(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CancelParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"cancelled": b.cancel(req.SessionID)}, nil
}

func (b *Bridge) cancelNotification(params json.RawMessage) error {
	var req runtimeacp.CancelParams
	if err := json.Unmarshal(params, &req); err != nil {
		return fmt.Errorf("invalid session/cancel params: %w", err)
	}
	b.cancel(req.SessionID)
	return nil
}

func (b *Bridge) closeSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CloseSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.cancel(req.SessionID)
	return map[string]any{}, nil
}

func (b *Bridge) deleteSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.DeleteSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.cancel(req.SessionID)
	b.mu.Lock()
	delete(b.sessions, req.SessionID)
	b.mu.Unlock()
	return map[string]any{}, nil
}

func (b *Bridge) session(id string) (*sessionState, *acp.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[id]
	if state == nil {
		return nil, notFound("session not found", id)
	}
	clone := *state
	clone.Config = cloneStringMap(state.Config)
	clone.AdditionalDirectories = append([]string(nil), state.AdditionalDirectories...)
	clone.MCPServers = append([]runtimeacp.MCPServer(nil), state.MCPServers...)
	return &clone, nil
}

func (b *Bridge) beginPrompt(sessionID string, cancel context.CancelFunc) *acp.RPCError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active[sessionID] != nil {
		return &acp.RPCError{Code: -32009, Message: "session busy", Data: sessionID}
	}
	b.active[sessionID] = cancel
	return nil
}

func (b *Bridge) endPrompt(sessionID string) {
	b.mu.Lock()
	delete(b.active, sessionID)
	b.mu.Unlock()
}

func (b *Bridge) cancel(sessionID string) bool {
	b.mu.Lock()
	cancel := b.active[sessionID]
	b.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (b *Bridge) newID() string {
	if b.spec.NewID != nil {
		return b.spec.NewID()
	}
	return fmt.Sprintf("session-%d", b.nextID.Add(1))
}

func (b *Bridge) selectOption(id string) (SelectConfigOption, bool) {
	for _, option := range b.spec.Options {
		if option.ID == id {
			return option, true
		}
	}
	return SelectConfigOption{}, false
}

func (b *Bridge) configOptions(state *sessionState) []map[string]any {
	if len(b.spec.Options) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(b.spec.Options))
	for _, option := range b.spec.Options {
		current := state.Config[option.ID]
		if current == "" {
			current = option.DefaultValue
		}
		values := make([]map[string]any, 0, len(option.Options))
		for _, value := range option.Options {
			item := map[string]any{
				"value": value.Value,
				"name":  value.Name,
			}
			if value.Description != "" {
				item["description"] = value.Description
			}
			values = append(values, item)
		}
		item := map[string]any{
			"type":         "select",
			"id":           option.ID,
			"name":         option.Name,
			"currentValue": current,
			"options":      values,
		}
		if option.Description != "" {
			item["description"] = option.Description
		}
		if option.Category != "" {
			item["category"] = option.Category
		}
		out = append(out, item)
	}
	return out
}

func defaultConfig(options []SelectConfigOption) map[string]string {
	out := make(map[string]string, len(options))
	for _, option := range options {
		if option.ID != "" && option.DefaultValue != "" {
			out[option.ID] = option.DefaultValue
		}
	}
	return out
}

func selectOptionAllows(option SelectConfigOption, value string) bool {
	for _, candidate := range option.Options {
		if candidate.Value == value {
			return true
		}
	}
	return false
}

type setConfigRequest struct {
	SessionID string
	ConfigID  string
	Value     string
}

func decodeSetConfigOption(params json.RawMessage) (setConfigRequest, *acp.RPCError) {
	var req struct {
		SessionID string          `json:"sessionId"`
		ConfigID  string          `json:"configId"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return setConfigRequest{}, invalidParams("invalid params", err.Error())
	}
	if req.SessionID == "" {
		return setConfigRequest{}, invalidParams("session id is required", nil)
	}
	if req.ConfigID == "" {
		return setConfigRequest{}, invalidParams("config id is required", nil)
	}
	var value string
	if err := json.Unmarshal(req.Value, &value); err != nil {
		return setConfigRequest{}, invalidParams("value must be a string", err.Error())
	}
	if value == "" {
		return setConfigRequest{}, invalidParams("value is required", nil)
	}
	return setConfigRequest{SessionID: req.SessionID, ConfigID: req.ConfigID, Value: value}, nil
}

func decodeParams(params json.RawMessage, target any) *acp.RPCError {
	if err := json.Unmarshal(params, target); err != nil {
		return invalidParams("invalid params", err.Error())
	}
	return nil
}

func methodContext(ctx *acp.MethodContext) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx.Context()
}

func notFound(message string, data any) *acp.RPCError {
	return &acp.RPCError{Code: -32001, Message: message, Data: data}
}

func invalidParams(message string, data any) *acp.RPCError {
	return &acp.RPCError{Code: -32602, Message: message, Data: data}
}

func commandErrorData(result adapterprocess.Result, err error) map[string]any {
	data := map[string]any{
		"error": err.Error(),
	}
	stderr := strings.TrimSpace(string(result.Stderr))
	if stderr != "" {
		data["stderr"] = stderr
	}
	if result.StderrTruncated {
		data["stderr_truncated"] = true
	}
	return data
}

func PromptText(params runtimeacp.PromptParams) string {
	var parts []string
	for _, block := range params.Prompt {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		case "resource":
			if block.Resource != nil {
				if text := strings.TrimSpace(block.Resource.Text); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func RequirePromptText(params runtimeacp.PromptParams) (string, error) {
	text := PromptText(params)
	if text == "" {
		return "", errors.New("prompt text is required")
	}
	return text, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
