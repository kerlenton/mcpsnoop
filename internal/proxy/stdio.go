package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
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

	var seq atomic.Uint64
	emit := func(dir Direction, raw []byte, text string, truncated bool) {
		env := Envelope{
			SessionID:   cfg.SessionID,
			ServerLabel: cfg.Label,
			Seq:         seq.Add(1),
			TS:          time.Now(),
			Direction:   dir,
			Transport:   TransportStdio,
			Truncated:   truncated,
		}
		if raw != nil {
			// Copy, because the underlying buffer is reused by the next read.
			env.Raw = append([]byte(nil), raw...)
		}
		env.Text = text
		sink.Emit(env)
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
	observe := func(dir Direction, line []byte, truncated bool) {
		raw, text := splitObserved(line)
		emit(dir, raw, text, truncated)
	}

	// Emit the session meta first (seq 1) so the hub can replay this server.
	// jsonwire: this is the one Envelope.Raw the proxy builds rather than copies,
	// and a command or cwd containing & would otherwise be the only escaped frame
	// in an otherwise verbatim log.
	if meta, mErr := jsonwire.Marshal(SessionMeta{Command: cfg.Command, CWD: cwd()}); mErr == nil {
		emit(DirectionMeta, meta, "", false)
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
		pumpFrames(in, srvStdin, maxFrameBytes, func(line []byte, truncated bool) {
			observe(ClientToServer, line, truncated)
		})
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// server -> client
	go func() {
		defer wg.Done()
		pumpFrames(srvStdout, out, maxFrameBytes, func(line []byte, truncated bool) {
			observe(ServerToClient, line, truncated)
		})
	}()

	// server stderr -> our stderr (forwarded) + observed line-by-line
	go func() {
		defer wg.Done()
		pumpLines(srvStderr, errOut, func(line string) { emit(ServerStderr, nil, line, false) })
	}()

	wg.Wait()
	waitErr := cmd.Wait()

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
// observe, along with whether the observed copy was cut at observeCap. The exact
// bytes read are always written to dst first, so a slow or failing observer can
// never affect the forwarded stream. Lines longer than observeCap are still
// forwarded in full, only the copy is short.
//
// The flag matters as much as the copy. A cut line is not valid JSON, so without
// it the store reads the fragment as a stray non-JSON line and reports the
// stream as corrupted, which is a diagnosis about the server rather than about
// the observer's own cap.
func pumpFrames(src io.Reader, dst io.Writer, observeCap int, observe func(line []byte, truncated bool)) {
	r := bufio.NewReaderSize(src, 64<<10)
	var pending []byte // accumulated bytes of the current (unterminated) line
	var truncated bool
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			// Forward verbatim, immediately.
			if _, werr := dst.Write(chunk); werr != nil {
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
			if chunk[len(chunk)-1] == '\n' {
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
					observe(line, truncated)
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
				observe(pending, truncated)
			}
			return
		}
	}
}

// pumpLines copies src->dst and reports each complete line as a string.
func pumpLines(src io.Reader, dst io.Writer, observe func(line string)) {
	r := bufio.NewReaderSize(src, 64<<10)
	var pending []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if _, werr := dst.Write(chunk); werr != nil {
				return
			}
			if len(pending) < maxFrameBytes {
				pending = append(pending, chunk...)
			}
			if chunk[len(chunk)-1] == '\n' {
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
				observe(string(line))
				pending = nil
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the buffer, keep reading
		}
		if err != nil {
			if len(pending) > 0 {
				observe(string(pending))
			}
			return
		}
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
