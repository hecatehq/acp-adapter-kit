package runtimeacp

import (
	"context"
	"encoding/json"
	"errors"
)

type MCPMessageParams struct {
	ConnectionID string          `json:"connectionId"`
	Method       string          `json:"method"`
	Params       json.RawMessage `json:"params,omitempty"`
}

func MCPMessage(ctx context.Context, client JSONRPCClient, params MCPMessageParams) (json.RawMessage, error) {
	return requestRaw(ctx, client, "mcp/message", params)
}

func NotifyMCPMessage(ctx context.Context, client Notifier, params MCPMessageParams) error {
	if client == nil {
		return errors.New("runtime ACP notifier is required")
	}
	return client.Notify(ctx, "mcp/message", params)
}
