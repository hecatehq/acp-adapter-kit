package runtimeacp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestAuthenticateSendsMethodID(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{}`)}

	if err := runtimeacp.Authenticate(context.Background(), client, runtimeacp.AuthenticateParams{MethodID: "agent-login"}); err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if client.method != "authenticate" {
		t.Fatalf("method = %q, want authenticate", client.method)
	}
	var params runtimeacp.AuthenticateParams
	mustJSONRoundTrip(t, client.params, &params)
	if params.MethodID != "agent-login" {
		t.Fatalf("MethodID = %q, want agent-login", params.MethodID)
	}
}

func TestLogoutSendsEmptyRequest(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{}`)}

	if err := runtimeacp.Logout(context.Background(), client); err != nil {
		t.Fatalf("Logout returned error: %v", err)
	}
	if client.method != "logout" {
		t.Fatalf("method = %q, want logout", client.method)
	}
}

type recordingACPClient struct {
	method string
	params any
	result json.RawMessage
}

func (c *recordingACPClient) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	c.method = method
	c.params = params
	return c.result, nil
}

func mustJSONRoundTrip(t testing.TB, value any, target any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
}
