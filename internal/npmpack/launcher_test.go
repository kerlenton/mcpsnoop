package npmpack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The launcher is the one file in the npm distribution that runs on a user's
// machine, and it stands in the pipe between an MCP client and its server. The
// tests below drive the real file with the real node, against a stand-in for
// the binary, because what matters about it is behaviour under a signal and at
// a pipe, which reading it cannot settle.

// launcher builds a node_modules tree holding the real launcher and a stand-in
// for this machine's binary, and returns the launcher's path inside it.
func launcher(t *testing.T, stub string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in binary is a shell script and the signals are POSIX ones")
	}
	if _, err := exec.LookPath("node"); err != nil {
		// A Go contributor has no reason to have node installed, so locally
		// this steps aside. In CI it must not: a silent skip would leave the
		// only file that runs on a user's machine with no gate at all.
		if os.Getenv("CI") != "" {
			t.Fatal("node is missing, so the launcher would go untested; install it in the workflow")
		}
		t.Skip("node is not installed, so the launcher cannot be run")
	}
	var host Target
	for _, tgt := range Targets() {
		if tgt.GOOS == runtime.GOOS && tgt.GOARCH == runtime.GOARCH {
			host = tgt
		}
	}
	if host.OS == "" {
		t.Skipf("the npm distribution does not cover %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	root := t.TempDir()
	self := filepath.Join(root, "node_modules", "mcpsnoop", "bin")
	if err := os.MkdirAll(self, 0o755); err != nil {
		t.Fatal(err)
	}
	copyInto(t, filepath.Join(source(t), "bin", "mcpsnoop.js"), filepath.Join(self, "mcpsnoop.js"))

	if stub != "" {
		pkg := filepath.Join(root, "node_modules", "@mcpsnoop", host.Dir())
		if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "package.json"),
			[]byte(`{"name":"`+host.Package()+`","version":"0.0.0-test"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "bin", host.Binary()), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(self, "mcpsnoop.js")
}

// run drives the launcher and returns what a caller would see.
func run(t *testing.T, path, stdin string, args ...string) (stdout, stderr string, code int, signal syscall.Signal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", append([]string{path}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("the launcher never exited, so a client waiting on it would hang")
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return out.String(), errBuf.String(), -1, ws.Signal()
	}
	return out.String(), errBuf.String(), cmd.ProcessState.ExitCode(), 0
}

func TestTheLauncherPassesItsArgumentsThroughUnchanged(t *testing.T) {
	// The stand-in prints its arguments itself. Anything that re-parsed them on
	// the way, node -e included, would eat the -- and the test would be
	// measuring that instead of the launcher.
	path := launcher(t, `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done
`)
	// mcpsnoop is told what to wrap after a --, and the flags meant for the
	// wrapped server look exactly like flags meant for the wrapper.
	stdout, stderr, code, _ := run(t, path, "", "--", "node", "build/index.js", "--port", "3000")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	got := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	want := []string{"--", "node", "build/index.js", "--port", "3000"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("the binary was called with %q, want %q", got, want)
	}
}

func TestTheLauncherReturnsTheBinarysExitCode(t *testing.T) {
	path := launcher(t, "#!/bin/sh\nexit 42\n")
	// mcpsnoop check is meant to be a CI gate, and mcpsnoop reports a wrapped
	// server's exit code as its own. A wrapper that flattened either one would
	// turn a red build green.
	if _, _, code, _ := run(t, path, ""); code != 42 {
		t.Errorf("exit %d, want 42", code)
	}
}

func TestTheLauncherDiesTheWayTheBinaryDied(t *testing.T) {
	path := launcher(t, "#!/bin/sh\nkill -TERM $$\n")
	_, _, code, sig := run(t, path, "")
	// Installing a signal handler takes away node's own default, which is to
	// die. Raising the signal again while the handler is still installed only
	// calls the handler, and the wrapper sits in its event loop for ever.
	if sig != syscall.SIGTERM {
		t.Errorf("exited with code %d and signal %v, want death by SIGTERM", code, sig)
	}
}

func TestTheLauncherForwardsASignalToTheBinary(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "signalled")
	path := launcher(t, `#!/bin/sh
trap 'printf caught > "`+mark+`"; exit 0' TERM
printf up > "`+mark+`.ready"
while :; do sleep 0.05; done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", path)
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(mark + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the stand-in binary never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A client stops its server by signalling the process it launched, which is
	// the wrapper. Without forwarding, the shim never hears about it.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the launcher did not exit after being signalled")
	}
	if _, err := os.Stat(mark); err != nil {
		t.Error("the binary was never signalled, so mcpsnoop's own shutdown never ran")
	}
}

func TestTheLauncherExplainsAMissingPlatformPackage(t *testing.T) {
	path := launcher(t, "")
	stdout, stderr, code, _ := run(t, path, "")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	// npm drops an optional dependency for reasons that are not the user's
	// fault, so the way out has to be in the message rather than in a stack
	// trace naming a file inside node_modules.
	for _, want := range []string{"@mcpsnoop/", "npm install", "npm cache clean", "go install"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the message does not mention %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		// Anything on stdout is a JSON-RPC frame as far as the client is
		// concerned, so an error written there corrupts the stream.
		t.Errorf("wrote %q to stdout, which the client reads as protocol", stdout)
	}
}

func TestTheLauncherAddsNothingToTheStream(t *testing.T) {
	path := launcher(t, "#!/bin/sh\ncat\n")
	// The whole point of the shim is to be transparent, and the wrapper sits
	// in the same pipe. A byte added or a newline dropped here breaks framing
	// for every client.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	stdout, stderr, code, _ := run(t, path, body)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if stdout != body {
		t.Errorf("the stream came back changed:\n got %q\nwant %q", stdout, body)
	}
}

func TestTheLauncherRunsABinaryThatLostItsExecuteBit(t *testing.T) {
	path := launcher(t, "#!/bin/sh\nexit 21\n")
	// npm carries the mode through, and TestBuildLeavesTheBinaryExecutable
	// keeps it on going in. Other clients have not always, and the result is a
	// package that installs cleanly and then cannot start at all.
	var host Target
	for _, tgt := range Targets() {
		if tgt.GOOS == runtime.GOOS && tgt.GOARCH == runtime.GOARCH {
			host = tgt
		}
	}
	binary := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "@mcpsnoop", host.Dir(), "bin", host.Binary())
	if err := os.Chmod(binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code, _ := run(t, path, ""); code != 21 {
		t.Errorf("exit %d, want 21, so the binary never ran", code)
	}
}
