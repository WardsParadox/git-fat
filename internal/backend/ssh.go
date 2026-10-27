package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type sshTarget struct {
	Host string
	User string
	Port string
}

// sftpFS adapts an sftp.Client to remoteFS.
type sftpFS struct{ c *sftp.Client }

func (s sftpFS) Stat(n string) (os.FileInfo, error) { return s.c.Stat(n) }
func (s sftpFS) MkdirAll(d string) error            { return s.c.MkdirAll(d) }
func (s sftpFS) Create(n string) (io.WriteCloser, error) {
	return s.c.OpenFile(n, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
}
func (s sftpFS) Open(n string) (io.ReadCloser, error) { return s.c.Open(n) }
func (s sftpFS) Rename(a, b string) error             { return s.c.Rename(a, b) }
func (s sftpFS) Remove(n string) error                { return s.c.Remove(n) }

// dialSFTP opens SSH connections, at most maxSessionsPerConn SFTP sessions
// each, until n sessions exist. Every connection goes through the same proxy
// chain.
func dialSFTP(ctx context.Context, t sshTarget, n int, logf func(string, ...any)) (fsPool, error) {
	cfg := newSSHConfig()
	settings := cfg.resolve(t)
	pool := &chanPool{ch: make(chan remoteFS, n)}
	for opened := 0; opened < n; {
		conn, closeConn, err := connectSSH(ctx, cfg, settings, logf)
		if err != nil {
			pool.Close()
			return nil, err
		}
		pool.closers = append(pool.closers, func() error { closeConn(); return nil })
		for i := 0; i < maxSessionsPerConn && opened < n; i++ {
			c, err := sftp.NewClient(conn,
				sftp.UseConcurrentWrites(true),
				sftp.UseConcurrentReads(true),
				sftp.MaxConcurrentRequestsPerFile(64),
			)
			if err != nil {
				pool.Close()
				return nil, fmt.Errorf("starting SFTP session on %s: %w", settings.hostPort(), err)
			}
			pool.closers = append(pool.closers, c.Close)
			pool.ch <- sftpFS{c}
			opened++
		}
	}
	return pool, nil
}

// OpenSSH defaults to MaxSessions 10 per connection; stay below it.
const maxSessionsPerConn = 8

type sshSettings struct {
	host, port, user string
	identityFiles    []string
	knownHosts       []string
	proxyCommand     string
	proxyJump        []sshTarget
}

func (s sshSettings) hostPort() string { return net.JoinHostPort(s.host, s.port) }

// sshConfig reads ~/.ssh/config. Each dial gets its own copy, so changes to the
// home directory (as in tests) are respected.
type sshConfig struct {
	us   *ssh_config.UserSettings
	home string
}

func newSSHConfig() *sshConfig {
	home, _ := os.UserHomeDir()
	us := &ssh_config.UserSettings{}
	us.ConfigFinder(func() string { return filepath.Join(home, ".ssh", "config") })
	return &sshConfig{us: us, home: home}
}

// resolve merges the .gitfat values with ~/.ssh/config. Explicit .gitfat
// values win, as the old rsync --rsh=ssh -l/-p options did.
func (c *sshConfig) resolve(t sshTarget) sshSettings {
	s := sshSettings{host: t.Host, port: t.Port, user: t.User}
	if hn := c.us.Get(t.Host, "HostName"); hn != "" {
		s.host = hn
	}
	if s.port == "" {
		s.port = c.us.Get(t.Host, "Port")
	}
	if s.port == "" {
		s.port = "22"
	}
	if s.user == "" {
		s.user = c.us.Get(t.Host, "User")
	}
	if s.user == "" {
		if u, err := user.Current(); err == nil {
			s.user = u.Username
		}
	}
	for _, f := range c.us.GetAll(t.Host, "IdentityFile") {
		s.identityFiles = append(s.identityFiles, expandHome(f))
	}

	known := strings.Fields(c.us.Get(t.Host, "UserKnownHostsFile"))
	if len(known) == 0 || strings.EqualFold(known[0], "none") {
		known = []string{"~/.ssh/known_hosts"}
	}
	for _, f := range known {
		f = expandHome(f)
		if _, err := os.Stat(f); err == nil {
			s.knownHosts = append(s.knownHosts, f)
		}
	}

	if jump := c.us.Get(t.Host, "ProxyJump"); jump != "" && !strings.EqualFold(jump, "none") {
		for _, hop := range strings.Split(jump, ",") {
			s.proxyJump = append(s.proxyJump, parseJumpHost(strings.TrimSpace(hop)))
		}
	} else if pc := c.us.Get(t.Host, "ProxyCommand"); pc != "" && !strings.EqualFold(pc, "none") {
		s.proxyCommand = pc
	}
	return s
}

// parseJumpHost parses "[user@]host[:port]".
func parseJumpHost(spec string) sshTarget {
	var t sshTarget
	if i := strings.LastIndexByte(spec, '@'); i >= 0 {
		t.User, spec = spec[:i], spec[i+1:]
	}
	if h, p, err := net.SplitHostPort(spec); err == nil {
		spec, t.Port = h, p
	} else if strings.Count(spec, ":") == 1 {
		spec, t.Port, _ = strings.Cut(spec, ":")
	}
	t.Host = strings.Trim(spec, "[]")
	return t
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// connectSSH connects to s through its ProxyJump chain or ProxyCommand. The
// returned func closes the target connection and every hop.
func connectSSH(ctx context.Context, cfg *sshConfig, s sshSettings, logf func(string, ...any)) (*ssh.Client, func(), error) {
	hops := make([]sshSettings, 0, len(s.proxyJump)+1)
	for _, j := range s.proxyJump {
		hops = append(hops, cfg.resolve(j))
	}
	hops = append(hops, s)

	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	var prev *ssh.Client
	for _, h := range hops {
		var dial func() (net.Conn, error)
		switch {
		case prev != nil:
			via, target := prev, h.hostPort()
			dial = func() (net.Conn, error) { return via.Dial("tcp", target) }
		case h.proxyCommand != "":
			dial = func() (net.Conn, error) { return dialProxyCommand(h) }
		default:
			dial = func() (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", h.hostPort())
			}
		}
		client, err := handshake(dial, h, logf)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		closers = append(closers, func() { client.Close() })
		prev = client
	}
	logf("Connected to %s as %s", s.hostPort(), s.user)
	return prev, closeAll, nil
}

// handshake dials and authenticates, checking the host key against known_hosts.
func handshake(dial func() (net.Conn, error), s sshSettings, logf func(string, ...any)) (*ssh.Client, error) {
	if len(s.knownHosts) == 0 {
		return nil, fmt.Errorf("no known_hosts file found; connect once with ssh to trust %s", s.hostPort())
	}
	hostKey, err := knownhosts.New(s.knownHosts...)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts: %w", err)
	}
	methods, closeAgent := authMethods(s, logf)
	defer closeAgent()
	cfg := &ssh.ClientConfig{User: s.user, Auth: methods, HostKeyCallback: hostKey}

	attempt := func() (*ssh.Client, error) {
		nc, err := dial()
		if err != nil {
			return nil, err
		}
		sc, chans, reqs, err := ssh.NewClientConn(nc, s.hostPort(), cfg)
		if err != nil {
			nc.Close()
			return nil, err
		}
		return ssh.NewClient(sc, chans, reqs), nil
	}
	client, err := attempt()
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) > 0 {
		// The server preferred a key type that known_hosts lacks. Retry asking
		// for the types we do know.
		cfg.HostKeyAlgorithms = algorithmsFor(ke.Want)
		client, err = attempt()
		errors.As(err, &ke)
	}
	if err != nil {
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return nil, fmt.Errorf("host %s is not in known_hosts; connect once with ssh to trust it", s.hostPort())
			}
			return nil, fmt.Errorf("host key for %s does not match known_hosts: %w", s.hostPort(), err)
		}
		return nil, fmt.Errorf("ssh %s@%s: %w", s.user, s.hostPort(), err)
	}
	return client, nil
}

func algorithmsFor(want []knownhosts.KnownKey) []string {
	var algs []string
	for _, k := range want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			algs = append(algs, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			algs = append(algs, t)
		}
	}
	return algs
}

// authMethods offers the ssh-agent first, then identity files.
func authMethods(s sshSettings, logf func(string, ...any)) ([]ssh.AuthMethod, func()) {
	var methods []ssh.AuthMethod
	closeAgent := func() {}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			closeAgent = func() { conn.Close() }
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	// ssh_config supplies a built-in IdentityFile default, so the usual key
	// names are always tried after any configured files.
	files := s.identityFiles
	home, _ := os.UserHomeDir()
	for _, n := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		files = append(files, filepath.Join(home, ".ssh", n))
	}
	seen := make(map[string]bool)
	var signers []ssh.Signer
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		pem, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			var pm *ssh.PassphraseMissingError
			if errors.As(err, &pm) {
				logf("Skipping %s: key is passphrase protected (load it into ssh-agent)", f)
			}
			continue
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	return methods, closeAgent
}
