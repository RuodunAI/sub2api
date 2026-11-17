// Package awseventstream reads and writes the AWS event stream framing
// (application/vnd.amazon.eventstream): Decoder and AppendMessage know the
// framing only and fit any service using it. ChunkSSEReader is narrower: it
// presents one kind of event stream, the "chunk" events of Bedrock's
// InvokeModelWithResponseStream, as server-sent events (FormatChunk). Only
// the standard library is used.
//
// A message on the wire:
//
//	total length    uint32  whole message, the two CRCs included
//	headers length  uint32
//	prelude CRC     uint32  CRC-32 (IEEE) of the 8 bytes before it
//	headers         headers-length bytes
//	payload         the rest
//	message CRC     uint32  CRC-32 (IEEE) of everything before it
//
// A header is a 1-byte name length, the name, a 1-byte value type and the
// value; a string value (type 7) carries a 2-byte length.
package awseventstream

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
)

// ContentType is the media type of a response framed as an event stream.
const ContentType = "application/vnd.amazon.eventstream"

// MaxMessageSize is the largest message the encoder produces and a decoder
// can be told to accept: the format's own limit of 16 MiB. It is also a
// decoder's limit unless WithMaxMessageSize lowers it.
const MaxMessageSize = 16 << 20

const (
	preludeSize = 12
	// minMessageSize is a message with neither headers nor payload.
	minMessageSize = preludeSize + 4
	// maxHeadersSize is the format's limit on the header block.
	maxHeadersSize = 128 << 10
	// readStep is how far ahead of the bytes received the message buffer
	// grows: a length announced in a prelude reserves nothing by itself.
	readStep = 64 << 10
	// retainSize is the largest buffer kept from one message to the next;
	// the buffer of a larger message is released after it.
	retainSize = 256 << 10
)

// Names and values of the headers that route a message.
const (
	HeaderMessageType   = ":message-type"
	HeaderEventType     = ":event-type"
	HeaderExceptionType = ":exception-type"
	HeaderErrorCode     = ":error-code"
	HeaderErrorMessage  = ":error-message"
	HeaderContentType   = ":content-type"

	MessageTypeEvent     = "event"
	MessageTypeException = "exception"
	MessageTypeError     = "error"
)

// ErrCorrupt is wrapped by every decoding error that means the stream cannot
// be trusted any further: a bad length, a checksum mismatch, a malformed
// header block.
var ErrCorrupt = errors.New("awseventstream: corrupt message")

// ErrTooLarge is wrapped by the error for a message the format allows but
// that is over the decoder's own limit (WithMaxMessageSize). The message is
// not read, so the stream cannot be decoded any further either.
var ErrTooLarge = errors.New("awseventstream: message over the size limit")

// Header is one message header. Only string values are kept: the decoder
// checks the values of the other types and leaves them out.
type Header struct {
	Name  string
	Value string
}

// Message is one decoded frame.
type Message struct {
	Headers []Header
	Payload []byte
}

// Header returns the value of the first header called name, "" without one.
func (m Message) Header(name string) string {
	for _, h := range m.Headers {
		if h.Name == name {
			return h.Value
		}
	}
	return ""
}

// Decoder reads messages from a stream. It asks the stream for no more than
// the message it is reading, so a message is returned as soon as its last
// byte arrived.
type Decoder struct {
	r   *bufio.Reader
	buf []byte
	max uint32
}

// DecoderOption adjusts a Decoder.
type DecoderOption func(*Decoder)

// WithMaxMessageSize makes the decoder refuse messages longer than n bytes
// (ErrTooLarge), for streams whose messages are known to be small. Values
// outside the format's range are brought into it.
func WithMaxMessageSize(n int) DecoderOption {
	return func(d *Decoder) { d.max = uint32(min(max(n, minMessageSize), MaxMessageSize)) }
}

// NewDecoder returns a decoder reading from r.
func NewDecoder(r io.Reader, opts ...DecoderOption) *Decoder {
	d := &Decoder{r: bufio.NewReaderSize(r, 32<<10), max: MaxMessageSize}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Decode returns the next message. Its payload is only valid until the next
// call. At a clean end of the stream it returns io.EOF; a stream that ends
// inside a message returns io.ErrUnexpectedEOF, a malformed message an error
// wrapping ErrCorrupt, a message over the decoder's limit an error wrapping
// ErrTooLarge, and a failing reader that reader's error.
func (d *Decoder) Decode() (Message, error) {
	var prelude [preludeSize]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		return Message{}, err // io.EOF only when not a single byte was read
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return Message{}, fmt.Errorf("%w: prelude checksum mismatch", ErrCorrupt)
	}
	// The lengths are bounded before anything is read or allocated for them.
	if total < minMessageSize || total > MaxMessageSize {
		return Message{}, fmt.Errorf("%w: message length %d outside %d..%d", ErrCorrupt, total, minMessageSize, MaxMessageSize)
	}
	if headersLen > maxHeadersSize || headersLen > total-minMessageSize {
		return Message{}, fmt.Errorf("%w: headers length %d in a message of %d", ErrCorrupt, headersLen, total)
	}
	if total > d.max {
		return Message{}, fmt.Errorf("%w: message length %d, limit %d", ErrTooLarge, total, d.max)
	}
	rest, err := d.read(int(total) - preludeSize)
	if err != nil {
		return Message{}, err
	}
	body, sum := rest[:len(rest)-4], binary.BigEndian.Uint32(rest[len(rest)-4:])
	if crc32.Update(crc32.ChecksumIEEE(prelude[:]), crc32.IEEETable, body) != sum {
		return Message{}, fmt.Errorf("%w: message checksum mismatch", ErrCorrupt)
	}
	headers, err := decodeHeaders(body[:headersLen])
	if err != nil {
		return Message{}, err
	}
	return Message{Headers: headers, Payload: body[headersLen:]}, nil
}

// read reads the n bytes after a prelude into the decoder's buffer. The
// buffer grows as the bytes arrive, readStep at a time, so a prelude
// announcing a large message costs nothing until the message is really sent;
// a buffer that grew past retainSize for one message is not kept for the next.
func (d *Decoder) read(n int) ([]byte, error) {
	if cap(d.buf) > retainSize {
		d.buf = nil
	}
	buf := d.buf[:0]
	for len(buf) < n {
		step := min(n-len(buf), readStep)
		buf = slices.Grow(buf, step)
		got, err := io.ReadFull(d.r, buf[len(buf):len(buf)+step])
		buf = buf[:len(buf)+got]
		if err != nil {
			d.buf = buf
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	d.buf = buf
	return buf, nil
}

// Value types of a header.
const (
	typeTrue = iota
	typeFalse
	typeByte
	typeInt16
	typeInt32
	typeInt64
	typeBytes
	typeString
	typeTimestamp
	typeUUID
)

// fixedValueSize is the size of the fixed-width value types.
var fixedValueSize = map[byte]int{typeTrue: 0, typeFalse: 0, typeByte: 1, typeInt16: 2, typeInt32: 4, typeInt64: 8, typeTimestamp: 8, typeUUID: 16}

func decodeHeaders(b []byte) ([]Header, error) {
	var out []Header
	for len(b) > 0 {
		name, rest, ok := cutPrefixed(b, 1)
		if !ok || len(name) == 0 || len(rest) == 0 {
			return nil, fmt.Errorf("%w: truncated header name", ErrCorrupt)
		}
		typ := rest[0]
		value, rest, ok := cutValue(typ, rest[1:])
		if !ok {
			return nil, fmt.Errorf("%w: bad value of header %q (type %d)", ErrCorrupt, name, typ)
		}
		if typ == typeString {
			out = append(out, Header{Name: string(name), Value: string(value)})
		}
		b = rest
	}
	return out, nil
}

// cutValue splits the value of a header of type typ off b.
func cutValue(typ byte, b []byte) (value, rest []byte, ok bool) {
	if typ == typeBytes || typ == typeString {
		return cutPrefixed(b, 2)
	}
	n, known := fixedValueSize[typ]
	if !known || len(b) < n {
		return nil, nil, false
	}
	return b[:n], b[n:], true
}

// cutPrefixed splits a field with a big-endian length prefix of width bytes
// (1 or 2) off b.
func cutPrefixed(b []byte, width int) (field, rest []byte, ok bool) {
	if len(b) < width {
		return nil, nil, false
	}
	n := int(b[0])
	if width == 2 {
		n = int(binary.BigEndian.Uint16(b))
	}
	b = b[width:]
	if len(b) < n {
		return nil, nil, false
	}
	return b[:n], b[n:], true
}

// AppendMessage appends the encoding of a message with string headers, in
// the order given, to dst. It is the counterpart of Decoder for tests and
// mock upstreams. A message that cannot be encoded returns dst unchanged
// and an error.
func AppendMessage(dst []byte, headers []Header, payload []byte) ([]byte, error) {
	headersLen := 0
	for _, h := range headers {
		if len(h.Name) == 0 || len(h.Name) > 255 || len(h.Value) > 0x7fff {
			return dst, fmt.Errorf("awseventstream: header %q cannot be encoded", h.Name)
		}
		headersLen += 1 + len(h.Name) + 1 + 2 + len(h.Value)
	}
	total := minMessageSize + headersLen + len(payload)
	if headersLen > maxHeadersSize || total > MaxMessageSize {
		return dst, fmt.Errorf("awseventstream: message of %d bytes is too large", total)
	}
	start := len(dst)
	dst = binary.BigEndian.AppendUint32(dst, uint32(total))
	dst = binary.BigEndian.AppendUint32(dst, uint32(headersLen))
	dst = binary.BigEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start:]))
	for _, h := range headers {
		dst = append(dst, byte(len(h.Name)))
		dst = append(dst, h.Name...)
		dst = append(dst, typeString)
		dst = binary.BigEndian.AppendUint16(dst, uint16(len(h.Value)))
		dst = append(dst, h.Value...)
	}
	dst = append(dst, payload...)
	return binary.BigEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start:])), nil
}
