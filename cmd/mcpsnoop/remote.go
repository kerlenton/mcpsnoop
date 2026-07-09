package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/kerlenton/mcpsnoop/internal/paths"
)

type remoteTunnelOptions struct {
	Target             string
	RemoteHome         string
	RemoteMCPSnoopHome string
}

// runRemote prints the SSH reverse tunnel command for live remote viewing. It
// deliberately does not exec SSH, so users keep full control over credentials,
// host verification, jump hosts, and local SSH policy.
func runRemote(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop remote", flag.ExitOnError)
	var opts remoteTunnelOptions
	fs.StringVar(&opts.RemoteHome, "remote-home", "", "remote user's home directory (default: /home/<user> from user@host)")
	fs.StringVar(&opts.RemoteMCPSnoopHome, "remote-mcpsnoop-home", "", "remote MCPSNOOP_HOME directory (overrides --remote-home)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mcpsnoop remote [--remote-home /home/user | --remote-mcpsnoop-home /path] <user@host>\n\n")
		fmt.Fprintf(os.Stderr, "Print the ssh -R command that forwards the remote mcpsnoop socket to this workstation.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	opts.Target = fs.Arg(0)

	cmd, err := remoteSSHCommand(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop remote:", err)
		return 2
	}
	fmt.Println(cmd)
	return 0
}

func remoteSSHCommand(opts remoteTunnelOptions) (string, error) {
	remoteSocket, err := remoteSocketPath(opts)
	if err != nil {
		return "", err
	}
	localSocket, err := localSocketPath()
	if err != nil {
		return "", err
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
	}, " "), nil
}

func remoteSocketPath(opts remoteTunnelOptions) (string, error) {
	if opts.Target == "" {
		return "", fmt.Errorf("missing SSH target")
	}
	if opts.RemoteHome != "" && opts.RemoteMCPSnoopHome != "" {
		return "", fmt.Errorf("use either --remote-home or --remote-mcpsnoop-home, not both")
	}
	if opts.RemoteMCPSnoopHome != "" {
		if !path.IsAbs(opts.RemoteMCPSnoopHome) {
			return "", fmt.Errorf("--remote-mcpsnoop-home must be an absolute path")
		}
		return path.Join(opts.RemoteMCPSnoopHome, "hub.sock"), nil
	}

	remoteHome := opts.RemoteHome
	if remoteHome == "" {
		user := sshTargetUser(opts.Target)
		if user == "" {
			return "", fmt.Errorf("cannot infer remote home from %q; pass --remote-home or --remote-mcpsnoop-home", opts.Target)
		}
		remoteHome = path.Join("/home", user)
	}
	if !path.IsAbs(remoteHome) {
		return "", fmt.Errorf("--remote-home must be an absolute path")
	}
	return path.Join(remoteHome, ".local", "state", "mcpsnoop", "hub.sock"), nil
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
