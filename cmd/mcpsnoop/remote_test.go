package main

import (
	"path/filepath"
	"testing"
)

func TestRemoteSSHCommandDefaultsRemoteHomeFromSSHUser(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, err := remoteSSHCommand(remoteTunnelOptions{Target: "remote-user@remote-host"})
	if err != nil {
		t.Fatal(err)
	}

	localSocket := filepath.Join(stateDir, "hub.sock")
	want := "ssh -N -o StreamLocalBindUnlink=yes -R /home/remote-user/.local/state/mcpsnoop/hub.sock:" + localSocket + " remote-user@remote-host"
	if got != want {
		t.Fatalf("remoteSSHCommand() = %q, want %q", got, want)
	}
}

func TestRemoteSSHCommandUsesRemoteMCPSnoopHome(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, err := remoteSSHCommand(remoteTunnelOptions{
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
}

func TestRemoteSSHCommandRequiresRemoteHomeWhenTargetHasNoUser(t *testing.T) {
	t.Setenv("MCPSNOOP_HOME", t.TempDir())

	_, err := remoteSSHCommand(remoteTunnelOptions{Target: "prod"})
	if err == nil {
		t.Fatal("remoteSSHCommand() error = nil, want error")
	}
}

func TestRemoteSSHCommandQuotesSocketBinding(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state dir")
	t.Setenv("MCPSNOOP_HOME", stateDir)

	got, err := remoteSSHCommand(remoteTunnelOptions{
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
