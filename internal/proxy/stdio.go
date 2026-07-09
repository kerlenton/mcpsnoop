package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
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
// data path itself is unbounded (we stream in chunks); this only bounds the
// copy we hand to the Sink so a pathological line can't blow up memory.
const maxFrameBytes = 16 << 20 // 16 MiB

// RunStdio spawns the wrapped server and proxies stdio transparently between the
// client (our stdin/stdout) and the server, observing every newline-delimited
// JSON-RPC frame. It returns the server's exit code and any startup error.
//
// Transparency contract: bytes are forwarded verbatim and ordering is preserved;
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
	emit := func(dir Direction, raw []byte, text string) {
		env := Envelope{
			SessionID:   cfg.SessionID,
			ServerLabel: cfg.Label,
			Seq:         seq.Add(1),
			TS:          time.Now(),
			Direction:   dir,
			Transport:   "stdio",
		}
		if raw != nil {
			// Copy: the underlying buffer is reused by the next read.
			env.Raw = append([]byte(nil), raw...)
		}
		env.Text = text
		sink.Emit(env)
	}

	// observe routes a framed protocol line to Raw when it is valid JSON, or to
	// Text otherwise, so a stray non-JSON line still reaches the hub instead of
	// failing to encode. Forwarding is unaffected: the bytes are already written
	// downstream before observe runs.
	observe := func(dir Direction, line []byte) {
		raw, text := splitObserved(line)
		emit(dir, raw, text)
	}

	// Emit the session meta first (seq 1) so the hub can replay this server.
	if meta, mErr := json.Marshal(SessionMeta{Command: cfg.Command, CWD: cwd()}); mErr == nil {
		emit(DirectionMeta, meta, "")
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// client -> server
	go func() {
		defer wg.Done()
		// Closing the server's stdin signals EOF so it can shut down cleanly.
		defer srvStdin.Close()
		pumpFrames(in, srvStdin, func(line []byte) { observe(ClientToServer, line) })
	}()

	// server -> client
	go func() {
		defer wg.Done()
		pumpFrames(srvStdout, out, func(line []byte) { observe(ServerToClient, line) })
	}()

	// server stderr -> our stderr (forwarded) + observed line-by-line
	go func() {
		defer wg.Done()
		pumpLines(srvStderr, errOut, func(line string) { emit(ServerStderr, nil, line) })
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
// observe. The exact bytes read are always written to dst first, so a slow or
// failing observer can never affect the forwarded stream. Lines longer than
// maxFrameBytes are still forwarded; only the observed copy is truncated.
func pumpFrames(src io.Reader, dst io.Writer, observe func(line []byte)) {
	r := bufio.NewReaderSize(src, 64<<10)
	var pending []byte // accumulated bytes of the current (unterminated) line
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
			if len(pending) < maxFrameBytes {
				pending = append(pending, chunk...)
			}
			if chunk[len(chunk)-1] == '\n' {
				line := pending
				// Strip trailing \n and optional \r.
				line = line[:len(line)-1]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				if len(line) > 0 {
					observe(line)
				}
				pending = nil
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the buffer; keep reading
		}
		if err != nil {
			if len(pending) > 0 {
				observe(pending)
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
				line := pending[:len(pending)-1]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				observe(string(line))
				pending = nil
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the buffer; keep reading
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
