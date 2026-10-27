//go:build !windows

package fat

import (
	"sync"
	"syscall"
)

var (
	umaskOnce  sync.Once
	umaskValue int
)

// umask reads the process umask once; reading it means briefly changing it.
func umask() int {
	umaskOnce.Do(func() {
		umaskValue = syscall.Umask(0)
		syscall.Umask(umaskValue)
	})
	return umaskValue
}
