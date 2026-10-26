package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/WardsParadox/git-fat/internal/gitutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const s3MaxAttempts = 5

type s3Backend struct {
	client    *s3.Client // listing, always the standard endpoint
	xfer      *s3.Client // uploads and downloads; accelerated when enabled
	bucket    string
	endpoint  string
	pathStyle bool
	prefix    string
	objDir    string
	jobs      int
	logf      func(string, ...any)

	partSize int64         // bytes; 0 means the default
	sem      chan struct{} // bounds in-flight part requests across all objects
}

func newS3(ctx context.Context, o Options, bucket string) (*s3Backend, error) {
	bucket, urlPrefix := splitBucket(bucket)

	lk := SystemLookup(ctx, o.GitFatFile)
	profile := ResolveProfile(o.Profile, lk)
	region := ResolveRegion(o.Region, lk)
	o.Logf("s3: profile=%q (%s) region=%q (%s)", profile.Value, profile.Source, region.Value, region.Source)

	endpoint, _ := gitutil.ConfigGet(ctx, "s3.endpoint", o.GitFatFile)
	pathStyle := configBool(ctx, "s3.pathstyle", o.GitFatFile)
	prefix, _ := gitutil.ConfigGet(ctx, "s3.prefix", o.GitFatFile)
	if prefix == "" {
		prefix = urlPrefix
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRetryMaxAttempts(s3MaxAttempts),
		config.WithRetryMode(aws.RetryModeAdaptive),
		// Newer default checksums are rejected by some S3-compatible stores.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if profile.Value != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile.Value))
	}
	if region.Value != "" {
		opts = append(opts, config.WithRegion(region.Value))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	newClient := func(c aws.Config) *s3.Client {
		return s3.NewFromConfig(c, func(so *s3.Options) {
			if endpoint != "" {
				so.BaseEndpoint = aws.String(endpoint)
			}
			so.UsePathStyle = pathStyle
		})
	}
	if cfg.Region == "" {
		if endpoint != "" {
			cfg.Region = "us-east-1"
		} else {
			boot := cfg.Copy()
			boot.Region = "us-east-1"
			r, err := manager.GetBucketRegion(ctx, newClient(boot), bucket)
			if err != nil {
				return nil, fmt.Errorf("no AWS region configured (use --region, fat.region or s3.region): %w", err)
			}
			cfg.Region = r
		}
	}
	client := newClient(cfg)
	xfer := client

	accel := ResolveAccelerate(o.Accelerate, lk)
	want, perr := parseGitBool(accel.Value)
	if perr != nil {
		warnf("ignoring accelerate setting from %s: %v", accel.Source, perr)
	}
	if want {
		ok, reason := decideAcceleration(ctx, client, bucket, endpoint, pathStyle)
		if ok {
			xfer = s3.NewFromConfig(cfg, func(so *s3.Options) { so.UseAccelerate = true })
			o.Logf("s3: transfer acceleration enabled")
		} else {
			warnf("S3 transfer acceleration was requested (%s) but is unavailable: %s; using the standard endpoint", accel.Source, reason)
			o.Logf("s3: transfer acceleration disabled: %s", reason)
		}
	} else {
		o.Logf("s3: transfer acceleration disabled: not requested")
	}

	return &s3Backend{
		client:    client,
		xfer:      xfer,
		endpoint:  endpoint,
		pathStyle: pathStyle,
		bucket:    bucket,
		prefix:    prefix,
		objDir:    o.ObjDir,
		jobs:      o.jobs(ctx, "s3.jobs", defaultS3Jobs()),
		partSize:  s3PartSizeFromConfig(ctx, o),
		logf:      o.Logf,
	}, nil
}

// splitBucket separates "s3://bucket/some/prefix" into bucket and prefix.
func splitBucket(v string) (bucket, prefix string) {
	v = strings.TrimPrefix(v, "s3://")
	bucket, prefix, _ = strings.Cut(v, "/")
	return bucket, strings.Trim(prefix, "/")
}

// list returns the size of every remote object under the prefix, keyed by name.
func (b *s3Backend) list(ctx context.Context) (map[string]int64, error) {
	sizes := make(map[string]int64)
	pager := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(b.prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s: %w", b.bucket, err)
		}
		for _, obj := range page.Contents {
			sizes[strings.TrimPrefix(aws.ToString(obj.Key), b.prefix)] = aws.ToInt64(obj.Size)
		}
	}
	return sizes, nil
}
