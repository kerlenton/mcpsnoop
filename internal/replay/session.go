package replay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// A session replay drives a server with the client half of a recorded session.
// It is the inverse of mock, which plays the server half to a client. The
// server under test is the thing that changed, so its answers are not compared
// byte for byte here: the replay produces a capture of its own, through the
// same stdio proxy that records a live session, and check and diff judge it.
//
// What the replay keeps from the recording is the client's behaviour. The same
// frames go out in the same order, each one only after the answers and server
// requests that came before it in the recording have come again, which keeps
// what the client did at once concurrent and what it did in turn sequential,
// without replaying the minutes a model spent thinking. Values the server
// handed the client and the client sent back are carried over, server requests
// are answered with what the client answered then, and a cancellation goes out
// after the same delay it did, unless the call was answered first.

// DefaultWait is how long a replay waits for any one answer the recording got
// before it carries on without it.
const DefaultWait = 10 * time.Second

// DefaultStartWait is how long a server the replay starts for HTTP gets to
// begin listening, which a build tool in front of it can make slow.
const DefaultStartWait = 30 * time.Second

// stopGrace is how long a stopped server gets to let go of its pipes.
const stopGrace = 5 * time.Second

// Recording is one captured session's frames, ready to replay.
type Recording struct {
	SessionID string
	Label     string
	Transport string
	frames    []proxy.Envelope
	// listens says an HTTP recording shows the server reaching the client
	// outside the answer to any request, which only a GET stream allows.
	listens bool
}

// ClientFrames counts the frames the client sent in the recording.
func (r *Recording) ClientFrames() int {
	n := 0
	for _, env := range r.frames {
		if env.Direction == proxy.ClientToServer {
			n++
		}
	}
	return n
}

// RedactedClientFrames returns the seq of every client frame mcpsnoop's own
// redaction rewrote when it was recorded. Replaying one sends the placeholder
// to the server in place of whatever the client really sent.
func (r *Recording) RedactedClientFrames() []uint64 {
	var seqs []uint64
	for _, env := range r.frames {
		if env.Direction == proxy.ClientToServer && env.Redacted {
			seqs = append(seqs, env.Seq)
		}
	}
	return seqs
}

// LoadRecording reads a capture and keeps the session named sessionID, or the
// first session in it when sessionID is empty.
func LoadRecording(r io.Reader, sessionID string) (*Recording, error) {
	var rec *Recording
	err := proxy.Decode(r, func(env proxy.Envelope) {
		if rec == nil && (sessionID == "" || env.SessionID == sessionID) {
			rec = &Recording{SessionID: env.SessionID, Label: env.ServerLabel}
		}
		if rec == nil || env.SessionID != rec.SessionID {
			return
		}
		if rec.Transport == "" {
			rec.Transport = env.Transport
		}
		if env.Direction == proxy.ClientToServer || env.Direction == proxy.ServerToClient {
			rec.frames = append(rec.frames, env)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("not a capture: %w", err)
	}
	if rec == nil {
		if sessionID != "" {
			return nil, fmt.Errorf("no session %q in the capture", sessionID)
		}
		return nil, errors.New("the capture holds no session")
	}
	// A GET carries no body, so the proxy records no client frame for it, and
	// the only trace of one is server frames on an exchange no client frame
	// opened.
	opened := map[uint64]bool{}
	for _, env := range rec.frames {
		if env.Direction == proxy.ClientToServer && env.Exchange != 0 {
			opened[env.Exchange] = true
		}
	}
	for _, env := range rec.frames {
		if env.Direction == proxy.ServerToClient && env.Exchange != 0 && !opened[env.Exchange] && len(env.Raw) > 0 {
			rec.listens = true
			break
		}
	}
	return rec, nil
}

// SessionOptions configures a session replay.
type SessionOptions struct {
	// Command starts the server under test. Over stdio the replay speaks to it
	// on its stdin and stdout. With Target set it is started and left to listen,
	// and may be empty when the server is already running.
	Command []string
	// Target is the MCP endpoint of a Streamable HTTP server, which makes the
	// replay speak HTTP. Headers are added to every request, which is how a
	// credential reaches the server, since a capture never holds one.
	Target  string
	Headers []string
	// StartWait bounds how long a started HTTP server gets to accept
	// connections. Zero means DefaultStartWait.
	StartWait time.Duration
	// ServerOutput receives what a started HTTP server writes to stdout and
	// stderr. Nil discards it.
	ServerOutput io.Writer
	// Label and SessionID name the replayed capture, and Sink receives it.
	Label     string
	SessionID string
	Sink      proxy.Sink
	// Wait bounds how long the replay waits for any one answer the recording
	// got. Zero means DefaultWait.
	Wait time.Duration
	// SkipTools names tools whose calls are not sent. ReadOnly adds every tool
	// the recording's own listing did not mark read-only.
	SkipTools []string
	ReadOnly  bool
}

// SessionReport says what a replay did and what it could not.
type SessionReport struct {
	// Sent counts the client frames written to the server.
	Sent int
	// Skipped lists the recorded client frames that were not sent, and why.
	Skipped []string
	// Missing lists what the recording got and the replay waited for in vain.
	Missing []string
	// Unanswered lists server requests the replay had no recorded answer for.
	Unanswered []string
	// ServerExited is set when the server stopped before the replay finished.
	ServerExited bool
	// ExitCode is the server's own, once it has stopped.
	ExitCode int
}

// slot is one client request: what the recording got for it, and what the
// replay has.
type slot struct {
	id         string // the request's id as raw JSON
	method     string
	tool       string
	recordedAt time.Time
	recorded   *node  // the recorded response, nil when there was none
	taskID     string // the task that response handed out, if any
	taskParam  string // the task a tasks/* request is about
	version    string // the revision the request declared
	skipped    bool

	// The replay's side, guarded by the engine's lock.
	sent     bool
	sentAt   time.Time
	answered bool
	gaveUp   bool
}

// serverRequest names one request the server sent, by method and by its place
// among the server's requests of that method, which is how a recorded one is
// told from its replayed counterpart when the server numbers them afresh.
type serverRequest struct {
	method  string
	ordinal int
	gaveUp  bool
}

// await is one thing the recording got before a client frame went out.
type await struct {
	slot    *slot
	request *serverRequest
}

// element is one JSON-RPC message of a client frame. A frame is one message,
// or a batch of them on stdio.
type element struct {
	node    *node
	opens   *slot          // a request
	answers *serverRequest // a response to a server request
	cancels *slot          // a notifications/cancelled
	delay   time.Duration  // how long after its request the cancellation went out
	// closes says the client cancelled by leaving the HTTP exchange rather than
	// with a notification, so the element has no frame of its own.
	closes bool
	ping   bool // an answer to a server ping, which the replay gives itself
}

type step struct {
	env      proxy.Envelope
	elements []*element
	batch    bool
	waits    []await
	skip     string
}

// ReplaySession runs the recording's client half against the server opts
// names, and reports what it did. The replayed capture goes to opts.Sink.
func ReplaySession(ctx context.Context, rec *Recording, opts SessionOptions) (SessionReport, error) {
	if len(opts.Command) == 0 && opts.Target == "" {
		return SessionReport{}, errors.New("replay: no server command")
	}
	if opts.Wait <= 0 {
		opts.Wait = DefaultWait
	}
	if opts.StartWait <= 0 {
		opts.StartWait = DefaultStartWait
	}
	steps, slots, recordedServed := plan(rec)
	skipTools := make(map[string]bool, len(opts.SkipTools))
	for _, name := range opts.SkipTools {
		skipTools[name] = true
	}
	if opts.ReadOnly {
		readOnly := recordedReadOnly(slots)
		for _, s := range slots {
			if s.method == "tools/call" && !readOnly[s.tool] {
				skipTools[s.tool] = true
			}
		}
	}
	markSkipped(steps, slots, skipTools)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := &engine{
		wait:     opts.Wait,
		inflight: map[string]*slot{},
		served:   map[string][]json.RawMessage{},
		recorded: recordedServed,
		carried:  newCarried(),
	}
	e.cond = sync.NewCond(&e.mu)

	var (
		finish func()
		err    error
	)
	if opts.Target != "" {
		finish, err = e.startHTTP(ctx, rec, opts)
	} else {
		finish = e.startStdio(ctx, cancel, opts)
	}
	if err != nil {
		return SessionReport{}, err
	}
	e.run(steps, slots)
	finish()

	e.mu.Lock()
	defer e.mu.Unlock()
	// The proxy returns an error only when the server could not be started or
	// waited on, never for the server's own exit code, so whatever was queued
	// before it failed never reached anything.
	if e.startErr != nil {
		return e.report, fmt.Errorf("replay: %w", e.startErr)
	}
	e.report.ExitCode = e.exitCode
	return e.report, nil
}

// startStdio runs the server behind the stdio proxy that records live
// sessions, speaking to it through the proxy's own client side, and returns
// what ends the session.
func (e *engine) startStdio(ctx context.Context, cancel context.CancelFunc, opts SessionOptions) func() {
	in := newClientInput()
	outR, outW := io.Pipe()
	e.side = stdioSide{in: in}

	done := make(chan struct{})
	go func() {
		defer close(done)
		code, err := proxy.RunStdio(ctx, proxy.StdioConfig{
			Command:   opts.Command,
			Label:     opts.Label,
			SessionID: opts.SessionID,
			Sink:      opts.Sink,
			In:        in,
			Out:       outW,
			Err:       io.Discard,
		})
		e.mu.Lock()
		e.exited = true
		e.exitCode = code
		e.startErr = err
		e.cond.Broadcast()
		e.mu.Unlock()
	}()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		e.read(outR)
	}()

	return func() {
		// Close the client's side, which is the end of the session to a stdio
		// server, then give it the same patience as any answer before stopping it.
		_ = in.Close()
		select {
		case <-done:
		case <-time.After(opts.Wait):
			cancel()
			select {
			case <-done:
			case <-time.After(stopGrace):
				// A server started through a launcher, npx say, can leave a child
				// holding its pipes open after the launcher is killed, and the proxy
				// then waits on them forever. The replay is over either way.
			}
		}
		_ = outW.Close()
		<-readerDone
	}
}

// clientSide is how frames reach the server: a line on its stdin, or a POST.
// env is the recorded envelope a frame came from, and nil for one the replay
// writes itself, which is only ever an answer to a ping.
type clientSide interface {
	send(frame []byte, env *proxy.Envelope)
	// cancel gives up on a request the way the transport has a client do it,
	// for a recording whose client cancelled by leaving an HTTP exchange. It
	// returns why it could not, empty when it did.
	cancel(id, version string) string
}

// stdioSide writes each frame as one line of the server's stdin.
type stdioSide struct {
	in io.Writer
}

func (s stdioSide) send(frame []byte, _ *proxy.Envelope) {
	_, _ = s.in.Write(append(append([]byte(nil), frame...), '\n'))
}

// cancel sends the notification a client over stdio cancels with. A 2025 client
// that left an HTTP exchange disconnected, which was not a cancellation, and
// stdio has no way to say it.
func (s stdioSide) cancel(id, version string) string {
	if version < "2026-07-28" {
		return "a " + orUnknown(version) + " client leaving an HTTP exchange disconnected rather than cancelled, which stdio has no way to say"
	}
	s.send([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":`+id+`}}`), nil)
	return ""
}

func orUnknown(version string) string {
	if version == "" {
		return "pre-2026"
	}
	return version
}

// plan turns the recording into the steps a replay takes and the requests it
// tracks. Each client frame waits on the answers and server requests recorded
// since the frame before it, which is how the order the client saw survives.
func plan(rec *Recording) ([]*step, []*slot, map[string]int) {
	var (
		steps   []*step
		slots   []*slot
		pending []await
		open    = map[string]*slot{}
		byID    = map[string]*serverRequest{}
		counts  = map[string]int{}
		// carried is the first request each HTTP exchange carried, which is what
		// the client leaving that exchange gave up on.
		carried = map[uint64]*slot{}
	)
	for _, env := range answersAfterRequests(rec.frames) {
		switch env.Direction {
		case proxy.ClientToServer:
			st := &step{env: env, waits: pending}
			pending = nil
			steps = append(steps, st)
			switch {
			case env.Abandoned:
				// The client left an exchange before its answer, an act with no frame
				// of its own. It is replayed as the cancellation it was, the way the
				// transport the replay speaks has a client cancel.
				if s := carried[env.Exchange]; s != nil && env.Exchange != 0 {
					st.elements = []*element{{cancels: s, closes: true, delay: env.TS.Sub(s.recordedAt)}}
				} else {
					st.skip = "the client left an exchange whose request the recording does not hold"
				}
				continue
			case env.Truncated:
				st.skip = "it was cut at the frame-size limit when it was recorded"
				continue
			case len(env.Raw) == 0:
				st.skip = "it was not JSON when it was recorded"
				continue
			}
			root, err := parseNode(env.Raw)
			if err != nil {
				st.skip = "it does not parse as JSON"
				continue
			}
			for _, n := range messagesOf(root, &st.batch) {
				el := &element{node: n}
				st.elements = append(st.elements, el)
				msg, ok := proxy.ParseRPC(n.encode())
				if !ok {
					continue
				}
				switch {
				case msg.Method != "" && len(msg.ID) > 0:
					s := &slot{id: string(msg.ID), method: msg.Method, recordedAt: env.TS, version: env.MCPProtocolVersion}
					if v := n.get("params").get("_meta").get("io.modelcontextprotocol/protocolVersion"); v != nil && v.kind == stringNode {
						s.version = v.str
					}
					if msg.Method == "tools/call" {
						s.tool = paramString(n, "name")
					}
					if isTaskRequest(msg.Method) {
						s.taskParam = paramString(n, "taskId")
					}
					if env.Exchange != 0 && carried[env.Exchange] == nil {
						carried[env.Exchange] = s
					}
					open[s.id] = s
					slots = append(slots, s)
					el.opens = s
				case msg.Method == "notifications/cancelled":
					if s := open[cancelledID(n)]; s != nil {
						el.cancels = s
						el.delay = env.TS.Sub(s.recordedAt)
					}
				case msg.Method == "" && len(msg.ID) > 0:
					if ref := byID[string(msg.ID)]; ref != nil {
						if ref.method == "ping" {
							el.ping = true
						} else {
							el.answers = ref
						}
					}
				}
			}
		case proxy.ServerToClient:
			if len(env.Raw) == 0 {
				continue
			}
			root, err := parseNode(env.Raw)
			if err != nil {
				continue
			}
			var batch bool
			for _, n := range messagesOf(root, &batch) {
				msg, ok := proxy.ParseRPC(n.encode())
				if !ok {
					continue
				}
				switch {
				case msg.Method == "" && len(msg.ID) > 0:
					if s := open[string(msg.ID)]; s != nil && s.recorded == nil {
						s.recorded = n
						s.taskID = handedOutTask(n)
						pending = append(pending, await{slot: s})
					}
				case msg.Method != "" && len(msg.ID) > 0:
					ref := &serverRequest{method: msg.Method, ordinal: counts[msg.Method]}
					counts[msg.Method]++
					byID[string(msg.ID)] = ref
					if msg.Method != "ping" {
						pending = append(pending, await{request: ref})
					}
				}
			}
		}
	}
	return steps, slots, counts
}

// markSkipped holds back the calls to skipped tools, and everything that only
// makes sense after one: a retry of the same call, a cancellation of it, and a
// request about the task it handed out.
func markSkipped(steps []*step, slots []*slot, tools map[string]bool) {
	if len(tools) == 0 {
		return
	}
	tasks := map[string]bool{}
	for _, s := range slots {
		if s.method == "tools/call" && tools[s.tool] {
			s.skipped = true
			if s.taskID != "" {
				tasks[s.taskID] = true
			}
		}
	}
	for _, s := range slots {
		if isTaskRequest(s.method) && tasks[s.taskParam] {
			s.skipped = true
		}
	}
	for _, st := range steps {
		if st.skip != "" || len(st.elements) == 0 {
			continue
		}
		skipped := 0
		for _, el := range st.elements {
			if (el.opens != nil && el.opens.skipped) || (el.cancels != nil && el.cancels.skipped) {
				skipped++
			}
		}
		switch {
		case skipped == 0:
		case skipped == len(st.elements):
			st.skip = "it belongs to a skipped tool call"
		default:
			st.skip = "it is a batch holding a skipped tool call"
		}
	}
}

// recordedReadOnly reads the recording's own tools/list answers for the tools
// that declared readOnlyHint true.
func recordedReadOnly(slots []*slot) map[string]bool {
	readOnly := map[string]bool{}
	for _, s := range slots {
		if s.method != "tools/list" || s.recorded == nil {
			continue
		}
		tools := s.recorded.get("result").get("tools")
		if tools == nil || tools.kind != arrayNode {
			continue
		}
		for _, tool := range tools.items {
			name := tool.get("name")
			hint := tool.get("annotations").get("readOnlyHint")
			if name != nil && name.kind == stringNode && hint != nil && hint.kind == boolNode && hint.boolean {
				readOnly[name.str] = true
			}
		}
	}
	return readOnly
}

type engine struct {
	mu   sync.Mutex
	cond *sync.Cond
	side clientSide
	wait time.Duration

	inflight map[string]*slot             // sent requests not yet answered, by id
	served   map[string][]json.RawMessage // the server's requests by method, the ids in arrival order
	recorded map[string]int               // how many of each the recording holds
	// negotiated is the revision a replayed initialize settled on.
	negotiated string
	// schemas holds the inputSchema of every tool the replayed server listed,
	// which is where a client over HTTP reads the Mcp-Param headers a call needs.
	schemas  map[string]json.RawMessage
	carried  *carried
	report   SessionReport
	exited   bool
	exitCode int
	startErr error
}

// read follows the server's side of the session.
func (e *engine) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for scanner.Scan() {
		root, err := parseNode(bytes.TrimSpace(scanner.Bytes()))
		if err != nil {
			continue
		}
		var batch bool
		for _, n := range messagesOf(root, &batch) {
			e.receive(n)
		}
	}
	// A line past the scanner's limit ends the scan with an error. Whatever the
	// server says after it still has to be drained, or its writes block.
	_, _ = io.Copy(io.Discard, r)
}

func (e *engine) receive(n *node) {
	msg, ok := proxy.ParseRPC(n.encode())
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		s := e.inflight[string(msg.ID)]
		if s == nil {
			return
		}
		delete(e.inflight, s.id)
		s.answered = true
		switch s.method {
		case "initialize":
			if v := n.get("result").get("protocolVersion"); v != nil && v.kind == stringNode {
				e.negotiated = v.str
			}
		case "tools/list":
			e.learnTools(n.get("result").get("tools"))
		}
		if s.recorded != nil {
			e.carried.learn(s.recorded.get("result"), n.get("result"))
			e.carried.learn(s.recorded.get("error"), n.get("error"))
		}
		e.cond.Broadcast()
	case msg.Method == "ping" && len(msg.ID) > 0:
		// Every client answers a ping, and the replay is the client now. It is
		// answered as it arrives rather than from the recording, since a ping is
		// about the moment rather than the session.
		go e.side.send([]byte(`{"jsonrpc":"2.0","id":`+string(msg.ID)+`,"result":{}}`), nil)
	case msg.Method != "" && len(msg.ID) > 0:
		e.served[msg.Method] = append(e.served[msg.Method], msg.ID)
		if n := len(e.served[msg.Method]); n > e.recorded[msg.Method] {
			// The recording has no answer for this one, so the server waits on a
			// client that will never reply, and the reader should know why.
			e.report.Unanswered = append(e.report.Unanswered,
				fmt.Sprintf("%s #%d (id %s), which the recording never answered", msg.Method, n, msg.ID))
		}
		e.cond.Broadcast()
	}
}

// receiveRaw takes one message, or a batch of them, as the server sent it.
func (e *engine) receiveRaw(raw []byte) {
	root, err := parseNode(bytes.TrimSpace(raw))
	if err != nil {
		return
	}
	var batch bool
	for _, n := range messagesOf(root, &batch) {
		e.receive(n)
	}
}

// learnTools keeps the input schema of every tool in a replayed listing. A
// later page, or a later listing, replaces what an earlier one said about the
// same tool, as it would for a client.
func (e *engine) learnTools(tools *node) {
	if tools == nil || tools.kind != arrayNode {
		return
	}
	for _, tool := range tools.items {
		name, schema := tool.get("name"), tool.get("inputSchema")
		if name == nil || name.kind != stringNode || schema == nil {
			continue
		}
		if e.schemas == nil {
			e.schemas = map[string]json.RawMessage{}
		}
		e.schemas[name.str] = schema.encode()
	}
}

// paramHeaders is what a conforming client sends with a tools/call over HTTP,
// read from the definition the replayed server gave for the tool and the
// arguments actually being sent. ok is false when the server gave none the
// replay can use.
func (e *engine) paramHeaders(params []byte) ([]proxy.MCPParamHeader, bool) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &call) != nil || call.Name == "" {
		return nil, false
	}
	e.mu.Lock()
	schema, known := e.schemas[call.Name]
	e.mu.Unlock()
	if !known {
		return nil, false
	}
	return store.ParamHeaders(schema, call.Arguments)
}

// negotiatedVersion is the revision the replayed handshake settled on, empty
// before one answered.
func (e *engine) negotiatedVersion() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.negotiated
}

// giveUp settles requests whose answer can no longer come, a POST the server
// refused with nothing to read as an answer say, so nothing waits out the full
// patience on them, and says why.
func (e *engine) giveUp(ids []string, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	settled := false
	for _, id := range ids {
		if s := e.inflight[id]; s != nil {
			delete(e.inflight, id)
			s.answered = true
			s.gaveUp = true
			settled = true
		}
	}
	if settled || len(ids) == 0 {
		e.report.Missing = append(e.report.Missing, reason)
	}
	e.cond.Broadcast()
}

// closed settles a request whose answer stream the replay closed to cancel it.
// Nothing can come on a closed stream, so whatever waits on the answer stops
// waiting, and it is not missing, since the replay chose to stop.
func (e *engine) closed(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.inflight[id]; s != nil {
		delete(e.inflight, id)
		s.answered = true
		s.gaveUp = true
		e.cond.Broadcast()
	}
}

// carriedHeader is a recorded Mcp-Param value with what the replay carried
// applied, since the header mirrors an argument and has to match the argument
// actually sent.
func (e *engine) carriedHeader(value string) string {
	decoded, ok := proxy.DecodeHeaderValue(value)
	if !ok {
		return value
	}
	e.mu.Lock()
	n := &node{kind: stringNode, str: decoded}
	changed := e.carried.applyAt(n, "")
	e.mu.Unlock()
	if !changed {
		return value
	}
	return proxy.EncodeHeaderValue(n.str)
}

// run sends the steps in order.
func (e *engine) run(steps []*step, slots []*slot) {
	for _, st := range steps {
		if st.skip != "" {
			e.skip(st, st.skip)
			continue
		}
		for _, a := range st.waits {
			e.await(a, time.Now().Add(e.wait))
		}
		if e.stopped() {
			break
		}
		e.send(st)
	}
	// The answers still owed. One deadline for all of them, so a server that
	// stopped answering costs one wait rather than one per call.
	deadline := time.Now().Add(e.wait)
	for _, s := range slots {
		if s.recorded != nil {
			e.await(await{slot: s}, deadline)
		}
	}
}

func (e *engine) stopped() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exited {
		e.report.ServerExited = true
	}
	return e.exited
}

// await blocks until the replay has what the recording had, the server stops,
// or the deadline passes. What never comes is reported once.
func (e *engine) await(a await, deadline time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	satisfied := func() bool {
		switch {
		case a.slot != nil:
			s := a.slot
			return s.skipped || !s.sent || s.answered || s.gaveUp
		default:
			return a.request.gaveUp || len(e.served[a.request.method]) > a.request.ordinal
		}
	}
	timer := time.AfterFunc(time.Until(deadline), func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer timer.Stop()
	for !satisfied() && !e.exited && time.Now().Before(deadline) {
		e.cond.Wait()
	}
	if satisfied() {
		return
	}
	if a.slot != nil {
		a.slot.gaveUp = true
		e.report.Missing = append(e.report.Missing, fmt.Sprintf("no answer to %s %s", describe(a.slot), outcome(e.exited, e.wait)))
		return
	}
	a.request.gaveUp = true
	e.report.Missing = append(e.report.Missing, fmt.Sprintf("the server never sent %s #%d %s",
		a.request.method, a.request.ordinal+1, outcome(e.exited, e.wait)))
}

func outcome(exited bool, wait time.Duration) string {
	if exited {
		return "before the server exited"
	}
	return "within " + wait.String()
}

func describe(s *slot) string {
	if s.tool != "" {
		return s.method + " " + strconv.Quote(s.tool) + " (id " + s.id + ")"
	}
	return s.method + " (id " + s.id + ")"
}

func (e *engine) skip(st *step, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.report.Skipped = append(e.report.Skipped, fmt.Sprintf("frame %d, since %s", st.env.Seq, reason))
}

// send writes one recorded client frame, as recorded unless something in it
// has to change.
func (e *engine) send(st *step) {
	if len(st.elements) == 1 && st.elements[0].closes {
		el := st.elements[0]
		if !e.cancelStillMeant(el) {
			e.skip(st, "the call it gave up on was answered before the client would have left")
			return
		}
		if reason := e.side.cancel(el.cancels.id, el.cancels.version); reason != "" {
			e.skip(st, reason)
			return
		}
		e.mu.Lock()
		e.report.Sent++
		e.mu.Unlock()
		return
	}
	changed := false
	var kept []*element
	for _, el := range st.elements {
		switch {
		case el.ping:
			e.skip(st, "the replay answers the server's pings itself")
			continue
		case el.cancels != nil:
			if !e.cancelStillMeant(el) {
				e.skip(st, "the call it cancels was answered before the client would have given up")
				continue
			}
		case el.answers != nil:
			id, ok := e.servedID(el.answers)
			if !ok {
				e.skip(st, fmt.Sprintf("the server never sent the %s it answers", el.answers.method))
				continue
			}
			setMember(el.node, "id", id)
			changed = true
		}
		e.mu.Lock()
		if e.carried.apply(el.node) {
			changed = true
		}
		if el.opens != nil {
			el.opens.sent = true
			el.opens.sentAt = time.Now()
			e.inflight[el.opens.id] = el.opens
		}
		e.mu.Unlock()
		kept = append(kept, el)
	}
	if len(kept) == 0 {
		return
	}
	line := st.env.Raw
	if changed || len(kept) != len(st.elements) {
		line = encodeElements(kept, st.batch)
	}
	env := st.env
	e.side.send(compact(line), &env)
	e.mu.Lock()
	e.report.Sent++
	e.mu.Unlock()
}

// cancelStillMeant waits out the delay the recorded cancellation came after,
// and says whether the client would still send it. A call the replayed server
// answered in that time is one the client had no reason to give up on.
func (e *engine) cancelStillMeant(el *element) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := el.cancels
	if s.skipped || !s.sent {
		return false
	}
	delay := min(el.delay, e.wait)
	deadline := s.sentAt.Add(delay)
	timer := time.AfterFunc(time.Until(deadline), func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer timer.Stop()
	for !s.answered && !e.exited && time.Now().Before(deadline) {
		e.cond.Wait()
	}
	return !s.answered
}

func (e *engine) servedID(ref *serverRequest) (*node, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := e.served[ref.method]
	if len(ids) <= ref.ordinal {
		return nil, false
	}
	id, err := parseNode(ids[ref.ordinal])
	return id, err == nil
}

// messagesOf returns the messages of a frame, the elements of a batch or the
// frame itself.
func messagesOf(root *node, batch *bool) []*node {
	if root.kind == arrayNode {
		*batch = true
		return root.items
	}
	return []*node{root}
}

func encodeElements(elements []*element, batch bool) []byte {
	if !batch {
		return elements[0].node.encode()
	}
	arr := &node{kind: arrayNode}
	for _, el := range elements {
		arr.items = append(arr.items, el.node)
	}
	return arr.encode()
}

func compact(line []byte) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, line) != nil {
		return line
	}
	return buf.Bytes()
}

func setMember(obj *node, key string, value *node) {
	for i := range obj.members {
		if obj.members[i].key == key {
			obj.members[i].value = value
			return
		}
	}
	obj.members = append(obj.members, member{key: key, value: value})
}

func paramString(n *node, key string) string {
	v := n.get("params").get(key)
	if v == nil || v.kind != stringNode {
		return ""
	}
	return v.str
}

// cancelledID is the request a notifications/cancelled names, as raw JSON.
func cancelledID(n *node) string {
	v := n.get("params").get("requestId")
	if v == nil {
		return ""
	}
	return string(bytes.TrimSpace(v.encode()))
}

// handedOutTask returns the task id a response handed out, from the
// 2026-07-28 shape where it sits beside resultType and from the older one
// where a task object carries it.
func handedOutTask(response *node) string {
	result := response.get("result")
	for _, holder := range []*node{result, result.get("task")} {
		if id := holder.get("taskId"); id != nil && id.kind == stringNode {
			return id.str
		}
	}
	return ""
}

func isTaskRequest(method string) bool {
	switch method {
	case "tasks/get", "tasks/result", "tasks/cancel":
		return true
	}
	return false
}

// clientInput is the server's stdin as the proxy reads it. A write never
// blocks, because a server that stopped reading must not stall the replay
// behind it, and closing it is the end of the session to a stdio server.
type clientInput struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newClientInput() *clientInput {
	c := &clientInput{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *clientInput) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	c.buf.Write(p)
	c.cond.Signal()
	return len(p), nil
}

func (c *clientInput) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.buf.Len() == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.buf.Len() == 0 {
		return 0, io.EOF
	}
	return c.buf.Read(p)
}

func (c *clientInput) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.cond.Broadcast()
	return nil
}
