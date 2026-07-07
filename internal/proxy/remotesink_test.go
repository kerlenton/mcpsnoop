package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewRemoteSinkRequiresAddressAndToken(t *testing.T) {
	cases := []struct {
		name string
		cfg  RemoteSinkConfig
	}{
		{name: "missing address", cfg: RemoteSinkConfig{Token: "secret"}},
		{name: "missing token", cfg: RemoteSinkConfig{Addr: "127.0.0.1:7447"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := NewRemoteSink(tc.cfg, 1)
			if err == nil {
				sink.Close()
				t.Fatal("NewRemoteSink succeeded, want error")
			}
		})
	}
}

func TestRemoteTLSConfig(t *testing.T) {
	cfg, err := remoteTLSConfig(RemoteSinkConfig{Addr: "example.com:7447", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "example.com" {
		t.Fatalf("ServerName = %q, want example.com", cfg.ServerName)
	}
	if cfg.MinVersion == 0 {
		t.Fatal("MinVersion was not set")
	}
}

func TestRemoteTLSConfigRejectsBadCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(path, []byte("not a pem certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteTLSConfig(RemoteSinkConfig{Addr: "example.com:7447", Token: "secret", CAFile: path}); err == nil {
		t.Fatal("remoteTLSConfig succeeded with invalid CA file, want error")
	}
}

func TestRemoteTLSConfigRejectsAddressWithoutPort(t *testing.T) {
	if _, err := remoteTLSConfig(RemoteSinkConfig{Addr: "example.com", Token: "secret"}); err == nil {
		t.Fatal("remoteTLSConfig succeeded without host:port, want error")
	}
}
