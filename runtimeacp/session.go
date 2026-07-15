package runtimeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type Notifier interface {
	Notify(ctx context.Context, method string, params any) error
}

type NewSessionParams struct {
	CWD                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers,omitempty"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

type NewSessionResult struct {
	SessionID     string            `json:"sessionId"`
	ConfigOptions []json.RawMessage `json:"configOptions,omitempty"`
	Modes         json.RawMessage   `json:"modes,omitempty"`

	raw json.RawMessage
}

type ForkSessionParams struct {
	SessionID             string      `json:"sessionId"`
	CWD                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers,omitempty"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

type ForkSessionResult struct {
	SessionID     string            `json:"sessionId"`
	ConfigOptions []json.RawMessage `json:"configOptions,omitempty"`
	Modes         json.RawMessage   `json:"modes,omitempty"`

	raw json.RawMessage
}

type LoadSessionParams struct {
	SessionID             string      `json:"sessionId"`
	CWD                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers,omitempty"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

type ResumeSessionParams struct {
	SessionID             string      `json:"sessionId"`
	CWD                   string      `json:"cwd"`
	MCPServers            []MCPServer `json:"mcpServers,omitempty"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

type ListSessionsParams struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListSessionsResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`

	raw json.RawMessage
}

type SessionInfo struct {
	SessionID             string          `json:"sessionId"`
	CWD                   string          `json:"cwd"`
	AdditionalDirectories []string        `json:"additionalDirectories,omitempty"`
	Title                 string          `json:"title,omitempty"`
	UpdatedAt             string          `json:"updatedAt,omitempty"`
	Meta                  json.RawMessage `json:"_meta,omitempty"`
}

type MCPServer struct {
	Meta    map[string]any `json:"_meta,omitempty"`
	Type    string         `json:"type,omitempty"`
	ID      string         `json:"id,omitempty"`
	Name    string         `json:"name"`
	Command string         `json:"command,omitempty"`
	Args    []string       `json:"args,omitempty"`
	Env     []EnvVariable  `json:"env,omitempty"`
	URL     string         `json:"url,omitempty"`
	Headers []HTTPHeader   `json:"headers,omitempty"`
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type ContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	MimeType     string            `json:"mimeType,omitempty"`
	Data         string            `json:"data,omitempty"`
	URI          string            `json:"uri,omitempty"`
	Name         string            `json:"name,omitempty"`
	Resource     *EmbeddedResource `json:"resource,omitempty"`
	Title        string            `json:"title,omitempty"`
	Description  string            `json:"description,omitempty"`
	Size         *int64            `json:"size,omitempty"`
	PreparedFile *PreparedFile     `json:"-"`
}

// PreparedFile identifies a private, prompt-scoped local copy of binary or
// linked ACP content. Command bridges populate this field before invoking a
// prompt builder. The path is never serialized onto the ACP wire.
type PreparedFile struct {
	Path        string
	SizeBytes   int64
	OriginalURI string
}

type EmbeddedResource struct {
	URI      string               `json:"uri"`
	Text     string               `json:"text,omitempty"`
	Blob     string               `json:"blob,omitempty"`
	MimeType string               `json:"mimeType,omitempty"`
	Kind     EmbeddedResourceKind `json:"-"`
}

// EmbeddedResourceKind preserves which required ACP embedded-resource union
// key was present, including when the text or blob value is empty.
type EmbeddedResourceKind string

const (
	EmbeddedResourceText EmbeddedResourceKind = "text"
	EmbeddedResourceBlob EmbeddedResourceKind = "blob"
)

// ContentKind resolves the embedded-resource union variant. Kind is required
// only when the selected text/blob value is empty; non-empty legacy literals
// continue to infer the variant.
func (r EmbeddedResource) ContentKind() (EmbeddedResourceKind, error) {
	if r.Text != "" && r.Blob != "" {
		return "", errors.New("embedded resource cannot contain text and blob")
	}
	switch r.Kind {
	case EmbeddedResourceText:
		if r.Blob != "" {
			return "", errors.New("text embedded resource cannot contain blob data")
		}
		return EmbeddedResourceText, nil
	case EmbeddedResourceBlob:
		if r.Text != "" {
			return "", errors.New("blob embedded resource cannot contain text data")
		}
		return EmbeddedResourceBlob, nil
	case "":
		switch {
		case r.Text != "":
			return EmbeddedResourceText, nil
		case r.Blob != "":
			return EmbeddedResourceBlob, nil
		default:
			return "", errors.New("embedded resource must select text or blob content")
		}
	default:
		return "", fmt.Errorf("unknown embedded resource kind %q", r.Kind)
	}
}

func (r *EmbeddedResource) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("cannot unmarshal embedded resource into nil receiver")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, hasURI := fields["uri"]
	_, hasText := fields["text"]
	_, hasBlob := fields["blob"]
	if !hasURI {
		return errors.New("embedded resource URI is required")
	}
	var uri *string
	if err := json.Unmarshal(fields["uri"], &uri); err != nil || uri == nil {
		return errors.New("embedded resource URI must be a string")
	}
	if hasText == hasBlob {
		return errors.New("embedded resource must contain exactly one of text or blob")
	}
	selectedKey := "text"
	if hasBlob {
		selectedKey = "blob"
	}
	var selectedValue *string
	if err := json.Unmarshal(fields[selectedKey], &selectedValue); err != nil || selectedValue == nil {
		return fmt.Errorf("embedded resource %s must be a string", selectedKey)
	}
	type wireResource struct {
		URI      string `json:"uri"`
		Text     string `json:"text"`
		Blob     string `json:"blob"`
		MimeType string `json:"mimeType,omitempty"`
	}
	var wire wireResource
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = EmbeddedResource{
		URI:      *uri,
		Text:     wire.Text,
		Blob:     wire.Blob,
		MimeType: wire.MimeType,
	}
	if hasText {
		r.Text = *selectedValue
		r.Kind = EmbeddedResourceText
	} else {
		r.Blob = *selectedValue
		r.Kind = EmbeddedResourceBlob
	}
	return nil
}

func (r EmbeddedResource) MarshalJSON() ([]byte, error) {
	kind, err := r.ContentKind()
	if err != nil {
		return nil, err
	}
	type wireResource struct {
		URI      string  `json:"uri"`
		Text     *string `json:"text,omitempty"`
		Blob     *string `json:"blob,omitempty"`
		MimeType string  `json:"mimeType,omitempty"`
	}
	wire := wireResource{URI: r.URI, MimeType: r.MimeType}
	switch kind {
	case EmbeddedResourceText:
		wire.Text = &r.Text
	case EmbeddedResourceBlob:
		wire.Blob = &r.Blob
	}
	return json.Marshal(wire)
}

type PromptResult struct {
	StopReason StopReason `json:"stopReason"`

	raw json.RawMessage
}

type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled"
)

type CancelParams struct {
	SessionID string `json:"sessionId"`
}

type CloseSessionParams struct {
	SessionID string `json:"sessionId"`
}

type DeleteSessionParams struct {
	SessionID string `json:"sessionId"`
}

func NewSession(ctx context.Context, client JSONRPCClient, params NewSessionParams) (NewSessionResult, error) {
	return newSession(ctx, client, params)
}

func NewSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (NewSessionResult, error) {
	return newSession(ctx, client, params)
}

func newSession(ctx context.Context, client JSONRPCClient, params any) (NewSessionResult, error) {
	resultData, err := requestRaw(ctx, client, "session/new", params)
	if err != nil {
		return NewSessionResult{}, err
	}
	var result NewSessionResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return NewSessionResult{}, fmt.Errorf("decode session/new result: %w", err)
	}
	if result.SessionID == "" {
		return NewSessionResult{}, errors.New("session/new result missing sessionId")
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r NewSessionResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias NewSessionResult
	return json.Marshal(alias(r))
}

func ForkSession(ctx context.Context, client JSONRPCClient, params ForkSessionParams) (ForkSessionResult, error) {
	return forkSession(ctx, client, params)
}

func ForkSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (ForkSessionResult, error) {
	return forkSession(ctx, client, params)
}

func forkSession(ctx context.Context, client JSONRPCClient, params any) (ForkSessionResult, error) {
	resultData, err := requestRaw(ctx, client, "session/fork", params)
	if err != nil {
		return ForkSessionResult{}, err
	}
	var result ForkSessionResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return ForkSessionResult{}, fmt.Errorf("decode session/fork result: %w", err)
	}
	if result.SessionID == "" {
		return ForkSessionResult{}, errors.New("session/fork result missing sessionId")
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r ForkSessionResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias ForkSessionResult
	return json.Marshal(alias(r))
}

func Prompt(ctx context.Context, client JSONRPCClient, params PromptParams) (PromptResult, error) {
	return prompt(ctx, client, params)
}

func PromptRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (PromptResult, error) {
	return prompt(ctx, client, params)
}

func prompt(ctx context.Context, client JSONRPCClient, params any) (PromptResult, error) {
	resultData, err := requestRaw(ctx, client, "session/prompt", params)
	if err != nil {
		return PromptResult{}, err
	}
	var result PromptResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return PromptResult{}, fmt.Errorf("decode session/prompt result: %w", err)
	}
	if result.StopReason == "" {
		return PromptResult{}, errors.New("session/prompt result missing stopReason")
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r PromptResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias PromptResult
	return json.Marshal(alias(r))
}

func LoadSession(ctx context.Context, client JSONRPCClient, params LoadSessionParams) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/load", params)
}

func LoadSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/load", params)
}

func ResumeSession(ctx context.Context, client JSONRPCClient, params ResumeSessionParams) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/resume", params)
}

func ResumeSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/resume", params)
}

func ListSessions(ctx context.Context, client JSONRPCClient, params ListSessionsParams) (ListSessionsResult, error) {
	resultData, err := requestRaw(ctx, client, "session/list", params)
	if err != nil {
		return ListSessionsResult{}, err
	}
	var result ListSessionsResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return ListSessionsResult{}, fmt.Errorf("decode session/list result: %w", err)
	}
	if result.Sessions == nil {
		result.Sessions = []SessionInfo{}
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r ListSessionsResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias ListSessionsResult
	return json.Marshal(alias(r))
}

func Cancel(ctx context.Context, client Notifier, params CancelParams) error {
	if client == nil {
		return errors.New("runtime ACP notifier is required")
	}
	return client.Notify(ctx, "session/cancel", params)
}

func CloseSession(ctx context.Context, client JSONRPCClient, params CloseSessionParams) error {
	return closeSession(ctx, client, params)
}

func CloseSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) error {
	return closeSession(ctx, client, params)
}

func closeSession(ctx context.Context, client JSONRPCClient, params any) error {
	var result map[string]any
	return requestInto(ctx, client, "session/close", params, &result)
}

func DeleteSession(ctx context.Context, client JSONRPCClient, params DeleteSessionParams) error {
	return deleteSession(ctx, client, params)
}

func DeleteSessionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) error {
	return deleteSession(ctx, client, params)
}

func deleteSession(ctx context.Context, client JSONRPCClient, params any) error {
	var result map[string]any
	return requestInto(ctx, client, "session/delete", params, &result)
}

func requestRaw(ctx context.Context, client JSONRPCClient, method string, params any) (json.RawMessage, error) {
	if client == nil {
		return nil, errors.New("runtime ACP client is required")
	}
	resultData, err := client.Request(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if len(resultData) == 0 {
		return json.RawMessage("null"), nil
	}
	return append(json.RawMessage(nil), resultData...), nil
}

func requestInto(ctx context.Context, client JSONRPCClient, method string, params any, out any) error {
	if client == nil {
		return errors.New("runtime ACP client is required")
	}
	resultData, err := client.Request(ctx, method, params)
	if err != nil {
		return err
	}
	if len(resultData) == 0 || string(resultData) == "null" {
		return nil
	}
	if err := json.Unmarshal(resultData, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}
