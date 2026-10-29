// Package progress reports aggregate transfer progress on stderr.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

const (
	tick        = 100 * time.Millisecond
	minBarWidth = 8
	maxBarWidth = 40
	fallbackCol = 80
	byteField   = 10 // widest IEC rendering, e.g. "1024.0 KiB"
)

// Tracker counts files and bytes across concurrent transfers.
type Tracker struct {
	label      string
	off        bool
	out        io.Writer
	tty        bool
	width      func() int
	start      time.Time
	files      atomic.Int64
	totalFiles int64
	bytes      atomic.Int64
	totalBytes atomic.Int64
	stop       chan struct{}
	done       chan struct{}

	// Sampled by loop only, for the smoothed rate shown in the bar.
	lastAt    time.Time
	lastBytes int64
	rate      float64
}

// New starts a tracker. Progress lines are drawn only when enabled and stderr
// is a terminal; a disabled tracker counts nothing and prints nothing, so
// callers need no second code path.
func New(label string, totalFiles int, totalBytes int64, enabled bool) *Tracker {
	t := newTracker(label, totalFiles, totalBytes, enabled)
	go t.loop()
	return t
}

// newTracker builds a tracker without starting the drawing loop.
func newTracker(label string, totalFiles int, totalBytes int64, enabled bool) *Tracker {
	t := &Tracker{
		off:        !enabled,
		label:      label,
		out:        os.Stderr,
		tty:        enabled && term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("TERM") != "dumb",
		width:      stderrWidth,
		start:      time.Now(),
		totalFiles: int64(totalFiles),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	t.lastAt = t.start
	t.totalBytes.Store(totalBytes)
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
	if t.off || t.totalFiles == 0 {
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
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-tk.C:
			fmt.Fprintf(t.out, "\r\x1b[K%s", t.render(now, t.width()))
		}
	}
}

// sample updates the smoothed transfer rate from the bytes seen since the last
// call. Smoothing keeps the rate and ETA from jumping as parts land in bursts.
func (t *Tracker) sample(now time.Time, got int64) float64 {
	if d := now.Sub(t.lastAt).Seconds(); d > 0 {
		inst := float64(got-t.lastBytes) / d
		if t.rate == 0 {
			t.rate = inst
		} else {
			t.rate = 0.7*t.rate + 0.3*inst
		}
	}
	t.lastAt, t.lastBytes = now, got
	return t.rate
}

// render builds one progress line sized to cols columns. The bar is dropped
// when the terminal is too narrow for the counters it annotates.
func (t *Tracker) render(now time.Time, cols int) string {
	got, want := t.bytes.Load(), t.totalBytes.Load()
	rate := t.sample(now, got)

	var pct string
	var frac float64
	if want > 0 {
		frac = min(float64(got)/float64(want), 1)
		pct = fmt.Sprintf("%3.0f%%", frac*100)
	}

	// Every field is padded to its widest form so the bar beside it keeps one
	// width for the whole run instead of jittering as the counters grow.
	right := fmt.Sprintf("%*s", byteField, Bytes(got))
	if want > 0 {
		right += "/" + Bytes(want)
	}
	right += fmt.Sprintf("  %*s  %*d/%d files", byteField+2,
		Bytes(int64(rate))+"/s", digits(t.totalFiles), t.files.Load(), t.totalFiles)
	if want > 0 {
		right += fmt.Sprintf("  %-9s", etaField(want-got, rate))
	}

	head := t.label
	if pct != "" {
		head += " " + pct
	}
	line := head + "  " + right

	// Spend whatever room is left on the bar, between the two halves.
	bar := cols - len([]rune(line)) - 3 // brackets plus a separating space
	if bar > maxBarWidth {
		bar = maxBarWidth
	}
	if pct == "" || bar < minBarWidth {
		return truncate(line, cols)
	}
	full := int(frac*float64(bar) + 0.5)
	return fmt.Sprintf("%s [%s%s] %s", head,
		strings.Repeat("█", full), strings.Repeat("░", bar-full), right)
}

// etaField renders the ETA with its label, blank while the rate is unknown.
func etaField(left int64, rate float64) string {
	if s := remaining(left, rate); s != "" {
		return "ETA " + s
	}
	return ""
}

func digits(n int64) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// remaining formats the time left at the current rate, or "" when unknowable.
func remaining(left int64, rate float64) string {
	if left <= 0 || rate <= 0 {
		return ""
	}
	secs := float64(left) / rate
	if secs > 99*60*60 {
		return ""
	}
	d := time.Duration(secs) * time.Second
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func truncate(s string, cols int) string {
	r := []rune(s)
	if cols > 0 && len(r) > cols {
		return string(r[:cols])
	}
	return s
}

func stderrWidth() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return fallbackCol
	}
	return w
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
