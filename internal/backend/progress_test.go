package backend

import (
	"strings"
	"testing"
)

func TestProgressPrecedence(t *testing.T) {
	env := map[string]string{"GIT_FAT_PROGRESS": "false"}
	cfg := map[string]string{"fat.progress": "true"}
	file := map[string]string{"s3.progress": "false"}

	cases := []struct {
		name, flag string
		lk         Lookup
		want, from string
	}{
		{"flag wins", "false", lookup(env, cfg, file), "false", "--progress flag"},
		{"env next", "", lookup(env, cfg, file), "false", "GIT_FAT_PROGRESS environment variable"},
		{"git config next", "", lookup(nil, cfg, file), "true", "git config fat.progress"},
		{".gitfat section next", "", lookup(nil, nil, file), "false", ".gitfat s3.progress"},
		{".gitfat core last", "", lookup(nil, nil, map[string]string{"core.progress": "off"}), "false", ".gitfat core.progress"},
		{"default on", "", lookup(nil, nil, nil), "true", "default"},
	}
	for _, tc := range cases {
		got := ResolveProgress(tc.flag, "s3", tc.lk, func(string, ...any) {})
		if got.Value != tc.want || got.Source != tc.from {
			t.Errorf("%s: got %+v, want %q from %q", tc.name, got, tc.want, tc.from)
		}
	}
}

// The rsync section only applies to rsync-style remotes.
func TestProgressSectionFollowsKind(t *testing.T) {
	file := map[string]string{"rsync.progress": "false"}
	if got := ResolveProgress("", "rsync", lookup(nil, nil, file), nil); got.Value != "false" {
		t.Errorf("rsync kind: got %+v, want false from .gitfat rsync.progress", got)
	}
	if got := ResolveProgress("", "s3", lookup(nil, nil, file), nil); got.Value != "true" {
		t.Errorf("s3 kind: got %+v, want the default", got)
	}
}

// An unparseable value warns once and falls through to the next source.
func TestProgressInvalidValueSkipped(t *testing.T) {
	var warnings []string
	warn := func(f string, a ...any) { warnings = append(warnings, f) }
	got := ResolveProgress("", "s3", lookup(
		map[string]string{"GIT_FAT_PROGRESS": "maybe"},
		map[string]string{"fat.progress": "true"}, nil), warn)
	if got.Value != "true" || got.Source != "git config fat.progress" {
		t.Errorf("got %+v, want true from git config fat.progress", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "invalid") {
		t.Errorf("warnings = %v, want one about the invalid value", warnings)
	}
}
