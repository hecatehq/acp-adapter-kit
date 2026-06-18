package runtimejsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

const DefaultMaxMessageBytes = 1024 * 1024

type ConnectSpec struct {
	Launcher        runtimeproc.Launcher
	Launch          runtimeproc.LaunchSpec
	MaxMessageBytes int
	EventBuffer     int
}

type Client struct {
	proc *runtimeproc.Process

	maxMessageBytes int
	events          chan Event

	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan responseResult
	abandoned map[int64]struct{}
	done      chan struct{}
	doneErr   error
	finish    sync.Once
}

type Event struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("runtime rpc error %d: %s", e.Code, e.Message)
}

type responseResult struct {
	result json.RawMessage
	err    error
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type cancelRequestParams struct {
	RequestID json.RawMessage `json:"requestId"`
}

func Connect(ctx context.Context, spec ConnectSpec) (*Client, error) {
	if spec.Launcher == nil {
		spec.Launcher = runtimeproc.NewDefaultLauncher()
	}
	if spec.MaxMessageBytes <= 0 {
		spec.MaxMessageBytes = DefaultMaxMessageBytes
	}
	proc, err := spec.Launcher.Launch(ctx, spec.Launch)
	if err != nil {
		return nil, err
	}
	client := &Client{
		proc:            proc,
		maxMessageBytes: spec.MaxMessageBytes,
		events:          make(chan Event, max(1, spec.EventBuffer)),
		pending:         map[int64]chan responseResult{},
		abandoned:       map[int64]struct{}{},
		done:            make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, resultCh, err := c.registerPending()
	if err != nil {
		return nil, err
	}
	if err := c.write(envelope{JSONRPC: "2.0", ID: rawID(id), Method: method, Params: mustRawJSON(params)}); err != nil {
		c.removePending(id)
		return nil, err
	}

	select {
	case result := <-resultCh:
		return result.result, result.err
	case <-ctx.Done():
		c.abandonPending(id)
		c.notifyCancelRequest(id)
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.errOrClosed()
	}
}

func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- c.write(envelope{JSONRPC: "2.0", Method: method, Params: mustRawJSON(params)})
	}()
	select {
	case err := <-writeErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.errOrClosed()
	}
}

func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any, rpcErr *RPCError) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(id) == 0 {
		return errors.New("runtime response id is required")
	}
	msg := envelope{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...)}
	if rpcErr != nil {
		msg.Error = rpcErr
	} else {
		msg.Result = mustRawJSON(result)
	}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- c.write(msg)
	}()
	select {
	case err := <-writeErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.errOrClosed()
	}
}

func (c *Client) Events() <-chan Event {
	return c.events
}

func (c *Client) Kill() error {
	if c == nil || c.proc == nil {
		return nil
	}
	return c.proc.Kill()
}

func (c *Client) Wait() error {
	if c == nil {
		return nil
	}
	<-c.done
	return c.doneErr
}

func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.proc.Stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), c.maxMessageBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg envelope
		if err := json.Unmarshal(line, &msg); err != nil {
			c.finishWithError(fmt.Errorf("decode runtime message: %w", err))
			return
		}
		if err := c.handleMessage(msg); err != nil {
			c.finishWithError(err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.finishWithError(fmt.Errorf("read runtime message: %w", err))
		return
	}
	if err := c.proc.Wait(); err != nil {
		c.finishWithError(err)
		return
	}
	c.finishWithError(nil)
}

func (c *Client) handleMessage(msg envelope) error {
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		return fmt.Errorf("runtime message has invalid jsonrpc version %q", msg.JSONRPC)
	}
	if len(msg.ID) != 0 && msg.Method == "" {
		id, err := decodeID(msg.ID)
		if err != nil {
			return err
		}
		c.mu.Lock()
		resultCh := c.pending[id]
		delete(c.pending, id)
		_, abandoned := c.abandoned[id]
		delete(c.abandoned, id)
		c.mu.Unlock()
		if resultCh == nil {
			if abandoned {
				return nil
			}
			return fmt.Errorf("runtime response for unknown id %d", id)
		}
		if msg.Error != nil {
			resultCh <- responseResult{err: msg.Error}
			return nil
		}
		resultCh <- responseResult{result: append(json.RawMessage(nil), msg.Result...)}
		return nil
	}
	if msg.Method == "" {
		return errors.New("runtime message missing method")
	}
	event := Event{
		ID:     append(json.RawMessage(nil), msg.ID...),
		Method: msg.Method,
		Params: append(json.RawMessage(nil), msg.Params...),
	}
	select {
	case c.events <- event:
	case <-c.done:
	}
	return nil
}

func (c *Client) write(msg envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.done:
		return c.errOrClosed()
	default:
	}
	encoder := json.NewEncoder(c.proc.Stdin)
	if err := encoder.Encode(msg); err != nil {
		return fmt.Errorf("write runtime message: %w", err)
	}
	return nil
}

func (c *Client) registerPending() (int64, chan responseResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return 0, nil, c.errOrClosed()
	default:
	}
	c.nextID++
	id := c.nextID
	resultCh := make(chan responseResult, 1)
	c.pending[id] = resultCh
	return id, resultCh, nil
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

func (c *Client) abandonPending(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[id]; ok {
		delete(c.pending, id)
		c.abandoned[id] = struct{}{}
	}
}

func (c *Client) notifyCancelRequest(id int64) {
	_ = c.write(envelope{
		JSONRPC: "2.0",
		Method:  "$/cancel_request",
		Params:  mustRawJSON(cancelRequestParams{RequestID: rawID(id)}),
	})
}

func (c *Client) finishWithError(err error) {
	c.finish.Do(func() {
		c.doneErr = err
		pendingErr := err
		if pendingErr == nil {
			pendingErr = io.EOF
		}
		c.mu.Lock()
		for id, resultCh := range c.pending {
			delete(c.pending, id)
			resultCh <- responseResult{err: pendingErr}
		}
		c.mu.Unlock()
		close(c.done)
		close(c.events)
	})
}

func (c *Client) errOrClosed() error {
	if c.doneErr != nil {
		return c.doneErr
	}
	return io.EOF
}

func mustRawJSON(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func rawID(id int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(id, 10))
}

func decodeID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, fmt.Errorf("decode runtime response id %s: %w", string(raw), err)
	}
	return id, nil
}
