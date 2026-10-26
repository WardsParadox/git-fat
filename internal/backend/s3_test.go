package backend

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 implements just enough of the S3 API for path-style list, put and get.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte

	accelStatus string // returned by GetBucketAccelerateConfiguration
	accelDenied bool

	uploads   map[string]map[int][]byte // uploadId -> part number -> data
	nextID    int
	aborted   int
	rangeGets int
	corrupt   bool // flip a byte in every ranged response after the first part
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	_, accel := r.URL.Query()["accelerate"]
	switch {
	case r.Method == http.MethodGet && accel:
		if f.accelDenied {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
			return
		}
		fmt.Fprintf(w, `<AccelerateConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		if f.accelStatus != "" {
			fmt.Fprintf(w, `<Status>%s</Status>`, f.accelStatus)
		}
		fmt.Fprint(w, `</AccelerateConfiguration>`)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		type content struct {
			Key  string
			Size int64
		}
		var res struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			IsTruncated bool
			Contents    []content
		}
		for k, v := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				res.Contents = append(res.Contents, content{k, int64(len(v))})
			}
		}
		xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		if f.uploads == nil {
			f.uploads = map[string]map[int][]byte{}
		}
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
		n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		b, _ := io.ReadAll(r.Body)
		f.uploads[r.URL.Query().Get("uploadId")][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
	case r.Method == http.MethodPost && r.URL.Query().Has("uploadId"):
		var req struct {
			Part []struct{ PartNumber int }
		}
		xml.NewDecoder(r.Body).Decode(&req)
		parts := f.uploads[r.URL.Query().Get("uploadId")]
		var all []byte
		for _, p := range req.Part {
			all = append(all, parts[p.PartNumber]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && r.URL.Query().Has("uploadId"):
		delete(f.uploads, r.URL.Query().Get("uploadId"))
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
		w.Header().Set("ETag", `"x"`)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		if r.Header.Get("Range") != "" {
			f.rangeGets++
			if f.corrupt && !strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
				b = append([]byte(nil), b...)
				b[len(b)-1] ^= 0xff
			}
		}
		http.ServeContent(w, r, key, time.Time{}, strings.NewReader(string(b)))
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func newTestS3(t *testing.T, objDir string) (*s3Backend, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("a", "b", ""),
	})
	return &s3Backend{client: client, xfer: client, bucket: "bucket", objDir: objDir, jobs: 4, logf: func(string, ...any) {}}, fake
}

func writeObject(t *testing.T, dir, content string) string {
	t.Helper()
	sum := sha1.Sum([]byte(content))
	name := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestS3PushPullRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	var names []string
	for i := 0; i < 20; i++ {
		names = append(names, writeObject(t, src, fmt.Sprintf("object %d %s", i, strings.Repeat("x", i*1000))))
	}

	pusher, fake := newTestS3(t, src)
	if err := pusher.Push(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != len(names) {
		t.Fatalf("remote has %d objects, want %d", len(fake.objects), len(names))
	}

	puller := *pusher
	puller.objDir = dst
	if err := puller.Pull(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		got, err := os.ReadFile(filepath.Join(dst, n))
		if err != nil || string(got) != string(fake.objects[n]) {
			t.Fatalf("object %s not restored: %v", n, err)
		}
	}
}

func TestS3PullRejectsCorruptObject(t *testing.T) {
	dst := t.TempDir()
	b, fake := newTestS3(t, dst)
	name := "0123456789012345678901234567890123456789"
	fake.objects[name] = []byte("not what the name says")
	if err := b.Pull(context.Background(), []string{name}); err == nil {
		t.Fatal("expected hash mismatch error")
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestSplitBucket(t *testing.T) {
	for _, tc := range []struct{ in, bucket, prefix string }{
		{"s3://b", "b", ""},
		{"b", "b", ""},
		{"s3://b/", "b", ""},
		{"s3://b/some/prefix/", "b", "some/prefix"},
	} {
		if b, p := splitBucket(tc.in); b != tc.bucket || p != tc.prefix {
			t.Errorf("splitBucket(%q) = %q, %q", tc.in, b, p)
		}
	}
}

func TestS3PullReportsMissingButKeepsGoing(t *testing.T) {
	dst := t.TempDir()
	b, fake := newTestS3(t, dst)
	have := "present object"
	sum := sha1.Sum([]byte(have))
	name := hex.EncodeToString(sum[:])
	fake.objects[name] = []byte(have)
	gone := "0123456789012345678901234567890123456789"

	err := b.Pull(context.Background(), []string{gone, name})
	var me *MissingError
	if !errors.As(err, &me) || len(me.Names) != 1 || me.Names[0] != gone {
		t.Fatalf("err = %v, want MissingError for %s", err, gone)
	}
	if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
		t.Fatalf("available object was not downloaded: %v", err)
	}
}

func tmpFilesIn(t *testing.T, dir string, objects ...string) []string {
	t.Helper()
	known := map[string]bool{}
	for _, o := range objects {
		known[o] = true
	}
	entries, _ := os.ReadDir(dir)
	var extra []string
	for _, e := range entries {
		if !known[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

func TestS3MultipartRoundTripAtDifferentJobs(t *testing.T) {
	content := strings.Repeat("0123456789abcdef", 200_000/16) // ~200 KB
	for _, jobs := range []int{1, 8} {
		src, dst := t.TempDir(), t.TempDir()
		name := writeObject(t, src, content)
		b, fake := newTestS3(t, src)
		b.jobs, b.partSize = jobs, 64<<10 // 4 parts

		if err := b.Push(context.Background(), []string{name}); err != nil {
			t.Fatalf("-j %d push: %v", jobs, err)
		}
		if string(fake.objects[name]) != content || len(fake.uploads) == 0 {
			t.Fatalf("-j %d: multipart upload did not assemble the object", jobs)
		}

		b.objDir = dst
		if err := b.Pull(context.Background(), []string{name}); err != nil {
			t.Fatalf("-j %d pull: %v", jobs, err)
		}
		got, _ := os.ReadFile(filepath.Join(dst, name))
		if string(got) != content {
			t.Fatalf("-j %d: downloaded content differs", jobs)
		}
		if fake.rangeGets < 3 {
			t.Fatalf("-j %d: %d ranged GETs, want at least 3", jobs, fake.rangeGets)
		}
		if extra := tmpFilesIn(t, dst, name); len(extra) != 0 {
			t.Fatalf("-j %d: leftover files %v", jobs, extra)
		}
	}
}

func TestS3PullRejectsCorruptRange(t *testing.T) {
	content := strings.Repeat("fedcba9876543210", 200_000/16)
	src, dst := t.TempDir(), t.TempDir()
	name := writeObject(t, src, content)
	b, fake := newTestS3(t, dst)
	b.jobs, b.partSize = 8, 64<<10
	fake.objects[name] = []byte(content)
	fake.corrupt = true

	if err := b.Pull(context.Background(), []string{name}); err == nil {
		t.Fatal("corrupt part was accepted")
	}
	if extra := tmpFilesIn(t, dst); len(extra) != 0 {
		t.Fatalf("leftover files %v", extra)
	}
}

func TestS3PartSizingAndConcurrency(t *testing.T) {
	b := &s3Backend{jobs: 8}
	b.ensure()
	if got := b.partSizeFor(1 << 30); got != 8<<20 {
		t.Errorf("1 GiB part size = %d, want 8 MiB", got)
	}
	// 100 GiB at 8 MiB would be 12,800 parts; the size must grow to fit.
	size := int64(100 << 30)
	ps := b.partSizeFor(size)
	if ps%(1<<20) != 0 || (size+ps-1)/ps > 9000 {
		t.Errorf("100 GiB part size = %d gives %d parts", ps, (size+ps-1)/ps)
	}
	if got := b.budget(); got != 32 {
		t.Errorf("budget at -j 8 = %d, want 32", got)
	}
	if got := (&s3Backend{jobs: 2}).budget(); got != 16 {
		t.Errorf("budget at -j 2 = %d, want the minimum 16", got)
	}
	for remaining, want := range map[int64]int{1: 32, 4: 8, 8: 4, 200: 4} {
		if got := b.concurrency(remaining); got != want {
			t.Errorf("concurrency(%d files) = %d, want %d", remaining, got, want)
		}
	}
}
