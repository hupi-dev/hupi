// Command hupi-mcp is a stdio bridge to a HUPI gateway's /mcp endpoint,
// for MCP clients that only launch local processes (Claude Desktop, or
// any client configured with a "command" instead of a "url"). See
// docs/MCP.md.
//
// It is a byte-level relay: each newline-delimited JSON-RPC message from
// stdin is POSTed to the gateway, and each JSON-RPC message in the
// response is written to stdout. It never interprets tools or results,
// so the client and gateway negotiate protocol and capabilities end to
// end, and new gateway tools need no bridge update. It holds no database
// credentials, encryption keys or LLM keys: only the gateway URL and the
// user's own HUPI API key. Only protocol messages go to stdout; logs go
// to stderr.
//
// Environment:
//
//	HUPI_BASE_URL     gateway base URL (default http://127.0.0.1:8787)
//	HUPI_API_KEY      HUPI API key, sent as a bearer token (omit on Tier 1/2)
//	HUPI_TEAM_ID      use the team workspace /v1/team/{id}/mcp instead of /mcp
//	HUPI_MCP_TIMEOUT  per-request timeout, a Go duration (default 2m)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBaseURL = "http://127.0.0.1:8787"
	defaultTimeout = 2 * time.Minute
	maxMessage     = 8 << 20 // largest stdin line or response accepted
	maxInFlight    = 8
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Error("hupi-mcp: bad configuration", "error", err)
		os.Exit(2)
	}
	if cfg.insecureKey() {
		log.Warn("hupi-mcp: sending an API key over plain http to a non-loopback host; use https", "endpoint", cfg.endpoint)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := newRelay(cfg, &http.Client{Timeout: cfg.timeout}, os.Stdout, log)
	if err := r.run(ctx, os.Stdin); err != nil {
		log.Error("hupi-mcp: stopped", "error", err)
		os.Exit(1)
	}
}

type config struct {
	endpoint string
	apiKey   string
	timeout  time.Duration
}

func configFromEnv(getenv func(string) string) (config, error) {
	base := strings.TrimRight(strings.TrimSpace(getenv("HUPI_BASE_URL")), "/")
	if base == "" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, fmt.Errorf("HUPI_BASE_URL %q must be an http(s) URL like %s", base, defaultBaseURL)
	}
	endpoint := base + "/mcp"
	if team := strings.TrimSpace(getenv("HUPI_TEAM_ID")); team != "" {
		endpoint = base + "/v1/team/" + url.PathEscape(team) + "/mcp"
	}
	timeout := defaultTimeout
	if v := getenv("HUPI_MCP_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("HUPI_MCP_TIMEOUT %q must be a positive duration like 90s", v)
		}
		timeout = d
	}
	return config{endpoint: endpoint, apiKey: strings.TrimSpace(getenv("HUPI_API_KEY")), timeout: timeout}, nil
}

func (c config) insecureKey() bool {
	u, err := url.Parse(c.endpoint)
	if err != nil || c.apiKey == "" || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

type relay struct {
	cfg    config
	client *http.Client
	log    *slog.Logger

	outMu sync.Mutex
	out   io.Writer

	mu              sync.Mutex
	protocolVersion string
	sessionID       string
}

func newRelay(cfg config, client *http.Client, out io.Writer, log *slog.Logger) *relay {
	return &relay{cfg: cfg, client: client, out: out, log: log}
}

// run relays until stdin closes (or ctx is cancelled), then waits for
// in-flight requests.
func (r *relay) run(ctx context.Context, in io.Reader) error {
	type lineOrErr struct {
		line []byte
		err  error
	}
	lines := make(chan lineOrErr)
	go func() {
		br := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := readLine(br)
			select {
			case lines <- lineOrErr{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var next lineOrErr
		select {
		case next = <-lines:
		case <-ctx.Done():
			return nil
		}
		if msg := bytes.TrimSpace(next.line); len(msg) > 0 {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r.forward(ctx, msg)
			}()
		}
		if errors.Is(next.err, io.EOF) {
			return nil
		}
		if next.err != nil {
			return next.err
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxMessage {
			// Drain the rest of the oversized line and report it.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			return nil, fmt.Errorf("stdin message exceeds %d bytes", maxMessage)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// envelope is the part of a JSON-RPC message the relay looks at.
type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (r *relay) forward(ctx context.Context, msg []byte) {
	var env envelope
	isBatch := len(msg) > 0 && msg[0] == '['
	if !isBatch {
		if err := json.Unmarshal(msg, &env); err != nil {
			r.writeError(nil, -32700, "parse error: stdin line is not a JSON-RPC message")
			return
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.endpoint, bytes.NewReader(msg))
	if err != nil {
		r.fail(env, fmt.Sprintf("build request: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "hupi-mcp")
	if r.cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.apiKey)
	}
	r.mu.Lock()
	if r.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", r.protocolVersion)
	}
	if r.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", r.sessionID)
	}
	r.mu.Unlock()

	resp, err := r.client.Do(req)
	if err != nil {
		r.log.Warn("hupi-mcp: gateway unreachable", "endpoint", r.cfg.endpoint, "error", err)
		r.fail(env, fmt.Sprintf("HUPI gateway unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		r.mu.Lock()
		r.sessionID = sid
		r.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted:
		return // a notification or response the gateway acknowledged
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		r.log.Warn("hupi-mcp: gateway rejected request", "status", resp.StatusCode, "method", env.Method)
		r.fail(env, gatewayErrorMessage(resp.StatusCode, body))
		return
	}

	body := io.LimitReader(resp.Body, maxMessage)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = readSSE(body, func(data []byte) { r.emit(env, data) })
	} else {
		var data []byte
		if data, err = io.ReadAll(body); err == nil && len(bytes.TrimSpace(data)) > 0 {
			r.emit(env, data)
		}
	}
	if err != nil {
		r.fail(env, fmt.Sprintf("reading HUPI gateway response: %v", err))
	}
}

// emit writes one response message, noting the negotiated protocol
// version when it's the initialize result.
func (r *relay) emit(req envelope, data []byte) {
	if req.Method == "initialize" {
		var res struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(data, &res) == nil && res.Result.ProtocolVersion != "" {
			r.mu.Lock()
			r.protocolVersion = res.Result.ProtocolVersion
			r.mu.Unlock()
		}
	}
	r.writeLine(data)
}

// writeLine writes data as one compact stdout line; stdio framing
// forbids embedded newlines.
func (r *relay) writeLine(data []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		r.log.Warn("hupi-mcp: dropping non-JSON gateway output", "error", err)
		return
	}
	buf.WriteByte('\n')
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if _, err := r.out.Write(buf.Bytes()); err != nil {
		r.log.Error("hupi-mcp: write to stdout failed", "error", err)
	}
}

// fail answers a request with a JSON-RPC error. Notifications (no id)
// get no reply, per JSON-RPC, so the failure is only logged.
func (r *relay) fail(req envelope, message string) {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		r.log.Warn("hupi-mcp: notification not delivered", "method", req.Method, "reason", message)
		return
	}
	r.writeError(req.ID, -32000, message)
}

func (r *relay) writeError(id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	data, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, message}})
	r.writeLine(data)
}

func gatewayErrorMessage(status int, body []byte) string {
	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	hint := ""
	switch status {
	case http.StatusUnauthorized:
		hint = " (check HUPI_API_KEY)"
	case http.StatusForbidden:
		hint = " (not allowed: wrong team, or blocked by the gateway's Origin/Host guard)"
	case http.StatusNotFound:
		hint = " (no MCP endpoint at this URL: check HUPI_BASE_URL and HUPI_TEAM_ID, and that the gateway has HUPI_ENABLE_MCP on)"
	}
	msg := fmt.Sprintf("HUPI gateway returned %d %s%s", status, http.StatusText(status), hint)
	if detail != "" {
		msg += ": " + detail
	}
	return msg
}

// readSSE calls emit with each event's data (multi-line data joined with
// newlines, per the SSE spec).
func readSSE(body io.Reader, emit func([]byte)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var data [][]byte
	flush := func() {
		if len(data) > 0 {
			emit(bytes.Join(data, []byte("\n")))
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(line, []byte("data:"))
			v = bytes.TrimPrefix(v, []byte(" "))
			data = append(data, append([]byte(nil), v...))
		}
	}
	flush()
	return sc.Err()
}
