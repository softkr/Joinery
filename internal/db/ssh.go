package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const sshTimeout = 10 * time.Second

// KnownHostsPath is the file SSH host keys are verified against: the
// DBCAT_KNOWN_HOSTS environment variable, else ~/.ssh/known_hosts.
func KnownHostsPath() string {
	if p := os.Getenv("DBCAT_KNOWN_HOSTS"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// hostKeyCallback verifies the server key against known_hosts. There is
// deliberately no "trust anything" mode: an unknown host is refused with its
// fingerprint and the way to register it, and a changed key is always refused.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := KnownHostsPath()
	if path == "" {
		return nil, errors.New("known_hosts 경로를 알 수 없습니다")
	}
	check, err := knownhosts.New(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func(host string, _ net.Addr, key ssh.PublicKey) error {
				return unknownHostError(host, key, path)
			}, nil
		}
		return nil, fmt.Errorf("known_hosts(%s): %w", path, err)
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := check(host, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return unknownHostError(host, key, path)
			}
			return fmt.Errorf("SSH 호스트 키가 known_hosts와 다릅니다 (%s). 중간자 공격일 수 있어 연결을 거부했습니다.\n받은 지문: %s",
				host, ssh.FingerprintSHA256(key))
		}
		return err
	}, nil
}

// UnknownHostError is returned when the SSH server's host key is not in
// known_hosts. It carries what the UI needs to let the user verify the
// fingerprint and, if they agree, register the key with TrustHostKey.
type UnknownHostError struct {
	Host        string // as known_hosts addresses it: host:port
	Key         string // the key in authorized_keys format ("ssh-ed25519 AAAA...")
	Fingerprint string // SHA256:...
	Path        string // known_hosts file that would be modified
}

func (e *UnknownHostError) Error() string {
	h, p, _ := net.SplitHostPort(e.Host)
	target := h
	if p != "" && p != "22" {
		target = "-p " + p + " " + h
	}
	return fmt.Sprintf("알 수 없는 SSH 호스트입니다: %s\n지문: %s\n지문이 맞는지 확인한 뒤 등록하세요: ssh-keyscan %s >> %s",
		e.Host, e.Fingerprint, target, e.Path)
}

func unknownHostError(host string, key ssh.PublicKey, path string) error {
	return &UnknownHostError{
		Host:        host,
		Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
		Fingerprint: ssh.FingerprintSHA256(key),
		Path:        path,
	}
}

// TrustHostKey appends host's key to known_hosts. key is the authorized_keys
// form from UnknownHostError and wantFP the fingerprint the user was shown: the
// key is only stored when it still has exactly that fingerprint, so what was
// confirmed on screen is what gets trusted.
func TrustHostKey(host, key, wantFP string) error {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		return fmt.Errorf("호스트 키를 해석할 수 없습니다: %w", err)
	}
	if got := ssh.FingerprintSHA256(pub); got != wantFP {
		return fmt.Errorf("지문이 일치하지 않습니다 (%s ≠ %s): 등록하지 않았습니다", got, wantFP)
	}
	if h, p, err := net.SplitHostPort(host); err != nil || h == "" || p == "" || strings.ContainsAny(host, " \t\r\n,|") {
		return fmt.Errorf("호스트 주소가 올바르지 않습니다: %q", host)
	}
	path := KnownHostsPath()
	if path == "" {
		return errors.New("known_hosts 경로를 알 수 없습니다")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line := knownhosts.Line([]string{host}, pub) + "\n"
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line // keep the previous last entry on its own line
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// expandHome turns a leading "~/" into the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func sshAuth(c *SSHConfig) ([]ssh.AuthMethod, error) {
	if c.KeyFile != "" {
		pem, err := os.ReadFile(expandHome(c.KeyFile))
		if err != nil {
			return nil, fmt.Errorf("SSH 개인키를 읽을 수 없습니다: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			if c.Password == "" {
				return nil, errors.New("SSH 개인키에 암호가 걸려 있습니다: 키 암호를 입력하세요")
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(c.Password))
		}
		if err != nil {
			return nil, fmt.Errorf("SSH 개인키를 해석할 수 없습니다: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if c.Password == "" {
		return nil, errors.New("SSH 비밀번호 또는 개인키 파일이 필요합니다")
	}
	return []ssh.AuthMethod{ssh.Password(c.Password)}, nil
}

// dialSSH opens the tunnel's SSH connection.
func dialSSH(ctx context.Context, c *SSHConfig) (*ssh.Client, error) {
	if c.Host == "" || c.User == "" {
		return nil, errors.New("SSH 호스트와 사용자 이름이 필요합니다")
	}
	port := c.Port
	if port == 0 {
		port = 22
	}
	auth, err := sshAuth(c)
	if err != nil {
		return nil, err
	}
	hk, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))
	conf := &ssh.ClientConfig{User: c.User, Auth: auth, HostKeyCallback: hk, Timeout: sshTimeout}

	d := net.Dialer{Timeout: sshTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("SSH 서버에 연결할 수 없습니다: %w", err)
	}
	// The handshake has no context of its own; unblock it if ctx ends first.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, conf)
	stop()
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("SSH: %w", err)
	}
	return ssh.NewClient(conn, chans, reqs), nil
}
