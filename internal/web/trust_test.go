package web

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"dbcat/internal/store"
)

// handshakeOnlySSH accepts SSH handshakes (no client auth) so the client's
// host-key check runs; it returns the server address and its key.
func handshakeOnlySSH(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ssh.ServerConfig{NoClientAuth: true}
	conf.AddHostKey(signer)
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
			go func() {
				defer nc.Close()
				if conn, chans, reqs, err := ssh.NewServerConn(nc, conf); err == nil {
					go ssh.DiscardRequests(reqs)
					go func() {
						for c := range chans {
							_ = c.Reject(ssh.Prohibited, "no")
						}
					}()
					conn.Wait()
				}
			}()
		}
	}()
	return ln.Addr().String(), signer.PublicKey()
}

func TestTrustHostKeyFlow(t *testing.T) {
	known := filepath.Join(t.TempDir(), "known_hosts")
	t.Setenv("DBCAT_KNOWN_HOSTS", known)
	addr, key := handshakeOnlySSH(t)
	host, port, _ := net.SplitHostPort(addr)

	st, _ := store.OpenAt(filepath.Join(t.TempDir(), "c.json"))
	s, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	test := func() string {
		return post("/conn/test", url.Values{
			"kind": {"postgres"}, "name": {"t"}, "host": {"db.internal"}, "user": {"u"},
			"ssh_on": {"on"}, "ssh_host": {host}, "ssh_port": {port}, "ssh_user": {"u"}, "ssh_password": {"p"},
		}).Body.String()
	}

	// 1. unknown host: the dialog offers a trust button carrying host, key and fingerprint
	out := test()
	wantFP := ssh.FingerprintSHA256(key)
	// base64 fingerprints can contain '+', which html/template escapes in attributes
	if !strings.Contains(out, `data-action="trust-host"`) || !strings.Contains(out, strings.ReplaceAll(wantFP, "+", "&#43;")) {
		t.Fatalf("expected a trust button with the fingerprint:\n%s", out)
	}
	attr := func(name string) string {
		m := regexp.MustCompile(`data-` + name + `="([^"]*)"`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no data-%s in %s", name, out)
		}
		return strings.NewReplacer("&#43;", "+", "&#34;", `"`, "&amp;", "&").Replace(m[1])
	}
	form := url.Values{"host": {attr("host")}, "key": {attr("key")}, "fp": {attr("fp")}}

	// 2. a request whose fingerprint does not match the key is refused and writes nothing
	bad := url.Values{"host": form["host"], "key": form["key"], "fp": {"SHA256:something-else"}}
	if rec := post("/conn/trust-host", bad); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("mismatched fingerprint = %d, want 422", rec.Code)
	}
	if _, err := os.Stat(known); err == nil {
		t.Fatal("known_hosts written for a rejected request")
	}

	// 3. confirmed request registers the key; the button disappears afterwards
	if rec := post("/conn/trust-host", form); rec.Code != 200 || !strings.Contains(rec.Header().Get("HX-Trigger"), "toast") {
		t.Fatalf("trust = %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if b, _ := os.ReadFile(known); !strings.Contains(string(b), "ssh-ed25519") {
		t.Fatalf("known_hosts = %q", b)
	}
	if out := test(); strings.Contains(out, "trust-host") {
		t.Errorf("trust button still offered after registering:\n%s", out)
	}
}
