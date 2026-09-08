package daemon

import (
	"bytes"
	"fmt"
	"sync"
)

// defaultMaxOutputBytes bounds captured job output when the config does not say otherwise.
// It sits under gRPC's 4 MiB default receive limit, with room for the rest of the message.
const defaultMaxOutputBytes = 3 << 20

// cappedBuffer collects output up to a limit and then records how much it dropped.
//
// Job output used to be captured into an unbounded bytes.Buffer. A job printing in a loop grew
// the worker's memory until it died, taking every co-tenant job with it; and any job that
// produced more than 4 MiB had its result permanently rejected by the master's gRPC receive
// limit, after which the worker retried that same oversized message forever.
//
// It is safe for concurrent use because exec wires the same writer to both the process's stdout
// and stderr pipes when they are distinct objects, and callers read it from another goroutine.
type cappedBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int
	dropped int
}

func newCappedBuffer(limit int) *cappedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &cappedBuffer{limit: limit}
}

// Write implements io.Writer. It always reports the full length as written: the process should
// not see a short write and fail, since truncation is our policy, not its error.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.dropped += len(p) - room
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Len returns the number of bytes retained.
func (b *cappedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Dropped returns the number of bytes discarded past the limit.
func (b *cappedBuffer) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// String returns the retained output, with a trailing note when anything was dropped so the
// truncation is visible to whoever reads the result.
func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropped == 0 {
		return b.buf.String()
	}
	return fmt.Sprintf("%s\n[tasch: output truncated, %d bytes dropped past the %d byte limit]",
		b.buf.String(), b.dropped, b.limit)
}
