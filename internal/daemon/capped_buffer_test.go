package daemon

import (
	"strings"
	"sync"
	"testing"
)

// TestCappedBufferStopsAtLimit is the regression test for unbounded output capture: a chatty
// job grew the worker's memory without limit, and any result over gRPC's 4 MiB receive cap was
// rejected permanently while the worker retried it forever.
func TestCappedBufferStopsAtLimit(t *testing.T) {
	b := newCappedBuffer(100)

	n, err := b.Write([]byte(strings.Repeat("x", 250)))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The writer must see a full write; truncation is our policy, not the process's error.
	if n != 250 {
		t.Errorf("Write returned %d, want 250", n)
	}
	if got := b.Len(); got != 100 {
		t.Errorf("retained %d bytes, want 100", got)
	}
	if got := b.Dropped(); got != 150 {
		t.Errorf("dropped %d bytes, want 150", got)
	}
}

// TestCappedBufferAnnouncesTruncation confirms the loss is visible in the result rather than
// silent.
func TestCappedBufferAnnouncesTruncation(t *testing.T) {
	b := newCappedBuffer(10)
	b.Write([]byte(strings.Repeat("y", 50)))

	out := b.String()
	if !strings.Contains(out, "output truncated") {
		t.Errorf("String() = %q, want a truncation note", out)
	}
	if !strings.Contains(out, "40 bytes dropped") {
		t.Errorf("String() = %q, want the dropped byte count", out)
	}
}

// TestCappedBufferUnderLimitIsExact confirms normal output is untouched.
func TestCappedBufferUnderLimitIsExact(t *testing.T) {
	b := newCappedBuffer(1000)
	b.Write([]byte("hello "))
	b.Write([]byte("world"))

	if got := b.String(); got != "hello world" {
		t.Errorf("String() = %q, want %q", got, "hello world")
	}
	if b.Dropped() != 0 {
		t.Errorf("dropped %d bytes, want 0", b.Dropped())
	}
}

// TestCappedBufferAcrossWrites confirms the limit applies cumulatively, not per write.
func TestCappedBufferAcrossWrites(t *testing.T) {
	b := newCappedBuffer(10)
	for i := 0; i < 5; i++ {
		b.Write([]byte("abcd"))
	}
	if got := b.Len(); got != 10 {
		t.Errorf("retained %d bytes, want 10", got)
	}
	if got := b.Dropped(); got != 10 {
		t.Errorf("dropped %d bytes, want 10", got)
	}
}

// TestCappedBufferIsConcurrencySafe runs under -race; exec can write from multiple goroutines.
func TestCappedBufferIsConcurrencySafe(t *testing.T) {
	b := newCappedBuffer(1 << 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Write([]byte("chunk"))
			}
		}()
	}
	wg.Wait()

	if got := b.Len(); got != 16*100*5 {
		t.Errorf("retained %d bytes, want %d", got, 16*100*5)
	}
}
