//go:build !windows

package fat

import "os"

func removeObject(path string) error { return os.Remove(path) }
