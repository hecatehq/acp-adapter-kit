package acptest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
)

type Client struct {
	t      testing.TB
	server *acp.Server
	nextID int
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acp.RPCError   `json:"error,omitempty"`
}

func NewClient(t testing.TB, server *acp.Server) *Client {
	t.Helper()
	return &Client{t: t, server: server}
}

func (c *Client) Request(method string, params any) Response {
	c.t.Helper()
	c.nextID++
	return c.RequestWithID(c.nextID, method, params)
}

func (c *Client) RequestWithID(id any, method string, params any) Response {
	c.t.Helper()

	responses := c.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if len(responses) != 1 {
		c.t.Fatalf("got %d responses, want 1", len(responses))
	}
	return responses[0]
}

func (c *Client) Notify(method string, params any) {
	c.t.Helper()

	responses := c.Send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if len(responses) != 0 {
		c.t.Fatalf("notification produced %d responses, want 0", len(responses))
	}
}

func (c *Client) Send(envelope any) []Response {
	c.t.Helper()

	var input bytes.Buffer
	if err := json.NewEncoder(&input).Encode(envelope); err != nil {
		c.t.Fatalf("encode request: %v", err)
	}
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(input.Bytes(), &request); err != nil {
		c.t.Fatalf("decode request: %v", err)
	}
	if len(request.ID) == 0 {
		return c.sendStatic(input.Bytes())
	}
	return c.sendUntilResponse(input.Bytes(), request.ID)
}

func (c *Client) sendStatic(input []byte) []Response {
	c.t.Helper()

	var output bytes.Buffer
	if err := c.server.Serve(bytes.NewReader(input), &output); err != nil {
		c.t.Fatalf("serve request: %v", err)
	}
	return decodeResponses(c.t, output.Bytes())
}

func (c *Client) sendUntilResponse(input []byte, requestID json.RawMessage) []Response {
	c.t.Helper()

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		err := c.server.Serve(inputReader, outputWriter)
		_ = outputWriter.Close()
		serveDone <- err
	}()
	type decodedResponse struct {
		response Response
		err      error
	}
	decoded := make(chan decodedResponse)
	stop := make(chan struct{})
	go func() {
		defer close(decoded)
		decoder := json.NewDecoder(outputReader)
		for {
			var response Response
			if err := decoder.Decode(&response); err != nil {
				if err != io.EOF {
					select {
					case decoded <- decodedResponse{err: err}:
					case <-stop:
					}
				}
				return
			}
			select {
			case decoded <- decodedResponse{response: response}:
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		_ = inputWriter.Close()
		_ = outputReader.Close()
	}()

	if _, err := inputWriter.Write(input); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
	responses := make([]Response, 0, 1)
	for {
		result, ok := <-decoded
		if !ok {
			err := <-serveDone
			if err != nil {
				c.t.Fatalf("serve request: %v", err)
			}
			c.t.Fatalf("server closed before response %s", requestID)
		}
		if result.err != nil {
			c.t.Fatalf("decode response: %v", result.err)
		}
		responses = append(responses, result.response)
		if result.response.Method == "" && bytes.Equal(result.response.ID, requestID) {
			break
		}
	}
	if err := inputWriter.Close(); err != nil {
		c.t.Fatalf("close input: %v", err)
	}
	for result := range decoded {
		if result.err != nil {
			c.t.Fatalf("decode response: %v", result.err)
		}
		responses = append(responses, result.response)
	}
	if err := <-serveDone; err != nil {
		c.t.Fatalf("serve request: %v", err)
	}
	return responses
}

func (c *Client) SendRaw(raw string) []Response {
	c.t.Helper()

	return c.sendStatic([]byte(raw))
}

func (r Response) ResultInto(t testing.TB, target any) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("response has error: %+v", *r.Error)
	}
	if len(r.Result) == 0 {
		t.Fatal("response has no result")
	}
	if err := json.Unmarshal(r.Result, target); err != nil {
		t.Fatalf("decode result: %v\n%s", err, string(r.Result))
	}
}

func (r Response) ParamsInto(t testing.TB, target any) {
	t.Helper()
	if r.Method == "" {
		t.Fatalf("response is not a notification: %#v", r)
	}
	if len(r.Params) == 0 {
		t.Fatal("notification has no params")
	}
	if err := json.Unmarshal(r.Params, target); err != nil {
		t.Fatalf("decode params: %v\n%s", err, string(r.Params))
	}
}

func decodeResponses(t testing.TB, output []byte) []Response {
	t.Helper()

	var responses []Response
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var response Response
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v\n%s", err, scanner.Text())
		}
		responses = append(responses, response)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan responses: %v", err)
	}
	return responses
}
