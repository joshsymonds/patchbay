package proxy

import (
	"bytes"
	"time"
)

// captureLimitBytes caps how much of a response the tee holds in memory: 64
// MiB. A response bigger than that is not a transcript worth keeping, and it is
// certainly not worth an out-of-memory kill of the gateway.
const captureLimitBytes = 64 << 20

// sseObservationBufferBytes caps the incomplete SSE event kept while looking
// for the first content delta. A single event is small, and dropping an
// unusually large incomplete event keeps observation from doubling capture
// memory.
const sseObservationBufferBytes = 64 << 10

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
	// deliberate size-ceiling case: the two have different causes and have
	// different outcomes in accounting.
	truncated bool

	// firstContentAt is the time the first text or thinking content delta was
	// received. It remains zero when the response is not an SSE stream or has no
	// such delta.
	firstContentAt time.Time
	observeSSE     bool
	sseBuffer      []byte
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

// observeChunk records the first content delta after a complete SSE event has
// arrived. The observation is separate from capture storage so response bytes
// remain untouched and arbitrary read boundaries are harmless.
func (c *capture) observeChunk(chunk []byte, receivedAt time.Time) {
	if !c.observeSSE || !c.firstContentAt.IsZero() {
		return
	}

	c.sseBuffer = append(c.sseBuffer, chunk...)
	if len(c.sseBuffer) > sseObservationBufferBytes {
		c.sseBuffer = c.sseBuffer[len(c.sseBuffer)-sseObservationBufferBytes:]
	}
	c.sseBuffer = bytes.ReplaceAll(c.sseBuffer, []byte("\r\n"), []byte("\n"))

	for {
		separator := bytes.Index(c.sseBuffer, []byte("\n\n"))
		if separator < 0 {
			return
		}
		event := c.sseBuffer[:separator]
		c.sseBuffer = c.sseBuffer[separator+2:]
		if isContentDeltaEvent(event) {
			c.firstContentAt = receivedAt
			return
		}
	}
}

func isContentDeltaEvent(event []byte) bool {
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		payload, ok := bytes.CutPrefix(line, []byte(sseDataPrefix))
		if ok {
			data = append(data, bytes.TrimSpace(payload)...)
		}
	}
	if len(data) == 0 {
		return false
	}

	fields, ok := decodeObject(data)
	if !ok {
		return false
	}
	rawType, found := fields["type"]
	if !found {
		return false
	}
	eventType, ok := decodeString(rawType)
	if !ok || eventType != "content_block_delta" {
		return false
	}
	rawDelta, found := fields["delta"]
	if !found {
		return false
	}
	delta, ok := decodeObject(rawDelta)
	if !ok {
		return false
	}
	rawDeltaType, found := delta["type"]
	if !found {
		return false
	}
	deltaType, ok := decodeString(rawDeltaType)
	return ok && (deltaType == "text_delta" || deltaType == "thinking_delta")
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
