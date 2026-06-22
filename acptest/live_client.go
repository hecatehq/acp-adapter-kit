package acptest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type LiveServer interface {
	Serve(io.Reader, io.Writer) error
}

type LiveClientOption func(*LiveClient)

type LiveResponseHandler func(*LiveClient, Response)

type LiveClient struct {
	t          testing.TB
	input      *io.PipeWriter
	responses  chan Response
	decodeDone chan error
	serveDone  chan error
	writeMu    sync.Mutex
	handler    LiveResponseHandler
}

func NewLiveClient(t testing.TB, server LiveServer, opts ...LiveClientOption) *LiveClient {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	client := &LiveClient{
		t:          t,
		input:      inputWriter,
		responses:  make(chan Response, 64),
		decodeDone: make(chan error, 1),
		serveDone:  make(chan error, 1),
	}
	for _, opt := range opts {
		opt(client)
	}
	go func() {
		err := server.Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		client.serveDone <- err
	}()
	go client.decode(outputReader)
	t.Cleanup(func() {
		_ = inputWriter.Close()
		select {
		case err := <-client.serveDone:
			if err != nil {
				t.Errorf("ACP server returned error during cleanup: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for ACP server cleanup")
		}
		select {
		case err := <-client.decodeDone:
			if err != nil {
				t.Errorf("decode ACP response during cleanup: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for ACP response decoder cleanup")
		}
	})
	return client
}

func WithLiveResponseHandler(handler LiveResponseHandler) LiveClientOption {
	return func(client *LiveClient) {
		client.handler = handler
	}
}

func WithAutoAllowPermissions() LiveClientOption {
	return WithLiveResponseHandler(func(client *LiveClient, response Response) {
		client.AutoAllowPermission(response)
	})
}

func WithAutoRejectPermissions() LiveClientOption {
	return WithLiveResponseHandler(func(client *LiveClient, response Response) {
		client.AutoRejectPermission(response)
	})
}

func WithAutoCancelPermissions() LiveClientOption {
	return WithLiveResponseHandler(func(client *LiveClient, response Response) {
		client.AutoCancelPermission(response)
	})
}

func (c *LiveClient) Request(id string, method string, params any, timeout time.Duration) []Response {
	c.t.Helper()
	c.Write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	return c.CollectUntilResponse(id, timeout)
}

func (c *LiveClient) Notify(method string, params any) {
	c.t.Helper()
	c.Write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *LiveClient) PromptText(id, sessionID, prompt string, timeout time.Duration) []Response {
	c.t.Helper()
	return c.Request(id, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	}, timeout)
}

func (c *LiveClient) PromptTextAndCancel(id, sessionID, prompt string, cancelAfter, timeout time.Duration) []Response {
	c.t.Helper()
	c.Write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": prompt}},
		},
	})
	timer := time.NewTimer(cancelAfter)
	defer timer.Stop()
	cancelled := false
	var out []Response
	deadline := time.After(timeout)
	for {
		select {
		case <-timer.C:
			c.Notify("session/cancel", map[string]any{"sessionId": sessionID})
			cancelled = true
		case response, ok := <-c.responses:
			if !ok {
				c.failDecodeClosed()
			}
			out = append(out, response)
			c.HandleResponse(response)
			if ResponseIDEquals(response.ID, id) && response.Method == "" {
				if !cancelled {
					c.t.Fatalf("prompt %q completed before cancellation was sent: %#v", id, out)
				}
				return out
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for cancelled prompt %q", id)
		}
	}
}

func (c *LiveClient) CollectUntilResponse(id string, timeout time.Duration) []Response {
	c.t.Helper()
	deadline := time.After(timeout)
	var out []Response
	for {
		select {
		case response, ok := <-c.responses:
			if !ok {
				c.failDecodeClosed()
			}
			out = append(out, response)
			c.HandleResponse(response)
			if ResponseIDEquals(response.ID, id) && response.Method == "" {
				return out
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for response %q", id)
		}
	}
}

func (c *LiveClient) AssertNoLateResponse(id string, duration time.Duration) {
	c.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case response, ok := <-c.responses:
			if !ok {
				c.failDecodeClosed()
			}
			if ResponseIDEquals(response.ID, id) && response.Method == "" {
				c.t.Fatalf("late response for %q after cancellation: %#v", id, response)
			}
			c.HandleResponse(response)
		case <-timer.C:
			return
		}
	}
}

func (c *LiveClient) Write(envelope any) {
	c.t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := json.NewEncoder(c.input).Encode(envelope); err != nil {
		c.t.Fatalf("write ACP envelope: %v", err)
	}
}

func (c *LiveClient) HandleResponse(response Response) {
	c.t.Helper()
	if c.handler != nil {
		c.handler(c, response)
	}
}

func (c *LiveClient) AutoAllowPermission(response Response) {
	c.t.Helper()
	if response.Method != "session/request_permission" || len(response.ID) == 0 {
		return
	}
	var req struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	response.ParamsInto(c.t, &req)
	optionID := firstAllowOption(req.Options)
	if optionID == "" {
		c.t.Fatalf("permission request has no allow option: %#v", req.Options)
	}
	c.WritePermissionOutcome(response.ID, "selected", optionID)
}

func (c *LiveClient) AutoRejectPermission(response Response) {
	c.t.Helper()
	if response.Method != "session/request_permission" || len(response.ID) == 0 {
		return
	}
	var req struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	response.ParamsInto(c.t, &req)
	optionID := firstRejectOption(req.Options)
	if optionID == "" {
		c.t.Fatalf("permission request has no reject option: %#v", req.Options)
	}
	c.WritePermissionOutcome(response.ID, "selected", optionID)
}

func (c *LiveClient) AutoCancelPermission(response Response) {
	c.t.Helper()
	if response.Method != "session/request_permission" || len(response.ID) == 0 {
		return
	}
	c.WritePermissionOutcome(response.ID, "cancelled", "")
}

func (c *LiveClient) WritePermissionOutcome(id json.RawMessage, outcome, optionID string) {
	c.t.Helper()
	outcomePayload := map[string]any{
		"outcome": outcome,
	}
	result := map[string]any{
		"outcome": outcomePayload,
	}
	if optionID != "" {
		outcomePayload["optionId"] = optionID
	}
	c.Write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      append(json.RawMessage(nil), id...),
		Result:  result,
	})
}

func ResponseIDEquals(raw json.RawMessage, want string) bool {
	var got string
	return json.Unmarshal(raw, &got) == nil && got == want
}

func (c *LiveClient) decode(output io.Reader) {
	decoder := json.NewDecoder(output)
	for {
		var response Response
		if err := decoder.Decode(&response); err != nil {
			if err == io.EOF {
				c.decodeDone <- nil
			} else {
				c.decodeDone <- err
			}
			close(c.responses)
			return
		}
		c.responses <- response
	}
}

func (c *LiveClient) failDecodeClosed() {
	c.t.Helper()
	select {
	case err := <-c.decodeDone:
		if err != nil {
			c.t.Fatalf("decode ACP response: %v", err)
		}
		c.t.Fatal("ACP response stream closed before expected response")
	default:
		c.t.Fatal("ACP response stream closed before expected response")
	}
}

func firstAllowOption(options []struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}) string {
	for _, option := range options {
		if strings.Contains(strings.ToLower(option.Kind), "allow") || strings.Contains(strings.ToLower(option.OptionID), "allow") {
			return option.OptionID
		}
	}
	return ""
}

func firstRejectOption(options []struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}) string {
	for _, option := range options {
		kind := strings.ToLower(option.Kind)
		id := strings.ToLower(option.OptionID)
		if strings.Contains(kind, "reject") ||
			strings.Contains(kind, "deny") ||
			strings.Contains(kind, "block") ||
			strings.Contains(id, "reject") ||
			strings.Contains(id, "deny") ||
			strings.Contains(id, "block") {
			return option.OptionID
		}
	}
	return ""
}

func UniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
