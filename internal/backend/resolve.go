package backend

import (
	"context"
	"os"
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
