//go:build !windows

package fat

import (
	"sync"
	"syscall"
	"testing"
)

func TestUmaskConcurrentAndRestored(t *testing.T) {
	want := syscall.Umask(0o022)
	defer syscall.Umask(want)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); umask() }()
	}
	wg.Wait()
	cur := syscall.Umask(0o022)
	if cur != 0o022 {
		t.Fatalf("process umask left at %o", cur)
	}
}
