package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalPullReportsMissingButKeepsGoing(t *testing.T) {
	remote, dst := t.TempDir(), t.TempDir()
	name := writeObject(t, remote, "local object")
	b := &copyBackend{pool: localPool{}, root: remote, join: filepath.Join, objDir: dst, jobs: 2, logf: func(string, ...any) {}}
	gone := "0123456789012345678901234567890123456789"

	err := b.Pull(context.Background(), []string{gone, name})
	var me *MissingError
	if !errors.As(err, &me) || me.Names[0] != gone {
		t.Fatalf("err = %v, want MissingError", err)
	}
	if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPushLeavesNoTempFiles(t *testing.T) {
	src, remote := t.TempDir(), filepath.Join(t.TempDir(), "store")
	name := writeObject(t, src, "pushed")
	b := &copyBackend{pool: localPool{}, root: remote, join: filepath.Join, objDir: src, jobs: 2, logf: func(string, ...any) {}}
	if err := b.Push(context.Background(), []string{name}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(remote)
	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("remote contents: %v", entries)
	}
}
