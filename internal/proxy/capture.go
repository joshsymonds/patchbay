package proxy

// captureLimitBytes caps how much of a response the tee holds in memory: 64
// MiB. A response bigger than that is not a transcript worth keeping, and it is
// certainly not worth an out-of-memory kill of the gateway.
const captureLimitBytes = 64 << 20

// capture is the passive side of the tee: it accumulates a copy of the response
// as it streams past, and gives up entirely rather than ever interfering with
// it. Past the ceiling the whole capture is dropped, so a record built from it
// reports nothing rather than reporting half a stream as if it were whole.
//
// A capture belongs to one request. It is written by the goroutine streaming
// the response and read afterwards by the goroutine building the record.
type capture struct {
	buf     []byte
	dropped bool

	// truncated marks a capture whose stream was cut short mid-flight — a client
	// write failure or a non-EOF upstream read error — so the record is not
	// summarized as a completed turn. It is distinct from dropped, which is the
	// deliberate size-ceiling case: the two have different causes and are logged
	// apart.
	truncated bool
}

// write copies one streamed chunk, or drops the capture if that would exceed
// the ceiling. It cannot fail: nothing here is allowed to stop the stream.
func (c *capture) write(chunk []byte) {
	if c.dropped {
		return
	}

	if len(c.buf)+len(chunk) > captureLimitBytes {
		c.dropped = true
		c.buf = nil

		return
	}

	c.buf = append(c.buf, chunk...)
}

// bytes returns what was captured, or nothing at all if the capture was
// dropped at the ceiling or truncated mid-stream. A truncated prefix is a
// partial turn, and summarizing it would record a completed turn that never
// finished, so it yields nothing just as a dropped capture does.
func (c *capture) bytes() []byte {
	if c == nil || c.dropped || c.truncated {
		return nil
	}

	return c.buf
}
