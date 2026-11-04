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
	EventProgress // while receiving: Bytes so far of Total, file Name
)

// Event is reported to the UI through Server.OnEvent.
type Event struct {
	Kind     EventKind
	DeviceID string        // peer's key fingerprint
	Device   string        // peer's friendly name
	PeerIP   string        // EventPaired: the address the peer connected from
	Path     string        // EventReceived: where the file was saved
	Folder   string        // EventReceived: shared folder key
	Bytes    int64         // EventReceived: file size
	Duration time.Duration // EventReceived: time spent receiving
	Err      error         // EventFailed
	Transfer string        // transfer id (EventProgress, EventReceived)
	Name     string        // EventProgress: file being received
	Total    int64         // EventProgress: bytes in the whole transfer
	Done     bool          // EventReceived: last file of the transfer
	Index    int           // EventProgress: file number, 0-based
	Count    int           // EventProgress: files in the transfer
}

// Server holds everything needed to answer peers.
type Server struct {
	Name     string
	Identity *identity.Identity
	Trust    *trust.Store
	Folders  *files.Folders
	OnEvent  func(Event)
	// ReportReach, if set, is sent in every pong: whether we can connect back to
	// the pinging device. The phone simulator uses it to imitate a blocked firewall.
	ReportReach *bool
	// Throttle (testing) limits how fast files are received, in bytes per second.
	Throttle int64

	mu         sync.Mutex
	pairCode   string
	pairExpiry time.Time
	pairFails  int
	contacts   map[string]time.Time
	active     map[string]net.Conn // transfer id → its connection, while receiving
}

func (s *Server) track(id string, c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = map[string]net.Conn{}
	}
	if c == nil {
		delete(s.active, id)
	} else {
		s.active[id] = c
	}
}

// Cancel stops receiving a transfer: the connection is closed, and the partly
// received files are deleted (as for any interrupted transfer).
func (s *Server) Cancel(transferID string) {
	s.mu.Lock()
	c := s.active[transferID]
	s.mu.Unlock()
	if c != nil {
		if tc, ok := c.(*tls.Conn); ok {
			if tcp, ok := tc.NetConn().(*net.TCPConn); ok {
				tcp.SetLinger(0) // reset: the sender stops at once
			}
			tc.NetConn().Close()
			return
		}
		c.Close()
	}
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
	if trusted {
		s.noteContact(peerID)
	}

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
			if s.ReportReach != nil {
				err = protocol.WriteFrame(conn, map[string]any{"type": "pong", "reach": *s.ReportReach})
			} else {
				err = protocol.WriteFrame(conn, protocol.Message{Type: "pong"})
			}
		case "folders_request":
			err = s.sendFolders(conn)
		case "list_request":
			var req protocol.ListRequest
			json.Unmarshal(raw, &req)
			err = s.sendList(conn, req)
		case "file_offer":
			var offer protocol.FileOffer
			if json.Unmarshal(raw, &offer) != nil {
				return
			}
			s.track(offer.TransferID, c)
			err = s.receive(conn, peerID, peer.Name, offer)
			s.track(offer.TransferID, nil)
			if err != nil {
				s.emit(Event{Kind: EventFailed, DeviceID: peerID, Device: peer.Name, Err: err})
				return
			}
		default:
			err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "unknown_type"})
		}
		if err != nil {
			return // the peer hung up between messages: normal, nothing to report
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

// noteContact records that a paired device just reached us. If a phone shows online
// to us but never reaches us, something (usually Windows Firewall) blocks it.
func (s *Server) noteContact(id string) {
	s.mu.Lock()
	if s.contacts == nil {
		s.contacts = map[string]time.Time{}
	}
	s.contacts[id] = time.Now()
	s.mu.Unlock()
}

// LastContact says when a paired device last connected to us (zero if never this run).
func (s *Server) LastContact(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contacts[id]
}

func (s *Server) sendList(w io.Writer, req protocol.ListRequest) error {
	dirs, entries, err := s.Folders.ListDir(req.Folder, req.Path)
	if err != nil {
		return protocol.WriteFrame(w, protocol.Error{Type: "error", Code: "bad_folder", Message: "not a shared folder"})
	}
	resp := protocol.ListResponse{Type: "list_response", Folder: req.Folder, Path: req.Path, Dirs: dirs, Files: []protocol.ListFile{}}
	if resp.Dirs == nil {
		resp.Dirs = []string{}
	}
	for _, e := range entries {
		resp.Files = append(resp.Files, protocol.ListFile{Name: e.Name, Size: e.Size, Modified: e.Modified.UnixMilli()})
	}
	// Keep the reply under the frame limit on huge folders.
	for len(resp.Files) > 2000 {
		resp.Files = resp.Files[:2000]
	}
	return protocol.WriteFrame(w, resp)
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
func (s *Server) receive(conn io.ReadWriter, peerID, from string, offer protocol.FileOffer) error {
	reject := func(reason string) error {
		return protocol.WriteFrame(conn, protocol.FileReject{Type: "file_reject", TransferID: offer.TransferID, Reason: reason})
	}
	dir, err := s.Folders.Resolve(offer.Folder, offer.Path)
	if err != nil {
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
	var total, doneBefore int64
	for _, f := range offer.Files {
		total += f.Size
	}
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
		pw := &progressWriter{s: s, ev: Event{Kind: EventProgress, DeviceID: peerID, Device: from, Transfer: offer.TransferID,
			Name: names[i], Total: total, Index: i, Count: len(names)}, done: doneBefore}
		var src io.Reader = conn
		if s.Throttle > 0 {
			src = &slowReader{r: conn, rate: s.Throttle}
		}
		_, err = io.CopyN(io.MultiWriter(out, h, pw), src, f.Size)
		doneBefore += f.Size
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
		s.emit(Event{Kind: EventReceived, DeviceID: peerID, Device: from, Path: final, Bytes: f.Size, Duration: time.Since(start),
			Folder: offer.Folder, Transfer: offer.TransferID, Done: i == len(offer.Files)-1})
	}
	return protocol.WriteFrame(conn, protocol.TransferDone{Type: "transfer_done", TransferID: offer.TransferID, Saved: saved})
}

func (s *Server) emit(e Event) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

// slowReader (testing) reads at most rate bytes per second.
type slowReader struct {
	r    io.Reader
	rate int64
}

func (s *slowReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	time.Sleep(time.Duration(int64(n) * int64(time.Second) / s.rate))
	return n, err
}

// progressWriter reports receive progress at most ~6 times a second.
type progressWriter struct {
	s    *Server
	ev   Event
	done int64
	last time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) > 150*time.Millisecond {
		p.last = time.Now()
		ev := p.ev
		ev.Bytes = p.done
		p.s.emit(ev)
	}
	return len(b), nil
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
