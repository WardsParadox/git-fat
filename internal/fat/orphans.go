package fat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/WardsParadox/git-fat/internal/gitutil"
)

// Orphan is a working-tree file that still holds a placeholder.
type Orphan struct {
	Digest string
	Path   string
}

// decodeFile reports the digest held by a placeholder file, or "".
func decodeFile(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if n := fi.Size(); n != int64(magicV1) && n != int64(magicV2) {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	digest, _, err := Decode(buf)
	if err != nil {
		return ""
	}
	return digest
}

// Orphans lists tracked files matching patterns that are unsmudged placeholders.
func (r *Repo) Orphans(ctx context.Context, patterns []string) ([]Orphan, error) {
	if len(patterns) == 0 || (len(patterns) == 1 && patterns[0] == "") {
		patterns = []string{"."}
	}
	out, err := gitutil.Output(ctx, append([]string{"ls-files", "-z"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	var orphans []Orphan
	for _, name := range bytes.Split(out, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		if d := decodeFile(string(name)); d != "" {
			orphans = append(orphans, Orphan{d, string(name)})
		}
	}
	return orphans, nil
}

// Checkout restores every orphan whose object is in the local store.
func (r *Repo) Checkout(ctx context.Context, showOrphans bool) error {
	if err := r.AssertInitDone(ctx); err != nil {
		return err
	}
	orphans, err := r.Orphans(ctx, nil)
	if err != nil {
		return err
	}
	var restore bytes.Buffer
	for _, o := range orphans {
		objPath := filepath.Join(r.ObjDir, o.Digest)
		f, err := os.Open(objPath)
		if err != nil {
			if showOrphans {
				fmt.Printf("Data unavailable: %s %s\n", o.Digest, o.Path)
			}
			continue
		}
		f.Close()
		fmt.Printf("Restoring %s -> %s\n", o.Digest, o.Path)
		// git caches the stat of the previously smudged file and would not
		// re-smudge it. Moving the mtime forward invalidates that cache.
		if fi, err := os.Lstat(o.Path); err == nil {
			mt := fi.ModTime().Add(time.Second)
			if err := os.Chtimes(o.Path, mt, mt); err != nil {
				return err
			}
		}
		restore.WriteString(o.Path)
		restore.WriteByte(0)
	}
	if restore.Len() == 0 {
		return nil
	}
	// One checkout-index for all paths instead of one process per file.
	cmd := gitutil.Command(ctx, "checkout-index", "--index", "--force", "-z", "--stdin")
	cmd.Stdin = &restore
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
