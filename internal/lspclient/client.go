// Package lspclient is a minimal LSP client for tests and measurements.
package lspclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Client talks to an LSP server over a pair of streams.
type Client struct {
	// OnRequest answers a request sent by the server. When nil, every request gets a null result.
	OnRequest func(method string, params json.RawMessage) any
	// OnNotification receives the notifications sent by the server. It is called from the reading goroutine.
	OnNotification func(method string, params json.RawMessage)

	w       io.Writer
	mu      sync.Mutex
	nextID  int
	pending map[int]chan response
	// done is closed when the server closes its output. err holds the reason.
	done  chan struct{}
	err   error
	start func()
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type message struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	response
}

// New returns a client that reads the output of the server from r and writes its input to w.
// Set OnRequest and OnNotification before calling Start.
func New(r io.Reader, w io.Writer) *Client {
	c := &Client{w: w, pending: map[int]chan response{}, done: make(chan struct{})}
	c.start = func() { go c.read(bufio.NewReaderSize(r, 1<<16)) }
	return c
}

// Start begins reading the output of the server.
func (c *Client) Start() { c.start() }

func (c *Client) read(r *bufio.Reader) {
	defer close(c.done)
	for {
		msg, err := readMessage(r)
		if err != nil {
			c.err = err
			return
		}
		switch {
		case msg.Method != "" && msg.ID != nil:
			var result any
			if c.OnRequest != nil {
				result = c.OnRequest(msg.Method, msg.Params)
			}
			_ = c.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		case msg.Method != "":
			if c.OnNotification != nil {
				c.OnNotification(msg.Method, msg.Params)
			}
		case msg.ID != nil:
			id, err := strconv.Atoi(string(*msg.ID))
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg.response
			}
		}
	}
}

func readMessage(r *bufio.Reader) (*message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
			if length, err = strconv.Atoi(strings.TrimSpace(value)); err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", value, err)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	msg := &message{}
	if err := json.Unmarshal(body, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *Client) write(v any) error {
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

// Call sends a request and decodes the result into result, which may be nil to discard it.
func (c *Client) Call(method string, params, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, res.Error.Message, res.Error.Code)
		}
		if result == nil || len(res.Result) == 0 {
			return nil
		}
		return json.Unmarshal(res.Result, result)
	case <-c.done:
		return fmt.Errorf("%s: the server closed its output: %v", method, c.err)
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
