package fat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/WardsParadox/git-fat/internal/gitutil"
)

// largeBlobs returns the size of every blob larger than threshold.
func (r *Repo) largeBlobs(ctx context.Context, threshold int64) (map[string]int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := newGroup(cancel)
	revList := gitutil.Command(ctx, "rev-list", "--all", "--objects")
	revOut, _ := revList.StdoutPipe()
	check := gitutil.Command(ctx, "cat-file", "--batch-check")
	checkIn, _ := check.StdinPipe()
	checkOut, _ := check.StdoutPipe()
	if err := startAll(revList, check); err != nil {
		return nil, err
	}

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

	blobs := make(map[string]int64)
	numBlobs := 0
	g.Go(func() error {
		sc := bufio.NewScanner(checkOut)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 3 || f[1] != "blob" {
				continue
			}
			size, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil {
				return err
			}
			numBlobs++
			if size > threshold {
				blobs[f[0]] = size
			}
		}
		return sc.Err()
	})
	werr := g.Wait()
	for _, c := range []interface{ Wait() error }{revList, check} {
		if err := c.Wait(); err != nil && werr == nil {
			werr = err
		}
	}
	r.Verbose("%d of %d blobs are > %d bytes", len(blobs), numBlobs, threshold)
	return blobs, werr
}

// Find prints .gitattributes lines for every path that ever held a blob larger
// than maxSize, biggest first.
func (r *Repo) Find(ctx context.Context, maxSize int64) error {
	blobs, err := r.largeBlobs(ctx, maxSize)
	if err != nil {
		return err
	}

	revList := gitutil.Command(ctx, "rev-list", "--all")
	diffTree := gitutil.Command(ctx, "diff-tree", "--root", "--no-renames", "--no-commit-id",
		"--diff-filter=AMCR", "-r", "--stdin", "-z")
	pipe, _ := revList.StdoutPipe()
	diffTree.Stdin = pipe
	out, _ := diffTree.StdoutPipe()
	if err := startAll(revList, diffTree); err != nil {
		return err
	}

	var order []string
	sizes := make(map[string]map[int64]struct{})
	br := bufio.NewReaderSize(out, bufSize)
	readField := func() (string, error) {
		s, err := br.ReadString(0)
		return strings.TrimSuffix(s, "\x00"), err
	}
	for {
		meta, err := readField()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		path, err := readField()
		if err != nil {
			return err
		}
		fields := strings.Fields(meta)
		if len(fields) < 5 {
			continue
		}
		size, ok := blobs[fields[3]]
		if !ok {
			continue
		}
		if sizes[path] == nil {
			sizes[path] = make(map[int64]struct{})
			order = append(order, path)
		}
		sizes[path][size] = struct{}{}
	}
	if err := diffTree.Wait(); err != nil {
		return err
	}
	if err := revList.Wait(); err != nil {
		return err
	}

	maxLen := 0
	maxSizes := make(map[string]int64, len(order))
	for _, p := range order {
		if n := utf8.RuneCountInString(p); n > maxLen {
			maxLen = n
		}
		for s := range sizes[p] {
			if s > maxSizes[p] {
				maxSizes[p] = s
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return maxSizes[order[i]] > maxSizes[order[j]] })
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, p := range order {
		fmt.Fprintf(w, "%-*s filter=fat -text # %10d %d\n", maxLen, p, maxSizes[p], len(sizes[p]))
	}
	return nil
}

// IndexFilter rewrites the index so that the listed files are stored as
// placeholders. It is meant for git filter-branch --index-filter.
func (r *Repo) IndexFilter(ctx context.Context, listFile string, manageGitattributes bool) error {
	data, err := os.ReadFile(listFile)
	if err != nil {
		return err
	}
	var fileList []string
	fileSet := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, dup := fileSet[line]; !dup {
			fileSet[line] = struct{}{}
			fileList = append(fileList, line)
		}
	}
	if err := r.Setup(); err != nil {
		return err
	}

	lsFiles := gitutil.Command(ctx, "ls-files", "-s", "-z")
	lsOut, _ := lsFiles.StdoutPipe()
	update := gitutil.Command(ctx, "update-index", "-z", "--index-info")
	update.Stderr = os.Stderr
	updIn, _ := update.StdinPipe()
	if err := startAll(lsFiles, update); err != nil {
		return err
	}
	w := bufio.NewWriter(updIn)

	br := bufio.NewReaderSize(lsOut, bufSize)
	for {
		entry, err := br.ReadString(0)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entry = strings.TrimSuffix(entry, "\x00")
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if _, want := fileSet[name]; !want {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		mode, blob, stage := f[0], f[1], f[2]
		if mode == "120000" { // symlink
			continue
		}
		cleaned, err := r.cleanBlob(ctx, blob)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s %s %s\t%s\x00", mode, cleaned, stage, name)
	}
	if err := lsFiles.Wait(); err != nil {
		return err
	}

	if manageGitattributes {
		mode, stage := "100644", "0"
		var lines []string
		out, err := gitutil.Output(ctx, "ls-files", "-s", ".gitattributes")
		if f := strings.Fields(string(out)); err == nil && len(f) >= 3 {
			mode, stage = f[0], f[2]
			body, err := gitutil.Output(ctx, "cat-file", "blob", f[1])
			if err != nil {
				return err
			}
			lines = strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		}
		for _, name := range fileList {
			lines = append(lines, strings.Fields(name)[0]+" filter=fat -text")
		}
		hash := gitutil.Command(ctx, "hash-object", "-w", "--stdin")
		hash.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
		sha, err := hash.Output()
		if err != nil {
			return fmt.Errorf("git hash-object: %w", err)
		}
		fmt.Fprintf(w, "%s %s %s\t.gitattributes\x00", mode, strings.TrimSpace(string(sha)), stage)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	updIn.Close()
	return update.Wait()
}

// cleanBlob returns the placeholder blob for a content blob, memoised on disk
// so repeated rewrites of the same blob are cheap.
func (r *Repo) cleanBlob(ctx context.Context, blob string) (string, error) {
	dir := filepath.Join(r.GitDir, "fat", "index-filter")
	hashFile := filepath.Join(dir, blob)
	if b, err := os.ReadFile(hashFile); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	cat := gitutil.Command(ctx, "cat-file", "blob", blob)
	catOut, _ := cat.StdoutPipe()
	hash := gitutil.Command(ctx, "hash-object", "-w", "--stdin")
	hashIn, _ := hash.StdinPipe()
	hashOut, _ := hash.StdoutPipe()
	if err := startAll(cat, hash); err != nil {
		return "", err
	}
	cleanErr := make(chan error, 1)
	go func() {
		err := r.Clean(catOut, hashIn)
		if err != nil {
			io.Copy(io.Discard, catOut)
		}
		hashIn.Close()
		cleanErr <- err
	}()
	sha, _ := io.ReadAll(hashOut)
	err := <-cleanErr
	if werr := cat.Wait(); err == nil {
		err = werr
	}
	if werr := hash.Wait(); err == nil {
		err = werr
	}
	if err != nil {
		return "", err
	}
	cleaned := strings.TrimSpace(string(sha))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return cleaned, os.WriteFile(hashFile, []byte(cleaned+"\n"), 0o666)
}
