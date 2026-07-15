package runtimeacp_test

import (
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestRuntimeACPProtocolVersionMatchesCoderSDK(t *testing.T) {
	if runtimeacp.ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Fatalf("runtime ACP protocol version = %d, want SDK version %d", runtimeacp.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
}

func TestInitializeParamsKeepAdapterJSONShape(t *testing.T) {
	title := "Adapter"
	local := runtimeacp.InitializeParams{
		ProtocolVersion: runtimeacp.ProtocolVersion,
		ClientCapabilities: runtimeacp.ClientCapabilities{
			Terminal: true,
			FS: runtimeacp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
		ClientInfo: runtimeacp.ImplementationInfo{
			Name:    "adapter",
			Title:   title,
			Version: "1.0.0",
		},
	}
	localMap := marshalObject(t, local)
	want := map[string]any{
		"protocolVersion": float64(runtimeacp.ProtocolVersion),
		"clientCapabilities": map[string]any{
			"terminal": true,
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
		},
		"clientInfo": map[string]any{
			"name":    "adapter",
			"title":   title,
			"version": "1.0.0",
		},
	}
	if !reflect.DeepEqual(localMap, want) {
		t.Fatalf("initialize JSON shape = %#v, want %#v", localMap, want)
	}

	upstreamMap := marshalObject(t, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Terminal: true,
			Fs: sdk.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
		ClientInfo: &sdk.Implementation{
			Name:    "adapter",
			Title:   &title,
			Version: "1.0.0",
		},
	})
	upstreamCaps := upstreamMap["clientCapabilities"].(map[string]any)
	if _, ok := upstreamCaps["auth"]; !ok {
		t.Fatal("SDK initialize shape no longer emits auth; re-check whether runtimeacp.InitializeParams can alias the SDK type")
	}
}

func TestClientCapabilitiesWithAuthMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.ClientCapabilities{
			Auth:     &runtimeacp.AuthCapabilities{Terminal: true},
			Terminal: true,
			FS: runtimeacp.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
		sdk.ClientCapabilities{
			Auth:     sdk.AuthCapabilities{Terminal: true},
			Terminal: true,
			Fs: sdk.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	)
}

func TestCancelParamsMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.CancelParams{SessionID: "sess-test"},
		sdk.CancelNotification{SessionId: sdk.SessionId("sess-test")},
	)
}

func TestCloseSessionParamsMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.CloseSessionParams{SessionID: "sess-test"},
		sdk.CloseSessionRequest{SessionId: sdk.SessionId("sess-test")},
	)
}

func TestForkSessionParamsMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.ForkSessionParams{
			SessionID:             "sess-test",
			CWD:                   "/tmp/project",
			AdditionalDirectories: []string{"/tmp/shared"},
		},
		sdk.UnstableForkSessionRequest{
			SessionId:             sdk.SessionId("sess-test"),
			Cwd:                   "/tmp/project",
			AdditionalDirectories: []string{"/tmp/shared"},
		},
	)
}

func TestMCPMessageParamsMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.MCPMessageParams{
			ConnectionID: "mcp-conn",
			Method:       "tools/list",
			Params:       json.RawMessage(`{"cursor":"next"}`),
		},
		sdk.UnstableMessageMcpRequest{
			ConnectionId: sdk.UnstableMcpConnectionId("mcp-conn"),
			Method:       "tools/list",
			Params:       map[string]any{"cursor": "next"},
		},
	)
}

func TestMCPMessageNotificationParamsMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.MCPMessageParams{
			ConnectionID: "mcp-conn",
			Method:       "notifications/initialized",
			Params:       json.RawMessage(`{"ok":true}`),
		},
		sdk.UnstableMessageMcpNotification{
			ConnectionId: sdk.UnstableMcpConnectionId("mcp-conn"),
			Method:       "notifications/initialized",
			Params:       map[string]any{"ok": true},
		},
	)
}

func TestACPMCPServerMatchesCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.MCPServer{
			Type: "acp",
			ID:   "mcp-acp-1",
			Name: "Hosted MCP",
		},
		sdk.McpServer{
			Acp: &sdk.McpServerAcpInline{
				Type: "acp",
				Id:   sdk.McpServerAcpId("mcp-acp-1"),
				Name: "Hosted MCP",
			},
		},
	)
}

func TestEmptyEmbeddedResourceVariantsMatchCoderSDKJSONShape(t *testing.T) {
	const uri = "memory:///empty"
	tests := []struct {
		name     string
		local    runtimeacp.EmbeddedResource
		upstream sdk.EmbeddedResourceResource
		kind     runtimeacp.EmbeddedResourceKind
	}{
		{
			name:  "empty text",
			local: runtimeacp.EmbeddedResource{URI: uri, Kind: runtimeacp.EmbeddedResourceText},
			upstream: sdk.EmbeddedResourceResource{
				TextResourceContents: &sdk.TextResourceContents{Uri: uri, Text: ""},
			},
			kind: runtimeacp.EmbeddedResourceText,
		},
		{
			name:  "empty blob",
			local: runtimeacp.EmbeddedResource{URI: uri, Kind: runtimeacp.EmbeddedResourceBlob},
			upstream: sdk.EmbeddedResourceResource{
				BlobResourceContents: &sdk.BlobResourceContents{Uri: uri, Blob: ""},
			},
			kind: runtimeacp.EmbeddedResourceBlob,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertSameJSONShape(t, test.local, test.upstream)
			raw := mustMarshalJSON(t, test.local)
			var roundTrip runtimeacp.EmbeddedResource
			if err := json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			kind, err := roundTrip.ContentKind()
			if err != nil || kind != test.kind {
				t.Fatalf("round-trip kind = %q, %v; want %q", kind, err, test.kind)
			}
		})
	}
}

func TestEmbeddedResourceRejectsNullRequiredValues(t *testing.T) {
	for _, raw := range []string{
		`{"uri":null,"text":"content"}`,
		`{"uri":"memory:///null","text":null}`,
		`{"uri":"memory:///null","blob":null}`,
	} {
		var resource runtimeacp.EmbeddedResource
		if err := json.Unmarshal([]byte(raw), &resource); err == nil {
			t.Fatalf("json.Unmarshal(%s) succeeded, want required string error", raw)
		}
	}
}

func TestMCPAgentCapabilitiesMatchCoderSDKJSONShape(t *testing.T) {
	assertSameJSONShape(t,
		runtimeacp.MCPAgentCapabilities{
			ACP:  true,
			HTTP: true,
			SSE:  true,
		},
		sdk.McpCapabilities{
			Acp:  true,
			Http: true,
			Sse:  true,
		},
	)
}

func assertSameJSONShape(t testing.TB, local any, upstream any) {
	t.Helper()
	localMap := marshalObject(t, local)
	upstreamMap := marshalObject(t, upstream)
	if !reflect.DeepEqual(localMap, upstreamMap) {
		localJSON := mustMarshalJSON(t, local)
		upstreamJSON := mustMarshalJSON(t, upstream)
		t.Fatalf("JSON shape mismatch\nlocal: %s\nsdk:   %s", localJSON, upstreamJSON)
	}
}

func marshalObject(t testing.TB, value any) map[string]any {
	t.Helper()
	raw := mustMarshalJSON(t, value)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode JSON %s: %v", raw, err)
	}
	return decoded
}

func mustMarshalJSON(t testing.TB, value any) []byte {
	t.Helper()
	localJSON, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return localJSON
}
