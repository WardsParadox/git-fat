package fat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
	"golang.org/x/sync/errgroup"
)

// Referenced returns the digests of all fat objects referenced by rev (default
// HEAD) or, when all is set, by every ref. It runs
// rev-list | cat-file --batch-check | cat-file --batch with each stage streaming.
func (r *Repo) Referenced(ctx context.Context, rev string, all bool) (map[string]struct{}, error) {
	switch {
	case all:
		rev = "--all"
	case rev == "":
		var err error
		if rev, err = gitutil.RevParse(ctx, "HEAD"); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := newGroup(cancel)

	revList := gitutil.Command(ctx, "rev-list", "--objects", rev)
	revOut, err := revList.StdoutPipe()
	if err != nil {
		return nil, err
	}
	check := gitutil.Command(ctx, "cat-file", "--batch-check")
	checkIn, _ := check.StdinPipe()
	checkOut, _ := check.StdoutPipe()
	batch := gitutil.Command(ctx, "cat-file", "--batch")
	batchIn, _ := batch.StdinPipe()
	batchOut, _ := batch.StdoutPipe()

	if err := startAll(revList, check, batch); err != nil {
		return nil, err
	}

	// rev-list output -> bare object names
	g.Go(func() error {
		defer checkIn.Close()
		w := bufio.NewWriter(checkIn)
		br := bufio.NewReaderSize(revOut, bufSize)
		for {
			line, err := br.ReadString('\n')
			if f := strings.Fields(line); len(f) > 0 {
				w.WriteString(f[0] + "\n")
			}
			if err == io.EOF {
				return w.Flush()
			}
			if err != nil {
				return err
			}
		}
	})

	// batch-check output -> blobs whose size matches a placeholder
	g.Go(func() error {
		defer batchIn.Close()
		w := bufio.NewWriter(batchIn)
		sc := bufio.NewScanner(checkOut)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 3 || f[1] != "blob" {
				continue
			}
			if size, _ := strconv.Atoi(f[2]); size == magicV1 || size == magicV2 {
				w.WriteString(f[0] + "\n")
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		return w.Flush()
	})

	refs := make(map[string]struct{})
	g.Go(func() error {
		br := bufio.NewReader(batchOut)
		for {
			header, err := br.ReadString('\n')
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			f := strings.Fields(header)
			if len(f) != 3 {
				continue // "<sha> missing"
			}
			size, err := strconv.Atoi(f[2])
			if err != nil {
				return fmt.Errorf("unexpected cat-file output %q", header)
			}
			content := make([]byte, size+1) // trailing LF delimiter
			if _, err := io.ReadFull(br, content); err != nil {
				return err
			}
			if digest, _, err := Decode(content[:size]); err == nil {
				refs[digest] = struct{}{}
			}
		}
	})

	werr := g.Wait()
	for _, c := range []interface{ Wait() error }{revList, check, batch} {
		if err := c.Wait(); err != nil && werr == nil {
			werr = err
		}
	}
	if werr != nil {
		return nil, werr
	}
	return refs, nil
}

// group runs pipeline stages and cancels the rest when one fails.
type group struct {
	errgroup.Group
	cancel context.CancelFunc
}

func newGroup(cancel context.CancelFunc) *group { return &group{cancel: cancel} }

func (g *group) Go(f func() error) {
	g.Group.Go(func() error {
		err := f()
		if err != nil {
			g.cancel()
		}
		return err
	})
}

// startAll starts the commands in order. If one fails to start, the ones
// already running are killed and reaped.
func startAll(cmds ...*exec.Cmd) error {
	for i, c := range cmds {
		if err := c.Start(); err != nil {
			for _, started := range cmds[:i] {
				started.Process.Kill()
				started.Wait()
			}
			return err
		}
	}
	return nil
}
