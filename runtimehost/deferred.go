package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimebridge"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
)

var ErrNotInitialized = errors.New("runtime host is not initialized")

type DeferredHost struct {
	ctx    context.Context
	spec   Spec
	events chan runtimejsonrpc.Event

	mu   sync.RWMutex
	host *Host
}

func NewDeferred(ctx context.Context, spec Spec) *DeferredHost {
	if ctx == nil {
		ctx = context.Background()
	}
	events := make(chan runtimejsonrpc.Event)
	close(events)
	return &DeferredHost{
		ctx:    ctx,
		spec:   spec,
		events: events,
	}
}

func (h *DeferredHost) Initialize(params json.RawMessage) (any, *acp.RPCError) {
	var req struct {
		ClientCapabilities runtimeacp.ClientCapabilities `json:"clientCapabilities,omitempty"`
	}
	if len(params) != 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.host != nil {
		return h.host.InitializeResult(), nil
	}

	spec := h.spec
	spec.ClientCapabilities = req.ClientCapabilities
	host, err := Start(h.ctx, spec)
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "start runtime host", Data: err.Error()}
	}
	h.host = host
	return host.InitializeResult(), nil
}

func (h *DeferredHost) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	client, err := h.runtimeClient()
	if err != nil {
		return nil, err
	}
	return client.Request(ctx, method, params)
}

func (h *DeferredHost) Notify(ctx context.Context, method string, params any) error {
	client, err := h.runtimeClient()
	if err != nil {
		return err
	}
	return client.Notify(ctx, method, params)
}

func (h *DeferredHost) Respond(ctx context.Context, id json.RawMessage, result any, rpcErr *runtimejsonrpc.RPCError) error {
	client, err := h.runtimeClient()
	if err != nil {
		return err
	}
	return client.Respond(ctx, id, result, rpcErr)
}

func (h *DeferredHost) Events() <-chan runtimejsonrpc.Event {
	h.mu.RLock()
	host := h.host
	h.mu.RUnlock()
	if host == nil {
		return h.events
	}
	client := host.RuntimeClient()
	if client == nil {
		return h.events
	}
	return client.Events()
}

func (h *DeferredHost) Close() error {
	h.mu.Lock()
	host := h.host
	h.host = nil
	h.mu.Unlock()
	if host == nil {
		return nil
	}
	return host.Close()
}

func (h *DeferredHost) runtimeClient() (runtimebridge.RuntimeClient, error) {
	h.mu.RLock()
	host := h.host
	h.mu.RUnlock()
	if host == nil {
		return nil, ErrNotInitialized
	}
	client := host.RuntimeClient()
	if client == nil {
		return nil, ErrNotInitialized
	}
	return client, nil
}
