package backend

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// tempObject is a download in progress inside the object directory. Commit
// verifies the SHA-1 against the object name and renames it into place.
type tempObject struct {
	*os.File
	objDir string
	name   string
	hash   hash.Hash
	done   bool
}

func newTempObject(objDir, name string) (*tempObject, error) {
	f, err := os.CreateTemp(objDir, ".tmp-"+name+"-")
	if err != nil {
		return nil, err
	}
	return &tempObject{File: f, objDir: objDir, name: name, hash: sha1.New()}, nil
}

// Write hashes as it writes; use it for sequential streaming downloads.
func (t *tempObject) Write(p []byte) (int, error) {
	t.hash.Write(p)
	return t.File.Write(p)
}

// verifyByReading hashes the file from disk, for writers that use WriteAt.
func (t *tempObject) verifyByReading() error {
	if _, err := t.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	t.hash.Reset()
	_, err := io.Copy(t.hash, t.File)
	return err
}

// Commit finalises the object. Pass reread when data was written with WriteAt.
func (t *tempObject) Commit(reread bool, mode os.FileMode) error {
	if reread {
		if err := t.verifyByReading(); err != nil {
			return err
		}
	}
	if err := t.File.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(t.hash.Sum(nil)); got != t.name {
		return fmt.Errorf("object %s: downloaded data has hash %s", t.name, got)
	}
	if err := os.Chmod(t.File.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(t.File.Name(), filepath.Join(t.objDir, t.name)); err != nil {
		return err
	}
	t.done = true
	return nil
}

// Discard removes the temporary file unless it was committed.
func (t *tempObject) Discard() {
	if !t.done {
		t.File.Close()
		os.Remove(t.File.Name())
	}
}

// countingReader reports bytes read to add.
type countingReader struct {
	io.Reader
	add func(int64)
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.add(int64(n))
	return n, err
}

// countingWriter reports bytes written to add.
type countingWriter struct {
	io.Writer
	add func(int64)
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.Writer.Write(p)
	c.add(int64(n))
	return n, err
}
