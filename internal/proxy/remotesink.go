package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

// RemoteSink streams envelopes to a remote hub over TLS. Like SocketSink, it is
// best-effort and drops rather than back-pressuring MCP traffic.
type RemoteSink struct {
	*SocketSink
}

// RemoteSinkConfig configures the secure remote stream.
type RemoteSinkConfig struct {
	Addr   string
	Token  string
	CAFile string
}

// NewRemoteSink dials a remote TLS hub lazily and authenticates with token.
func NewRemoteSink(cfg RemoteSinkConfig, buffer int) (*RemoteSink, error) {
	if cfg.Addr == "" {
		return nil, errors.New("remote sink address is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("remote sink token is required")
	}
	tlsCfg, err := remoteTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	s := newSocketSink(cfg.Addr, buffer, func(ctx context.Context, addr string) (net.Conn, error) {
		var d tls.Dialer
		d.Config = tlsCfg
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		enc := json.NewEncoder(conn)
		if err := enc.Encode(remoteHello{Version: "1", Token: cfg.Token}); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	})
	return &RemoteSink{SocketSink: s}, nil
}

type remoteHello struct {
	Version string `json:"mcpsnoop_remote"`
	Token   string `json:"token"`
}

func remoteTLSConfig(cfg RemoteSinkConfig) (*tls.Config, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: host,
	}
	if cfg.CAFile == "" {
		return tlsCfg, nil
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("remote sink CA file contains no PEM certificates")
	}
	tlsCfg.RootCAs = roots
	return tlsCfg, nil
}

func defaultDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func newSocketSink(addr string, buffer int, dial func(context.Context, string) (net.Conn, error)) *SocketSink {
	if buffer <= 0 {
		buffer = 4096
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &SocketSink{
		addr:   addr,
		ch:     make(chan Envelope, buffer),
		cancel: cancel,
		done:   make(chan struct{}),
		dial:   dial,
	}
	go s.run(ctx)
	return s
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
