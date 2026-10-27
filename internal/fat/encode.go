// Package fat implements the git-fat object store and git integration.
package fat

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const cookie = "#$# git-fat "

// ErrDecode reports that data is not a git-fat placeholder.
var ErrDecode = errors.New("not a git-fat placeholder")

// EncodeV1 is the legacy placeholder: no size field.
func EncodeV1(digest string, _ int64) string {
	return fmt.Sprintf("%s%s\n", cookie, digest)
}

// EncodeV2 is the current placeholder. Twenty digits hold any 64-bit size.
func EncodeV2(digest string, size int64) string {
	return fmt.Sprintf("%s%s %20d\n", cookie, digest, size)
}

// Decode parses a placeholder. size is -1 when the placeholder has none (v1).
func Decode(b []byte) (digest string, size int64, err error) {
	s := string(b)
	if !strings.HasPrefix(s, cookie) {
		return "", -1, ErrDecode
	}
	parts := strings.Fields(s[len(cookie):])
	if len(parts) == 0 || !validDigest(parts[0]) {
		return "", -1, ErrDecode
	}
	size = -1
	if len(parts) > 1 {
		n, perr := strconv.ParseInt(parts[1], 10, 64)
		if perr != nil {
			return "", -1, ErrDecode
		}
		size = n
	}
	return parts[0], size, nil
}

func validDigest(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type encoder func(digest string, size int64) string

func magicLen(enc encoder) int {
	return len(enc(fmt.Sprintf("%x", sha1.Sum([]byte("dummy"))), 5))
}

var (
	magicV1 = magicLen(EncodeV1)
	magicV2 = magicLen(EncodeV2)
)
