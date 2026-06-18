package runtimeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
)

const ProtocolVersion = 1

var ErrUnsupportedProtocolVersion = errors.New("unsupported ACP protocol version")

type JSONRPCClient interface {
	Request(ctx context.Context, method string, params any) (json.RawMessage, error)
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         ImplementationInfo `json:"clientInfo,omitempty"`
}

type ClientCapabilities struct {
	Auth     *AuthCapabilities      `json:"auth,omitempty"`
	FS       FileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                   `json:"terminal,omitempty"`
}

type AuthCapabilities struct {
	Terminal bool `json:"terminal,omitempty"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

type ImplementationInfo struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion   int                `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities  `json:"agentCapabilities,omitempty"`
	AgentInfo         ImplementationInfo `json:"agentInfo,omitempty"`
	AuthMethods       []json.RawMessage  `json:"authMethods,omitempty"`

	raw json.RawMessage
}

type AgentCapabilities struct {
	LoadSession         bool                       `json:"loadSession,omitempty"`
	PromptCapabilities  PromptCapabilities         `json:"promptCapabilities,omitempty"`
	MCPCapabilities     MCPAgentCapabilities       `json:"mcpCapabilities,omitempty"`
	Auth                map[string]json.RawMessage `json:"auth,omitempty"`
	SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type MCPAgentCapabilities struct {
	ACP  bool `json:"acp,omitempty"`
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

func Initialize(ctx context.Context, client JSONRPCClient, params InitializeParams) (InitializeResult, error) {
	if client == nil {
		return InitializeResult{}, errors.New("runtime ACP client is required")
	}
	if params.ProtocolVersion == 0 {
		params.ProtocolVersion = ProtocolVersion
	}
	resultData, err := client.Request(ctx, "initialize", params)
	if err != nil {
		return InitializeResult{}, err
	}
	var result InitializeResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return InitializeResult{}, fmt.Errorf("decode initialize result: %w", err)
	}
	if result.ProtocolVersion != params.ProtocolVersion {
		return InitializeResult{}, fmt.Errorf("%w: requested %d got %d", ErrUnsupportedProtocolVersion, params.ProtocolVersion, result.ProtocolVersion)
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r InitializeResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias InitializeResult
	return json.Marshal(alias(r))
}

func NewRuntimeJSONRPCClient(client *runtimejsonrpc.Client) JSONRPCClient {
	return client
}
