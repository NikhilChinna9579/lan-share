// Package protocol implements the wire format described in protocol/PROTOCOL.md.
//
// A frame is a 4-byte big-endian length followed by that many bytes of JSON.
// File contents are NOT framed; they are streamed raw between frames.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version sent in every hello message.
const Version = 1

// MaxFrameSize caps a single control message (PROTOCOL.md §4).
const MaxFrameSize = 1 << 20 // 1 MiB

// ErrFrameTooLarge is returned when a peer announces a frame over MaxFrameSize.
var ErrFrameTooLarge = errors.New("protocol: frame too large")

// Message is any control message. Type must always be set.
type Message struct {
	Type string `json:"type"`
}

// WriteFrame encodes msg as JSON and writes it as one length-prefixed frame.
func WriteFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("protocol: encode: %w", err)
	}
	if len(body) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	// One Write call for header+body keeps it to a single TLS record for small frames.
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame and returns its raw JSON plus its "type" field.
// The caller decodes the JSON into the concrete message struct for that type.
func ReadFrame(r io.Reader) (msgType string, raw []byte, err error) {
	var header [4]byte
	if _, err = io.ReadFull(r, header[:]); err != nil {
		return "", nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > MaxFrameSize {
		return "", nil, ErrFrameTooLarge
	}
	raw = make([]byte, n)
	if _, err = io.ReadFull(r, raw); err != nil {
		return "", nil, err
	}
	var m Message
	if err = json.Unmarshal(raw, &m); err != nil {
		return "", nil, fmt.Errorf("protocol: bad frame: %w", err)
	}
	if m.Type == "" {
		return "", nil, errors.New("protocol: bad frame: missing type")
	}
	return m.Type, raw, nil
}
