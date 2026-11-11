package db

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testSSHServer is a minimal in-process SSH server: password "pw" or one
// authorized key for user "tun", and TCP forwarding (direct-tcpip) only.
type testSSHServer struct {
	addr    string
	hostKey ssh.Signer
}

func newSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, priv
}

func startSSHServer(t *testing.T, authorized ssh.PublicKey) *testSSHServer {
	t.Helper()
	hostKey, _ := newSigner(t)
	conf := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "tun" && string(pw) == "pw" {
				return nil, nil
			}
			return nil, io.EOF
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && c.User() == "tun" && string(key.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, io.EOF
		},
	}
	conf.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSH(nc, conf)
		}
	}()
	return &testSSHServer{addr: ln.Addr().String(), hostKey: hostKey}
}

func serveSSH(nc net.Conn, conf *ssh.ServerConfig) {
	defer nc.Close()
	conn, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "direct-tcpip" {
			_ = nch.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		// payload: string host, uint32 port, string origHost, uint32 origPort
		p := nch.ExtraData()
		hl := int(binary.BigEndian.Uint32(p))
		host := string(p[4 : 4+hl])
		port := binary.BigEndian.Uint32(p[4+hl:])
		target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		if err != nil {
			_ = nch.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			target.Close()
			continue
		}
		go ssh.DiscardRequests(creqs)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); io.Copy(ch, target); ch.CloseWrite() }()
		go func() { defer wg.Done(); io.Copy(target, ch); target.Close() }()
		go func() { wg.Wait(); ch.Close() }()
	}
	conn.Close()
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// trust writes a known_hosts file that trusts key for the server's address.
func trust(t *testing.T, srv *testSSHServer, key ssh.PublicKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{srv.addr}, key) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBCAT_KNOWN_HOSTS", path)
}

func sshCfg(srv *testSSHServer) *SSHConfig {
	host, portStr, _ := net.SplitHostPort(srv.addr)
	port, _ := strconv.Atoi(portStr)
	return &SSHConfig{Host: host, Port: port, User: "tun", Password: "pw"}
}

func roundTrip(t *testing.T, cl *ssh.Client, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := cl.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("dial through tunnel: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestSSHTunnelPassword(t *testing.T) {
	srv := startSSHServer(t, nil)
	trust(t, srv, srv.hostKey.PublicKey())
	cl, err := dialSSH(context.Background(), sshCfg(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	roundTrip(t, cl, startEcho(t))
}

func TestSSHTunnelKeyAndPassphrase(t *testing.T) {
	userSigner, userPriv := newSigner(t)
	srv := startSSHServer(t, userSigner.PublicKey())
	trust(t, srv, srv.hostKey.PublicKey())
	dir := t.TempDir()

	plain := filepath.Join(dir, "id_plain")
	block, err := ssh.MarshalPrivateKey(userPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, plain, block)

	locked := filepath.Join(dir, "id_locked")
	block, err = ssh.MarshalPrivateKeyWithPassphrase(userPriv, "", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, locked, block)

	cfg := sshCfg(srv)
	cfg.Password = ""
	cfg.KeyFile = plain
	cl, err := dialSSH(context.Background(), cfg)
	if err != nil {
		t.Fatalf("plain key: %v", err)
	}
	cl.Close()

	cfg.KeyFile = locked
	if _, err := dialSSH(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "암호") {
		t.Fatalf("locked key without passphrase must ask for it, got %v", err)
	}
	cfg.Password = "wrong"
	if _, err := dialSSH(context.Background(), cfg); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	cfg.Password = "s3cret"
	cl, err = dialSSH(context.Background(), cfg)
	if err != nil {
		t.Fatalf("locked key: %v", err)
	}
	cl.Close()

	cfg.KeyFile = filepath.Join(dir, "missing")
	if _, err := dialSSH(context.Background(), cfg); err == nil {
		t.Fatal("missing key file accepted")
	}
}

func writePEM(t *testing.T, path string, block *pem.Block) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSSHHostKeyPolicy(t *testing.T) {
	srv := startSSHServer(t, nil)

	// 1. known_hosts missing -> unknown host, with fingerprint and registration hint
	t.Setenv("DBCAT_KNOWN_HOSTS", filepath.Join(t.TempDir(), "nope"))
	_, err := dialSSH(context.Background(), sshCfg(srv))
	if err == nil || !strings.Contains(err.Error(), ssh.FingerprintSHA256(srv.hostKey.PublicKey())) || !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Fatalf("missing known_hosts must refuse with fingerprint + hint, got %v", err)
	}

	// 2. empty known_hosts -> unknown host
	empty := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(empty, nil, 0o600)
	t.Setenv("DBCAT_KNOWN_HOSTS", empty)
	if _, err := dialSSH(context.Background(), sshCfg(srv)); err == nil || !strings.Contains(err.Error(), "알 수 없는 SSH 호스트") {
		t.Fatalf("unknown host must be refused, got %v", err)
	}

	// 3. a different key registered for this host -> changed-key refusal
	other, _ := newSigner(t)
	trust(t, srv, other.PublicKey())
	if _, err := dialSSH(context.Background(), sshCfg(srv)); err == nil || !strings.Contains(err.Error(), "다릅니다") {
		t.Fatalf("changed host key must be refused, got %v", err)
	}

	// 4. correct key -> works; wrong password still fails
	trust(t, srv, srv.hostKey.PublicKey())
	bad := sshCfg(srv)
	bad.Password = "nope"
	if _, err := dialSSH(context.Background(), bad); err == nil {
		t.Fatal("wrong password accepted")
	}
	cl, err := dialSSH(context.Background(), sshCfg(srv))
	if err != nil {
		t.Fatal(err)
	}
	cl.Close()
}

func TestSSHConfigValidation(t *testing.T) {
	for name, c := range map[string]*SSHConfig{
		"no host":     {User: "u", Password: "p"},
		"no user":     {Host: "h", Password: "p"},
		"no password": {Host: "h", User: "u"},
	} {
		if _, err := dialSSH(context.Background(), c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for in, want := range map[string]string{
		"~/.ssh/id": filepath.Join(home, ".ssh/id"), "~": home,
		"/abs/path": "/abs/path", "rel/~/x": "rel/~/x", "~user/x": "~user/x", "": "",
	} {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrustHostKey(t *testing.T) {
	srv := startSSHServer(t, nil)
	path := filepath.Join(t.TempDir(), "sub", "known_hosts") // directory does not exist yet
	t.Setenv("DBCAT_KNOWN_HOSTS", path)

	// 1. the unknown-host error is typed and carries what the user must verify
	_, err := dialSSH(context.Background(), sshCfg(srv))
	var uh *UnknownHostError
	if !errors.As(err, &uh) {
		t.Fatalf("want *UnknownHostError, got %T: %v", err, err)
	}
	if uh.Host != srv.addr || uh.Fingerprint != ssh.FingerprintSHA256(srv.hostKey.PublicKey()) || !strings.HasPrefix(uh.Key, "ssh-ed25519 ") {
		t.Fatalf("error fields = %+v", uh)
	}

	// 2. wrong fingerprint or malformed input is rejected and writes nothing
	for name, args := range map[string][3]string{
		"fingerprint mismatch": {uh.Host, uh.Key, "SHA256:other"},
		"garbage key":          {uh.Host, "not a key", uh.Fingerprint},
		"bad host":             {"no-port", uh.Key, uh.Fingerprint},
		"host with space":      {"a b:22", uh.Key, uh.Fingerprint},
		"host list":            {"a:22,b:22", uh.Key, uh.Fingerprint},
	} {
		if err := TrustHostKey(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("known_hosts must not exist after rejected requests")
	}

	// 3. confirming registers the key (file 0600) and the next connection works
	if err := TrustHostKey(uh.Host, uh.Key, uh.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("known_hosts mode: %v %v", st, err)
	}
	cl, err := dialSSH(context.Background(), sshCfg(srv))
	if err != nil {
		t.Fatalf("connect after trusting: %v", err)
	}
	cl.Close()

	// 4. an existing entry without a trailing newline is not corrupted
	data, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.TrimRight(string(data), "\n")), 0o600)
	other := startSSHServer(t, nil)
	_, err = dialSSH(context.Background(), sshCfg(other))
	if !errors.As(err, &uh) {
		t.Fatal(err)
	}
	if err := TrustHostKey(uh.Host, uh.Key, uh.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(readFile(t, path)), "\n") + 1; lines != 2 {
		t.Fatalf("expected two entries on separate lines, file:\n%s", readFile(t, path))
	}
	for _, s := range []*testSSHServer{srv, other} {
		cl, err := dialSSH(context.Background(), sshCfg(s))
		if err != nil {
			t.Fatalf("server %s after both trusted: %v", s.addr, err)
		}
		cl.Close()
	}

	// 5. a CHANGED key is not an UnknownHostError: no trust path is offered
	impostor := startSSHServer(t, nil)
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{impostor.addr}, srv.hostKey.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = dialSSH(context.Background(), sshCfg(impostor))
	if err == nil || errors.As(err, &uh) || !strings.Contains(err.Error(), "다릅니다") {
		t.Fatalf("changed key must be a hard failure, got %T: %v", err, err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
