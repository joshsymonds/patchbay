package awssig

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The fixed widths of the vnd.amazon.eventstream framing: a prelude of two
// 32-bit lengths, a CRC over that prelude, and a CRC over the whole message.
// headBytes is everything before the first header byte; overheadBytes is what a
// message costs before a single header or payload byte.
const (
	preludeBytes  = 8
	checksumBytes = 4
	headBytes     = preludeBytes + checksumBytes
	overheadBytes = headBytes + checksumBytes
)

// stringValueType is the header value type this decoder understands. Bedrock's
// own headers — :event-type, :message-type, :content-type, :exception-type —
// are all strings; any other type is a message shape patchbay has never seen and
// will not guess at.
const stringValueType = 7

// The widths inside one header entry: a one-byte name length, a one-byte value
// type, and a two-byte value length.
const (
	nameLengthBytes  = 1
	valueTypeBytes   = 1
	valueLengthBytes = 2
)

// maxMessageBytes caps a single event-stream message at 16 MiB. The length
// comes off the wire, so without a ceiling an upstream — or a corrupted frame —
// could name an allocation of any size at all.
const maxMessageBytes = 16 << 20

// readBufferBytes sizes the buffered reader in front of the response body, so a
// stream that arrives in small chunks is not read a field at a time.
const readBufferBytes = 32768

// Message is one decoded event-stream message: its string headers, keyed by
// name, and the raw payload that followed them.
type Message struct {
	Headers map[string]string
	Payload []byte
}

// Decoder reads event-stream messages from a stream. It is not safe for
// concurrent use: one decoder belongs to one response body.
type Decoder struct {
	reader *bufio.Reader
}

// NewDecoder returns a Decoder reading messages from reader.
func NewDecoder(reader io.Reader) *Decoder {
	return &Decoder{reader: bufio.NewReaderSize(reader, readBufferBytes)}
}

// Next returns the next message, io.EOF at the clean end of the stream, or an
// error describing the corruption that stopped it. A stream that ends mid-frame
// is an error, not an end: the last message was never whole.
func (d *Decoder) Next() (Message, error) {
	head := make([]byte, headBytes)

	if _, err := io.ReadFull(d.reader, head); err != nil {
		if errors.Is(err, io.EOF) {
			return Message{}, io.EOF
		}

		return Message{}, fmt.Errorf("read event-stream prelude: %w", err)
	}

	if crc32.ChecksumIEEE(head[:preludeBytes]) != binary.BigEndian.Uint32(head[preludeBytes:]) {
		return Message{}, errors.New("event-stream prelude checksum does not match")
	}

	total := binary.BigEndian.Uint32(head[:checksumBytes])
	headersLength := binary.BigEndian.Uint32(head[checksumBytes:preludeBytes])

	if total < overheadBytes || total > maxMessageBytes {
		return Message{}, fmt.Errorf("event-stream message length %d is out of range", total)
	}

	if uint64(headersLength)+overheadBytes > uint64(total) {
		return Message{}, fmt.Errorf("event-stream headers length %d overruns the message", headersLength)
	}

	rest := make([]byte, total-headBytes)
	if _, err := io.ReadFull(d.reader, rest); err != nil {
		return Message{}, fmt.Errorf("read event-stream message: %w", err)
	}

	body := rest[:len(rest)-checksumBytes]

	sum := crc32.Update(crc32.ChecksumIEEE(head), crc32.IEEETable, body)
	if sum != binary.BigEndian.Uint32(rest[len(rest)-checksumBytes:]) {
		return Message{}, errors.New("event-stream message checksum does not match")
	}

	headers, err := parseHeaders(body[:headersLength])
	if err != nil {
		return Message{}, err
	}

	return Message{Headers: headers, Payload: body[headersLength:]}, nil
}

// parseHeaders decodes the header block: a run of name, type and value entries,
// every one of which must fit inside the block it claims to live in.
func parseHeaders(block []byte) (map[string]string, error) {
	headers := make(map[string]string)

	for at := 0; at < len(block); {
		name, valueType, next, err := readHeaderName(block, at)
		if err != nil {
			return nil, err
		}

		if valueType != stringValueType {
			return nil, fmt.Errorf("event-stream header %q has unsupported value type %d", name, valueType)
		}

		value, after, err := readHeaderValue(block, next)
		if err != nil {
			return nil, err
		}

		headers[name] = value
		at = after
	}

	return headers, nil
}

// readHeaderName reads one header's name and value type, returning the offset
// of the value that follows.
func readHeaderName(block []byte, at int) (string, byte, int, error) {
	if at+nameLengthBytes > len(block) {
		return "", 0, 0, errors.New("event-stream header block is truncated")
	}

	length := int(block[at])
	at += nameLengthBytes

	if at+length+valueTypeBytes > len(block) {
		return "", 0, 0, errors.New("event-stream header name is truncated")
	}

	name := string(block[at : at+length])
	at += length

	return name, block[at], at + valueTypeBytes, nil
}

// readHeaderValue reads one string header value, returning the offset the next
// header starts at.
func readHeaderValue(block []byte, at int) (string, int, error) {
	if at+valueLengthBytes > len(block) {
		return "", 0, errors.New("event-stream header value length is truncated")
	}

	length := int(binary.BigEndian.Uint16(block[at : at+valueLengthBytes]))
	at += valueLengthBytes

	if at+length > len(block) {
		return "", 0, errors.New("event-stream header value is truncated")
	}

	return string(block[at : at+length]), at + length, nil
}
