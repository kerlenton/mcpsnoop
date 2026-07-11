package main

import (
	"path/filepath"
	"testing"
)

func TestRemoteSSHCommandDefaultsRemoteHomeFromSSHUser(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, assumedHome, err := remoteSSHCommand(remoteTunnelOptions{Target: "remote-user@remote-host"})
	if err != nil {
		t.Fatal(err)
	}

	localSocket := filepath.Join(stateDir, "hub.sock")
	want := "ssh -N -o StreamLocalBindUnlink=yes -R /home/remote-user/.local/state/mcpsnoop/hub.sock:" + localSocket + " remote-user@remote-host"
	if got != want {
		t.Fatalf("remoteSSHCommand() = %q, want %q", got, want)
	}
	if assumedHome != "/home/remote-user" {
		t.Fatalf("assumedHome = %q, want %q", assumedHome, "/home/remote-user")
	}
}

func TestRemoteSSHCommandUsesRemoteMCPSnoopHome(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, assumedHome, err := remoteSSHCommand(remoteTunnelOptions{
		Target:             "prod",
		RemoteMCPSnoopHome: "/srv/mcpsnoop-state",
	})
	if err != nil {
		t.Fatal(err)
	}

	localSocket := filepath.Join(stateDir, "hub.sock")
	want := "ssh -N -o StreamLocalBindUnlink=yes -R /srv/mcpsnoop-state/hub.sock:" + localSocket + " prod"
	if got != want {
		t.Fatalf("remoteSSHCommand() = %q, want %q", got, want)
	}
	if assumedHome != "" {
		t.Fatalf("assumedHome = %q, want empty when --remote-mcpsnoop-home is set", assumedHome)
	}
}

func TestRemoteSSHCommandRequiresRemoteHomeWhenTargetHasNoUser(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())

	_, _, err := remoteSSHCommand(remoteTunnelOptions{Target: "prod"})
	if err == nil {
		t.Fatal("remoteSSHCommand() error = nil, want error")
	}
}

func TestRemoteSSHCommandQuotesSocketBinding(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state dir")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, _, err := remoteSSHCommand(remoteTunnelOptions{
		Target:             "remote-user@remote-host",
		RemoteMCPSnoopHome: "/srv/mcpsnoop state",
	})
	if err != nil {
		t.Fatal(err)
	}

	localSocket := filepath.Join(stateDir, "hub.sock")
	want := "ssh -N -o StreamLocalBindUnlink=yes -R '/srv/mcpsnoop state/hub.sock:" + localSocket + "' remote-user@remote-host"
	if got != want {
		t.Fatalf("remoteSSHCommand() = %q, want %q", got, want)
	}
}

func TestRemoteSSHCommandUsesRemoteXDGStateHome(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, assumedHome, err := remoteSSHCommand(remoteTunnelOptions{
		Target:             "remote-user@remote-host",
		RemoteXDGStateHome: "/var/lib/state",
	})
	if err != nil {
		t.Fatal(err)
	}

	localSocket := filepath.Join(stateDir, "hub.sock")
	want := "ssh -N -o StreamLocalBindUnlink=yes -R /var/lib/state/mcpsnoop/hub.sock:" + localSocket + " remote-user@remote-host"
	if got != want {
		t.Fatalf("remoteSSHCommand() = %q, want %q", got, want)
	}
	if assumedHome != "" {
		t.Fatalf("assumedHome = %q, want empty when --remote-xdg-state-home is set", assumedHome)
	}
}

func TestRemoteSSHCommandRejectsMultipleLocationOverrides(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())

	_, _, err := remoteSSHCommand(remoteTunnelOptions{
		Target:             "remote-user@remote-host",
		RemoteHome:         "/Users/remote-user",
		RemoteXDGStateHome: "/var/lib/state",
	})
	if err == nil {
		t.Fatal("remoteSSHCommand() error = nil, want error for two location overrides")
	}
}

func TestRemoteUnsupportedOS(t *testing.T) {
	if _, unsupported := remoteUnsupportedOS("windows"); !unsupported {
		t.Fatal("windows workstation should be reported as unsupported")
	}
	for _, goos := range []string{"linux", "darwin"} {
		if _, unsupported := remoteUnsupportedOS(goos); unsupported {
			t.Fatalf("%s workstation should be supported", goos)
		}
	}
}

func TestRemoteSSHCommandNoAssumedHomeWhenRemoteHomeSet(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())

	_, assumedHome, err := remoteSSHCommand(remoteTunnelOptions{
		Target:     "remote-user@remote-host",
		RemoteHome: "/Users/remote-user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if assumedHome != "" {
		t.Fatalf("assumedHome = %q, want empty when --remote-home is set", assumedHome)
	}
}
