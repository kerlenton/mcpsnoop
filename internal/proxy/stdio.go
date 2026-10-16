package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

func cwd() string {
	d, _ := os.Getwd()
	return d
}

// StdioConfig configures a transparent stdio proxy run.
type StdioConfig struct {
	// Command is the wrapped server command and its arguments, e.g.
	// {"node", "build/index.js"}.
	Command []string
	// Label identifies this server in the hub/TUI.
	Label string
	// SessionID uniquely identifies this proxy process instance.
	SessionID string
	// Sink receives observed envelopes (best-effort). If nil, tracing is off.
	Sink Sink

	// In/Out/Err default to the process's os.Stdin/os.Stdout/os.Stderr. They are
	// exposed for testing.
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// maxFrameBytes caps a single JSON-RPC line we will buffer while peeking. The
// data path itself is unbounded (we stream in chunks), this only bounds the
// copy we hand to the Sink so a pathological line can't blow up memory.
//
// The taps take their cap as an argument rather than reading this directly, so a
// test can exercise the truncation path without a global to mutate. newBodyTap
// already worked that way; pumpFrames and sseTap now match it.
const maxFrameBytes = 16 << 20 // 16 MiB

// RunStdio spawns the wrapped server and proxies stdio transparently between the
// client (our stdin/stdout) and the server, observing every newline-delimited
// JSON-RPC frame. It returns the server's exit code and any startup error.
//
// Transparency contract. Bytes are forwarded verbatim and ordering is preserved,
// observation is best-effort and never blocks or alters the data path.
func RunStdio(ctx context.Context, cfg StdioConfig) (exitCode int, err error) {
	if len(cfg.Command) == 0 {
		return 1, errors.New("proxy: empty command")
	}
	in := orReader(cfg.In, os.Stdin)
	out := orWriter(cfg.Out, os.Stdout)
	errOut := orWriter(cfg.Err, os.Stderr)
	sink := cfg.Sink
	if sink == nil {
		sink = NopSink()
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = os.Environ()

	srvStdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	srvStdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	srvStderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}

	if err := cmd.Start(); err != nil {
		return 1, err
	}

	// rec numbers every frame and hands it to the sink under one lock, in the
	// order the frames were read rather than the order they were observed. Three
	// pumps record at once, the client, the server and stderr, and an atomic
	// counter only makes each number unique, not the order they reach the sink in.
	// The store infers a dropped frame from a forward jump in Seq, so a pair that
	// arrived swapped was counted as loss on a capture that had lost nothing, and
	// that invented figure reached check --fail-on incomplete, the JSON export,
	// the HAR comment, the OTLP attributes and the TUI banner alike.
	rec := newRecorder(sink)
	frame := func(dir Direction, raw []byte, text string, truncated bool) *Envelope {
		env := &Envelope{
			SessionID:   cfg.SessionID,
			ServerLabel: cfg.Label,
			Direction:   dir,
			Transport:   TransportStdio,
			Truncated:   truncated,
			Text:        text,
		}
		if raw != nil {
			// Copy, because the underlying buffer is reused by the next read.
			env.Raw = append([]byte(nil), raw...)
		}
		return env
	}

	// observe routes a framed protocol line to Raw when it is valid JSON, or to
	// Text otherwise, so a stray non-JSON line still reaches the hub instead of
	// failing to encode. Forwarding is unaffected, the bytes are already written
	// downstream before observe runs.
	//
	// A truncated line takes the same route, and must: it is a fragment, so it is
	// never valid JSON, and Envelope.Raw is a json.RawMessage that both sinks
	// encode with encoding/json. Putting a fragment there makes the envelope fail
	// to marshal, and the sinks answer that differently but both badly. AsyncSink
	// discards the write, so the frame never reaches the trace file; SocketSink
	// reads the error as the hub having gone away and drops the connection. The
	// flag is what says the copy is short; splitObserved decides where the bytes
	// go.
	observe := func(dir Direction) func(place, []byte, bool) {
		return func(p place, line []byte, truncated bool) {
			raw, text := splitObserved(line)
			rec.fill(p, frame(dir, raw, text, truncated))
		}
	}

	// Emit the session meta first (seq 1) so the hub can replay this server.
	// jsonwire: this is the one Envelope.Raw the proxy builds rather than copies,
	// and a command or cwd containing & would otherwise be the only escaped frame
	// in an otherwise verbatim log.
	if meta, mErr := jsonwire.Marshal(SessionMeta{Command: cfg.Command, CWD: cwd()}); mErr == nil {
		rec.fill(rec.take(), frame(DirectionMeta, meta, "", false))
	}

	// client -> server. Deliberately NOT awaited below. This pump blocks reading the
	// client's stdin, which stays open as long as the client is alive, so if the
	// server exits on its own this read never returns. Waiting on it would hang
	// cmd.Wait and the exit code would never be reported. We wait only on the two
	// server-side pumps, which end when the server closes its stdout/stderr (i.e.
	// when it exits). The detached write racing cmd.Wait closing the pipe is safe.
	// os.File is internally locked and the write just returns an error, ending the
	// pump. The process exits right after RunStdio returns anyway.
	go func() {
		// Closing the server's stdin signals EOF so it can shut down cleanly.
		defer srvStdin.Close()
		pumpFrames(in, srvStdin, maxFrameBytes, rec, observe(ClientToServer))
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// server -> client
	go func() {
		defer wg.Done()
		pumpFrames(srvStdout, out, maxFrameBytes, rec, observe(ServerToClient))
	}()

	// server stderr -> our stderr (forwarded) + observed line-by-line
	go func() {
		defer wg.Done()
		pumpLines(srvStderr, errOut, rec, func(p place, line string) {
			rec.fill(p, frame(ServerStderr, nil, line, false))
		})
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	// A frame the server sent last can be waiting on a client frame read before
	// it, whose pump is not awaited above. It reaches the sink before the caller
	// closes it.
	rec.flush(time.Second)

	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode(), nil
	}
	if waitErr != nil {
		return 1, waitErr
	}
	return 0, nil
}

// pumpFrames copies src->dst losslessly while splitting on newlines for
// observation. Each complete line (without the trailing newline) is passed to
// observe, along with the place it took in rec and whether the observed copy was
// cut at observeCap. The exact bytes read are always written to dst first, so a
// slow or failing observer can never affect the forwarded stream. Lines longer
// than observeCap are still forwarded in full, only the copy is short.
//
// The place is taken just before the chunk that ends a line is forwarded, which
// is the last moment before the other peer has the whole line and can answer
// it. Whatever it answers is read after that, takes a later place, and so is
// recorded after the line. observe must settle the place, and a line that is
// not observed has its place let go here.
//
// The flag matters as much as the copy. A cut line is not valid JSON, so without
// it the store reads the fragment as a stray non-JSON line and reports the
// stream as corrupted, which is a diagnosis about the server rather than about
// the observer's own cap.
func pumpFrames(src io.Reader, dst io.Writer, observeCap int, rec *recorder, observe func(p place, line []byte, truncated bool)) {
	r := bufio.NewReaderSize(src, 64<<10)
	var pending []byte // accumulated bytes of the current (unterminated) line
	var truncated bool
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			ends := chunk[len(chunk)-1] == '\n'
			var p place
			if ends {
				p = rec.take()
			}
			// Forward verbatim, immediately.
			if _, werr := dst.Write(chunk); werr != nil {
				if ends {
					rec.drop(p)
				}
				return
			}
			if f, ok := dst.(interface{ Flush() error }); ok {
				_ = f.Flush()
			}
			switch room := observeCap - len(pending); {
			case room <= 0:
				truncated = true
			case len(chunk) > room:
				pending = append(pending, chunk[:room]...)
				truncated = true
			default:
				pending = append(pending, chunk...)
			}
			if ends {
				// The chunk ends the line, but the copy may not: once the cap is
				// reached the terminator is among the bytes we stopped taking. Strip
				// only what is actually present, or a truncated line loses a byte of
				// its own content to a terminator that never made it into the copy.
				line := pending
				if len(line) > 0 && line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
					if len(line) > 0 && line[len(line)-1] == '\r' {
						line = line[:len(line)-1]
					}
				}
				if len(line) > 0 {
					observe(p, line, truncated)
				} else {
					rec.drop(p)
				}
				pending = nil
				truncated = false
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the buffer, keep reading
		}
		if err != nil {
			if len(pending) > 0 {
				observe(rec.take(), pending, truncated)
			}
			return
		}
	}
}

// pumpLines copies src->dst and reports each complete line as a string, with the
// place it took in rec, taken the way pumpFrames takes one.
func pumpLines(src io.Reader, dst io.Writer, rec *recorder, observe func(p place, line string)) {
	r := bufio.NewReaderSize(src, 64<<10)
	var pending []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			ends := chunk[len(chunk)-1] == '\n'
			var p place
			if ends {
				p = rec.take()
			}
			if _, werr := dst.Write(chunk); werr != nil {
				if ends {
					rec.drop(p)
				}
				return
			}
			if len(pending) < maxFrameBytes {
				pending = append(pending, chunk...)
			}
			if ends {
				// Same care as pumpFrames: past the cap the copy no longer ends with
				// the terminator, so strip only what is there. Stderr is text and
				// goes to Text either way, so a cut line costs a byte rather than a
				// misdiagnosis, but the arithmetic should still be honest.
				line := pending
				if len(line) > 0 && line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
					if len(line) > 0 && line[len(line)-1] == '\r' {
						line = line[:len(line)-1]
					}
				}
				observe(p, string(line))
				pending = nil
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the buffer, keep reading
		}
		if err != nil {
			if len(pending) > 0 {
				observe(rec.take(), string(pending))
			}
			return
		}
	}
}

// recorder numbers frames in the order they were read and hands them to the
// sink in that order.
//
// A pump forwards a line before it observes it, so that observing can never
// hold up the data path, and the other peer can answer the line the moment it
// is forwarded. Numbering frames as they were observed let that answer, read and
// observed by the other pump, take the earlier number while the line was still
// being observed, and the capture showed a client answering a request it had
// not yet been sent. A replay plans from that order, and sent the answer first.
//
// So a line takes its place just before it is forwarded, and the sink gets
// frames in place order. One observed early waits for those before it, which
// costs the capture a moment and never costs the data path anything. A forward
// blocked on a peer that stopped reading holds back the frames after it until
// it completes, which delays what the capture shows and not what either peer
// receives.
type recorder struct {
	sink Sink

	mu      sync.Mutex
	next    uint64               // the next place to hand out
	emitted uint64               // every place below this is settled
	seq     uint64               // the last Seq handed to the sink
	settled map[uint64]*Envelope // places settled out of turn, nil for one let go
}

// place is a frame's position in the capture and the time it was read.
type place struct {
	n  uint64
	ts time.Time
}

func newRecorder(sink Sink) *recorder {
	return &recorder{sink: sink, settled: map[uint64]*Envelope{}}
}

// take hands out the next place. Every place taken has to be settled, by fill or
// drop, or no frame after it reaches the sink.
func (r *recorder) take() place {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := place{n: r.next, ts: time.Now()}
	r.next++
	return p
}

// fill settles a place with its frame, stamped with the time the frame was read.
func (r *recorder) fill(p place, env *Envelope) {
	env.TS = p.ts
	r.settle(p, env)
}

// drop settles a place that holds no frame, a line that was never forwarded or
// was empty.
func (r *recorder) drop(p place) {
	r.settle(p, nil)
}

// settle records a place's outcome and emits every frame now in turn. Seq is
// given out here rather than with the place, so a place let go leaves no gap the
// store would count as a lost frame.
func (r *recorder) settle(p place, env *Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settled[p.n] = env
	for {
		next, ok := r.settled[r.emitted]
		if !ok {
			return
		}
		delete(r.settled, r.emitted)
		r.emitted++
		if next != nil {
			r.seq++
			next.Seq = r.seq
			r.sink.Emit(*next)
		}
	}
}

// flush waits, for at most patience, until every place taken so far is settled.
func (r *recorder) flush(patience time.Duration) {
	r.mu.Lock()
	target := r.next
	r.mu.Unlock()
	deadline := time.Now().Add(patience)
	for {
		r.mu.Lock()
		done := r.emitted >= target
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func orReader(r io.Reader, def io.Reader) io.Reader {
	if r != nil {
		return r
	}
	return def
}

func orWriter(w io.Writer, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}
