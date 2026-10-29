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
	"strings"
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
		switch {
		case errors.Is(err, errHelp):
			return
		case errors.As(err, &ee):
			os.Exit(int(ee))
		}
		fmt.Fprintf(os.Stderr, "git-fat: %v\n", err)
		os.Exit(1)
	}
}

type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// errHelp is returned after --help has printed usage.
var errHelp = flag.ErrHelp

// usageError reports a command-line mistake and exits with status 2.
func usageError(format string, args ...any) error {
	fmt.Fprintf(os.Stderr, "git-fat: "+format+"\n", args...)
	fmt.Fprintln(os.Stderr, usage)
	return exitError(2)
}

// parseArgs parses flags that may be mixed with positional arguments. Anything
// after a bare "--" is returned separately, untouched.
func parseArgs(fs *flag.FlagSet, args []string) (pos, after []string, hasAfter bool, err error) {
	for i, a := range args {
		if a == "--" {
			args, after, hasAfter = args[:i], args[i+1:], true
			break
		}
	}
	for {
		if perr := fs.Parse(args); perr != nil {
			if errors.Is(perr, flag.ErrHelp) {
				return nil, nil, false, errHelp
			}
			return nil, nil, false, exitError(2) // flag already printed the problem
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, after, hasAfter, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet("git fat "+name, flag.ContinueOnError)
}

// noArgs parses a command that takes no flags or arguments.
func noArgs(name string, args []string) error {
	pos, _, _, err := parseArgs(newFlagSet(name), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("git fat %s takes no arguments", name)
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return exitError(2)
	}
	cmd, args := args[0], args[1:]
	if cmd == "version" || cmd == "--version" {
		fmt.Println("git-fat", version)
		return nil
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Println(usage)
		return nil
	}

	// Parse flags before touching the repository, so --help and mistakes work anywhere.
	var run func(repo *fat.Repo) error
	switch cmd {
	case "filter-clean", "filter-smudge":
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Println("git fat " + cmd + ": git filter driver; reads stdin, writes stdout")
			return nil
		}
		run = func(repo *fat.Repo) error {
			if err := repo.Setup(); err != nil {
				return err
			}
			if cmd == "filter-clean" {
				return repo.Clean(os.Stdin, os.Stdout)
			}
			return repo.Smudge(os.Stdin, os.Stdout)
		}
	case "init", "checkout", "verify":
		if err := noArgs(cmd, args); err != nil {
			return err
		}
		run = func(repo *fat.Repo) error {
			switch cmd {
			case "init":
				return repo.Init(ctx)
			case "checkout":
				return repo.Checkout(ctx, true)
			}
			if err := repo.Setup(); err != nil {
				return err
			}
			if repo.Verify() != nil {
				return exitError(1)
			}
			return nil
		}
	case "status":
		fs, tf := newFlags("status")
		pos, _, _, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usageError("git fat status takes no arguments")
		}
		run = func(repo *fat.Repo) error {
			return noCommits(repo.Status(ctx, *tf.all), "report")
		}
	case "push":
		fs, tf := newFlags("push")
		pos, _, _, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usageError("git fat push takes no arguments")
		}
		run = func(repo *fat.Repo) error { return noCommits(cmdPush(ctx, repo, tf), "push") }
	case "pull":
		fs, tf := newFlags("pull")
		pos, patterns, hasPatterns, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return usageError("git fat pull takes at most one revision")
		}
		if !hasPatterns {
			patterns = []string{""}
		}
		run = func(repo *fat.Repo) error { return noCommits(cmdPull(ctx, repo, tf, pos, patterns), "pull") }
	case "gc":
		fs := newFlagSet("gc")
		dry := fs.Bool("dry-run", false, "list what would be removed without removing it")
		fs.BoolVar(dry, "n", false, "shorthand for --dry-run")
		headOnly := fs.Bool("head-only", false, "keep only objects referenced from HEAD")
		pos, _, _, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usageError("git fat gc takes no arguments")
		}
		run = func(repo *fat.Repo) error {
			if err := repo.Setup(); err != nil {
				return err
			}
			return noCommits(repo.GC(ctx, fat.GCOptions{DryRun: *dry, HeadOnly: *headOnly}), "collect")
		}
	case "find":
		pos, _, _, err := parseArgs(newFlagSet("find"), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError("usage: git fat find <size-in-bytes>")
		}
		n, perr := strconv.ParseInt(pos[0], 10, 64)
		if perr != nil {
			return usageError("invalid size %q", pos[0])
		}
		run = func(repo *fat.Repo) error { return repo.Find(ctx, n) }
	case "index-filter":
		fs := newFlagSet("index-filter")
		manage := fs.Bool("manage-gitattributes", false, "add enrolled files to .gitattributes")
		pos, _, _, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageError("usage: git fat index-filter <file-list> [--manage-gitattributes]")
		}
		run = func(repo *fat.Repo) error { return repo.IndexFilter(ctx, pos[0], *manage) }
	case "profile":
		fs := newProfileFlags()
		pos, _, _, err := parseArgs(fs.fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 && (pos[0] != "set" || len(pos) != 2) {
			return usageError("usage: git fat profile [set <name>]")
		}
		run = func(repo *fat.Repo) error { return cmdProfile(ctx, repo, fs, pos) }
	case "s3-info":
		fs, tf := newFlags("s3-info")
		pos, _, _, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usageError("git fat s3-info takes no arguments")
		}
		run = func(repo *fat.Repo) error {
			lines, err := backend.S3Info(ctx, tf.backendOptions(repo))
			if err != nil {
				return err
			}
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		}
	default:
		return usageError("unknown command %q", cmd)
	}

	repo, err := fat.Open(ctx)
	if err != nil {
		return err
	}
	return run(repo)
}

// noCommits turns the "no HEAD yet" error into a friendly note.
func noCommits(err error, verb string) error {
	if errors.Is(err, fat.ErrNoCommits) {
		fmt.Fprintf(os.Stderr, "git-fat: no commits yet, nothing to %s\n", verb)
		return nil
	}
	return err
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
	prog    *bool
	noProg  *bool
}

func newFlags(name string) (*flag.FlagSet, *transferFlags) {
	fs := newFlagSet(name)
	tf := &transferFlags{
		all:     fs.Bool("all", false, "consider every ref, not just HEAD"),
		profile: fs.String("profile", "", "AWS profile"),
		region:  fs.String("region", "", "AWS region"),
		jobs:    new(int),
		accel:   fs.Bool("accelerate", false, "use S3 transfer acceleration if the bucket has it enabled"),
		noAccel: fs.Bool("no-accelerate", false, "do not use S3 transfer acceleration"),
		prog:    fs.Bool("progress", false, "draw a transfer progress bar (default when stderr is a terminal)"),
		noProg:  fs.Bool("no-progress", false, "do not report transfer progress"),
	}
	tf.partMB = fs.Int("part-size", 0, "S3 multipart part size in MB")
	fs.IntVar(tf.jobs, "jobs", 0, "parallel transfers")
	fs.IntVar(tf.jobs, "j", 0, "parallel transfers (shorthand)")
	return fs, tf
}

// progress reports the --progress/--no-progress choice as a git boolean, or ""
// when neither flag was given.
func (tf *transferFlags) progress() string {
	switch {
	case *tf.noProg:
		return "false"
	case *tf.prog:
		return "true"
	}
	return ""
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

// noteIgnored logs transfer options that only apply to S3 when another backend is in use.
func (tf *transferFlags) noteIgnored(ctx context.Context, repo *fat.Repo) {
	if backend.Kind(ctx, repo.GitFatPath()) == "s3" {
		return
	}
	var set []string
	if *tf.profile != "" {
		set = append(set, "--profile")
	}
	if *tf.region != "" {
		set = append(set, "--region")
	}
	if *tf.partMB != 0 {
		set = append(set, "--part-size")
	}
	if *tf.accel {
		set = append(set, "--accelerate")
	}
	if *tf.noAccel {
		set = append(set, "--no-accelerate")
	}
	if len(set) > 0 {
		repo.Verbose("ignoring S3-only options for this remote: %s", strings.Join(set, " "))
	}
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
		Progress:   tf.progress(),
		Logf:       repo.Verbose,
	}
}

func cmdPush(ctx context.Context, repo *fat.Repo, tf *transferFlags) error {
	tf.noteIgnored(ctx, repo)
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

func cmdPull(ctx context.Context, repo *fat.Repo, tf *transferFlags, pos, patterns []string) error {
	tf.noteIgnored(ctx, repo)
	if err := repo.AssertInitDone(ctx); err != nil {
		return err
	}
	if err := repo.Setup(); err != nil {
		return err
	}

	rev := ""
	if len(pos) > 0 {
		var err error
		if rev, err = gitutil.RevParse(ctx, pos[0]); err != nil {
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

type profileFlags struct {
	fs      *flag.FlagSet
	profile *string
	region  *string
}

func newProfileFlags() *profileFlags {
	fs := newFlagSet("profile")
	return &profileFlags{
		fs:      fs,
		profile: fs.String("profile", "", "AWS profile"),
		region:  fs.String("region", "", "AWS region"),
	}
}

func cmdProfile(ctx context.Context, repo *fat.Repo, pf *profileFlags, pos []string) error {
	if len(pos) == 2 { // profile set <name>
		return gitutil.ConfigSet(ctx, "fat.profile", pos[1], "")
	}
	lk := backend.SystemLookup(ctx, repo.GitFatPath())
	p := backend.ResolveProfile(*pf.profile, lk)
	r := backend.ResolveRegion(*pf.region, lk)
	fmt.Printf("profile: %s (%s)\n", orDefault(p.Value), p.Source)
	fmt.Printf("region:  %s (%s)\n", orDefault(r.Value), r.Source)
	a := backend.ResolveAccelerate("", lk)
	fmt.Printf("accelerate: %s (%s)\n", a.Value, a.Source)
	pr := backend.ResolveProgress("", backend.Kind(ctx, repo.GitFatPath()), lk, repo.Verbose)
	fmt.Printf("progress:   %s (%s)\n", pr.Value, pr.Source)
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
