// Package backend moves fat objects between the local store and a remote.
package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
)

// Backend transfers named objects between the local object directory and a remote.
type Backend interface {
	Push(ctx context.Context, names []string) error
	Pull(ctx context.Context, names []string) error
}

// Options carries everything needed to pick and configure a backend.
type Options struct {
	GitFatFile string // path of the shared .gitfat file
	ObjDir     string
	Profile    string // --profile
	Region     string // --region
	Accelerate string // --accelerate / --no-accelerate: "true", "false" or ""
	Progress   string // --progress / --no-progress: "true", "false" or ""
	Jobs       int    // -j/--jobs; 0 means unset
	PartSizeMB int    // --part-size; 0 means unset
	Logf       func(format string, args ...any)
}

// New builds the backend described by .gitfat.
func New(ctx context.Context, o Options) (Backend, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if bucket, ok := gitutil.ConfigGet(ctx, "s3.bucket", o.GitFatFile); ok && bucket != "" {
		return newS3(ctx, o, bucket)
	}
	remote, ok := gitutil.ConfigGet(ctx, "rsync.remote", o.GitFatFile)
	if !ok || remote == "" {
		return nil, fmt.Errorf("no s3.bucket or rsync.remote in %s", o.GitFatFile)
	}
	return newCopyBackend(ctx, o, remote)
}

func configBool(ctx context.Context, key, file string) bool {
	v, _ := gitutil.ConfigGet(ctx, key, file)
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// errMissing marks an object that is absent on the remote.
var errMissing = errors.New("object not found on remote")

// MissingError lists objects a pull could not find on the remote. Everything
// else was transferred.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return fmt.Sprintf("%d object(s) not found on remote: %s", len(e.Names), strings.Join(e.Names, " "))
}

func missingError(names []string) error {
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return &MissingError{names}
}
