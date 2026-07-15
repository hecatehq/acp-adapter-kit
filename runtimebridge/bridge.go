package runtimebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
)

type RuntimeClient interface {
	runtimeacp.JSONRPCClient
	runtimeacp.Notifier
	Events() <-chan runtimejsonrpc.Event
	Respond(ctx context.Context, id json.RawMessage, result any, rpcErr *runtimejsonrpc.RPCError) error
}

type Bridge struct {
	runtime RuntimeClient
}

func New(runtime RuntimeClient) *Bridge {
	return &Bridge{runtime: runtime}
}

func (b *Bridge) Options() []acp.Option {
	return []acp.Option{
		acp.WithMethod("authenticate", b.authenticate),
		acp.WithMethod("logout", b.logout),
		acp.WithMethod("session/new", b.newSession),
		acp.WithMethod("session/fork", b.forkSession),
		acp.WithMethod("session/load", b.loadSession),
		acp.WithMethod("session/resume", b.resumeSession),
		acp.WithMethod("session/list", b.listSessions),
		acp.WithMethod("session/set_config_option", b.setConfigOption),
		acp.WithMethod("session/set_mode", b.setMode),
		acp.WithMethod("session/prompt", b.prompt),
		acp.WithMethod("mcp/message", b.mcpMessage),
		acp.WithConcurrentMethod("session/cancel", b.cancelMethod),
		acp.WithConcurrentMethod("session/close", b.closeSession),
		acp.WithMethod("session/delete", b.deleteSession),
		acp.WithNotification("session/cancel", b.cancelNotification),
		acp.WithNotification("mcp/message", b.mcpMessageNotification),
	}
}

func (b *Bridge) authenticate(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.AuthenticateParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	if err := runtimeacp.AuthenticateRaw(methodContext(ctx), b.runtime, params); err != nil {
		return nil, mapRuntimeError(err)
	}
	return map[string]any{}, nil
}

func (b *Bridge) logout(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req map[string]any
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	if err := runtimeacp.LogoutRaw(methodContext(ctx), b.runtime, params); err != nil {
		return nil, mapRuntimeError(err)
	}
	return map[string]any{}, nil
}

func (b *Bridge) newSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.NewSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.NewSessionRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) forkSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ForkSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.ForkSessionRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) loadSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.LoadSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.LoadSessionRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) resumeSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ResumeSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.ResumeSessionRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) listSessions(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ListSessionsParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	result, err := runtimeacp.ListSessions(methodContext(ctx), b.runtime, req)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	return result, nil
}

func (b *Bridge) setConfigOption(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.SetConfigOptionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.SetConfigOptionRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) setMode(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.SetModeParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.SetModeRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) prompt(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.PromptParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		return runtimeacp.PromptRaw(callCtx, b.runtime, params)
	})
}

func (b *Bridge) mcpMessage(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.MCPMessageParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	result, err := runtimeacp.MCPMessage(methodContext(ctx), b.runtime, req)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	return result, nil
}

func (b *Bridge) mcpMessageNotification(params json.RawMessage) error {
	var req runtimeacp.MCPMessageParams
	if err := json.Unmarshal(params, &req); err != nil {
		return fmt.Errorf("invalid mcp/message params: %w", err)
	}
	return runtimeacp.NotifyMCPMessage(context.Background(), b.runtime, req)
}

func (b *Bridge) cancelMethod(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CancelParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	if err := runtimeacp.Cancel(methodContext(ctx), b.runtime, req); err != nil {
		return nil, mapRuntimeError(err)
	}
	return map[string]any{"cancelled": true}, nil
}

func (b *Bridge) cancelNotification(params json.RawMessage) error {
	var req runtimeacp.CancelParams
	if err := json.Unmarshal(params, &req); err != nil {
		return fmt.Errorf("invalid session/cancel params: %w", err)
	}
	return runtimeacp.Cancel(context.Background(), b.runtime, req)
}

func (b *Bridge) closeSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CloseSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return b.callWithEvents(ctx, func(callCtx context.Context) (any, error) {
		if err := runtimeacp.CloseSessionRaw(callCtx, b.runtime, params); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	})
}

func (b *Bridge) deleteSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.DeleteSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	if err := runtimeacp.DeleteSessionRaw(methodContext(ctx), b.runtime, params); err != nil {
		return nil, mapRuntimeError(err)
	}
	return map[string]any{}, nil
}

func (b *Bridge) callWithEvents(ctx *acp.MethodContext, call func(context.Context) (any, error)) (any, *acp.RPCError) {
	type callResult struct {
		result any
		err    error
	}
	callCtx, cancelCall := context.WithCancel(methodContext(ctx))
	defer cancelCall()
	done := make(chan callResult, 1)
	go func() {
		result, err := call(callCtx)
		done <- callResult{result: result, err: err}
	}()

	for {
		select {
		case result := <-done:
			if result.err != nil {
				return nil, mapRuntimeError(result.err)
			}
			return result.result, nil
		case event, ok := <-b.runtime.Events():
			if !ok {
				return nil, mapRuntimeError(errors.New("runtime event stream closed"))
			}
			if len(event.ID) != 0 {
				type clientRequestResult struct {
					result json.RawMessage
					rpcErr *acp.RPCError
					err    error
				}
				clientDone := make(chan clientRequestResult, 1)
				go func() {
					result, rpcErr, err := ctx.RequestContext(callCtx, event.Method, eventParams(event))
					clientDone <- clientRequestResult{result: result, rpcErr: rpcErr, err: err}
				}()
				select {
				case clientResult := <-clientDone:
					if clientResult.err != nil {
						return nil, mapRuntimeError(fmt.Errorf("forward runtime client request %s: %w", string(event.ID), clientResult.err))
					}
					if err := b.runtime.Respond(context.Background(), event.ID, clientResult.result, runtimeError(clientResult.rpcErr)); err != nil {
						return nil, mapRuntimeError(fmt.Errorf("respond to runtime client request %s: %w", string(event.ID), err))
					}
				case result := <-done:
					// The runtime call owns every forwarded child request. Cancel and
					// await the forwarding goroutine before returning so RequestContext
					// abandons its pending id and queues best-effort cancellation ahead
					// of the enclosing prompt response.
					cancelCall()
					<-clientDone
					if result.err != nil {
						return nil, mapRuntimeError(result.err)
					}
					return result.result, nil
				}
				continue
			}
			if err := ctx.Notify(event.Method, eventParams(event)); err != nil {
				return nil, &acp.RPCError{Code: -32000, Message: "notification failed", Data: err.Error()}
			}
		}
	}
}

func methodContext(ctx *acp.MethodContext) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx.Context()
}

func runtimeError(rpcErr *acp.RPCError) *runtimejsonrpc.RPCError {
	if rpcErr == nil {
		return nil
	}
	var data json.RawMessage
	if rpcErr.Data != nil {
		if raw, err := json.Marshal(rpcErr.Data); err == nil {
			data = raw
		}
	}
	return &runtimejsonrpc.RPCError{
		Code:    rpcErr.Code,
		Message: rpcErr.Message,
		Data:    data,
	}
}

func eventParams(event runtimejsonrpc.Event) any {
	if len(event.Params) == 0 {
		return nil
	}
	return json.RawMessage(event.Params)
}

func decodeParams(params json.RawMessage, target any) *acp.RPCError {
	if err := json.Unmarshal(params, target); err != nil {
		return &acp.RPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
	}
	return nil
}

func mapRuntimeError(err error) *acp.RPCError {
	var rpcErr *runtimejsonrpc.RPCError
	if errors.As(err, &rpcErr) {
		var data any
		if len(rpcErr.Data) != 0 {
			data = json.RawMessage(rpcErr.Data)
		}
		return &acp.RPCError{Code: rpcErr.Code, Message: rpcErr.Message, Data: data}
	}
	return &acp.RPCError{Code: -32000, Message: "runtime bridge error", Data: err.Error()}
}
