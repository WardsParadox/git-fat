package fat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
)

// Repo is a git repository with git-fat support.
type Repo struct {
	Root   string // work tree root
	GitDir string
	ObjDir string

	// Verbose logs to stderr when GIT_FAT_VERBOSE is set.
	Verbose func(format string, args ...any)

	encode encoder
}

// Open locates the repository containing the current directory.
func Open(ctx context.Context) (*Repo, error) {
	out, err := gitutil.Output(ctx, "rev-parse", "--show-toplevel", "--git-dir")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("unexpected rev-parse output %q", out)
	}
	gitDir, err := filepath.Abs(lines[1])
	if err != nil {
		return nil, err
	}
	r := &Repo{
		Root:    lines[0],
		GitDir:  gitDir,
		ObjDir:  filepath.Join(gitDir, "fat", "objects"),
		Verbose: func(string, ...any) {},
		encode:  EncodeV2,
	}
	if os.Getenv("GIT_FAT_VERBOSE") != "" {
		r.Verbose = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	if os.Getenv("GIT_FAT_VERSION") == "1" {
		r.encode = EncodeV1
	}
	return r, nil
}

// Setup creates the object directory.
func (r *Repo) Setup() error {
	return os.MkdirAll(r.ObjDir, 0o777)
}

// GitFatPath is the location of the shared .gitfat configuration.
func (r *Repo) GitFatPath() string {
	return filepath.Join(r.Root, ".gitfat")
}

// IsInitDone reports whether the fat filter is configured.
func (r *Repo) IsInitDone(ctx context.Context) bool {
	for _, k := range []string{"filter.fat.clean", "filter.fat.smudge"} {
		if v, ok := gitutil.ConfigGet(ctx, k, ""); ok && v != "" {
			return true
		}
	}
	return false
}

// AssertInitDone fails unless git-fat has been initialised.
func (r *Repo) AssertInitDone(ctx context.Context) error {
	if !r.IsInitDone(ctx) {
		return fmt.Errorf("git-fat is not yet configured in this repository.\nRun \"git fat init\" to configure")
	}
	return nil
}

// Init configures the fat filter in the repository.
func (r *Repo) Init(ctx context.Context) error {
	if err := r.Setup(); err != nil {
		return err
	}
	if r.IsInitDone(ctx) {
		fmt.Println("Git fat already configured, check configuration in .git/config")
		return nil
	}
	if err := gitutil.ConfigSet(ctx, "filter.fat.clean", "git-fat filter-clean", ""); err != nil {
		return err
	}
	if err := gitutil.ConfigSet(ctx, "filter.fat.smudge", "git-fat filter-smudge", ""); err != nil {
		return err
	}
	fmt.Println("Initialized git fat")
	return nil
}
