// Package progress reports aggregate transfer progress on stderr.
package progress

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Tracker counts files and bytes across concurrent transfers.
type Tracker struct {
	label      string
	out        io.Writer
	tty        bool
	start      time.Time
	files      atomic.Int64
	totalFiles int64
	bytes      atomic.Int64
	totalBytes atomic.Int64
	stop       chan struct{}
	done       chan struct{}
}

// New starts a tracker. Progress lines are drawn only when stderr is a terminal.
func New(label string, totalFiles int, totalBytes int64) *Tracker {
	t := &Tracker{
		label:      label,
		out:        os.Stderr,
		tty:        isTerminal(os.Stderr),
		start:      time.Now(),
		totalFiles: int64(totalFiles),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	t.totalBytes.Store(totalBytes)
	go t.loop()
	return t
}

// AddTotal grows the expected byte count when sizes are learned lazily.
func (t *Tracker) AddTotal(n int64) { t.totalBytes.Add(n) }

// Add records n transferred bytes.
func (t *Tracker) Add(n int64) { t.bytes.Add(n) }

// FileDone records a completed (or skipped) file.
func (t *Tracker) FileDone() { t.files.Add(1) }

// Close stops drawing and prints a summary line.
func (t *Tracker) Close() {
	close(t.stop)
	<-t.done
	if t.totalFiles == 0 {
		return
	}
	fmt.Fprintf(t.out, "%s%s: %d/%d files, %s in %s\n", t.clear(), t.label,
		t.files.Load(), t.totalFiles, Bytes(t.bytes.Load()), time.Since(t.start).Round(time.Millisecond))
}

func (t *Tracker) clear() string {
	if t.tty {
		return "\r\x1b[K"
	}
	return ""
}

func (t *Tracker) loop() {
	defer close(t.done)
	if !t.tty {
		<-t.stop
		return
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			elapsed := time.Since(t.start).Seconds()
			rate := float64(t.bytes.Load()) / max(elapsed, 0.001)
			fmt.Fprintf(t.out, "\r\x1b[K%s: %d/%d files  %s / %s  %s/s",
				t.label, t.files.Load(), t.totalFiles,
				Bytes(t.bytes.Load()), Bytes(t.totalBytes.Load()), Bytes(int64(rate)))
		}
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Bytes formats a byte count in IEC units.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
