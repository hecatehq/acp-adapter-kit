package runtimeacp

import (
	"context"
	"encoding/json"
)

type SetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type SetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

func SetConfigOption(ctx context.Context, client JSONRPCClient, params SetConfigOptionParams) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/set_config_option", params)
}

func SetConfigOptionRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/set_config_option", params)
}

func SetMode(ctx context.Context, client JSONRPCClient, params SetModeParams) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/set_mode", params)
}

func SetModeRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) (json.RawMessage, error) {
	return requestRaw(ctx, client, "session/set_mode", params)
}
