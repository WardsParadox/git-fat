package backend

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobsPrecedence(t *testing.T) {
	env := map[string]string{"GIT_FAT_JOBS": "3"}
	cfg := map[string]string{"fat.jobs": "4"}
	file := map[string]string{"s3.jobs": "5", "rsync.jobs": "6"}
	none := lookup(nil, nil, nil)

	cases := []struct {
		name string
		flag int
		kind string
		lk   Lookup
		want int
		from string
	}{
		{"flag", 2, "s3", lookup(env, cfg, file), 2, "--jobs flag"},
		{"env", 0, "s3", lookup(env, cfg, file), 3, "GIT_FAT_JOBS environment variable"},
		{"git config", 0, "s3", lookup(nil, cfg, file), 4, "git config fat.jobs"},
		{".gitfat s3", 0, "s3", lookup(nil, nil, file), 5, ".gitfat s3.jobs"},
		{".gitfat rsync", 0, "rsync", lookup(nil, nil, file), 6, ".gitfat rsync.jobs"},
		{"rsync ignores s3.jobs", 0, "rsync", lookup(nil, nil, map[string]string{"s3.jobs": "5"}), defaultCopyJobs, "default"},
		{"s3 default", 0, "s3", none, defaultS3Jobs(), "default"},
		{"rsync default", 0, "rsync", none, defaultCopyJobs, "default"},
	}
	for _, tc := range cases {
		got := ResolveJobs(tc.flag, tc.kind, tc.lk, func(string, ...any) {})
		if got.Value != tc.want || got.Source != tc.from {
			t.Errorf("%s: got %+v, want %d from %q", tc.name, got, tc.want, tc.from)
		}
	}
}

func TestInvalidValuesWarnAndFallThrough(t *testing.T) {
	var warnings []string
	warn := func(f string, a ...any) { warnings = append(warnings, f) }
	env := map[string]string{"GIT_FAT_JOBS": "many"}
	cfg := map[string]string{"fat.jobs": "0"}
	file := map[string]string{"s3.jobs": "7"}

	got := ResolveJobs(-1, "s3", lookup(env, cfg, file), warn)
	if got.Value != 7 || got.Source != ".gitfat s3.jobs" {
		t.Fatalf("got %+v", got)
	}
	if len(warnings) != 3 { // -1 flag, "many", "0"
		t.Fatalf("got %d warnings: %v", len(warnings), warnings)
	}

	warnings = nil
	got = ResolveJobs(0, "rsync", lookup(map[string]string{"GIT_FAT_JOBS": "-2"}, nil, nil), warn)
	if got.Value != defaultCopyJobs || len(warnings) != 1 {
		t.Fatalf("got %+v, warnings %v", got, warnings)
	}
}

func TestPartSizePrecedence(t *testing.T) {
	quiet := func(string, ...any) {}
	env := map[string]string{"GIT_FAT_PART_MB": "32", "GIT_FAT_S3_PART_MB": "24"}
	cfg := map[string]string{"fat.partsize": "16"}
	file := map[string]string{"s3.partsize": "12"}

	cases := []struct {
		name string
		flag int
		lk   Lookup
		want int
		from string
	}{
		{"flag", 64, lookup(env, cfg, file), 64, "--part-size flag"},
		{"new env", 0, lookup(env, cfg, file), 32, "GIT_FAT_PART_MB environment variable"},
		{"deprecated env alias", 0, lookup(map[string]string{"GIT_FAT_S3_PART_MB": "24"}, cfg, file), 24, "GIT_FAT_S3_PART_MB environment variable"},
		{"new env beats alias", 0, lookup(env, nil, nil), 32, "GIT_FAT_PART_MB environment variable"},
		{"git config", 0, lookup(nil, cfg, file), 16, "git config fat.partsize"},
		{".gitfat", 0, lookup(nil, nil, file), 12, ".gitfat s3.partsize"},
		{"default", 0, lookup(nil, nil, nil), 8, "default"},
		{"invalid env falls to alias", 0, lookup(map[string]string{"GIT_FAT_PART_MB": "x", "GIT_FAT_S3_PART_MB": "24"}, nil, nil), 24, "GIT_FAT_S3_PART_MB environment variable"},
		{"invalid git config falls to .gitfat", 0, lookup(nil, map[string]string{"fat.partsize": "0"}, file), 12, ".gitfat s3.partsize"},
	}
	for _, tc := range cases {
		got := ResolvePartSize(tc.flag, tc.lk, quiet)
		if got.Value != tc.want || got.Source != tc.from {
			t.Errorf("%s: got %+v, want %d from %q", tc.name, got, tc.want, tc.from)
		}
	}
}

// Settings placed in .git/config instead of .gitfat keep working.
func TestRepoConfigFallbackForGitFatKeys(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "s3.jobs", "9")
	run("config", "s3.partsize", "20")
	t.Chdir(dir)

	lk := SystemLookup(t.Context(), filepath.Join(dir, ".gitfat"))
	lk.Env = func(string) string { return "" }
	if got := ResolveJobs(0, "s3", lk, func(string, ...any) {}); got.Value != 9 {
		t.Errorf("jobs = %+v", got)
	}
	if got := ResolvePartSize(0, lk, func(string, ...any) {}); got.Value != 20 {
		t.Errorf("partsize = %+v", got)
	}
}
