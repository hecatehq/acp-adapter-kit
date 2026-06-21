package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

const maxMessageBytes = 1024 * 1024

type RPCError = sdk.RequestError

type AdapterInfo struct {
	Name         string
	Title        string
	Version      string
	Capabilities Capabilities
}

type Capabilities struct {
	Images                bool
	EmbeddedContext       bool
	MCPHTTP               bool
	MCPSSE                bool
	LoadSession           bool
	SessionList           bool
	SessionResume         bool
	SessionClose          bool
	SessionDelete         bool
	AdditionalDirectories bool
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
}

type Server struct {
	info              AdapterInfo
	initialize        any
	initializeHandler InitializeHandler
	authMethods       []AuthMethod
	authLogout        bool
	methods           map[string]MethodHandler
	concurrent        map[string]bool
	notifications     map[string]NotificationHandler
}

type Option func(*Server)

type InitializeHandler func(params json.RawMessage) (any, *RPCError)

type MethodHandler func(ctx *MethodContext, params json.RawMessage) (any, *RPCError)

type NotificationHandler func(params json.RawMessage) error

type MethodContext struct {
	conn *connection
	ctx  context.Context
}

func (c *MethodContext) Context() context.Context {
	if c == nil || c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

func (c *MethodContext) Notify(method string, params any) error {
	if c == nil || c.conn == nil {
		return errors.New("method context is not connected")
	}
	return c.conn.write(serverNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

func (c *MethodContext) Request(method string, params any) (json.RawMessage, *RPCError, error) {
	if c == nil || c.conn == nil {
		return nil, nil, errors.New("method context is not connected")
	}
	id, resultCh, err := c.conn.registerRequest()
	if err != nil {
		return nil, nil, err
	}
	if err := c.conn.write(serverRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}); err != nil {
		c.conn.removeRequest(id)
		return nil, nil, err
	}
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, nil, result.err
		}
		return append(json.RawMessage(nil), result.result...), result.rpcErr, nil
	case <-c.Context().Done():
		c.conn.removeRequest(id)
		return nil, nil, c.Context().Err()
	}
}

func WithMethod(method string, handler MethodHandler) Option {
	return func(s *Server) {
		s.methods[method] = handler
	}
}

func WithConcurrentMethod(method string, handler MethodHandler) Option {
	return func(s *Server) {
		s.methods[method] = handler
		s.concurrent[method] = true
	}
}

func WithNotification(method string, handler NotificationHandler) Option {
	return func(s *Server) {
		s.notifications[method] = handler
	}
}

func WithInitializeResult(result any) Option {
	return func(s *Server) {
		s.initialize = result
	}
}

func WithInitializeHandler(handler InitializeHandler) Option {
	return func(s *Server) {
		s.initializeHandler = handler
	}
}

func WithAuthMethods(methods []AuthMethod) Option {
	return func(s *Server) {
		s.authMethods = append([]AuthMethod(nil), methods...)
	}
}

func WithAuthLogout() Option {
	return func(s *Server) {
		s.authLogout = true
	}
}

func NewServer(info AdapterInfo, opts ...Option) *Server {
	server := &Server{
		info:          info,
		methods:       map[string]MethodHandler{},
		concurrent:    map[string]bool{},
		notifications: map[string]NotificationHandler{},
	}
	for _, opt := range opts {
		opt(server)
	}
	return server
}

func (s *Server) Serve(input io.Reader, output io.Writer) error {
	if input == nil {
		return errors.New("input is required")
	}
	if output == nil {
		return errors.New("output is required")
	}

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	conn := &connection{
		scanner:   scanner,
		encoder:   json.NewEncoder(output),
		pending:   map[string]chan clientResponse{},
		responses: map[string]clientResponse{},
		active:    map[string]context.CancelFunc{},
		cancelled: map[string]struct{}{},
	}
	handlerErr := make(chan error, 1)
	sendHandlerErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case handlerErr <- err:
		default:
		}
	}
	methods := newMethodQueue()
	methodsDone := make(chan struct{})
	go func() {
		defer close(methodsDone)
		for {
			req, ok := methods.pop()
			if !ok {
				return
			}
			ctx, finish := conn.beginInboundRequest(req.ID)
			sendHandlerErr(conn.write(s.handle(ctx, req)))
			finish()
		}
	}()
	var concurrent sync.WaitGroup
	runConcurrent := func(req request) {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			ctx, finish := conn.beginInboundRequest(req.ID)
			sendHandlerErr(conn.write(s.handle(ctx, req)))
			finish()
		}()
	}

	for {
		msg, ok, err := conn.readMessage()
		if err != nil {
			var parseErr parseMessageError
			if !errors.As(err, &parseErr) {
				conn.finishRequests(err)
				return err
			}
			if err := conn.write(errorResponse(nil, -32700, "parse error", parseErr.Err.Error())); err != nil {
				conn.finishRequests(err)
				return err
			}
			continue
		}
		if !ok {
			break
		}
		if msg.ID != nil && msg.Method == "" {
			conn.deliverResponse(msg)
			continue
		}
		if msg.Method == "" {
			continue
		}
		if msg.ID == nil && msg.Method == "$/cancel_request" {
			conn.cancelInboundRequest(msg.Params)
			continue
		}
		if msg.ID == nil {
			if handler := s.notifications[msg.Method]; handler != nil {
				if err := handler(msg.Params); err != nil {
					conn.finishRequests(err)
					return err
				}
			}
			continue
		}
		req := request{
			JSONRPC: msg.JSONRPC,
			ID:      msg.ID,
			Method:  msg.Method,
			Params:  msg.Params,
		}
		if s.concurrent[msg.Method] {
			runConcurrent(req)
			continue
		}
		methods.push(req)
	}
	methods.close()
	conn.finishRequests(io.EOF)
	select {
	case err := <-handlerErr:
		return err
	case <-methodsDone:
	}
	concurrentDone := make(chan struct{})
	go func() {
		concurrent.Wait()
		close(concurrentDone)
	}()
	select {
	case err := <-handlerErr:
		return err
	case <-concurrentDone:
	}
	select {
	case err := <-handlerErr:
		return err
	default:
		return scanner.Err()
	}
}

func (s *Server) handle(ctx *MethodContext, req request) response {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return errorResponse(req.ID, -32600, "invalid request", "jsonrpc must be 2.0")
	}

	switch req.Method {
	case "initialize":
		if s.initializeHandler != nil {
			result, rpcErr := s.initializeHandler(req.Params)
			if rpcErr != nil {
				return response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
			}
			return resultResponse(req.ID, result)
		}
		if s.initialize != nil {
			return resultResponse(req.ID, s.initialize)
		}
		return resultResponse(req.ID, initializeResult{
			ProtocolVersion: sdk.ProtocolVersionNumber,
			AgentCapabilities: agentCapabilities{
				LoadSession: s.info.Capabilities.LoadSession,
				PromptCapabilities: promptCapabilities{
					Image:           s.info.Capabilities.Images,
					EmbeddedContext: s.info.Capabilities.EmbeddedContext,
				},
				MCPCapabilities: mcpCapabilities{
					HTTP: s.info.Capabilities.MCPHTTP,
					SSE:  s.info.Capabilities.MCPSSE,
				},
				Auth:                authCapabilitiesFromServer(s),
				SessionCapabilities: sessionCapabilitiesFromInfo(s.info),
			},
			AgentInfo: agentInfo{
				Name:    s.info.Name,
				Title:   s.info.Title,
				Version: s.info.Version,
			},
			AuthMethods: append([]AuthMethod{}, s.authMethods...),
		})
	case "authenticate",
		"document/didChange",
		"document/didClose",
		"document/didFocus",
		"document/didOpen",
		"document/didSave",
		"logout",
		"mcp/message",
		"nes/accept",
		"nes/close",
		"nes/reject",
		"nes/start",
		"nes/suggest",
		"providers/disable",
		"providers/list",
		"providers/set",
		"session/new",
		"session/fork",
		"session/load",
		"session/resume",
		"session/list",
		"session/set_config_option",
		"session/set_mode",
		"session/prompt",
		"session/cancel",
		"session/close",
		"session/delete":
		if handler := s.methods[req.Method]; handler != nil {
			result, rpcErr := handler(ctx, req.Params)
			if rpcErr != nil {
				return response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
			}
			return resultResponse(req.ID, result)
		}
		return errorResponse(req.ID, -32004, "not implemented", fmt.Sprintf("%s is not implemented in this scaffold", req.Method))
	default:
		if handler := s.methods[req.Method]; handler != nil {
			result, rpcErr := handler(ctx, req.Params)
			if rpcErr != nil {
				return response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
			}
			return resultResponse(req.ID, result)
		}
		return errorResponse(req.ID, -32601, "method not found", req.Method)
	}
}

type request struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type message struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type serverNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type serverRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

func resultResponse(id *json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id *json.RawMessage, code int, message string, data any) response {
	return response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message, Data: data}}
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

type agentCapabilities struct {
	LoadSession         bool                 `json:"loadSession,omitempty"`
	PromptCapabilities  promptCapabilities   `json:"promptCapabilities"`
	MCPCapabilities     mcpCapabilities      `json:"mcpCapabilities"`
	Auth                *authCapabilities    `json:"auth,omitempty"`
	SessionCapabilities *sessionCapabilities `json:"sessionCapabilities,omitempty"`
}

type authCapabilities struct {
	Logout *emptyObject `json:"logout,omitempty"`
}

type sessionCapabilities struct {
	List                  *emptyObject `json:"list,omitempty"`
	Resume                *emptyObject `json:"resume,omitempty"`
	Close                 *emptyObject `json:"close,omitempty"`
	Delete                *emptyObject `json:"delete,omitempty"`
	AdditionalDirectories *emptyObject `json:"additionalDirectories,omitempty"`
}

type emptyObject struct{}

type promptCapabilities struct {
	Image           bool `json:"image"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse,omitempty"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

func authCapabilitiesFromServer(s *Server) *authCapabilities {
	if s == nil || !s.authLogout {
		return nil
	}
	return &authCapabilities{Logout: &emptyObject{}}
}

func sessionCapabilitiesFromInfo(info AdapterInfo) *sessionCapabilities {
	caps := info.Capabilities
	out := &sessionCapabilities{}
	if caps.SessionList {
		out.List = &emptyObject{}
	}
	if caps.SessionResume {
		out.Resume = &emptyObject{}
	}
	if caps.SessionClose {
		out.Close = &emptyObject{}
	}
	if caps.SessionDelete {
		out.Delete = &emptyObject{}
	}
	if caps.AdditionalDirectories {
		out.AdditionalDirectories = &emptyObject{}
	}
	if out.List == nil &&
		out.Resume == nil &&
		out.Close == nil &&
		out.Delete == nil &&
		out.AdditionalDirectories == nil {
		return nil
	}
	return out
}

type connection struct {
	scanner   *bufio.Scanner
	encoder   *json.Encoder
	writeMu   sync.Mutex
	requestMu sync.Mutex
	nextID    int64
	pending   map[string]chan clientResponse
	responses map[string]clientResponse
	active    map[string]context.CancelFunc
	cancelled map[string]struct{}
	closed    bool
	closeErr  error
}

type clientResponse struct {
	result json.RawMessage
	rpcErr *RPCError
	err    error
}

type cancelRequestParams struct {
	RequestID json.RawMessage `json:"requestId"`
}

type methodQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
	items  []request
}

func newMethodQueue() *methodQueue {
	q := &methodQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *methodQueue) push(req request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, req)
	q.cond.Signal()
}

func (q *methodQueue) pop() (request, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return request{}, false
	}
	req := q.items[0]
	copy(q.items, q.items[1:])
	q.items[len(q.items)-1] = request{}
	q.items = q.items[:len(q.items)-1]
	return req, true
}

func (q *methodQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

func (c *connection) readMessage() (message, bool, error) {
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			return message{}, false, parseMessageError{Err: err}
		}
		return msg, true, nil
	}
	if err := c.scanner.Err(); err != nil {
		return message{}, false, err
	}
	return message{}, false, nil
}

func (c *connection) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(value)
}

func (c *connection) registerRequest() (json.RawMessage, <-chan clientResponse, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.nextID++
	raw, err := json.Marshal(fmt.Sprintf("server-%d", c.nextID))
	if err != nil {
		panic(err)
	}
	key := string(raw)
	resultCh := make(chan clientResponse, 1)
	if response, ok := c.responses[key]; ok {
		delete(c.responses, key)
		resultCh <- response
		return raw, resultCh, nil
	}
	if c.closed {
		if c.closeErr != nil {
			return nil, nil, c.closeErr
		}
		return nil, nil, io.EOF
	}
	c.pending[key] = resultCh
	return raw, resultCh, nil
}

func (c *connection) removeRequest(id json.RawMessage) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	delete(c.pending, string(id))
}

func (c *connection) beginInboundRequest(id *json.RawMessage) (*MethodContext, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	if id == nil {
		return &MethodContext{conn: c, ctx: ctx}, cancel
	}
	key := string(*id)
	c.requestMu.Lock()
	if _, ok := c.cancelled[key]; ok {
		delete(c.cancelled, key)
		cancel()
	} else {
		c.active[key] = cancel
	}
	c.requestMu.Unlock()
	return &MethodContext{conn: c, ctx: ctx}, func() {
		c.requestMu.Lock()
		delete(c.active, key)
		delete(c.cancelled, key)
		c.requestMu.Unlock()
		cancel()
	}
}

func (c *connection) cancelInboundRequest(params json.RawMessage) {
	var req cancelRequestParams
	if err := json.Unmarshal(params, &req); err != nil || len(req.RequestID) == 0 {
		return
	}
	c.requestMu.Lock()
	cancel := c.active[string(req.RequestID)]
	if cancel == nil {
		c.cancelled[string(req.RequestID)] = struct{}{}
	}
	c.requestMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *connection) deliverResponse(msg message) {
	if msg.ID == nil {
		return
	}
	response := clientResponse{
		result: append(json.RawMessage(nil), msg.Result...),
		rpcErr: msg.Error,
	}
	key := string(*msg.ID)
	c.requestMu.Lock()
	resultCh := c.pending[key]
	if resultCh != nil {
		delete(c.pending, key)
	}
	if resultCh == nil {
		c.responses[key] = response
	}
	c.requestMu.Unlock()
	if resultCh != nil {
		resultCh <- response
	}
}

func (c *connection) finishRequests(err error) {
	if err == nil {
		err = io.EOF
	}
	c.requestMu.Lock()
	if c.closed {
		c.requestMu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	pending := c.pending
	c.pending = map[string]chan clientResponse{}
	c.requestMu.Unlock()
	for _, resultCh := range pending {
		resultCh <- clientResponse{err: err}
	}
}

type parseMessageError struct {
	Err error
}

func (e parseMessageError) Error() string {
	return e.Err.Error()
}

func (e parseMessageError) Unwrap() error {
	return e.Err
}
