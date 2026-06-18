package runtimeacp

import (
	"context"
	"encoding/json"
)

type AuthenticateParams struct {
	MethodID string `json:"methodId"`
}

func Authenticate(ctx context.Context, client JSONRPCClient, params AuthenticateParams) error {
	return authenticate(ctx, client, params)
}

func AuthenticateRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) error {
	return authenticate(ctx, client, params)
}

func authenticate(ctx context.Context, client JSONRPCClient, params any) error {
	var result map[string]any
	return requestInto(ctx, client, "authenticate", params, &result)
}

func Logout(ctx context.Context, client JSONRPCClient) error {
	return LogoutRaw(ctx, client, json.RawMessage(`{}`))
}

func LogoutRaw(ctx context.Context, client JSONRPCClient, params json.RawMessage) error {
	var result map[string]any
	return requestInto(ctx, client, "logout", params, &result)
}
