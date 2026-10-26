package fat

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const bufSize = 64 * 1024

// Clean reads a working-tree file from in and writes its repository form to out.
// The content is streamed into the object store; out receives only the placeholder.
// Input that is already a placeholder is passed through unchanged.
func (r *Repo) Clean(in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, bufSize)
	head, err := br.Peek(magicV2 + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(head) <= magicV2 {
		if _, _, derr := Decode(head); derr == nil {
			_, err := out.Write(head)
			return err
		}
	}

	tmp, err := os.CreateTemp(r.ObjDir, "tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cached := false
	defer func() {
		if !cached {
			os.Remove(tmpName)
		}
	}()

	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), br)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	objFile := filepath.Join(r.ObjDir, digest)
	if _, err := os.Stat(objFile); err == nil {
		r.Verbose("git-fat filter-clean: cache already exists %s", objFile)
	} else {
		if err := os.Chmod(tmpName, ObjectMode()); err != nil {
			return err
		}
		if err := os.Rename(tmpName, objFile); err != nil {
			return err
		}
		cached = true
		r.Verbose("git-fat filter-clean: caching to %s", objFile)
	}
	_, err = io.WriteString(out, r.encode(digest, n))
	return err
}

// Smudge reads a repository blob from in and writes the working-tree form to out.
func (r *Repo) Smudge(in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, bufSize)
	head, err := br.Peek(magicV2 + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(head) <= magicV2 {
		if digest, size, derr := Decode(head); derr == nil {
			objFile := filepath.Join(r.ObjDir, digest)
			f, oerr := os.Open(objFile)
			if oerr != nil {
				r.Verbose("git-fat filter-smudge: fat object missing %s", objFile)
				enc := r.encode
				if size < 0 {
					enc = EncodeV1
				}
				_, err := io.WriteString(out, enc(digest, size))
				return err
			}
			defer f.Close()
			r.Verbose("git-fat filter-smudge: restoring from %s", objFile)
			_, err := io.Copy(out, f)
			return err
		}
	}
	r.Verbose("git-fat filter-smudge: not a managed file")
	_, err = io.Copy(out, br)
	return err
}
