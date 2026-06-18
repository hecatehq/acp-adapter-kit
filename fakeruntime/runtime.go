package fakeruntime

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hecatehq/acp-adapter-kit/acp"
)

type Runtime struct {
	mu       sync.Mutex
	nextID   int
	sessions map[string]*Session
}

type Session struct {
	ID        string
	CWD       string
	Cancelled bool
}

func New() *Runtime {
	return &Runtime{sessions: map[string]*Session{}}
}

func (r *Runtime) Options() []acp.Option {
	return []acp.Option{
		acp.WithMethod("session/new", r.newSession),
		acp.WithMethod("session/prompt", r.prompt),
		acp.WithMethod("session/cancel", r.cancelMethod),
		acp.WithMethod("session/close", r.closeSession),
		acp.WithNotification("session/cancel", r.cancelNotification),
	}
}

func (r *Runtime) newSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(err)
	}
	if req.CWD == "" {
		req.CWD = "."
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	id := fmt.Sprintf("fake-session-%d", r.nextID)
	r.sessions[id] = &Session{ID: id, CWD: req.CWD}
	return map[string]any{"sessionId": id, "cwd": req.CWD}, nil
}

func (r *Runtime) prompt(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(err)
	}
	if rpcErr := r.requireSession(req.SessionID); rpcErr != nil {
		return nil, rpcErr
	}

	promptText := ""
	if len(req.Prompt) > 0 {
		promptText = req.Prompt[0].Text
	}
	if r.consumeCancelled(req.SessionID) {
		if err := ctx.Notify("session/update", update(req.SessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       textContent("Fake runtime cancelled the prompt."),
		})); err != nil {
			return nil, notifyError(err)
		}
		return map[string]any{"stopReason": "cancelled"}, nil
	}

	toolID := "fake-tool-1"
	notifications := []map[string]any{
		update(req.SessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       textContent("Fake response: " + promptText),
		}),
		update(req.SessionID, map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    toolID,
			"title":         "Fake tool",
			"kind":          "think",
			"status":        "pending",
		}),
		update(req.SessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    toolID,
			"status":        "completed",
			"content": []map[string]any{{
				"type":    "content",
				"content": textContent("Fake tool completed."),
			}},
		}),
	}
	for _, notification := range notifications {
		if err := ctx.Notify("session/update", notification); err != nil {
			return nil, notifyError(err)
		}
	}
	return map[string]any{"stopReason": "end_turn"}, nil
}

func (r *Runtime) cancelMethod(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	sessionID, rpcErr := r.markCancelled(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if err := ctx.Notify("session/update", update(sessionID, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       textContent("Fake runtime received cancellation."),
	})); err != nil {
		return nil, notifyError(err)
	}
	return map[string]any{"cancelled": true}, nil
}

func (r *Runtime) cancelNotification(params json.RawMessage) error {
	_, rpcErr := r.markCancelled(params)
	if rpcErr != nil {
		return fmt.Errorf("%s", rpcErr.Message)
	}
	return nil
}

func (r *Runtime) closeSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[req.SessionID]; !ok {
		return nil, sessionNotFound(req.SessionID)
	}
	delete(r.sessions, req.SessionID)
	return map[string]any{"closed": true}, nil
}

func (r *Runtime) markCancelled(params json.RawMessage) (string, *acp.RPCError) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return "", invalidParams(err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[req.SessionID]
	if !ok {
		return "", sessionNotFound(req.SessionID)
	}
	session.Cancelled = true
	return req.SessionID, nil
}

func (r *Runtime) requireSession(sessionID string) *acp.RPCError {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[sessionID]; !ok {
		return sessionNotFound(sessionID)
	}
	return nil
}

func (r *Runtime) consumeCancelled(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[sessionID]
	if !ok || !session.Cancelled {
		return false
	}
	session.Cancelled = false
	return true
}

func update(sessionID string, update map[string]any) map[string]any {
	return map[string]any{"sessionId": sessionID, "update": update}
}

func textContent(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func invalidParams(err error) *acp.RPCError {
	return &acp.RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
}

func sessionNotFound(sessionID string) *acp.RPCError {
	return &acp.RPCError{Code: -32001, Message: "session not found", Data: sessionID}
}

func notifyError(err error) *acp.RPCError {
	return &acp.RPCError{Code: -32000, Message: "notification failed", Data: err.Error()}
}
