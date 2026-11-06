package server

// End-to-end tests: a fake "phone" client talks to a real server over TLS on localhost.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

type env struct {
	srv   *Server
	addr  string
	phone *identity.Identity
	inbox string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(filepath.Join(dir, "laptop"), "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := identity.LoadOrCreate(filepath.Join(dir, "phone"), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := trust.Open(filepath.Join(dir, "laptop"))
	inbox := filepath.Join(dir, "Downloads")
	folders := files.NewFolders("downloads")
	folders.Add("downloads", "Downloads", inbox)
	srv := &Server{Name: "Laptop", Identity: id, Trust: ts, Folders: folders}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)
	return &env{srv: srv, addr: ln.Addr().String(), phone: phone, inbox: inbox}
}

// dial connects as the phone, pinning the laptop's fingerprint, and swaps hellos.
func (e *env) dial(t *testing.T) (*tls.Conn, error) {
	t.Helper()
	want := e.srv.Identity.Fingerprint
	c, err := tls.Dial("tcp", e.addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{e.phone.Cert},
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil || identity.Fingerprint(cert) != want {
				return errors.New("pin mismatch")
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.Close() })
	if err := protocol.WriteFrame(c, protocol.Hello{Type: "hello", V: 1, Name: "Phone", Platform: "android"}); err != nil {
		return nil, err
	}
	typ, _, err := protocol.ReadFrame(c)
	if err != nil {
		return nil, err
	}
	if typ != "hello" {
		return nil, errors.New("expected hello, got " + typ)
	}
	return c, nil
}

func (e *env) pair(t *testing.T, code string) string {
	t.Helper()
	c, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	protocol.WriteFrame(c, protocol.PairRequest{Type: "pair_request", Code: code, Name: "Nikhil's phone"})
	typ, _, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func TestUnpairedPhoneIsRejected(t *testing.T) {
	e := setup(t)
	c, err := e.dial(t)
	if err != nil {
		return // rejected during the handshake: good
	}
	// TLS 1.3 may report the client-cert rejection only on the first read.
	protocol.WriteFrame(c, protocol.Message{Type: "ping"})
	if typ, _, err := protocol.ReadFrame(c); err == nil && typ == "pong" {
		t.Fatal("unpaired device got a pong")
	}
}

func TestPairingNeedsTheRightCode(t *testing.T) {
	e := setup(t)
	code, _, _ := e.srv.StartPairing()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if got := e.pair(t, wrong); got != "pair_fail" {
		t.Fatalf("wrong code gave %s", got)
	}
	if got := e.pair(t, code); got != "pair_ok" {
		t.Fatalf("right code gave %s", got)
	}
	if _, ok := e.srv.Trust.Get(e.phone.Fingerprint); !ok {
		t.Fatal("phone not in trust store")
	}
	if got := e.pair(t, code); got == "pair_ok" {
		t.Fatal("code worked twice")
	}
}

func TestPhonePinsTheLaptop(t *testing.T) {
	real := setup(t)
	fake := setup(t) // a different laptop: what an impostor on the same Wi-Fi would be
	fake.srv.StartPairing()
	// Point the phone at the impostor while it still pins the real laptop's key.
	real.addr = fake.addr
	if _, err := real.dial(t); err == nil {
		t.Fatal("phone accepted an impostor laptop")
	}
}

func send(t *testing.T, c *tls.Conn, name string, data []byte, sum string) (string, []byte) {
	t.Helper()
	protocol.WriteFrame(c, protocol.FileOffer{Type: "file_offer", TransferID: "t1", Folder: "downloads",
		Files: []protocol.OfferFile{{Name: name, Size: int64(len(data))}}})
	typ, raw, err := protocol.ReadFrame(c)
	if err != nil || typ != "file_accept" {
		t.Fatalf("offer answer %s %s %v", typ, raw, err)
	}
	c.Write(data)
	if sum == "" {
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	protocol.WriteFrame(c, protocol.FileEnd{Type: "file_end", Index: 0, SHA256: sum})
	typ, raw, _ = protocol.ReadFrame(c)
	return typ, raw
}

func TestSendFile(t *testing.T) {
	e := setup(t)
	code, _, _ := e.srv.StartPairing()
	e.pair(t, code)

	data := make([]byte, 3<<20+17) // a bit over 3 MiB
	rand.Read(data)
	c, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	typ, raw := send(t, c, "../../sneaky/photo.jpg", data, "")
	if typ != "transfer_done" {
		t.Fatalf("got %s %s", typ, raw)
	}
	var done protocol.TransferDone
	json.Unmarshal(raw, &done)
	if len(done.Saved) != 1 || done.Saved[0] != "photo.jpg" {
		t.Fatalf("saved %v", done.Saved)
	}
	got, err := os.ReadFile(filepath.Join(e.inbox, "photo.jpg"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("file content differs")
	}
}

func TestBadChecksumLeavesNothing(t *testing.T) {
	e := setup(t)
	code, _, _ := e.srv.StartPairing()
	e.pair(t, code)
	c, err := e.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	typ, _ := send(t, c, "x.bin", []byte("hello"), "00")
	if typ != "error" {
		t.Fatalf("got %s, want error", typ)
	}
	c.Close()
	// The server deletes the .part right after replying; give it a moment.
	for i := 0; i < 50; i++ {
		entries, _ := os.ReadDir(e.inbox)
		if len(entries) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, _ := os.ReadDir(e.inbox)
	t.Fatalf("left behind %v", entries)
}
