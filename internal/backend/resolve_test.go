package backend

import "testing"

func lookup(env map[string]string, gitCfg, gitFat map[string]string) Lookup {
	get := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	return Lookup{
		Env:       func(k string) string { return env[k] },
		GitConfig: get(gitCfg),
		GitFat:    get(gitFat),
	}
}

func TestProfilePrecedence(t *testing.T) {
	env := map[string]string{"GIT_FAT_PROFILE": "fromenv", "AWS_PROFILE": "awsenv"}
	cfg := map[string]string{"fat.profile": "fromcfg"}
	file := map[string]string{"s3.profile": "fromfile"}

	cases := []struct {
		name     string
		flag     string
		lk       Lookup
		want     string
		wantFrom string
	}{
		{"flag wins", "fromflag", lookup(env, cfg, file), "fromflag", "--profile flag"},
		{"env next", "", lookup(env, cfg, file), "fromenv", "GIT_FAT_PROFILE environment variable"},
		{"git config next", "", lookup(map[string]string{"AWS_PROFILE": "awsenv"}, cfg, file), "fromcfg", "git config fat.profile"},
		{".gitfat next", "", lookup(map[string]string{"AWS_PROFILE": "awsenv"}, nil, file), "fromfile", ".gitfat s3.profile"},
		{"AWS_PROFILE next", "", lookup(map[string]string{"AWS_PROFILE": "awsenv"}, nil, nil), "awsenv", "AWS_PROFILE environment variable"},
		{"SDK default", "", lookup(nil, nil, nil), "", "AWS SDK default"},
	}
	for _, tc := range cases {
		got := ResolveProfile(tc.flag, tc.lk)
		if got.Value != tc.want || got.Source != tc.wantFrom {
			t.Errorf("%s: got %+v, want %q from %q", tc.name, got, tc.want, tc.wantFrom)
		}
	}
}

func TestRegionPrecedence(t *testing.T) {
	env := map[string]string{"GIT_FAT_REGION": "us-east-2", "AWS_REGION": "ignored"}
	cfg := map[string]string{"fat.region": "eu-west-1"}
	file := map[string]string{"s3.region": "ap-south-1"}

	if got := ResolveRegion("sa-east-1", lookup(env, cfg, file)); got.Value != "sa-east-1" {
		t.Errorf("flag: %+v", got)
	}
	if got := ResolveRegion("", lookup(env, cfg, file)); got.Value != "us-east-2" {
		t.Errorf("env: %+v", got)
	}
	if got := ResolveRegion("", lookup(nil, cfg, file)); got.Value != "eu-west-1" {
		t.Errorf("git config: %+v", got)
	}
	if got := ResolveRegion("", lookup(nil, nil, file)); got.Value != "ap-south-1" {
		t.Errorf(".gitfat: %+v", got)
	}
	if got := ResolveRegion("", lookup(nil, nil, nil)); got.Value != "" || got.Source != "AWS SDK default" {
		t.Errorf("default: %+v", got)
	}
}
