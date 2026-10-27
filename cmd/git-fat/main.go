// Command git-fat stores large files outside git and syncs them with S3 or SSH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/WardsParadox/git-fat/internal/backend"
	"github.com/WardsParadox/git-fat/internal/fat"
	"github.com/WardsParadox/git-fat/internal/gitutil"
)

var version = "dev"

const usage = "Usage: git fat [init|status|push|pull|gc|verify|checkout|find|index-filter|profile|s3-info|version]"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(int(ee))
		}
		fmt.Fprintf(os.Stderr, "git-fat: %v\n", err)
		os.Exit(1)
	}
}

type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return exitError(1)
	}
	cmd, args := args[0], args[1:]
	if cmd == "version" || cmd == "--version" {
		fmt.Println("git-fat", version)
		return nil
	}

	switch cmd {
	case "init", "status", "push", "pull", "gc", "verify", "checkout", "find",
		"index-filter", "filter-clean", "filter-smudge", "profile", "s3-info":
	default:
		fmt.Fprintln(os.Stderr, usage)
		return exitError(1)
	}

	repo, err := fat.Open(ctx)
	if err != nil {
		return err
	}

	switch cmd {
	case "filter-clean":
		if err := repo.Setup(); err != nil {
			return err
		}
		return repo.Clean(os.Stdin, os.Stdout)
	case "filter-smudge":
		if err := repo.Setup(); err != nil {
			return err
		}
		return repo.Smudge(os.Stdin, os.Stdout)
	case "init":
		return repo.Init(ctx)
	case "status":
		fs, _ := newFlags("status")
		all := fs.Bool("all", false, "include every referenced object")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return repo.Status(ctx, *all)
	case "push":
		return cmdPush(ctx, repo, args)
	case "pull":
		return cmdPull(ctx, repo, args)
	case "gc":
		if err := repo.Setup(); err != nil {
			return err
		}
		return repo.GC(ctx)
	case "verify":
		if err := repo.Setup(); err != nil {
			return err
		}
		if err := repo.Verify(); err != nil {
			return exitError(1)
		}
		return nil
	case "checkout":
		return repo.Checkout(ctx, true)
	case "find":
		if len(args) < 1 {
			return errors.New("usage: git fat find <size-in-bytes>")
		}
		n, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid size %q", args[0])
		}
		return repo.Find(ctx, n)
	case "index-filter":
		if len(args) < 1 {
			return errors.New("usage: git fat index-filter <file-list> [--manage-gitattributes]")
		}
		manage := false
		for _, a := range args[1:] {
			manage = manage || a == "--manage-gitattributes"
		}
		return repo.IndexFilter(ctx, args[0], manage)
	case "profile":
		return cmdProfile(ctx, repo, args)
	case "s3-info":
		fs, tf := newFlags("s3-info")
		if err := fs.Parse(args); err != nil {
			return err
		}
		lines, err := backend.S3Info(ctx, tf.backendOptions(repo))
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	}
	return nil
}

// transferFlags are shared by push, pull and status-like commands that talk to a remote.
type transferFlags struct {
	all     *bool
	profile *string
	region  *string
	jobs    *int
	partMB  *int
	accel   *bool
	noAccel *bool
}

func newFlags(name string) (*flag.FlagSet, *transferFlags) {
	fs := flag.NewFlagSet("git fat "+name, flag.ContinueOnError)
	tf := &transferFlags{
		all:     fs.Bool("all", false, "consider every ref, not just HEAD"),
		profile: fs.String("profile", "", "AWS profile"),
		region:  fs.String("region", "", "AWS region"),
		jobs:    new(int),
		accel:   fs.Bool("accelerate", false, "use S3 transfer acceleration if the bucket has it enabled"),
		noAccel: fs.Bool("no-accelerate", false, "do not use S3 transfer acceleration"),
	}
	tf.partMB = fs.Int("part-size", 0, "S3 multipart part size in MB")
	fs.IntVar(tf.jobs, "jobs", 0, "parallel transfers")
	fs.IntVar(tf.jobs, "j", 0, "parallel transfers (shorthand)")
	return fs, tf
}

func (tf *transferFlags) accelerate() string {
	switch {
	case *tf.noAccel:
		return "false"
	case *tf.accel:
		return "true"
	}
	return ""
}

func (tf *transferFlags) backend(ctx context.Context, repo *fat.Repo) (backend.Backend, error) {
	return backend.New(ctx, tf.backendOptions(repo))
}

func (tf *transferFlags) backendOptions(repo *fat.Repo) backend.Options {
	return backend.Options{
		GitFatFile: repo.GitFatPath(),
		ObjDir:     repo.ObjDir,
		Profile:    *tf.profile,
		Region:     *tf.region,
		Jobs:       *tf.jobs,
		PartSizeMB: *tf.partMB,
		Accelerate: tf.accelerate(),
		Logf:       repo.Verbose,
	}
}

func cmdPush(ctx context.Context, repo *fat.Repo, args []string) error {
	fs, tf := newFlags("push")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := repo.Setup(); err != nil {
		return err
	}
	refs, err := repo.Referenced(ctx, "", *tf.all)
	if err != nil {
		return err
	}
	cat, err := repo.Catalog()
	if err != nil {
		return err
	}
	var names []string
	for name := range refs {
		if _, ok := cat[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	b, err := tf.backend(ctx, repo)
	if err != nil {
		return err
	}
	return b.Push(ctx, names)
}

func cmdPull(ctx context.Context, repo *fat.Repo, args []string) error {
	var patterns []string
	hasPatterns := false
	for i, a := range args {
		if a == "--" {
			patterns, args, hasPatterns = args[i+1:], args[:i], true
			break
		}
	}
	if !hasPatterns {
		patterns = []string{""}
	}
	fs, tf := newFlags("pull")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := repo.AssertInitDone(ctx); err != nil {
		return err
	}
	if err := repo.Setup(); err != nil {
		return err
	}

	rev := ""
	if fs.NArg() > 0 {
		var err error
		if rev, err = gitutil.RevParse(ctx, fs.Arg(0)); err != nil {
			return err
		}
	}
	refs, err := repo.Referenced(ctx, rev, *tf.all)
	if err != nil {
		return err
	}
	cat, err := repo.Catalog()
	if err != nil {
		return err
	}
	var want map[string]struct{}
	if !*tf.all { // patterns are ignored with --all
		orphans, err := repo.Orphans(ctx, patterns)
		if err != nil {
			return err
		}
		want = make(map[string]struct{}, len(orphans))
		for _, o := range orphans {
			want[o.Digest] = struct{}{}
		}
	}
	var names []string
	for name := range refs {
		if _, have := cat[name]; have {
			continue
		}
		if _, ok := want[name]; want != nil && !ok {
			continue
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		b, err := tf.backend(ctx, repo)
		if err != nil {
			return err
		}
		if err := b.Pull(ctx, names); err != nil {
			var missing *backend.MissingError
			if !errors.As(err, &missing) {
				return err
			}
			// Restore what did arrive, then report the rest.
			if cerr := repo.Checkout(ctx, false); cerr != nil {
				return cerr
			}
			return err
		}
	}
	return repo.Checkout(ctx, false)
}

func cmdProfile(ctx context.Context, repo *fat.Repo, args []string) error {
	if len(args) >= 1 && args[0] == "set" {
		if len(args) != 2 {
			return errors.New("usage: git fat profile set <name>")
		}
		return gitutil.ConfigSet(ctx, "fat.profile", args[1], "")
	}
	fs := flag.NewFlagSet("git fat profile", flag.ContinueOnError)
	flagProfile := fs.String("profile", "", "AWS profile")
	flagRegion := fs.String("region", "", "AWS region")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lk := backend.SystemLookup(ctx, repo.GitFatPath())
	p := backend.ResolveProfile(*flagProfile, lk)
	r := backend.ResolveRegion(*flagRegion, lk)
	fmt.Printf("profile: %s (%s)\n", orDefault(p.Value), p.Source)
	fmt.Printf("region:  %s (%s)\n", orDefault(r.Value), r.Source)
	a := backend.ResolveAccelerate("", lk)
	fmt.Printf("accelerate: %s (%s)\n", a.Value, a.Source)
	jobs, part := backend.TransferSettings(ctx, backend.Options{GitFatFile: repo.GitFatPath()})
	fmt.Printf("jobs:       %s\n", jobs)
	if backend.Kind(ctx, repo.GitFatPath()) == "s3" {
		fmt.Printf("partsize:   %s MB\n", part)
	}
	return nil
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
