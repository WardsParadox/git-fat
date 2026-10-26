package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/WardsParadox/git-fat/internal/fat"
	"github.com/WardsParadox/git-fat/internal/gitutil"
	"github.com/WardsParadox/git-fat/internal/progress"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"
)

const (
	mib             = 1 << 20
	defaultPartSize = 8 * mib
	minPartSize     = 5 * mib // S3 minimum for all but the last part
	maxParts        = 9000    // stay clear of the 10,000 part limit
	minObjParts     = 4
	partAttempts    = 3
	hashChunk       = 1 * mib
)

// s3PartSizeFromConfig reads GIT_FAT_S3_PART_MB, then s3.partsize (in MB).
func s3PartSizeFromConfig(ctx context.Context, o Options) int64 {
	v := os.Getenv("GIT_FAT_S3_PART_MB")
	if v == "" {
		v, _ = gitutil.ConfigGet(ctx, "s3.partsize", o.GitFatFile)
	}
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		warnf("ignoring invalid S3 part size %q (want a number of MB)", v)
		return 0
	}
	return max(int64(n)*mib, minPartSize)
}

// ensure sets defaults for fields a test or caller left zero.
func (b *s3Backend) ensure() {
	if b.partSize == 0 {
		b.partSize = defaultPartSize
	}
	if b.sem == nil {
		b.sem = make(chan struct{}, b.budget())
	}
}

// budget is the total number of in-flight part requests.
func (b *s3Backend) budget() int { return max(16, b.jobs*4) }

// partSizeFor grows the part size only when the object would need too many parts.
func (b *s3Backend) partSizeFor(size int64) int64 {
	ps := b.partSize
	if need := (size + maxParts - 1) / maxParts; need > ps {
		ps = (need + mib - 1) / mib * mib
	}
	return ps
}

// concurrency is the part parallelism for one object: the shared budget split
// across the objects that can run at once, never below minObjParts.
func (b *s3Backend) concurrency(remaining int64) int {
	active := int(min(int64(b.jobs), max(remaining, 1)))
	return max(minObjParts, b.budget()/active)
}

func (b *s3Backend) acquire(ctx context.Context) error {
	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *s3Backend) release() { <-b.sem }

// runParts runs fn for part indexes 0..n-1 on up to workers goroutines,
// dispatching in order.
func runParts(ctx context.Context, n, workers int, fn func(ctx context.Context, i int) error) error {
	g, ctx := errgroup.WithContext(ctx)
	var next atomic.Int64
	for w := 0; w < min(workers, n); w++ {
		g.Go(func() error {
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return nil
				}
				if err := fn(ctx, i); err != nil {
					return err
				}
			}
		})
	}
	return g.Wait()
}

func (b *s3Backend) Push(ctx context.Context, names []string) error {
	b.ensure()
	b.logf("Pushing to s3://%s", b.bucket)
	remote, err := b.list(ctx)
	if err != nil {
		return err
	}

	type item struct {
		name string
		size int64
	}
	var todo []item
	var total int64
	for _, name := range names {
		fi, err := os.Stat(filepath.Join(b.objDir, name))
		if err != nil {
			return err
		}
		if rs, ok := remote[name]; ok && rs == fi.Size() {
			continue
		}
		todo = append(todo, item{name, fi.Size()})
		total += fi.Size()
	}
	if len(todo) == 0 {
		return nil
	}
	// Largest first keeps the tail of the run from being one big object.
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].size > todo[j].size })

	prog := progress.New("push", len(todo), total)
	defer prog.Close()
	remaining := atomic.Int64{}
	remaining.Store(int64(len(todo)))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.jobs)
	for _, it := range todo {
		g.Go(func() error {
			b.logf("Processing %s", it.name)
			conc := b.concurrency(remaining.Load())
			remaining.Add(-1)
			if err := b.upload(ctx, it.name, it.size, conc, prog); err != nil {
				return fmt.Errorf("uploading %s: %w", it.name, err)
			}
			prog.FileDone()
			return nil
		})
	}
	return g.Wait()
}

// countingSection counts bytes as the SDK reads a part body.
type countingSection struct {
	*io.SectionReader
	add func(int64)
}

func (c countingSection) Read(p []byte) (int, error) {
	n, err := c.SectionReader.Read(p)
	c.add(int64(n))
	return n, err
}

func (b *s3Backend) upload(ctx context.Context, name string, size int64, conc int, prog *progress.Tracker) error {
	f, err := os.Open(filepath.Join(b.objDir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	key := aws.String(b.prefix + name)
	bucket := aws.String(b.bucket)
	body := func(off, n int64) countingSection {
		return countingSection{io.NewSectionReader(f, off, n), prog.Add}
	}

	ps := b.partSizeFor(size)
	if size <= ps {
		if err := b.acquire(ctx); err != nil {
			return err
		}
		defer b.release()
		_, err := b.xfer.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: body(0, size), ContentLength: aws.Int64(size)})
		return err
	}

	create, err := b.xfer.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key})
	if err != nil {
		return err
	}
	uploadID := create.UploadId
	abort := func() {
		b.xfer.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{Bucket: bucket, Key: key, UploadId: uploadID})
	}

	n := int((size + ps - 1) / ps)
	parts := make([]types.CompletedPart, n)
	err = runParts(ctx, n, conc, func(ctx context.Context, i int) error {
		off := int64(i) * ps
		length := min(ps, size-off)
		if err := b.acquire(ctx); err != nil {
			return err
		}
		defer b.release()
		out, err := b.xfer.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: key, UploadId: uploadID,
			PartNumber:    aws.Int32(int32(i + 1)),
			Body:          body(off, length),
			ContentLength: aws.Int64(length),
		})
		if err != nil {
			return fmt.Errorf("part %d: %w", i+1, err)
		}
		parts[i] = types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1))}
		return nil
	})
	if err == nil {
		_, err = b.xfer.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: bucket, Key: key, UploadId: uploadID,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
	}
	if err != nil {
		abort()
	}
	return err
}

func (b *s3Backend) Pull(ctx context.Context, names []string) error {
	b.ensure()
	b.logf("Pulling from s3://%s", b.bucket)
	remote, err := b.list(ctx)
	if err != nil {
		return err
	}

	type item struct {
		name string
		size int64
	}
	var todo []item
	var missing []string
	var total int64
	for _, name := range names {
		rs, ok := remote[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if fi, err := os.Stat(filepath.Join(b.objDir, name)); err == nil && fi.Size() == rs {
			continue
		}
		todo = append(todo, item{name, rs})
		total += rs
	}
	if len(todo) == 0 {
		return missingError(missing)
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].size > todo[j].size })

	prog := progress.New("pull", len(todo), total)
	defer prog.Close()
	remaining := atomic.Int64{}
	remaining.Store(int64(len(todo)))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.jobs)
	for _, it := range todo {
		g.Go(func() error {
			b.logf("Processing %s", it.name)
			conc := b.concurrency(remaining.Load())
			remaining.Add(-1)
			if err := b.download(ctx, it.name, it.size, conc, prog); err != nil {
				return fmt.Errorf("downloading %s: %w", it.name, err)
			}
			prog.FileDone()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return missingError(missing)
}

// download fetches ranged parts in parallel straight into the temp file while a
// hashing goroutine follows the contiguous prefix, reading completed parts back
// from the file in order. Memory stays at one hash chunk; there is no second
// pass over the file.
func (b *s3Backend) download(ctx context.Context, name string, size int64, conc int, prog *progress.Tracker) error {
	tmp, err := newTempObject(b.objDir, name)
	if err != nil {
		return err
	}
	defer tmp.Discard()
	if err := tmp.File.Truncate(size); err != nil {
		return err
	}

	ps := b.partSizeFor(size)
	n := int((size + ps - 1) / ps)
	done := make([]chan struct{}, n)
	for i := range done {
		done[i] = make(chan struct{})
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { // hasher
		buf := make([]byte, hashChunk)
		for i := 0; i < n; i++ {
			select {
			case <-done[i]:
			case <-gctx.Done():
				return gctx.Err()
			}
			off := int64(i) * ps
			end := min(off+ps, size)
			for off < end {
				m, err := tmp.File.ReadAt(buf[:min(int64(len(buf)), end-off)], off)
				tmp.hash.Write(buf[:m])
				off += int64(m)
				if err != nil && !(errors.Is(err, io.EOF) && off == end) {
					return err
				}
			}
		}
		return nil
	})
	g.Go(func() error {
		return runParts(gctx, n, conc, func(ctx context.Context, i int) error {
			off := int64(i) * ps
			end := min(off+ps, size)
			if err := b.fetchPart(ctx, name, tmp.File, off, end, prog); err != nil {
				return fmt.Errorf("part %d: %w", i+1, err)
			}
			close(done[i])
			return nil
		})
	})
	if err := g.Wait(); err != nil {
		return err
	}
	return tmp.Commit(false, fat.ObjectMode())
}

func (b *s3Backend) fetchPart(ctx context.Context, name string, f *os.File, off, end int64, prog *progress.Tracker) error {
	var err error
	for attempt := 0; attempt < partAttempts; attempt++ {
		if err = b.fetchOnce(ctx, name, f, off, end, prog); err == nil || ctx.Err() != nil || errors.Is(err, errMissing) {
			return err
		}
	}
	return err
}

func (b *s3Backend) fetchOnce(ctx context.Context, name string, f *os.File, off, end int64, prog *progress.Tracker) error {
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()
	out, err := b.xfer.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.prefix + name),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, end-1)),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return errMissing
		}
		return err
	}
	defer out.Body.Close()
	n, err := io.Copy(countingWriter{io.NewOffsetWriter(f, off), prog.Add}, out.Body)
	if err == nil && n != end-off {
		err = fmt.Errorf("short read: got %d of %d bytes", n, end-off)
	}
	return err
}
