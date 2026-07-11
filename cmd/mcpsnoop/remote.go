package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/kerlenton/mcpsnoop/internal/paths"
)

type remoteTunnelOptions struct {
	Target             string
	RemoteHome         string
	RemoteMCPSnoopHome string
	RemoteXDGStateHome string
}

// runRemote prints the SSH reverse tunnel command for live remote viewing. It
// deliberately does not exec SSH, so users keep full control over credentials,
// host verification, jump hosts, and local SSH policy.
func runRemote(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop remote", flag.ExitOnError)
	var opts remoteTunnelOptions
	fs.StringVar(&opts.RemoteHome, "remote-home", "", "remote home directory. The default is the Linux /home/<user> from user@host, so set this for macOS, root, or a custom home")
	fs.StringVar(&opts.RemoteMCPSnoopHome, "remote-mcpsnoop-home", "", "remote MCPSNOOP_HOME directory, when it is set on the remote")
	fs.StringVar(&opts.RemoteXDGStateHome, "remote-xdg-state-home", "", "remote XDG_STATE_HOME directory, when it is set on the remote")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mcpsnoop remote [--remote-home DIR | --remote-mcpsnoop-home DIR | --remote-xdg-state-home DIR] <user@host>\n\n")
		fmt.Fprintf(os.Stderr, "Print the ssh -R command that forwards the remote mcpsnoop socket to this workstation.\n")
		fmt.Fprintf(os.Stderr, "The remote must be Unix (Linux or macOS). SSH Unix-socket forwarding does not work to a Windows remote.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	opts.Target = fs.Arg(0)

	if msg, unsupported := remoteUnsupportedOS(runtime.GOOS); unsupported {
		fmt.Fprintln(os.Stderr, "mcpsnoop remote:", msg)
		return 2
	}

	cmd, assumedHome, err := remoteSSHCommand(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop remote:", err)
		return 2
	}
	if assumedHome != "" {
		fmt.Fprintf(os.Stderr, "mcpsnoop remote: assuming remote home %s, pass --remote-home for macOS, root, or a custom home\n", assumedHome)
	}
	fmt.Println(cmd)
	return 0
}

// remoteUnsupportedOS reports whether the workstation OS can originate the SSH
// Unix-socket tunnel. Windows OpenSSH does not do streamlocal -R forwarding, so
// the printed command would never work. Steer the user to the log-copy path
// instead of handing them a command that silently fails.
func remoteUnsupportedOS(goos string) (msg string, unsupported bool) {
	if goos == "windows" {
		return "the live tunnel needs a Unix workstation (Linux or macOS). SSH Unix-socket forwarding does not run on Windows, so copy the remote logs instead (see the post-mortem section in the README)", true
	}
	return "", false
}

// remoteSSHCommand returns the ssh -R command and, when the remote home was
// guessed rather than passed, the assumed home so the caller can warn about it.
func remoteSSHCommand(opts remoteTunnelOptions) (cmd, assumedHome string, err error) {
	remoteSocket, assumedHome, err := remoteSocketPath(opts)
	if err != nil {
		return "", "", err
	}
	localSocket, err := localSocketPath()
	if err != nil {
		return "", "", err
	}
	binding := remoteSocket + ":" + localSocket
	return strings.Join([]string{
		"ssh",
		"-N",
		"-o",
		"StreamLocalBindUnlink=yes",
		"-R",
		shellQuote(binding),
		shellQuote(opts.Target),
	}, " "), assumedHome, nil
}

// remoteSocketPath resolves the remote hub socket, mirroring the on-remote
// resolution order (MCPSNOOP_HOME, then XDG_STATE_HOME, then the home). At most
// one override may be set, and it names the single non-default piece. assumedHome is
// non-empty only when the home fell back to the Linux /home/<user> guess.
func remoteSocketPath(opts remoteTunnelOptions) (socket, assumedHome string, err error) {
	if opts.Target == "" {
		return "", "", fmt.Errorf("missing SSH target")
	}
	overrides := 0
	for _, v := range []string{opts.RemoteHome, opts.RemoteMCPSnoopHome, opts.RemoteXDGStateHome} {
		if v != "" {
			overrides++
		}
	}
	if overrides > 1 {
		return "", "", fmt.Errorf("set at most one of --remote-home, --remote-mcpsnoop-home, --remote-xdg-state-home")
	}

	switch {
	case opts.RemoteMCPSnoopHome != "":
		if !path.IsAbs(opts.RemoteMCPSnoopHome) {
			return "", "", fmt.Errorf("--remote-mcpsnoop-home must be an absolute path")
		}
		return path.Join(opts.RemoteMCPSnoopHome, "hub.sock"), "", nil
	case opts.RemoteXDGStateHome != "":
		if !path.IsAbs(opts.RemoteXDGStateHome) {
			return "", "", fmt.Errorf("--remote-xdg-state-home must be an absolute path")
		}
		return path.Join(opts.RemoteXDGStateHome, "mcpsnoop", "hub.sock"), "", nil
	}

	remoteHome := opts.RemoteHome
	if remoteHome == "" {
		user := sshTargetUser(opts.Target)
		if user == "" {
			return "", "", fmt.Errorf("cannot infer remote home from %q; pass --remote-home, --remote-mcpsnoop-home, or --remote-xdg-state-home", opts.Target)
		}
		remoteHome = path.Join("/home", user)
		assumedHome = remoteHome
	}
	if !path.IsAbs(remoteHome) {
		return "", "", fmt.Errorf("--remote-home must be an absolute path")
	}
	return path.Join(remoteHome, ".local", "state", "mcpsnoop", "hub.sock"), assumedHome, nil
}

func localSocketPath() (string, error) {
	socket := paths.SocketPath()
	if filepath.IsAbs(socket) {
		return socket, nil
	}
	return filepath.Abs(socket)
}

func sshTargetUser(target string) string {
	before, _, ok := strings.Cut(target, "@")
	if !ok || before == "" || strings.ContainsAny(before, "/:") {
		return ""
	}
	return before
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func shellSafe(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if strings.ContainsRune("@%_+=:,./-", r) {
			continue
		}
		return false
	}
	return true
}
