package acp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func TestRPCErrorUsesCoderSDKShape(t *testing.T) {
	got, err := json.Marshal(&RPCError{
		Code:    -32042,
		Message: "boom",
		Data:    map[string]any{"reason": "test"},
	})
	if err != nil {
		t.Fatalf("marshal local RPCError: %v", err)
	}
	want, err := json.Marshal(&sdk.RequestError{
		Code:    -32042,
		Message: "boom",
		Data:    map[string]any{"reason": "test"},
	})
	if err != nil {
		t.Fatalf("marshal sdk RequestError: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("RPCError JSON = %s, want %s", got, want)
	}
}

func TestDefaultInitializeUsesCoderSDKProtocolVersion(t *testing.T) {
	server := NewServer(AdapterInfo{Name: "codex-acp-adapter"})

	var out bytes.Buffer
	err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	envelopes := decodeServerEnvelopes(t, out.Bytes())
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want initialize response", len(envelopes))
	}
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(envelopes[0].Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Fatalf("protocolVersion = %d, want SDK version %d", result.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
}
