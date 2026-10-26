package fat

import "os"

// Windows refuses to delete read-only files.
func removeObject(path string) error {
	os.Chmod(path, 0o666)
	return os.Remove(path)
}
