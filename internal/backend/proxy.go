package backend

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// expandProxyCommand substitutes %h, %p, %r and %%, as ssh_config does.
func expandProxyCommand(cmd string, s sshSettings) string {
	var b strings.Builder
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '%' || i+1 == len(cmd) {
			b.WriteByte(cmd[i])
			continue
		}
		i++
		switch cmd[i] {
		case 'h':
			b.WriteString(s.host)
		case 'p':
			b.WriteString(s.port)
		case 'r':
			b.WriteString(s.user)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(cmd[i])
		}
	}
	return b.String()
}

// dialProxyCommand runs the proxy command and exposes its stdio as a net.Conn.
func dialProxyCommand(s sshSettings) (net.Conn, error) {
	line := expandProxyCommand(s.proxyCommand, s)
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", line)
	} else {
		cmd = exec.Command("sh", "-c", line)
	}
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ProxyCommand %q: %w", line, err)
	}
	return &cmdConn{cmd: cmd, in: in, out: out, addr: stringAddr(s.hostPort())}, nil
}

// cmdConn is a net.Conn over a child process's stdin and stdout.
type cmdConn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	addr stringAddr
	once sync.Once
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.in.Write(p) }

// Close ends the proxy process and waits for it.
func (c *cmdConn) Close() error {
	c.once.Do(func() {
		c.in.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
	})
	return nil
}

func (c *cmdConn) LocalAddr() net.Addr              { return c.addr }
func (c *cmdConn) RemoteAddr() net.Addr             { return c.addr }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }

type stringAddr string

func (stringAddr) Network() string  { return "proxy" }
func (a stringAddr) String() string { return string(a) }
