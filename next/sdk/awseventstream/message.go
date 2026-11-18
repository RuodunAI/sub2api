// Package awseventstream reads and writes the AWS event stream framing
// (application/vnd.amazon.eventstream): Decoder and AppendMessage know the
// framing only and fit any service using it. ChunkSSEReader is narrower: it
// presents one kind of event stream, the "chunk" events of Bedrock's
// InvokeModelWithResponseStream, as server-sent events (FormatChunk).
//
// Messages are decoded and encoded by the official library, aws-sdk-go-v2's
// aws/protocol/eventstream: the checksum of a message, its headers and their
// value types, and the bytes written are its work. Two things the library
// lets through are refused here (Decoder.Decode): it reads whatever length a
// prelude announces, so the package reads the prelude itself and checks the
// lengths against the format before handing it over; and it keeps the last of headers sharing a
// name, so a message repeating a header name is rejected after it.
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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// ContentType is the media type of a response framed as an event stream.
const ContentType = "application/vnd.amazon.eventstream"

// MaxMessageSize is the format's own limit on a message, 16 MiB: the
// largest message the encoder produces and the decoder accepts. A prelude
// announcing more is corrupt, not large.
const MaxMessageSize = 16 << 20

const (
	preludeSize = 12
	// minMessageSize is a message with neither headers nor payload.
	minMessageSize = preludeSize + 4
	// maxHeadersSize is the format's limit on the header block.
	maxHeadersSize = 128 << 10
	// maxHeaderNameSize and maxHeaderValueSize are the format's limits on
	// one header.
	maxHeaderNameSize  = 255
	maxHeaderValueSize = 1<<15 - 1
	// retainSize is the largest payload buffer kept from one message to the
	// next; the buffer of a larger message is released after it.
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

// Header is one message header. Only string values are kept: the values of
// the other types are decoded and left out. A name occurs once in a message:
// AWS never repeats one, and a decoded message that does is refused
// (ErrCorrupt), whatever the types of the values.
type Header struct {
	Name  string
	Value string
}

// Message is one decoded frame. Its headers have distinct names, so what
// Header returns for a name is that header's only value in the frame: a
// second ":message-type" cannot change how a message is routed.
type Message struct {
	Headers []Header
	Payload []byte
}

// Header returns the string value of the header called name, "" without one.
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
	sdk *eventstream.Decoder
	// buf is the payload buffer handed to the library from one message to
	// the next.
	buf []byte
}

// NewDecoder returns a decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 32<<10), sdk: eventstream.NewDecoder()}
}

// Decode returns the next message. Its payload is only valid until the next
// call. At a clean end of the stream it returns io.EOF; a stream that ends
// inside a message returns io.ErrUnexpectedEOF, a malformed message (one
// repeating a header name included) an error wrapping ErrCorrupt, and a
// failing reader that reader's error.
func (d *Decoder) Decode() (Message, error) {
	var prelude [preludeSize]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		return Message{}, err // io.EOF only when not a single byte was read
	}
	if err := d.checkPrelude(prelude); err != nil {
		return Message{}, err
	}
	if cap(d.buf) > retainSize {
		d.buf = nil
	}
	// The library gets the prelude back and reads the rest of the message
	// from the stream: exactly the length checked above. Its payload buffer
	// grows with the bytes that arrive, so a prelude announcing a large
	// message costs nothing until the message is really sent.
	src := &source{r: d.r}
	msg, err := d.sdk.Decode(io.MultiReader(bytes.NewReader(prelude[:]), src), d.buf)
	switch {
	case src.err == io.EOF:
		return Message{}, io.ErrUnexpectedEOF
	case src.err != nil:
		return Message{}, src.err
	case err != nil:
		// The stream delivered the whole message, so the message is at fault.
		return Message{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if msg.Payload != nil {
		d.buf = msg.Payload
	}
	if err := checkHeaders(msg.Headers, binary.BigEndian.Uint32(prelude[4:8])); err != nil {
		return Message{}, err
	}
	return Message{Headers: stringHeaders(msg.Headers), Payload: msg.Payload}, nil
}

// checkPrelude checks the lengths a prelude announces against the format,
// before anything is read for them. The library checks neither: it trusts
// the lengths once the prelude's checksum matches.
func (d *Decoder) checkPrelude(prelude [preludeSize]byte) error {
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	switch {
	case crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]):
		return fmt.Errorf("%w: prelude checksum mismatch", ErrCorrupt)
	case total < minMessageSize || total > MaxMessageSize:
		return fmt.Errorf("%w: message length %d outside %d..%d", ErrCorrupt, total, minMessageSize, MaxMessageSize)
	case headersLen > maxHeadersSize || headersLen > total-minMessageSize:
		return fmt.Errorf("%w: headers length %d in a message of %d", ErrCorrupt, headersLen, total)
	}
	return nil
}

// source remembers the error its reader ended with, which tells a message
// the stream cut short from a malformed one.
type source struct {
	r   io.Reader
	err error
}

func (s *source) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// checkHeaders refuses a header block of wireLen bytes the library decoded
// leniently into headers. The library keeps one header per name (the last
// value), so a block repeating a name comes back shorter than it was sent:
// writing the decoded headers out again and comparing the lengths finds a
// repeat of any value type, with the library's own knowledge of the types.
// The library also reads values longer than the format allows and refuses
// to write them; such a block is refused here too.
func checkHeaders(headers eventstream.Headers, wireLen uint32) error {
	var n byteCount
	switch err := eventstream.EncodeHeaders(&n, headers); {
	case err != nil:
		return fmt.Errorf("%w: a header is over the format's limits", ErrCorrupt)
	case uint32(n) != wireLen:
		return fmt.Errorf("%w: a header name is repeated", ErrCorrupt)
	}
	return nil
}

// byteCount counts the bytes written to it.
type byteCount int

func (n *byteCount) Write(p []byte) (int, error) {
	*n += byteCount(len(p))
	return len(p), nil
}

// stringHeaders are the headers with a string value.
func stringHeaders(headers eventstream.Headers) []Header {
	var out []Header
	for _, h := range headers {
		if v, ok := h.Value.(eventstream.StringValue); ok {
			out = append(out, Header{Name: h.Name, Value: string(v)})
		}
	}
	return out
}

// AppendMessage appends the encoding of a message with string headers, in
// the order given, to dst. It is the counterpart of Decoder for tests and
// mock upstreams. A message that cannot be encoded returns dst unchanged
// and an error.
func AppendMessage(dst []byte, headers []Header, payload []byte) ([]byte, error) {
	msg := eventstream.Message{Payload: payload}
	headersLen := 0
	for _, h := range headers {
		// The library's encoder accepts an empty name and bounds neither the
		// header block nor the message.
		if len(h.Name) == 0 || len(h.Name) > maxHeaderNameSize || len(h.Value) > maxHeaderValueSize {
			return dst, fmt.Errorf("awseventstream: header %q cannot be encoded", h.Name)
		}
		headersLen += 1 + len(h.Name) + 1 + 2 + len(h.Value)
		msg.Headers = append(msg.Headers, eventstream.Header{Name: h.Name, Value: eventstream.StringValue(h.Value)})
	}
	total := minMessageSize + headersLen + len(payload)
	if headersLen > maxHeadersSize || total > MaxMessageSize {
		return dst, fmt.Errorf("awseventstream: message of %d bytes is too large", total)
	}
	out := bytes.NewBuffer(dst)
	if err := eventstream.NewEncoder().Encode(out, msg); err != nil {
		return dst, fmt.Errorf("awseventstream: %w", err)
	}
	return out.Bytes(), nil
}
