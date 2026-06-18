package runtimehost_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimehost"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestDeferredHostIsColdBeforeInitialize(t *testing.T) {
	host := runtimehost.NewDeferred(context.Background(), runtimehost.Spec{})

	if _, err := host.Request(context.Background(), "session/new", nil); !errors.Is(err, runtimehost.ErrNotInitialized) {
		t.Fatalf("Request error = %v, want ErrNotInitialized", err)
	}
	if err := host.Notify(context.Background(), "session/cancel", nil); !errors.Is(err, runtimehost.ErrNotInitialized) {
		t.Fatalf("Notify error = %v, want ErrNotInitialized", err)
	}
	select {
	case _, ok := <-host.Events():
		if ok {
			t.Fatal("Events channel open before initialize, want closed placeholder")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading pre-initialize events")
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close before initialize returned error: %v", err)
	}
}

func TestDeferredHostRejectsInvalidInitializeParams(t *testing.T) {
	host := runtimehost.NewDeferred(context.Background(), runtimehost.Spec{})

	_, rpcErr := host.Initialize(json.RawMessage(`{"clientCapabilities":`))
	if rpcErr == nil {
		t.Fatal("Initialize returned nil RPC error, want invalid params")
	}
	if rpcErr.Code != -32602 {
		t.Fatalf("RPC code = %d, want -32602", rpcErr.Code)
	}
}

func TestDeferredHostStartsRuntimeOnInitialize(t *testing.T) {
	host := newDeferredHelperHost(t)

	result, rpcErr := host.Initialize(mustRawJSON(t, map[string]any{
		"clientCapabilities": map[string]any{
			"terminal": true,
			"auth":     map[string]any{"terminal": true},
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
		},
	}))
	if rpcErr != nil {
		t.Fatalf("Initialize RPC error = %+v", rpcErr)
	}
	initialize := result.(runtimeacp.InitializeResult)
	if initialize.AgentInfo.Name != "helper-runtime" {
		t.Fatalf("agent name = %q, want helper-runtime", initialize.AgentInfo.Name)
	}

	again, rpcErr := host.Initialize(nil)
	if rpcErr != nil {
		t.Fatalf("second Initialize RPC error = %+v", rpcErr)
	}
	if again.(runtimeacp.InitializeResult).AgentInfo.Name != "helper-runtime" {
		t.Fatalf("second initialize result = %#v, want retained result", again)
	}
}

func TestDeferredHostProxiesAfterInitialize(t *testing.T) {
	host := newDeferredHelperHost(t)
	if _, rpcErr := host.Initialize(mustRawJSON(t, map[string]any{
		"clientCapabilities": map[string]any{
			"terminal": true,
			"auth":     map[string]any{"terminal": true},
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
		},
	})); rpcErr != nil {
		t.Fatalf("Initialize RPC error = %+v", rpcErr)
	}

	raw, err := host.Request(context.Background(), "session/new", map[string]any{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatalf("decode session result: %v\n%s", err, string(raw))
	}
	if session.SessionID != "runtime-session" {
		t.Fatalf("sessionId = %q, want runtime-session", session.SessionID)
	}
}

func newDeferredHelperHost(t testing.TB) *runtimehost.DeferredHost {
	t.Helper()
	t.Setenv("GO_WANT_RUNTIMEHOST_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeHostHelper", "--", "happy"},
		InheritEnv: []string{"GO_WANT_RUNTIMEHOST_HELPER"},
	})
	host := runtimehost.NewDeferred(context.Background(), runtimehost.Spec{
		Launcher: launcher,
		Launch:   runtimeproc.LaunchSpec{WorkDir: t.TempDir()},
		ClientInfo: runtimeacp.ImplementationInfo{
			Name:    "test-adapter",
			Title:   "Test Adapter",
			Version: "test-version",
		},
	})
	t.Cleanup(func() {
		_ = host.Close()
	})
	return host
}

func mustRawJSON(t testing.TB, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return raw
}
