package backend

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// IntSetting is a resolved positive integer and where it came from.
type IntSetting struct {
	Value  int
	Source string
}

func (s IntSetting) String() string { return fmt.Sprintf("%d (%s)", s.Value, s.Source) }

// resolveInt applies the usual order: flag, env vars (first non-empty wins),
// git config fat.<name>, then each .gitfat key in order, then def. A value
// that is not a positive integer produces one warning and is skipped.
func resolveInt(name string, flag int, envs []string, fileKeys []string, def int, lk Lookup, warn func(string, ...any)) IntSetting {
	try := func(raw, source string) (int, bool) {
		if raw == "" {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 {
			warn("ignoring invalid %s %q from %s (want a positive integer)", name, raw, source)
			return 0, false
		}
		return n, true
	}
	if flag != 0 {
		if flag > 0 {
			return IntSetting{flag, "--" + name + " flag"}
		}
		warn("ignoring invalid %s %d from --%s flag (want a positive integer)", name, flag, name)
	}
	for _, e := range envs {
		if n, ok := try(lk.Env(e), e+" environment variable"); ok {
			return IntSetting{n, e + " environment variable"}
		}
	}
	if v, ok := lk.GitConfig("fat." + name); ok {
		if n, ok := try(v, "git config fat."+name); ok {
			return IntSetting{n, "git config fat." + name}
		}
	}
	for _, k := range fileKeys {
		if v, ok := lk.GitFat(k); ok {
			if n, ok := try(v, ".gitfat "+k); ok {
				return IntSetting{n, ".gitfat " + k}
			}
		}
	}
	return IntSetting{def, "default"}
}

// ResolveJobs resolves the number of parallel transfers. kind is "s3" or "rsync".
func ResolveJobs(flag int, kind string, lk Lookup, warn func(string, ...any)) IntSetting {
	def := defaultCopyJobs
	if kind == "s3" {
		def = defaultS3Jobs()
	}
	return resolveInt("jobs", flag, []string{"GIT_FAT_JOBS"}, []string{kind + ".jobs"}, def, lk, warn)
}

// ResolvePartSize resolves the S3 multipart part size in MB, before the 5 MB
// floor is applied. GIT_FAT_S3_PART_MB is a deprecated alias of GIT_FAT_PART_MB.
func ResolvePartSize(flagMB int, lk Lookup, warn func(string, ...any)) IntSetting {
	r := resolveInt("partsize", flagMB, []string{"GIT_FAT_PART_MB", "GIT_FAT_S3_PART_MB"}, []string{"s3.partsize"}, defaultPartSize/mib, lk, warn)
	if r.Source == "--partsize flag" {
		r.Source = "--part-size flag"
	}
	return r
}

func defaultS3Jobs() int { return min(16, 4*runtime.NumCPU()) }

// Kind reports which backend .gitfat selects: "s3" or "rsync".
func Kind(ctx context.Context, gitFatFile string) string {
	if v, ok := SystemLookup(ctx, gitFatFile).GitFat("s3.bucket"); ok && v != "" {
		return "s3"
	}
	return "rsync"
}

// TransferSettings resolves jobs and part size from the real environment.
func TransferSettings(ctx context.Context, o Options) (jobs, partMB IntSetting) {
	lk := SystemLookup(ctx, o.GitFatFile)
	return ResolveJobs(o.Jobs, Kind(ctx, o.GitFatFile), lk, warnf), ResolvePartSize(o.PartSizeMB, lk, warnf)
}
