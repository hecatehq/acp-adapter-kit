package runtimejsonrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

func TestRequestReturnsMatchingResult(t *testing.T) {
	client := newHelperClient(t)

	result, err := client.Request(context.Background(), "runtime/echo", map[string]any{
		"text": "hello",
	})
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	want := map[string]any{
		"method": "runtime/echo",
		"params": map[string]any{"text": "hello"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestNotifyWritesJSONRPCNotification(t *testing.T) {
	client := newHelperClient(t)

	if err := client.Notify(context.Background(), "runtime/ping", map[string]any{"ok": true}); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}

	event := nextEvent(t, client)
	if event.Method != "runtime/notification_seen" {
		t.Fatalf("event method = %q, want runtime/notification_seen", event.Method)
	}
	var params map[string]any
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params["method"] != "runtime/ping" {
		t.Fatalf("params = %#v, want method runtime/ping", params)
	}
}

func TestChildNotificationIsDeliveredBeforeResponse(t *testing.T) {
	client := newHelperClient(t)

	resultCh := make(chan error, 1)
	go func() {
		_, err := client.Request(context.Background(), "runtime/emit", map[string]any{"sessionId": "s1"})
		resultCh <- err
	}()

	event := nextEvent(t, client)
	if event.Method != "session/update" {
		t.Fatalf("event method = %q, want session/update", event.Method)
	}

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("Request returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestChildRequestIsDeliveredAsEvent(t *testing.T) {
	client := newHelperClient(t)

	resultCh := make(chan error, 1)
	go func() {
		_, err := client.Request(context.Background(), "runtime/child_request", nil)
		resultCh <- err
	}()

	event := nextEvent(t, client)
	if event.Method != "session/request_permission" {
		t.Fatalf("event method = %q, want session/request_permission", event.Method)
	}
	if string(event.ID) != `"child-1"` {
		t.Fatalf("event ID = %s, want child-1", event.ID)
	}

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("Request returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestChildRequestCanBeAnswered(t *testing.T) {
	client := newHelperClient(t)

	resultCh := make(chan childRequestResult, 1)
	go func() {
		result, err := client.Request(context.Background(), "runtime/child_request_wait", nil)
		resultCh <- childRequestResult{result: result, err: err}
	}()

	event := nextEvent(t, client)
	if event.Method != "session/request_permission" {
		t.Fatalf("event method = %q, want session/request_permission", event.Method)
	}
	if string(event.ID) != `"child-1"` {
		t.Fatalf("event ID = %s, want child-1", event.ID)
	}
	if err := client.Respond(context.Background(), event.ID, map[string]any{"outcome": "approved"}, nil); err != nil {
		t.Fatalf("Respond returned error: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("Request returned error: %v", result.err)
		}
		if !strings.Contains(string(result.result), "approved") {
			t.Fatalf("result = %s, want approved", result.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestChildRequestCanBeAnsweredWithError(t *testing.T) {
	client := newHelperClient(t)

	resultCh := make(chan childRequestResult, 1)
	go func() {
		result, err := client.Request(context.Background(), "runtime/child_request_wait", nil)
		resultCh <- childRequestResult{result: result, err: err}
	}()

	event := nextEvent(t, client)
	if err := client.Respond(context.Background(), event.ID, nil, &runtimejsonrpc.RPCError{
		Code:    -32060,
		Message: "denied",
	}); err != nil {
		t.Fatalf("Respond returned error: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("Request returned error: %v", result.err)
		}
		if !strings.Contains(string(result.result), "denied") {
			t.Fatalf("result = %s, want denied", result.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestErrorResponseReturnsRPCError(t *testing.T) {
	client := newHelperClient(t)

	_, err := client.Request(context.Background(), "runtime/error", nil)
	var rpcErr *runtimejsonrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %T %[1]v, want RPCError", err)
	}
	if rpcErr.Code != -32042 || rpcErr.Message != "runtime failed" {
		t.Fatalf("RPCError = %+v, want -32042 runtime failed", rpcErr)
	}
}

func TestMalformedChildStdoutFailsPendingRequest(t *testing.T) {
	client := newHelperClient(t)

	_, err := client.Request(context.Background(), "runtime/bad_json", nil)
	if err == nil || !strings.Contains(err.Error(), "decode runtime message") {
		t.Fatalf("Request error = %v, want decode error", err)
	}
	if waitErr := client.Wait(); waitErr == nil || !strings.Contains(waitErr.Error(), "decode runtime message") {
		t.Fatalf("Wait error = %v, want decode error", waitErr)
	}
}

func TestCleanChildExitMakesWaitReturnNil(t *testing.T) {
	client := newHelperClient(t)

	result, err := client.Request(context.Background(), "runtime/quit", nil)
	if err != nil {
		t.Fatalf("Request returned error: %v", err)
	}
	if !strings.Contains(string(result), "bye") {
		t.Fatalf("result = %s, want bye", result)
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v, want nil", err)
	}
	_, err = client.Request(context.Background(), "runtime/echo", nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("post-close Request error = %v, want EOF", err)
	}
}

func TestRequestContextCancellationDoesNotPoisonLaterResponse(t *testing.T) {
	client := newHelperClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.Request(ctx, "runtime/delay", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request error = %v, want deadline", err)
	}

	result, err := client.Request(context.Background(), "runtime/echo", map[string]any{"after": "timeout"})
	if err != nil {
		t.Fatalf("second Request returned error: %v", err)
	}
	if !strings.Contains(string(result), "timeout") {
		t.Fatalf("second result = %s, want timeout echo", result)
	}
}

func TestRequestContextCancellationSendsProtocolCancelRequest(t *testing.T) {
	client := newHelperClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, "runtime/wait_cancel", nil)
		resultCh <- err
	}()

	started := nextEvent(t, client)
	if started.Method != "runtime/request_started" {
		t.Fatalf("started event method = %q, want runtime/request_started", started.Method)
	}
	var startedParams struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(started.Params, &startedParams); err != nil {
		t.Fatalf("decode started params: %v", err)
	}
	cancel()

	cancelSeen := nextEvent(t, client)
	if cancelSeen.Method != "runtime/cancel_seen" {
		t.Fatalf("cancel event method = %q, want runtime/cancel_seen", cancelSeen.Method)
	}
	var cancelParams struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(cancelSeen.Params, &cancelParams); err != nil {
		t.Fatalf("decode cancel params: %v", err)
	}
	if string(cancelParams.RequestID) != string(startedParams.RequestID) {
		t.Fatalf("cancel request id = %s, want %s", cancelParams.RequestID, startedParams.RequestID)
	}

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Request error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled request")
	}
}

func newHelperClient(t testing.TB) *runtimejsonrpc.Client {
	t.Helper()
	t.Setenv("GO_WANT_RUNTIMEJSONRPC_HELPER", "1")
	launcher := runtimeproc.NewLauncher(runtimeproc.Config{
		Binary:     os.Args[0],
		Args:       []string{"-test.run=TestRuntimeJSONRPCHelper", "--"},
		InheritEnv: []string{"GO_WANT_RUNTIMEJSONRPC_HELPER"},
	})
	client, err := runtimejsonrpc.Connect(context.Background(), runtimejsonrpc.ConnectSpec{
		Launcher: launcher,
		Launch: runtimeproc.LaunchSpec{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Kill()
		_ = client.Wait()
	})
	return client
}

func nextEvent(t testing.TB, client *runtimejsonrpc.Client) runtimejsonrpc.Event {
	t.Helper()
	select {
	case event := <-client.Events():
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return runtimejsonrpc.Event{}
}

type childRequestResult struct {
	result json.RawMessage
	err    error
}

func TestRuntimeJSONRPCHelper(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIMEJSONRPC_HELPER") != "1" {
		return
	}
	var nextID int64
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := decoder.Decode(&msg); err != nil {
			return
		}
		if len(msg.ID) == 0 {
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "runtime/notification_seen",
				"params": map[string]any{
					"method": msg.Method,
					"params": json.RawMessage(msg.Params),
				},
			})
			continue
		}
		switch msg.Method {
		case "runtime/echo":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result": map[string]any{
					"method": msg.Method,
					"params": json.RawMessage(msg.Params),
				},
			})
		case "runtime/emit":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params":  map[string]any{"sessionId": "s1"},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"ok": true},
			})
		case "runtime/error":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error": map[string]any{
					"code":    -32042,
					"message": "runtime failed",
				},
			})
		case "runtime/bad_json":
			fmt.Fprintln(os.Stdout, "{not-json")
			return
		case "runtime/delay":
			time.Sleep(50 * time.Millisecond)
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"late": true},
			})
		case "runtime/wait_cancel":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "runtime/request_started",
				"params":  map[string]any{"requestId": json.RawMessage(msg.ID)},
			})
			var cancelMsg struct {
				Method string `json:"method"`
				Params struct {
					RequestID json.RawMessage `json:"requestId"`
				} `json:"params"`
			}
			if err := decoder.Decode(&cancelMsg); err != nil {
				return
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "runtime/cancel_seen",
				"params": map[string]any{
					"method":    cancelMsg.Method,
					"requestId": cancelMsg.Params.RequestID,
				},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"cancelled": true},
			})
		case "runtime/quit":
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"bye": true},
			})
			os.Exit(0)
		case "runtime/child_request":
			nextID++
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      fmt.Sprintf("child-%d", nextID),
				"method":  "session/request_permission",
				"params":  map[string]any{"toolCallId": "tool-1"},
			})
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  map[string]any{"ok": true},
			})
		case "runtime/child_request_wait":
			nextID++
			childID := fmt.Sprintf("child-%d", nextID)
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      childID,
				"method":  "session/request_permission",
				"params":  map[string]any{"toolCallId": "tool-1"},
			})
			var response struct {
				ID     json.RawMessage          `json:"id"`
				Result map[string]any           `json:"result,omitempty"`
				Error  *runtimejsonrpc.RPCError `json:"error,omitempty"`
			}
			if err := decoder.Decode(&response); err != nil {
				return
			}
			if string(response.ID) != `"`+childID+`"` {
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      json.RawMessage(msg.ID),
					"error":   map[string]any{"code": -32001, "message": "wrong child response id"},
				})
				continue
			}
			result := map[string]any{"childResult": response.Result}
			if response.Error != nil {
				result = map[string]any{"childError": response.Error.Message}
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"result":  result,
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error": map[string]any{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}
}
