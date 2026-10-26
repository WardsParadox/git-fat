package backend

import (
	"context"
	"strings"
	"testing"
)

func TestAccelerateParse(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "Yes": true, "on": true, "1": true, "false": false, "no": false, "off": false, "0": false, "": false} {
		if got, err := parseGitBool(in); err != nil || got != want {
			t.Errorf("parseGitBool(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseGitBool("maybe"); err == nil {
		t.Error("expected error for invalid boolean")
	}
}

func TestAcceleratePrecedence(t *testing.T) {
	env := map[string]string{"GIT_FAT_ACCELERATE": "false"}
	cfg := map[string]string{"fat.accelerate": "true"}
	file := map[string]string{"s3.accelerate": "false"}

	cases := []struct {
		name, flag string
		lk         Lookup
		want, from string
	}{
		{"flag wins", "true", lookup(env, cfg, file), "true", "--accelerate flag"},
		{"env next", "", lookup(env, cfg, file), "false", "GIT_FAT_ACCELERATE environment variable"},
		{"git config next", "", lookup(nil, cfg, file), "true", "git config fat.accelerate"},
		{".gitfat next", "", lookup(nil, nil, map[string]string{"s3.accelerate": "true"}), "true", ".gitfat s3.accelerate"},
		{"default off", "", lookup(nil, nil, nil), "false", "default"},
	}
	for _, tc := range cases {
		got := ResolveAccelerate(tc.flag, tc.lk)
		if got.Value != tc.want || got.Source != tc.from {
			t.Errorf("%s: got %+v, want %q from %q", tc.name, got, tc.want, tc.from)
		}
	}
}

func TestDecideAcceleration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		denied bool
		want   bool
		reason string
	}{
		{"enabled", "Enabled", false, true, ""},
		{"suspended", "Suspended", false, false, "suspended"},
		{"empty config", "", false, false, "not configured"},
		{"access denied", "", true, false, "AccessDenied"},
	} {
		b, fake := newTestS3(t, t.TempDir())
		fake.accelStatus, fake.accelDenied = tc.status, tc.denied
		got, reason := decideAcceleration(context.Background(), b.client, "bucket", "", false)
		if got != tc.want || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: got %v, %q", tc.name, got, reason)
		}
	}
}

func TestAccelerationRefusedWithoutCallingS3(t *testing.T) {
	b, fake := newTestS3(t, t.TempDir())
	fake.accelStatus = "Enabled"
	for _, tc := range []struct {
		endpoint  string
		pathStyle bool
		bucket    string
		reason    string
	}{
		{"http://minio:9000", false, "bucket", "s3.endpoint"},
		{"", true, "bucket", "s3.pathstyle"},
		{"", false, "my.dotted.bucket", "dot"},
	} {
		ok, reason := decideAcceleration(context.Background(), b.client, tc.bucket, tc.endpoint, tc.pathStyle)
		if ok || !strings.Contains(reason, tc.reason) {
			t.Errorf("%+v: got %v, %q", tc, ok, reason)
		}
	}
}
