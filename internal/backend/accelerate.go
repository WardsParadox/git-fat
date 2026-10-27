package backend

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// parseGitBool accepts the spellings git does for booleans.
func parseGitBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "", "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", v)
}

// ResolveAccelerate resolves whether transfer acceleration is requested:
// flag, GIT_FAT_ACCELERATE, git config fat.accelerate, .gitfat s3.accelerate.
func ResolveAccelerate(flag string, lk Lookup) Setting {
	s := Resolve("accelerate", flag, "", lk)
	if s.Value == "" {
		return Setting{"false", "default"}
	}
	return s
}

// accelerateRefusal returns why acceleration cannot be used with this
// configuration, or "".
func accelerateRefusal(endpoint string, pathStyle bool, bucket string) string {
	switch {
	case endpoint != "":
		return "s3.endpoint is set"
	case pathStyle:
		return "s3.pathstyle is set"
	case strings.Contains(bucket, "."):
		return "bucket name contains a dot"
	}
	return ""
}

// bucketAccelerateStatus returns "Enabled", "Suspended" or "" when the bucket
// has no acceleration configuration.
func bucketAccelerateStatus(ctx context.Context, c *s3.Client, bucket string) (string, error) {
	out, err := c.GetBucketAccelerateConfiguration(ctx, &s3.GetBucketAccelerateConfigurationInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return "", err
	}
	return string(out.Status), nil
}

// decideAcceleration reports whether the transfer client should use the
// accelerate endpoint. When it should not, reason says why. It never fails.
func decideAcceleration(ctx context.Context, c *s3.Client, bucket, endpoint string, pathStyle bool) (bool, string) {
	if r := accelerateRefusal(endpoint, pathStyle, bucket); r != "" {
		return false, r
	}
	status, err := bucketAccelerateStatus(ctx, c, bucket)
	switch {
	case err != nil:
		return false, "could not read the bucket's acceleration status: " + err.Error()
	case status == "Enabled":
		return true, ""
	case status == "":
		return false, "acceleration is not configured on the bucket"
	default:
		return false, "acceleration is " + strings.ToLower(status) + " on the bucket"
	}
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "git-fat: warning: "+format+"\n", args...)
}

// S3Info describes the resolved S3 settings and the bucket's acceleration status.
func S3Info(ctx context.Context, o Options) ([]string, error) {
	bucket, ok := gitutil.ConfigGet(ctx, "s3.bucket", o.GitFatFile)
	if !ok || bucket == "" {
		return nil, fmt.Errorf("no s3.bucket in %s", o.GitFatFile)
	}
	b, err := newS3(ctx, o, bucket)
	if err != nil {
		return nil, err
	}
	lk := SystemLookup(ctx, o.GitFatFile)
	p, r := ResolveProfile(o.Profile, lk), ResolveRegion(o.Region, lk)
	req := ResolveAccelerate(o.Accelerate, lk)
	status, err := bucketAccelerateStatus(ctx, b.client, b.bucket)
	switch {
	case err != nil:
		status = "unknown (" + err.Error() + ")"
	case status == "":
		status = "not configured"
	}
	jobs, part := b.jobsSetting, b.partSetting
	lines := []string{
		fmt.Sprintf("bucket:     %s", b.bucket),
		fmt.Sprintf("profile:    %s (%s)", orDefault(p.Value), p.Source),
		fmt.Sprintf("region:     %s (%s)", orDefault(r.Value), r.Source),
		fmt.Sprintf("accelerate: %s (%s)", req.Value, req.Source),
		fmt.Sprintf("jobs:       %s", jobs),
		fmt.Sprintf("partsize:   %s MB (minimum 5)", part),
		fmt.Sprintf("bucket acceleration status: %s", status),
	}
	if reason := accelerateRefusal(b.endpoint, b.pathStyle, b.bucket); reason != "" {
		lines = append(lines, "acceleration would be refused: "+reason)
	}
	return lines, nil
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
