package process

import "sync"

// ringBuffer keeps the most recent output of one program.
//
// Bounded, because a dev server left running overnight would otherwise be a
// process whose log is the whole of memory. The oldest bytes go first, which
// is the right end to lose: what a caller wants is what the program said a
// moment ago, and the beginning of a server's log is a banner.
//
// Offsets are counted in bytes written since the program started rather than
// as positions in the buffer, so a caller polling for new output has a cursor
// that stays meaningful after the buffer has wrapped. Asking for an offset
// that has already been overwritten is answered from the oldest byte still
// held, with a count of what was skipped — silently returning the wrong bytes
// is how a reader ends up looking at the middle of a line.
type ringBuffer struct {
	mu sync.Mutex

	data []byte
	// start is the offset of data[0] in the stream, so start+len(data) is the
	// total written.
	start int64
	limit int
}

func newRingBuffer(limit int) *ringBuffer {
	if limit <= 0 {
		limit = defaultBufferBytes
	}
	return &ringBuffer{limit: limit}
}

func (b *ringBuffer) Write(chunk []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, chunk...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = b.data[over:]
		b.start += int64(over)
	}
	return len(chunk), nil
}

// read answers with everything from offset onwards.
//
// It returns the offset to ask from next, and how many bytes between the one
// asked for and the first one returned had already been overwritten. A read
// from zero after the buffer has wrapped reports that loss too: a caller
// starting at the beginning is exactly the one who does not know the beginning
// is gone.
func (b *ringBuffer) read(offset int64) (output []byte, next int64, skipped int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	end := b.start + int64(len(b.data))
	if offset < 0 || offset < b.start {
		skipped = b.start - offset
		if offset < 0 {
			skipped = 0
		}
		offset = b.start
	}
	if offset > end {
		// Ahead of anything written, which means a caller kept a cursor from
		// a different process. Answered as "nothing new" rather than as an
		// error: the next poll after a restart is the common way to get here.
		return nil, end, skipped
	}

	from := int(offset - b.start)
	out := make([]byte, len(b.data)-from)
	copy(out, b.data[from:])
	return out, end, skipped
}

// dropped is how much has been lost to the limit.
func (b *ringBuffer) dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.start
}
