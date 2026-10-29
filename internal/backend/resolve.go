package backend

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
)

// Setting is a resolved value and where it came from.
type Setting struct {
	Value  string
	Source string
}

// Lookup abstracts the places a setting can come from, so resolution is testable.
type Lookup struct {
	Env       func(key string) string
	GitConfig func(key string) (string, bool) // repository/user git config
	GitFat    func(key string) (string, bool) // .gitfat
}

// SystemLookup reads the real environment, git config and .gitfat.
func SystemLookup(ctx context.Context, gitFatFile string) Lookup {
	return Lookup{
		Env:       os.Getenv,
		GitConfig: func(k string) (string, bool) { return gitutil.ConfigGet(ctx, k, "") },
		GitFat: func(k string) (string, bool) {
			v, ok := gitutil.ConfigGet(ctx, k, gitFatFile)
			return v, ok
		},
	}
}

// Resolve applies the precedence: flag, GIT_FAT_* env, git config fat.*,
// .gitfat s3.*, then the SDK default (reported as an empty value).
// Numeric settings (jobs, partsize) are resolved in numeric.go the same way.
func Resolve(name, flag, sdkEnv string, lk Lookup) Setting {
	if flag != "" {
		return Setting{flag, "--" + name + " flag"}
	}
	envKey := "GIT_FAT_" + strings.ToUpper(name)
	if v := lk.Env(envKey); v != "" {
		return Setting{v, envKey + " environment variable"}
	}
	if v, ok := lk.GitConfig("fat." + name); ok && v != "" {
		return Setting{v, "git config fat." + name}
	}
	if v, ok := lk.GitFat("s3." + name); ok && v != "" {
		return Setting{v, ".gitfat s3." + name}
	}
	if sdkEnv != "" {
		if v := lk.Env(sdkEnv); v != "" {
			return Setting{v, sdkEnv + " environment variable"}
		}
	}
	return Setting{"", "AWS SDK default"}
}

// ResolveProfile resolves the AWS profile name.
func ResolveProfile(flag string, lk Lookup) Setting {
	return Resolve("profile", flag, "AWS_PROFILE", lk)
}

// ResolveRegion resolves the AWS region.
func ResolveRegion(flag string, lk Lookup) Setting { return Resolve("region", flag, "", lk) }

// resolveBool applies the usual order for a git-style boolean: flag, env vars
// (first non-empty wins), git config fat.<name>, then each .gitfat key in
// order, then def. A value git would not accept as a boolean produces one
// warning and is skipped.
func resolveBool(name, flag string, envs, fileKeys []string, def bool, lk Lookup, warn func(string, ...any)) Setting {
	try := func(raw, source string) (Setting, bool) {
		if strings.TrimSpace(raw) == "" {
			return Setting{}, false
		}
		v, err := parseGitBool(raw)
		if err != nil {
			warn("ignoring invalid %s %q from %s (want a boolean)", name, raw, source)
			return Setting{}, false
		}
		return Setting{strconv.FormatBool(v), source}, true
	}
	if s, ok := try(flag, "--"+name+" flag"); ok {
		return s
	}
	for _, e := range envs {
		if s, ok := try(lk.Env(e), e+" environment variable"); ok {
			return s
		}
	}
	if v, ok := lk.GitConfig("fat." + name); ok {
		if s, ok := try(v, "git config fat."+name); ok {
			return s
		}
	}
	for _, k := range fileKeys {
		if v, ok := lk.GitFat(k); ok {
			if s, ok := try(v, ".gitfat "+k); ok {
				return s
			}
		}
	}
	return Setting{strconv.FormatBool(def), "default"}
}

// ResolveProgress resolves whether transfer progress is drawn. kind is "s3" or
// "rsync", naming the .gitfat section that can carry the shared default.
func ResolveProgress(flag, kind string, lk Lookup, warn func(string, ...any)) Setting {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	return resolveBool("progress", flag, []string{"GIT_FAT_PROGRESS"},
		[]string{kind + ".progress", "core.progress"}, true, lk, warn)
}

// progressEnabled reports whether a transfer should draw progress.
func progressEnabled(ctx context.Context, o Options, kind string) bool {
	s := ResolveProgress(o.Progress, kind, SystemLookup(ctx, o.GitFatFile), warnf)
	o.Logf("progress: %s (%s)", s.Value, s.Source)
	return s.Value == "true"
}
