package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// at returns a tracker holding the given counts, as the drawing loop would see
// it one second into a transfer.
func at(totalFiles int, totalBytes, got int64) (*Tracker, time.Time) {
	t := &Tracker{label: "pull", start: time.Unix(0, 0), totalFiles: int64(totalFiles)}
	t.lastAt = t.start
	t.totalBytes.Store(totalBytes)
	t.bytes.Store(got)
	return t, t.start.Add(time.Second)
}

func TestRenderBar(t *testing.T) {
	tr, now := at(4, 1000, 250)
	tr.files.Store(1)
	line := tr.render(now, 100)

	for _, want := range []string{"pull", " 25%", "1/4 files", "[", "]", "█", "░"} {
		if !strings.Contains(line, want) {
			t.Errorf("render() = %q, want it to contain %q", line, want)
		}
	}
	full := strings.Count(line, "█")
	empty := strings.Count(line, "░")
	if bar := full + empty; bar == 0 || abs(full-bar/4) > 1 {
		t.Errorf("bar %d full / %d empty, want a quarter filled", full, empty)
	}
}

func TestRenderNarrowDropsBar(t *testing.T) {
	tr, now := at(4, 1000, 250)
	line := tr.render(now, 40)
	if strings.Contains(line, "█") || strings.Contains(line, "░") {
		t.Errorf("render() = %q, want no bar at 40 columns", line)
	}
	if len([]rune(line)) > 40 {
		t.Errorf("render() = %q, %d runes, want <= 40", line, len([]rune(line)))
	}
}

func TestRenderUnknownTotal(t *testing.T) {
	tr, now := at(4, 0, 2048)
	line := tr.render(now, 100)
	if strings.Contains(line, "%") || strings.Contains(line, "ETA") {
		t.Errorf("render() = %q, want no percent or ETA without a total", line)
	}
	if !strings.Contains(line, "2.0 KiB") {
		t.Errorf("render() = %q, want the transferred byte count", line)
	}
}

func TestRenderETA(t *testing.T) {
	// 1 MiB/s with 3 MiB left.
	tr, now := at(1, 4<<20, 1<<20)
	if got := tr.render(now, 120); !strings.Contains(got, "ETA 3s") {
		t.Errorf("render() = %q, want ETA 3s", got)
	}
}

func TestRemaining(t *testing.T) {
	cases := []struct {
		left int64
		rate float64
		want string
	}{
		{0, 100, ""},
		{100, 0, ""},
		{150, 100, "1s"},
		{9000, 100, "1m30s"},
		{1 << 40, 1, ""}, // too far out to be useful
	}
	for _, c := range cases {
		if got := remaining(c.left, c.rate); got != c.want {
			t.Errorf("remaining(%d, %v) = %q, want %q", c.left, c.rate, got, c.want)
		}
	}
}

func TestBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1 << 30: "1.0 GiB"}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// The bar must not grow and shrink as the counters beside it change width.
func TestRenderStableWidth(t *testing.T) {
	var first int
	for i, got := range []int64{0, 1 << 10, 150 << 20, 1 << 30, 2 << 30} {
		tr, now := at(40, 2<<30, got)
		tr.files.Store(int64(i))
		tr.rate = float64(150 << 20)
		n := len([]rune(tr.render(now, 100)))
		if i == 0 {
			first = n
			continue
		}
		if n != first {
			t.Errorf("render at %s is %d runes, want %d", Bytes(got), n, first)
		}
	}
}

// A disabled tracker accepts every call but writes nothing.
func TestDisabledTrackerIsSilent(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker("pull", 2, 100, false)
	tr.out = &buf
	go tr.loop()
	tr.Add(50)
	tr.FileDone()
	tr.Close()
	if buf.Len() != 0 {
		t.Errorf("disabled tracker wrote %q, want nothing", buf.String())
	}
}

// An enabled tracker still prints its summary when stderr is not a terminal.
func TestEnabledTrackerPrintsSummary(t *testing.T) {
	var buf bytes.Buffer
	tr := newTracker("push", 2, 100, true)
	tr.out, tr.tty = &buf, false
	go tr.loop()
	tr.Add(100)
	tr.FileDone()
	tr.FileDone()
	tr.Close()
	if got := buf.String(); !strings.Contains(got, "push: 2/2 files, 100 B in") {
		t.Errorf("summary = %q, want the push totals", got)
	}
}
