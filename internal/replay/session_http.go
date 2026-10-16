package replay

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// A replay over Streamable HTTP posts every recorded client frame to the
// server, through a recording proxy of its own, so the capture it leaves is
// written by the same code as a live HTTP session. The server answers each POST
// with a JSON body, an event stream, or a bare 202, and every message in any of
// them reaches the engine as it would have reached the client.
//
// Requests go out concurrently, as an HTTP client sends them, and a slow one
// does not hold back the next. Notifications and answers to the server go out
// one at a time, waiting for the server to take each, which keeps
// notifications/initialized ahead of everything after it the way the
// recording had it. A 2026 request is cancelled by closing the stream its
// answer comes on, which is all that revision has a client over HTTP do.

// startHTTP starts the server when there is a command, puts a recording proxy
// in front of the target, and returns what ends the session.
func (e *engine) startHTTP(ctx context.Context, rec *Recording, opts SessionOptions) (func(), error) {
	target, err := url.Parse(opts.Target)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("replay: %q is not an http or https URL to replay against", opts.Target)
	}

	// Started here or already running, the server has to be listening before
	// the first frame, or every POST meets a refusal and the replay reads as a
	// server that failed every call rather than one that was never there.
	stopServer, err := e.startServer(ctx, target, opts)
	if err != nil {
		return nil, err
	}

	handler, err := proxy.NewHTTPHandler(proxy.HTTPConfig{
		Target: opts.Target, Label: opts.Label, SessionID: opts.SessionID, Sink: opts.Sink,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// The path and query are the target's own, since the proxy forwards the
	// query the client sent and the client here is the replay.
	endpoint := url.URL{Scheme: "http", Host: ln.Addr().String(), Path: target.Path, RawQuery: target.RawQuery}
	streams, stopStreams := context.WithCancel(ctx)
	side := &httpSide{
		e:        e,
		ctx:      streams,
		endpoint: endpoint.String(),
		headers:  opts.Headers,
		listen:   rec.listens,
	}
	e.side = side

	return func() {
		// Long-lived streams, a subscription or a GET, end with the session.
		// Everything the replay waited for has been waited for by now.
		stopStreams()
		side.wg.Wait()
		side.endSession(ctx)
		shutdown, cancel := context.WithTimeout(context.Background(), stopGrace)
		_ = srv.Shutdown(shutdown)
		cancel()
		if stopServer != nil {
			stopServer()
		}
	}, nil
}

// startServer runs the server command, when there is one, and waits for the
// target to accept connections, or for the server to exit, or for StartWait to
// pass. A target with no command is a server some earlier step started, which
// may still be on its way up.
func (e *engine) startServer(ctx context.Context, target *url.URL, opts SessionOptions) (func(), error) {
	address := target.Host
	if target.Port() == "" {
		port := "80"
		if target.Scheme == "https" {
			port = "443"
		}
		address = net.JoinHostPort(target.Hostname(), port)
	}
	if len(opts.Command) == 0 {
		if err := waitListening(address, opts.StartWait, nil); err != nil {
			return nil, err
		}
		return func() {}, nil
	}

	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	out := opts.ServerOutput
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	// An interrupt first, as a terminal would send, so the server can stop
	// cleanly, and the kill only if it will not.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("replay: start: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
		e.mu.Lock()
		e.exited = true
		if cmd.ProcessState != nil {
			e.exitCode = cmd.ProcessState.ExitCode()
		}
		e.cond.Broadcast()
		e.mu.Unlock()
	}()
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-exited:
		case <-time.After(stopGrace):
			_ = cmd.Process.Kill()
			<-exited
		}
	}

	if err := waitListening(address, opts.StartWait, exited); err != nil {
		stop()
		return nil, err
	}
	return stop, nil
}

// waitListening polls address until it accepts a connection. Listening is not
// the same as answering, but it is all the transport lets a client know before
// it sends something. exited, when set, ends the wait early for a server that
// stopped.
func waitListening(address string, patience time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(patience)
	for {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("replay: the server exited before it listened on %s", address)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("replay: nothing listened on %s within %s", address, patience)
		}
	}
}

// httpSide posts frames to the recording proxy and hands every message the
// server sends back to the engine.
type httpSide struct {
	e        *engine
	ctx      context.Context
	endpoint string
	headers  []string
	// listen says the recording had a GET stream, which is how a 2025 server
	// reaches a client outside the answer to one of its requests.
	listen bool

	mu        sync.Mutex
	sessionID string // Mcp-Session-Id, from whichever answer assigned one
	version   string // the MCP-Protocol-Version the client last sent
	listening bool
	// streams closes the answer stream of each request, by id. An entry
	// outlives its request, since closing a finished stream does nothing and
	// the notification it stands in for must not go out either way.
	streams map[string]stream

	wg sync.WaitGroup
}

func (h *httpSide) send(frame []byte, env *proxy.Envelope) {
	msg, ok := proxy.ParseRPC(frame)
	if ok && msg.Method == "notifications/cancelled" && h.closeStream(msg.Params) {
		return
	}
	if ok && msg.Method != "" && len(msg.ID) > 0 {
		// A request, which the server may take its time over, so the next frame
		// does not wait for it.
		ctx, cancel := context.WithCancel(h.ctx)
		h.mu.Lock()
		if h.streams == nil {
			h.streams = map[string]stream{}
		}
		h.streams[string(msg.ID)] = stream{close: cancel, modern: metaVersion(msg.Params) >= "2026-07-28"}
		h.mu.Unlock()
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer cancel()
			h.post(ctx, frame, env, msg)
		}()
		return
	}
	h.post(h.ctx, frame, env, msg)
	if ok && msg.Method == "notifications/initialized" {
		h.openListen()
	}
}

// closeStream cancels a 2026 request the way the transport has a client over
// HTTP do it, by closing the stream its answer comes on, and reports whether
// the request was one. That revision defines no client notification over HTTP,
// so the notifications/cancelled a session over stdio sends is not posted.
func (h *httpSide) closeStream(params []byte) bool {
	n, err := parseNode(params)
	if err != nil {
		return false
	}
	id := n.get("requestId")
	if id == nil {
		return false
	}
	key := string(bytes.TrimSpace(id.encode()))
	h.mu.Lock()
	st, ok := h.streams[key]
	h.mu.Unlock()
	if !ok || !st.modern {
		return false
	}
	st.close()
	h.e.closed(key)
	return true
}

// stream is one request's answer stream, and whether the request declared the
// revision that cancels by closing it.
type stream struct {
	close  context.CancelFunc
	modern bool
}

// cancel closes the stream of a request whose recorded client left its
// exchange, which is that act itself, on any revision.
func (h *httpSide) cancel(id, _ string) string {
	h.mu.Lock()
	st, ok := h.streams[id]
	h.mu.Unlock()
	if !ok {
		return "the request it gave up on was never sent"
	}
	st.close()
	h.e.closed(id)
	return ""
}

// post sends one frame and reads whatever comes back on it, until ctx ends.
func (h *httpSide) post(ctx context.Context, frame []byte, env *proxy.Envelope, msg proxy.RPCMessage) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(frame))
	if err != nil {
		return
	}
	h.requestHeaders(req, frame, env, msg)
	resp, err := replayClient.Do(req)
	if err != nil {
		// A stream the replay closed, to cancel the request or because the session
		// is over, is not a POST that failed.
		if ctx.Err() == nil {
			h.e.giveUp(requestIDs(frame), fmt.Sprintf("the POST carrying %s failed: %v", describeFrame(msg), err))
		}
		return
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		h.mu.Lock()
		h.sessionID = id
		h.mu.Unlock()
	}
	answered := h.readMessages(resp)
	if resp.StatusCode >= 400 && !answered {
		// Refused with nothing the replay can read as an answer, a 401 say. The
		// call is over, and waiting on it would only cost the patience.
		h.e.giveUp(requestIDs(frame), fmt.Sprintf("the server answered %s with HTTP %d", describeFrame(msg), resp.StatusCode))
	}
}

// requestHeaders sets what every POST needs and what the recorded frame said
// about itself. Routing headers go where the recorded client sent them, or
// where a request declares the revision that requires them, which is what a
// 2026 recording made over stdio needs to cross to HTTP. Their values are read
// from the body actually being sent, since the transport requires them to match
// it and a carried value can change it.
func (h *httpSide) requestHeaders(req *http.Request, frame []byte, env *proxy.Envelope, msg proxy.RPCMessage) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// The revision the client declared, in order of how directly it said so: the
	// header it sent, the _meta of a 2026 request, and otherwise the revision a
	// 2025 handshake settled on, which a recording made over stdio never had to
	// put in a header and a server over HTTP requires after the handshake.
	declared := metaVersion(msg.Params)
	h.mu.Lock()
	switch {
	case env != nil && env.MCPProtocolVersion != "":
		h.version = env.MCPProtocolVersion
	case declared != "":
		h.version = declared
	case h.version == "" && msg.Method != "initialize":
		h.version = h.e.negotiatedVersion()
	}
	version, session := h.version, h.sessionID
	h.mu.Unlock()
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if session != "" && msg.Method != "initialize" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	recorded := env != nil && env.MCPMethod != ""
	routed := (recorded || declared >= "2026-07-28") && msg.Method != "" && len(msg.ID) > 0
	if routed {
		req.Header.Set("Mcp-Method", msg.Method)
		if _, needs := methodsNeedingName[msg.Method]; needs || (env != nil && env.MCPName != "") {
			fallback := ""
			if env != nil {
				fallback = env.MCPName
			}
			if name := mcpName(msg.Params, fallback); name != "" {
				req.Header.Set("Mcp-Name", name)
			}
		}
	}
	var params []proxy.MCPParamHeader
	derived := false
	if routed && msg.Method == "tools/call" {
		// Read from the definition this server gave, as a client does, so a tool
		// that gained or renamed an annotation since the recording gets the
		// headers it now requires, and a recording made over stdio gets any at all.
		params, derived = h.e.paramHeaders(msg.Params)
	}
	if !derived && env != nil {
		// The recorded ones when the replayed server listed nothing to read them
		// from, a client that kept its tool list from an earlier session say.
		for _, header := range env.MCPParamHeaders {
			// Only the family the annotations produce, as for a single replayed call,
			// so a doctored capture cannot set a header the operator never passed.
			if !strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(header.Name), mcpParamPrefix) {
				continue
			}
			params = append(params, proxy.MCPParamHeader{Name: header.Name, Value: h.e.carriedHeader(header.Value)})
		}
	}
	for _, header := range params {
		req.Header.Set(header.Name, header.Value)
	}
	for _, header := range h.headers {
		name, value, ok := strings.Cut(header, ":")
		if !ok {
			continue
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
}

// readMessages hands every message of an answer to the engine, and says
// whether any of them was a response.
func (h *httpSide) readMessages(resp *http.Response) bool {
	if isEventStream(resp.Header.Get("Content-Type")) {
		return h.readStream(resp.Body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplayBody))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	h.e.receiveRaw(raw)
	return containsResponse(raw)
}

// readStream reads an event stream to its end, or until the session ends.
func (h *httpSide) readStream(body io.Reader) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxReplayBody)
	var data []byte
	answered := false
	flush := func() {
		if len(data) > 0 {
			h.e.receiveRaw(data)
			if containsResponse(data) {
				answered = true
			}
			data = data[:0]
		}
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(bytes.TrimSpace(line)) == 0:
			flush()
		case bytes.HasPrefix(line, []byte(":")):
			// A keep-alive comment, which the transport tells a client to ignore.
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
	flush()
	return answered
}

// openListen opens the GET stream a 2025 server uses to reach a client outside
// the answer to any one request, when the recording shows the client had one.
func (h *httpSide) openListen() {
	h.mu.Lock()
	if !h.listen || h.listening {
		h.mu.Unlock()
		return
	}
	h.listening = true
	h.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.endpoint, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		h.mu.Lock()
		if h.version != "" {
			req.Header.Set("MCP-Protocol-Version", h.version)
		}
		if h.sessionID != "" {
			req.Header.Set("Mcp-Session-Id", h.sessionID)
		}
		h.mu.Unlock()
		for _, header := range h.headers {
			if name, value, ok := strings.Cut(header, ":"); ok {
				req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
			}
		}
		resp, err := replayClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		// A 405 is a server that offers no such stream, which the transport
		// allows, and there is nothing to read.
		if resp.StatusCode == http.StatusOK && isEventStream(resp.Header.Get("Content-Type")) {
			h.readStream(resp.Body)
		}
	}()
}

// endSession tells a 2025 server the session is over, as a client that no
// longer needs it should. A server that does not allow it answers 405, which is
// fine.
func (h *httpSide) endSession(ctx context.Context) {
	h.mu.Lock()
	session := h.sessionID
	h.mu.Unlock()
	if session == "" {
		return
	}
	end, cancel := context.WithTimeout(ctx, stopGrace)
	defer cancel()
	req, err := http.NewRequestWithContext(end, http.MethodDelete, h.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	for _, header := range h.headers {
		if name, value, ok := strings.Cut(header, ":"); ok {
			req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	}
	if resp, err := replayClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// metaVersion is the revision a request declares in its own _meta.
func metaVersion(params []byte) string {
	n, err := parseNode(params)
	if err != nil {
		return ""
	}
	if v := n.get("_meta").get("io.modelcontextprotocol/protocolVersion"); v != nil && v.kind == stringNode {
		return v.str
	}
	return ""
}

// requestIDs returns the ids of the requests in a frame, as raw JSON.
func requestIDs(frame []byte) []string {
	root, err := parseNode(frame)
	if err != nil {
		return nil
	}
	var batch bool
	var ids []string
	for _, n := range messagesOf(root, &batch) {
		if msg, ok := proxy.ParseRPC(n.encode()); ok && msg.Method != "" && len(msg.ID) > 0 {
			ids = append(ids, string(msg.ID))
		}
	}
	return ids
}

// containsResponse reports whether a body holds a JSON-RPC response.
func containsResponse(raw []byte) bool {
	root, err := parseNode(bytes.TrimSpace(raw))
	if err != nil {
		return false
	}
	var batch bool
	for _, n := range messagesOf(root, &batch) {
		if msg, ok := proxy.ParseRPC(n.encode()); ok && msg.IsResponse() {
			return true
		}
	}
	return false
}

func describeFrame(msg proxy.RPCMessage) string {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		return msg.Method + " (id " + string(msg.ID) + ")"
	case msg.Method != "":
		return msg.Method
	case len(msg.ID) > 0:
		return "the answer to " + string(msg.ID)
	}
	return "a frame"
}
