// Package cdp implements the small subset of the Chrome DevTools Protocol used
// by muse2api, including a minimal RFC 6455 WebSocket client.
package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrClosed = errors.New("cdp: connection closed")

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type envelope struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type response struct {
	result json.RawMessage
	err    error
}

// Client is a synchronous CDP connection. Send serializes request IDs while a
// reader goroutine dispatches responses; protocol events are ignored.
type Client struct {
	conn net.Conn
	br   *bufio.Reader

	writeGate chan struct{}

	mu      sync.Mutex
	nextID  int
	pending map[int]chan response

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial connects to a ws:// DevTools endpoint.
func Dial(rawURL string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cdp: parse websocket url: %w", err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("cdp: unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("cdp: dial %s: %w", host, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cdp: websocket handshake: %w", err)
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	status, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cdp: websocket handshake: %w", err)
	}
	if !strings.Contains(status, " 101 ") {
		_ = conn.Close()
		return nil, fmt.Errorf("cdp: websocket handshake rejected: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("cdp: websocket handshake: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})

	c := &Client{
		conn:      conn,
		writeGate: make(chan struct{}, 1),
		br:        br,
		pending:   map[int]chan response{},
		closed:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Send performs one CDP call and returns the raw result object.
func (c *Client) Send(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	return c.SendContext(context.Background(), method, params, timeout)
}

func (c *Client) SendContext(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if params == nil {
		params = map[string]any{}
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer c.forget(id)
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := c.writeContext(ctx, 0x1, payload); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-ctx.Done():
		return nil, fmt.Errorf("cdp: %s: %w", method, ctx.Err())
	case <-c.closed:
		return nil, ErrClosed
	}
}

func (c *Client) forget(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Evaluate runs a JavaScript expression and returns its by-value result.
func (c *Client) Evaluate(expression string, awaitPromise bool, timeout time.Duration) (any, error) {
	return c.EvaluateContext(context.Background(), expression, awaitPromise, timeout)
}
func (c *Client) EvaluateContext(ctx context.Context, expression string, awaitPromise bool, timeout time.Duration) (any, error) {
	raw, err := c.SendContext(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	}, timeout)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cdp: decode evaluate result: %w", err)
	}
	if out.ExceptionDetails != nil {
		return nil, nil
	}
	return out.Result.Value, nil
}

// EvaluateString is a convenience wrapper for expressions expected to yield a
// string.
func (c *Client) EvaluateString(expression string, awaitPromise bool, timeout time.Duration) (string, error) {
	return c.EvaluateStringContext(context.Background(), expression, awaitPromise, timeout)
}
func (c *Client) EvaluateStringContext(ctx context.Context, expression string, awaitPromise bool, timeout time.Duration) (string, error) {
	v, err := c.EvaluateContext(ctx, expression, awaitPromise, timeout)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// Close terminates the connection and fails all pending calls. Pending
// channels are deliberately not closed: the reader may still be dispatching a
// response, and Send wakes on the closed signal instead.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
		c.mu.Lock()
		c.pending = map[int]chan response{}
		c.mu.Unlock()
	})
}

func (c *Client) readLoop() {
	defer c.Close()
	var message []byte
	var dataOpcode byte
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // close
			return
		case 0x9: // ping
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2:
			if message != nil {
				message = nil
			}
			dataOpcode = opcode
			message = append(message, payload...)
		case 0x0:
			if dataOpcode == 0 {
				continue
			}
			message = append(message, payload...)
		default:
			continue
		}
		if !fin {
			continue
		}
		if dataOpcode == 0x1 && len(message) > 0 {
			c.dispatch(message)
		}
		message = nil
		dataOpcode = 0
	}
}

func (c *Client) dispatch(data []byte) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	if env.ID == 0 {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[env.ID]
	if ok {
		delete(c.pending, env.ID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if env.Error != nil {
		ch <- response{err: fmt.Errorf("cdp: %s", env.Error.Message)}
	} else {
		ch <- response{result: env.Result}
	}
}

func (c *Client) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err = io.ReadFull(c.br, header); err != nil {
		return
	}
	fin = header[0]&0x80 != 0
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var buf [2]byte
		if _, err = io.ReadFull(c.br, buf[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint16(buf[:]))
	case 127:
		var buf [8]byte
		if _, err = io.ReadFull(c.br, buf[:]); err != nil {
			return
		}
		u := binary.BigEndian.Uint64(buf[:])
		if u > 1<<31 {
			err = fmt.Errorf("cdp: websocket frame too large (%d bytes)", u)
			return
		}
		length = int64(u)
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err = io.ReadFull(c.br, mask); err != nil {
			return
		}
	}
	payload = make([]byte, int(length))
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func (c *Client) writeFrame(opcode byte, payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.writeContext(ctx, opcode, payload)
}
func (c *Client) writeContext(ctx context.Context, opcode byte, payload []byte) error {
	select {
	case c.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
	defer func() { <-c.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	defer c.conn.SetWriteDeadline(time.Time{})

	header := make([]byte, 2, 14)
	header[0] = 0x80 | opcode
	length := len(payload)
	switch {
	case length < 126:
		header[1] = 0x80 | byte(length)
	case length < 65536:
		header[1] = 0x80 | 126
		header = binary.BigEndian.AppendUint16(header, uint16(length))
	default:
		header[1] = 0x80 | 127
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	masked := make([]byte, length)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if err := writeAll(c.conn, header); err != nil {
		c.Close()
		return err
	}
	err := writeAll(c.conn, masked)
	if err != nil {
		c.Close()
	}
	return err
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// HTTPJSON fetches a local DevTools HTTP endpoint.
func HTTPJSON(rawURL string, timeout time.Duration) (map[string]any, error) {
	return HTTPContext(context.Background(), http.MethodGet, rawURL, timeout)
}
func HTTPPutJSON(rawURL string, timeout time.Duration) (map[string]any, error) {
	return HTTPContext(context.Background(), http.MethodPut, rawURL, timeout)
}
func HTTPContext(ctx context.Context, method, rawURL string, timeout time.Duration) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
