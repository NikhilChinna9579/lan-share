// Package server accepts connections from paired devices: pairing, ping, folder
// lists and incoming files (PROTOCOL.md §3–§5).
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/trust"
)

const (
	idleTimeout     = 15 * time.Second
	pairingLifetime = 2 * time.Minute
	maxPairAttempts = 5
)

// EventKind says what happened, for the UI to show.
type EventKind int

const (
	EventPaired EventKind = iota
	EventReceived
	EventFailed
)

// Event is reported to the UI through Server.OnEvent.
type Event struct {
	Kind     EventKind
	DeviceID string        // peer's key fingerprint
	Device   string        // peer's friendly name
	PeerIP   string        // EventPaired: the address the peer connected from
	Path     string        // EventReceived: where the file was saved
	Bytes    int64         // EventReceived: file size
	Duration time.Duration // EventReceived: time spent receiving
	Err      error         // EventFailed
}

// Server holds everything needed to answer peers.
type Server struct {
	Name     string
	Identity *identity.Identity
	Trust    *trust.Store
	Folders  *files.Folders
	OnEvent  func(Event)

	mu         sync.Mutex
	pairCode   string
	pairExpiry time.Time
	pairFails  int
}

// StartPairing opens a 2-minute pairing window and returns its one-time code.
func (s *Server) StartPairing() (code string, expires time.Time, err error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairCode = fmt.Sprintf("%06d", n.Int64())
	s.pairExpiry = time.Now().Add(pairingLifetime)
	s.pairFails = 0
	return s.pairCode, s.pairExpiry, nil
}

func (s *Server) pairingOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairCode != "" && time.Now().Before(s.pairExpiry)
}

// TLSConfig is mutual TLS 1.3 with our own trust check instead of CA chains.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.Identity.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			id, err := fingerprintRaw(raw)
			if err != nil {
				return err
			}
			if _, ok := s.Trust.Get(id); ok || s.pairingOpen() {
				return nil
			}
			return errors.New("not_trusted: device is not paired")
		},
	}
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	tl := tls.NewListener(ln, s.TLSConfig())
	for {
		c, err := tl.Accept()
		if err != nil {
			return err
		}
		go s.handle(c.(*tls.Conn))
	}
}

func (s *Server) handle(c *tls.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(idleTimeout))
	if err := c.Handshake(); err != nil {
		return // untrusted or broken; nothing to say
	}
	peerID, _ := fingerprintRaw(rawCerts(c))
	peer, trusted := s.Trust.Get(peerID)

	conn := &idleConn{c}
	typ, raw, err := protocol.ReadFrame(conn)
	if err != nil || typ != "hello" {
		return
	}
	var hello protocol.Hello
	if json.Unmarshal(raw, &hello) != nil {
		return
	}
	if hello.V != protocol.Version {
		protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "version", Message: "unsupported protocol version"})
		return
	}
	if err := protocol.WriteFrame(conn, protocol.Hello{Type: "hello", V: protocol.Version, Name: s.Name, Platform: "windows"}); err != nil {
		return
	}

	for {
		typ, raw, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		if !trusted && typ != "pair_request" {
			protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "not_trusted"})
			return
		}
		switch typ {
		case "pair_request":
			var req protocol.PairRequest
			json.Unmarshal(raw, &req)
			if s.pair(conn, peerID, hostOf(c.RemoteAddr()), req) {
				trusted, peer = true, trust.Device{ID: peerID, Name: req.Name}
			}
		case "ping":
			err = protocol.WriteFrame(conn, protocol.Message{Type: "pong"})
		case "folders_request":
			err = s.sendFolders(conn)
		case "file_offer":
			var offer protocol.FileOffer
			if json.Unmarshal(raw, &offer) != nil {
				return
			}
			err = s.receive(conn, peer.Name, offer)
		default:
			err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "unknown_type"})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.emit(Event{Kind: EventFailed, Device: peer.Name, Err: err})
			}
			return
		}
	}
}

// pair checks the one-time code and, if it matches, trusts the peer.
func (s *Server) pair(w io.Writer, peerID, peerIP string, req protocol.PairRequest) bool {
	s.mu.Lock()
	reason := ""
	switch {
	case s.pairCode == "":
		reason = "not_pairing"
	case time.Now().After(s.pairExpiry):
		reason = "expired"
	case subtle.ConstantTimeCompare([]byte(req.Code), []byte(s.pairCode)) != 1:
		reason = "bad_code"
		s.pairFails++
		if s.pairFails >= maxPairAttempts {
			s.pairCode = "" // too many guesses: close the window
		}
	default:
		s.pairCode = "" // one use only
	}
	s.mu.Unlock()

	if reason != "" {
		protocol.WriteFrame(w, protocol.PairFail{Type: "pair_fail", Reason: reason})
		return false
	}
	name := req.Name
	if name == "" {
		name = "Phone"
	}
	if err := s.Trust.Add(trust.Device{ID: peerID, Name: name, PairedAt: time.Now()}); err != nil {
		protocol.WriteFrame(w, protocol.Error{Type: "error", Code: "internal"})
		return false
	}
	protocol.WriteFrame(w, protocol.PairOK{Type: "pair_ok", Name: s.Name})
	s.emit(Event{Kind: EventPaired, DeviceID: peerID, Device: name, PeerIP: peerIP})
	return true
}

func (s *Server) sendFolders(w io.Writer) error {
	keys, labels := s.Folders.List()
	resp := protocol.FoldersResponse{Type: "folders_response", Default: s.Folders.Default}
	for i := range keys {
		resp.Folders = append(resp.Folders, protocol.Folder{Key: keys[i], Label: labels[i]})
	}
	return protocol.WriteFrame(w, resp)
}

// receive implements the transfer rules of PROTOCOL.md §5.
func (s *Server) receive(conn io.ReadWriter, from string, offer protocol.FileOffer) error {
	reject := func(reason string) error {
		return protocol.WriteFrame(conn, protocol.FileReject{Type: "file_reject", TransferID: offer.TransferID, Reason: reason})
	}
	dir, ok := s.Folders.Path(offer.Folder)
	if !ok {
		return reject("bad_folder")
	}
	names := make([]string, len(offer.Files))
	for i, f := range offer.Files {
		n, err := files.SanitizeName(f.Name)
		if err != nil || f.Size < 0 {
			return reject("bad_name")
		}
		names[i] = n
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return reject("bad_folder")
	}
	if err := protocol.WriteFrame(conn, protocol.FileAccept{Type: "file_accept", TransferID: offer.TransferID}); err != nil {
		return err
	}

	var parts []string // .part files still on disk; deleted if we fail
	defer func() {
		for _, p := range parts {
			os.Remove(p)
		}
	}()

	saved := make([]string, 0, len(offer.Files))
	for i, f := range offer.Files {
		start := time.Now()
		part := files.UniquePath(dir, names[i]+".part")
		out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "io", Message: err.Error()})
			return err
		}
		parts = append(parts, part)
		h := sha256.New()
		_, err = io.CopyN(io.MultiWriter(out, h), conn, f.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("receiving %s: %w", names[i], err)
		}

		typ, raw, err := protocol.ReadFrame(conn)
		if err != nil {
			return err
		}
		var end protocol.FileEnd
		if typ != "file_end" || json.Unmarshal(raw, &end) != nil || end.Index != i {
			protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "bad_frame"})
			return errors.New("expected file_end")
		}
		if end.SHA256 != hex.EncodeToString(h.Sum(nil)) {
			protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "checksum"})
			return fmt.Errorf("%s: checksum mismatch", names[i])
		}
		final := files.UniquePath(dir, names[i])
		if err := os.Rename(part, final); err != nil {
			return err
		}
		parts = parts[:len(parts)-1]
		saved = append(saved, filepath.Base(final))
		s.emit(Event{Kind: EventReceived, Device: from, Path: final, Bytes: f.Size, Duration: time.Since(start)})
	}
	return protocol.WriteFrame(conn, protocol.TransferDone{Type: "transfer_done", TransferID: offer.TransferID, Saved: saved})
}

func (s *Server) emit(e Event) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

func hostOf(a net.Addr) string {
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return ""
}

func rawCerts(c *tls.Conn) [][]byte {
	var out [][]byte
	for _, cert := range c.ConnectionState().PeerCertificates {
		out = append(out, cert.Raw)
	}
	return out
}

func fingerprintRaw(raw [][]byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("no client certificate")
	}
	cert, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return "", err
	}
	return identity.Fingerprint(cert), nil
}

// idleConn pushes the deadline forward on every read and write, so a transfer can
// take as long as it needs but a silent peer is dropped after idleTimeout.
type idleConn struct{ c net.Conn }

func (i *idleConn) Read(p []byte) (int, error) {
	i.c.SetDeadline(time.Now().Add(idleTimeout))
	return i.c.Read(p)
}

func (i *idleConn) Write(p []byte) (int, error) {
	i.c.SetDeadline(time.Now().Add(idleTimeout))
	return i.c.Write(p)
}
