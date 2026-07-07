package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func env(session string, seq uint64, method string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: session,
		Seq:       seq,
		TS:        time.Now(),
		Direction: proxy.ClientToServer,
		Transport: "stdio",
		Raw:       json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, seq, method)),
	}
}

func writeTestCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// writeLog writes envelopes as JSONL to the session log for session.
func writeLog(t *testing.T, dir, session string, envs ...proxy.Envelope) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range envs {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHubBackfillLiveDedup verifies that file backfill and a live socket stream
// merge for the same session without duplicates, and that a fresh session over
// the socket comes through.
func TestHubBackfillLiveDedup(t *testing.T) {
	sessionsDir := t.TempDir()
	// Short socket path: macOS caps unix socket paths at ~104 bytes.
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("mcpsnoop-test-%d.sock", os.Getpid()))
	defer os.Remove(sockPath)

	// History on disk for session s1: seq 1..3.
	writeLog(t, sessionsDir, "s1", env("s1", 1, "initialize"), env("s1", 2, "tools/list"), env("s1", 3, "tools/call"))

	got := make(chan proxy.Envelope, 64)
	h := New(sockPath, sessionsDir, func(e proxy.Envelope) { got <- e })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	// Live shim: re-sends s1 seq 2,3 (overlap → must dedup) and 4 (new), plus a
	// brand-new session s2.
	sink := proxy.NewSocketSink(sockPath, 0)
	defer sink.Close()
	// Give the hub a moment to finish backfill and start accepting.
	time.Sleep(300 * time.Millisecond)
	for _, e := range []proxy.Envelope{
		env("s1", 2, "tools/list"), env("s1", 3, "tools/call"), env("s1", 4, "ping"),
		env("s2", 1, "initialize"), env("s2", 2, "tools/list"),
	} {
		sink.Emit(e)
	}

	// Expect 6 unique envelopes: s1{1,2,3,4} + s2{1,2}.
	maxSeq := map[string]uint64{}
	count := 0
	deadline := time.After(3 * time.Second)
	for count < 6 {
		select {
		case e := <-got:
			count++
			if e.Seq <= maxSeq[e.SessionID] {
				t.Fatalf("duplicate/out-of-order frame: session=%s seq=%d (already saw %d)", e.SessionID, e.Seq, maxSeq[e.SessionID])
			}
			maxSeq[e.SessionID] = e.Seq
		case <-deadline:
			t.Fatalf("timed out: got %d/6 frames, maxSeq=%v", count, maxSeq)
		}
	}
	if maxSeq["s1"] != 4 || maxSeq["s2"] != 2 {
		t.Fatalf("unexpected high-water marks: %v", maxSeq)
	}

	// No extra frames should arrive (e.g. a leaked duplicate).
	select {
	case e := <-got:
		t.Fatalf("unexpected extra frame: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHubRemoteTLSAuth(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeTestCert(t, dir)

	got := make(chan proxy.Envelope, 4)
	h := New("", dir, func(e proxy.Envelope) { got <- e }, WithRemote(RemoteConfig{
		Listen: "127.0.0.1:0", Token: "secret", CertFile: certFile, KeyFile: keyFile,
	}))
	ln, err := h.listenRemote()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.accept(ctx, ln, true)

	sink, err := proxy.NewRemoteSink(proxy.RemoteSinkConfig{
		Addr: ln.Addr().String(), Token: "secret", CAFile: certFile,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.Emit(env("remote", 1, "initialize"))

	select {
	case e := <-got:
		if e.SessionID != "remote" || e.Seq != 1 {
			t.Fatalf("unexpected envelope: %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for remote envelope")
	}
}

func TestHubRemoteRejectsBadToken(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeTestCert(t, dir)

	got := make(chan proxy.Envelope, 4)
	h := New("", dir, func(e proxy.Envelope) { got <- e }, WithRemote(RemoteConfig{
		Listen: "127.0.0.1:0", Token: "secret", CertFile: certFile, KeyFile: keyFile,
	}))
	ln, err := h.listenRemote()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.accept(ctx, ln, true)

	sink, err := proxy.NewRemoteSink(proxy.RemoteSinkConfig{
		Addr: ln.Addr().String(), Token: "wrong", CAFile: certFile,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.Emit(env("remote", 1, "initialize"))

	select {
	case e := <-got:
		t.Fatalf("bad token was accepted: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}
