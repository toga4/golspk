package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// request is a JSON-RPC message received from the client.
type request struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result"`
	Error   *rpcError        `json:"error,omitempty"`
}

type outgoing struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// conn reads and writes LSP messages framed with Content-Length headers.
type conn struct {
	r      *bufio.Reader
	mu     sync.Mutex
	w      io.Writer
	nextID int
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: bufio.NewReaderSize(r, 1<<16), w: w}
}

func (c *conn) read() (*request, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", value, err)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	req := &request{}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, err
	}
	return req, nil
}

func (c *conn) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

func (c *conn) reply(id *json.RawMessage, result any, rerr *rpcError) error {
	if rerr != nil {
		result = nil
	}
	return c.write(response{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
}

// notify sends a notification from the server to the client.
func (c *conn) notify(method string, params any) error {
	return c.write(outgoing{JSONRPC: "2.0", Method: method, Params: params})
}

// call sends a request from the server to the client. It does not wait for the response.
func (c *conn) call(method string, params any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return c.write(outgoing{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
}
