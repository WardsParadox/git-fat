package fat

import "testing"

const testDigest = "6ecec2e21d3033e7ba53e2db63f69dbd3a011fa8"

func TestMagicLens(t *testing.T) {
	if magicV1 != 53 || magicV2 != 74 {
		t.Fatalf("magic lengths = %d, %d; want 53, 74", magicV1, magicV2)
	}
}

// Fixture produced by the Python implementation: '#$# git-fat %s %20d\n'.
func TestEncodeV2MatchesPython(t *testing.T) {
	want := "#$# git-fat 6ecec2e21d3033e7ba53e2db63f69dbd3a011fa8                   14\n"
	if got := EncodeV2(testDigest, 14); got != want {
		t.Fatalf("EncodeV2 = %q, want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		size int64
	}{
		{"v1", EncodeV1(testDigest, 99), -1},
		{"v2", EncodeV2(testDigest, 12345), 12345},
		{"v2 large", EncodeV2(testDigest, 1<<40), 1 << 40},
	} {
		d, n, err := Decode([]byte(tc.in))
		if err != nil || d != testDigest || n != tc.size {
			t.Errorf("%s: got (%q, %d, %v)", tc.name, d, n, err)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"hello world",
		"#$# git-fat \n",
		"#$# git-fat nothex\n",
		"#$# git-fat " + testDigest + " abc\n",
	} {
		if _, _, err := Decode([]byte(in)); err == nil {
			t.Errorf("Decode(%q) succeeded", in)
		}
	}
}
