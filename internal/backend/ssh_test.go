package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// startSFTPServer runs an in-process SSH server that accepts any public key and
// serves SFTP on the real filesystem. It returns the listening port.
func startSFTPServer(t *testing.T, hostKey ssh.Signer) string {
	port, _ := startServer(t, hostKey)
	return port
}

// startServer is startSFTPServer that also forwards direct-tcpip channels
// (for ProxyJump) and counts them.
func startServer(t *testing.T, hostKey ssh.Signer) (string, *atomic.Int32) {
	t.Helper()
	forwards := new(atomic.Int32)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(hostKey)
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
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() == "direct-tcpip" {
						forwards.Add(1)
						go forward(nch)
						continue
					}
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range creqs {
							ok := req.Type == "subsystem" && strings.HasSuffix(string(req.Payload), "sftp")
							req.Reply(ok, nil)
							if ok {
								if srv, err := sftp.NewServer(ch); err == nil {
									srv.Serve()
								}
								ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port, forwards
}

func forward(nch ssh.NewChannel) {
	var req struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &req); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	dst, err := net.Dial("tcp", net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port))))
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(dst, ch); dst.Close() }()
	io.Copy(ch, dst)
	ch.Close()
}

// sshTestHome points HOME at a fresh directory holding a client key and returns
// the .ssh directory. addKnownHost trusts a server on 127.0.0.1:port.
func sshTestHome(t *testing.T) (sshDir string, addKnownHost func(port string, key ssh.Signer)) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	sshDir = filepath.Join(home, ".ssh")
	os.MkdirAll(sshDir, 0o700)
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pem.EncodeToMemory(block), 0o600)
	return sshDir, func(port string, key ssh.Signer) {
		line := knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", port)}, key.PublicKey())
		f, _ := os.OpenFile(filepath.Join(sshDir, "known_hosts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		defer f.Close()
		fmt.Fprintln(f, line)
	}
}

func newHostSigner(t *testing.T) ssh.Signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestHelperProxy is a ProxyCommand stand-in: it is re-executed by the tests
// and pipes stdio to the host and port given after "--".
func TestHelperProxy(t *testing.T) {
	if os.Getenv("GIT_FAT_PROXY_HELPER") == "" {
		t.Skip("helper process only")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if mark := os.Getenv("GIT_FAT_PROXY_MARK"); mark != "" {
		os.WriteFile(mark, []byte("used"), 0o600)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(args[0], args[1]))
	if err != nil {
		os.Exit(1)
	}
	go io.Copy(conn, os.Stdin)
	io.Copy(os.Stdout, conn)
	os.Exit(0)
}

// useSession checks that a session from pool can reach the filesystem.
func useSession(t *testing.T, pool fsPool) {
	t.Helper()
	defer pool.Close()
	fs, _ := pool.Get()
	defer pool.Put(fs)
	if _, err := fs.Stat(t.TempDir()); err != nil {
		t.Fatalf("sftp stat: %v", err)
	}
}

func TestSFTPProxyCommand(t *testing.T) {
	sshDir, trust := sshTestHome(t)
	signer := newHostSigner(t)
	port := startSFTPServer(t, signer)
	trust(port, signer)

	mark := filepath.Join(t.TempDir(), "mark")
	exe, _ := os.Executable()
	conf := fmt.Sprintf("Host proxied\n  HostName 127.0.0.1\n  ProxyCommand GIT_FAT_PROXY_HELPER=1 GIT_FAT_PROXY_MARK=%s %s -test.run=^TestHelperProxy$ -- %%h %%p\n", mark, exe)
	os.WriteFile(filepath.Join(sshDir, "config"), []byte(conf), 0o600)

	pool, err := dialSFTP(context.Background(), sshTarget{Host: "proxied", User: "u", Port: port}, 10, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	useSession(t, pool)
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("ProxyCommand was not run")
	}
}

func TestSFTPProxyJump(t *testing.T) {
	sshDir, trust := sshTestHome(t)
	jumpKey, targetKey := newHostSigner(t), newHostSigner(t)
	jumpPort, jumped := startServer(t, jumpKey)
	targetPort, _ := startServer(t, targetKey)
	trust(jumpPort, jumpKey)
	trust(targetPort, targetKey)

	conf := fmt.Sprintf("Host jumper\n  HostName 127.0.0.1\n  Port %s\n\nHost target\n  HostName 127.0.0.1\n  ProxyJump jumper\n", jumpPort)
	os.WriteFile(filepath.Join(sshDir, "config"), []byte(conf), 0o600)

	// 10 sessions need two connections, and both must go through the jump host.
	pool, err := dialSFTP(context.Background(), sshTarget{Host: "target", User: "u", Port: targetPort}, 10, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	useSession(t, pool)
	if got := jumped.Load(); got != 2 {
		t.Fatalf("jump host forwarded %d connections, want 2", got)
	}
}

func TestParseJumpHost(t *testing.T) {
	for in, want := range map[string]sshTarget{
		"h":        {Host: "h"},
		"u@h":      {Host: "h", User: "u"},
		"u@h:2200": {Host: "h", User: "u", Port: "2200"},
		"[::1]:22": {Host: "::1", Port: "22"},
	} {
		if got := parseJumpHost(in); got != want {
			t.Errorf("parseJumpHost(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestExpandProxyCommand(t *testing.T) {
	s := sshSettings{host: "h", port: "22", user: "u"}
	if got := expandProxyCommand("nc %h %p %r 100%% %x", s); got != "nc h 22 u 100% %x" {
		t.Fatalf("got %q", got)
	}
}

func TestSFTPPushPull(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	sshDir := filepath.Join(home, ".ssh")
	os.MkdirAll(sshDir, 0o700)

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pem.EncodeToMemory(block), 0o600)

	port := startSFTPServer(t, hostSigner)
	line := knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", port)}, hostSigner.PublicKey())
	os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(line+"\n"), 0o600)

	local, remote, restore := t.TempDir(), filepath.ToSlash(filepath.Join(t.TempDir(), "store", "nested")), t.TempDir()
	var names []string
	for i := 0; i < 12; i++ {
		names = append(names, writeObject(t, local, fmt.Sprintf("sftp object %d %s", i, strings.Repeat("y", i*5000))))
	}

	ctx := context.Background()
	newBackend := func(objDir string) *copyBackend {
		pool, err := dialSFTP(ctx, sshTarget{Host: "127.0.0.1", User: "tester", Port: port}, 20, func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		return &copyBackend{pool: pool, root: remote, join: path.Join, objDir: objDir, jobs: 20, logf: func(string, ...any) {}}
	}

	if err := newBackend(local).Push(ctx, names); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(remote, n)); err != nil {
			t.Fatalf("remote missing %s: %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(remote, n+".tmp")); err == nil {
			t.Fatalf("temp file left behind for %s", n)
		}
	}

	// An existing remote object is never overwritten.
	os.WriteFile(filepath.Join(remote, names[0]), []byte("sentinel"), 0o644)
	if err := newBackend(local).Push(ctx, names); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, names[0])); string(b) != "sentinel" {
		t.Fatal("push overwrote an existing remote object")
	}

	// The sentinel fails hash verification, the rest restore cleanly.
	if err := newBackend(restore).Pull(ctx, names[1:]); err != nil {
		t.Fatal(err)
	}
	if err := newBackend(restore).Pull(ctx, names[:1]); err == nil {
		t.Fatal("expected hash mismatch for corrupted remote object")
	}
	entries, _ := os.ReadDir(restore)
	if len(entries) != len(names)-1 {
		t.Fatalf("restore has %d files, want %d", len(entries), len(names)-1)
	}
}

func TestSFTPUnknownHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), nil, 0o600)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	port := startSFTPServer(t, signer)
	_, err := dialSFTP(context.Background(), sshTarget{Host: "127.0.0.1", User: "u", Port: port}, 1, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("err = %v, want known_hosts error", err)
	}
}
