package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

type hello struct {
	Type     string `json:"type"`
	V        int    `json:"v"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := hello{Type: "hello", V: Version, Name: "Laptop", Platform: "windows"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	typ, raw, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "hello" {
		t.Fatalf("type = %q, want hello", typ)
	}
	var out hello
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestRejectsOversizedFrame(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrameSize+1)
	_, _, err := ReadFrame(bytes.NewReader(header[:]))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestRejectsMissingType(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, map[string]int{"v": 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected error for frame without type")
	}
}

func TestFramesBackToBack(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, Message{Type: "ping"})
	_ = WriteFrame(&buf, Message{Type: "pong"})
	for _, want := range []string{"ping", "pong"} {
		typ, _, err := ReadFrame(&buf)
		if err != nil || typ != want {
			t.Fatalf("got %q, %v; want %q", typ, err, want)
		}
	}
}
