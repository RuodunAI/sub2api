package awseventstream

import (
	"bytes"
	"encoding/json"
	"io"
)

// SSEReader presents an event stream whose events carry a model's native
// stream events - the shape of Bedrock's InvokeModelWithResponseStream - as
// server-sent events:
//
//   - an event message with the payload {"bytes":"<base64>"} becomes
//     "event: <type>\ndata: <json>\n\n", where json is the decoded bytes and
//     type its "type" member (without one only the data line is written);
//   - an exception or error message becomes
//     "event: error\ndata: {"type":"error","error":{"type":…,"message":…}}\n\n"
//     and ends the stream;
//   - any other message, and an event without bytes, is skipped;
//   - a corrupt or truncated message fails the read: the stream was cut.
//
// Each Read returns at most the events of one message, as soon as that
// message is complete: nothing waits for a buffer to fill.
type SSEReader struct {
	dec     *Decoder
	pending []byte
	buf     bytes.Buffer
	err     error
}

// NewSSEReader returns a reader of the SSE form of the event stream r.
// Closing and cancelling stay with r: an error of r is returned as it is.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{dec: NewDecoder(r)}
}

// Read implements io.Reader.
func (s *SSEReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		s.next()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// next decodes one message into pending, or records why there is none.
func (s *SSEReader) next() {
	msg, err := s.dec.Decode()
	if err != nil {
		s.err = err
		return
	}
	s.buf.Reset()
	switch msg.Header(HeaderMessageType) {
	case MessageTypeEvent:
		writeEvent(&s.buf, msg.Payload)
	case MessageTypeException:
		writeError(&s.buf, msg.Header(HeaderExceptionType), exceptionMessage(msg.Payload))
		s.err = io.EOF
	case MessageTypeError:
		writeError(&s.buf, msg.Header(HeaderErrorCode), msg.Header(HeaderErrorMessage))
		s.err = io.EOF
	}
	s.pending = s.buf.Bytes()
}

// writeEvent writes the SSE event of an event message's payload; nothing for
// a payload without bytes.
func writeEvent(w *bytes.Buffer, payload []byte) {
	var chunk struct {
		Bytes []byte `json:"bytes"`
	}
	if json.Unmarshal(payload, &chunk) != nil || len(chunk.Bytes) == 0 {
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(chunk.Bytes, &event)
	writeSSE(w, event.Type, chunk.Bytes)
}

// writeError writes the terminal error event, in the shape of an Anthropic
// stream error.
func writeError(w *bytes.Buffer, typ, message string) {
	if typ == "" {
		typ = "upstream_error"
	}
	if message == "" {
		message = typ
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Type, body.Error.Type, body.Error.Message = "error", typ, message
	data, _ := json.Marshal(body)
	writeSSE(w, "error", data)
}

// exceptionMessage is the "message" member of an exception's JSON payload,
// or the payload itself when it is not that.
func exceptionMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &body) == nil && body.Message != "" {
		return body.Message
	}
	return string(bytes.TrimSpace(payload))
}

// writeSSE writes one event. A line break inside data would end the field,
// so JSON is compacted and anything else is split into one data line each.
func writeSSE(w *bytes.Buffer, event string, data []byte) {
	if event != "" && !bytes.ContainsAny([]byte(event), "\r\n") {
		w.WriteString("event: " + event + "\n")
	}
	if bytes.ContainsAny(data, "\r\n") {
		var compact bytes.Buffer
		if json.Compact(&compact, data) == nil {
			data = compact.Bytes()
		}
	}
	for _, line := range bytes.FieldsFunc(data, func(r rune) bool { return r == '\n' || r == '\r' }) {
		w.WriteString("data: ")
		w.Write(line)
		w.WriteByte('\n')
	}
	w.WriteByte('\n')
}
