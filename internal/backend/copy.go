package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/WardsParadox/git-fat/internal/fat"
	"github.com/WardsParadox/git-fat/internal/gitutil"
	"github.com/WardsParadox/git-fat/internal/progress"
	"golang.org/x/sync/errgroup"
)

// remoteFS is the small file API shared by local directories and SFTP.
type remoteFS interface {
	Stat(name string) (os.FileInfo, error)
	MkdirAll(dir string) error
	Create(name string) (io.WriteCloser, error)
	Open(name string) (io.ReadCloser, error)
	Rename(oldName, newName string) error
	Remove(name string) error
}

// fsPool hands out remoteFS sessions so concurrent workers never share one.
type fsPool interface {
	Get() (remoteFS, error)
	Put(remoteFS)
	Close() error
}

// copyBackend implements Push and Pull for any remoteFS: it skips objects that
// already exist, uploads via a temporary name, and verifies downloads.
type copyBackend struct {
	pool   fsPool
	root   string
	join   func(elem ...string) string
	objDir string
	jobs   int
	logf   func(string, ...any)
}

const defaultCopyJobs = 8

// newCopyBackend picks a local or SSH backend from an rsync-style remote.
func newCopyBackend(ctx context.Context, o Options, remote string) (Backend, error) {
	if strings.HasPrefix(remote, "rsync://") {
		return nil, fmt.Errorf("rsync:// daemon remotes are not supported; use host:/path over SSH or a local path")
	}
	if opts, ok := gitutil.ConfigGet(ctx, "rsync.options", o.GitFatFile); ok && opts != "" {
		o.Logf("rsync.options is ignored (%q)", opts)
	}
	jobs := o.jobs(ctx, "rsync.jobs", defaultCopyJobs)

	userHost, dir, remoteHost := splitRemote(remote)
	if !remoteHost {
		o.Logf("Using local store %s", remote)
		return &copyBackend{
			pool: localPool{}, root: remote, join: filepath.Join,
			objDir: o.ObjDir, jobs: jobs, logf: o.Logf,
		}, nil
	}

	user, host := "", userHost
	if i := strings.LastIndexByte(userHost, '@'); i >= 0 {
		user, host = userHost[:i], userHost[i+1:]
	}
	if isLoopback(host) {
		o.Logf("Treating %s as a local path", host)
		return &copyBackend{
			pool: localPool{}, root: dir, join: filepath.Join,
			objDir: o.ObjDir, jobs: jobs, logf: o.Logf,
		}, nil
	}
	port, _ := gitutil.ConfigGet(ctx, "rsync.sshport", o.GitFatFile)
	cfgUser, _ := gitutil.ConfigGet(ctx, "rsync.sshuser", o.GitFatFile)
	if user == "" {
		user = cfgUser
	}
	pool, err := dialSFTP(ctx, sshTarget{Host: host, User: user, Port: port}, jobs, o.Logf)
	if err != nil {
		return nil, err
	}
	o.Logf("Using sftp://%s%s", userHost, dir)
	return &copyBackend{pool: pool, root: dir, join: path.Join, objDir: o.ObjDir, jobs: jobs, logf: o.Logf}, nil
}

// splitRemote parses "[user@]host:path". A remote without a host, or a Windows
// drive path such as C:\store, is local.
func splitRemote(remote string) (userHost, dir string, isRemote bool) {
	i := strings.IndexByte(remote, ':')
	if i <= 0 || strings.ContainsAny(remote[:i], `/\`) {
		return "", remote, false
	}
	if runtime.GOOS == "windows" && i == 1 {
		return "", remote, false
	}
	return remote[:i], remote[i+1:], true
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (b *copyBackend) Push(ctx context.Context, names []string) error {
	defer b.pool.Close()
	b.logf("Pushing to %s", b.root)

	type item struct {
		name string
		size int64
	}
	var items []item
	var total int64
	for _, name := range names {
		fi, err := os.Stat(filepath.Join(b.objDir, name))
		if err != nil {
			return err
		}
		items = append(items, item{name, fi.Size()})
		total += fi.Size()
	}
	if len(items) == 0 {
		return nil
	}
	prog := progress.New("push", len(items), total)
	defer prog.Close()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.jobs)
	for _, it := range items {
		g.Go(func() error {
			fs, err := b.pool.Get()
			if err != nil {
				return err
			}
			defer b.pool.Put(fs)
			if err := b.push(ctx, fs, it.name, prog); err != nil {
				return fmt.Errorf("uploading %s: %w", it.name, err)
			}
			prog.FileDone()
			return nil
		})
	}
	return g.Wait()
}

func (b *copyBackend) push(ctx context.Context, fs remoteFS, name string, prog *progress.Tracker) error {
	dst := b.join(b.root, name)
	if _, err := fs.Stat(dst); err == nil {
		// Like rsync --ignore-existing: never overwrite.
		fi, _ := os.Stat(filepath.Join(b.objDir, name))
		if fi != nil {
			prog.Add(fi.Size())
		}
		return nil
	}
	if err := fs.MkdirAll(b.root); err != nil {
		return err
	}
	src, err := os.Open(filepath.Join(b.objDir, name))
	if err != nil {
		return err
	}
	defer src.Close()

	var rnd [6]byte
	rand.Read(rnd[:])
	tmpName := fmt.Sprintf("%s.tmp.%d.%x", dst, os.Getpid(), rnd)
	out, err := fs.Create(tmpName)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { out.Close() })
	_, err = io.Copy(out, countingReader{src, prog.Add})
	stop()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fs.Remove(tmpName)
		return err
	}
	local, _ := src.Stat()
	if fi, err := fs.Stat(tmpName); err != nil || local == nil || fi.Size() != local.Size() {
		fs.Remove(tmpName)
		return fmt.Errorf("uploaded size of %s does not match local file", name)
	}
	if err := fs.Rename(tmpName, dst); err != nil {
		fs.Remove(tmpName)
		return err
	}
	return nil
}

func (b *copyBackend) Pull(ctx context.Context, names []string) error {
	defer b.pool.Close()
	b.logf("Pulling from %s", b.root)
	if len(names) == 0 {
		return nil
	}
	prog := progress.New("pull", len(names), 0)
	defer prog.Close()
	var mu sync.Mutex
	var missing []string

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.jobs)
	for _, name := range names {
		g.Go(func() error {
			fs, err := b.pool.Get()
			if err != nil {
				return err
			}
			defer b.pool.Put(fs)
			err = b.pull(ctx, fs, name, prog)
			if errors.Is(err, errMissing) {
				mu.Lock()
				missing = append(missing, name)
				mu.Unlock()
				return nil
			}
			if err != nil {
				return fmt.Errorf("downloading %s: %w", name, err)
			}
			prog.FileDone()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return missingError(missing)
}

func (b *copyBackend) pull(ctx context.Context, fs remoteFS, name string, prog *progress.Tracker) error {
	src := b.join(b.root, name)
	fi, err := fs.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return errMissing
		}
		return err
	}
	prog.AddTotal(fi.Size())
	in, err := fs.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := newTempObject(b.objDir, name)
	if err != nil {
		return err
	}
	defer tmp.Discard()
	// in is closed on cancellation, which aborts the copy. Copying straight
	// from in lets SFTP use its concurrent WriteTo.
	stop := context.AfterFunc(ctx, func() { in.Close() })
	_, err = io.Copy(countingWriter{tmp, prog.Add}, in)
	stop()
	if err != nil {
		return err
	}
	return tmp.Commit(false, fat.ObjectMode())
}

type localFS struct{}

func (localFS) Stat(n string) (os.FileInfo, error) { return os.Stat(n) }
func (localFS) MkdirAll(d string) error            { return os.MkdirAll(d, 0o777) }
func (localFS) Create(n string) (io.WriteCloser, error) {
	return os.OpenFile(n, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
}
func (localFS) Open(n string) (io.ReadCloser, error) { return os.Open(n) }
func (localFS) Rename(a, b string) error             { return os.Rename(a, b) }
func (localFS) Remove(n string) error                { return os.Remove(n) }

type localPool struct{}

func (localPool) Get() (remoteFS, error) { return localFS{}, nil }
func (localPool) Put(remoteFS)           {}
func (localPool) Close() error           { return nil }

// chanPool is a fixed set of sessions shared through a channel.
type chanPool struct {
	ch      chan remoteFS
	closers []func() error
	once    sync.Once
}

func (p *chanPool) Get() (remoteFS, error) { return <-p.ch, nil }
func (p *chanPool) Put(fs remoteFS)        { p.ch <- fs }
func (p *chanPool) Close() error {
	var err error
	p.once.Do(func() {
		for _, c := range p.closers {
			if cerr := c(); cerr != nil && err == nil {
				err = cerr
			}
		}
	})
	return err
}
