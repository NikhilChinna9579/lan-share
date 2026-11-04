// Package server accepts connections from paired devices: pairing, ping, folder
// lists and incoming files (PROTOCOL.md §3–§5).
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/NikhilChinna9579/lan-share/windows/internal/transfer"
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
	EventSending  // while a device fetches from us: Bytes so far of Total
	EventSent     // a device fetched files from us: Count files, Bytes total
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
	Platform string // in our hello; "" = "windows"
	Identity *identity.Identity
	Trust    *trust.Store
	Folders  *files.Folders
	OnEvent  func(Event)
	// ReportReach, if set, is sent in every pong: whether we can connect back to
	// the pinging device. The phone simulator uses it to imitate a blocked firewall.
	ReportReach *bool
	// Throttle (testing) limits how fast files are received, in bytes per second.
	Throttle int64
	// PartialDir, if set, keeps big unfinished files (in the app's own folder) so the
	// sender's retry continues where it stopped.
	PartialDir string
	// AllowBrowse, if set and returning false, refuses listing and fetching files
	// (a paired device can still send here).
	AllowBrowse func() bool
	// Approve, if set, is asked before accepting files and may block until the person
	// answers (the sender waits up to a minute). false declines them.
	Approve func(peerID, from string, offer protocol.FileOffer) bool

	mu         sync.Mutex
	pairCode   string
	pairExpiry time.Time
	pairFails  int
	contacts   map[string]time.Time
	active     map[string]net.Conn // transfer id → its connection, while receiving
	cancelled  map[string]bool     // transfers this side cancelled (their partial files go)
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
	if s.cancelled == nil {
		s.cancelled = map[string]bool{}
	}
	s.cancelled[transferID] = true
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
	if err := protocol.WriteFrame(conn, protocol.Hello{Type: "hello", V: protocol.Version, Name: s.Name, Platform: s.platform()}); err != nil {
		return
	}
	if trusted && hello.Name != "" && hello.Name != peer.Name { // renamed on that device
		s.Trust.SetName(peerID, hello.Name)
		peer.Name = hello.Name
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
			if !s.browsable() {
				err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "not_allowed", Message: "browsing is turned off on " + s.Name})
				break
			}
			err = s.sendList(conn, req)
		case "search_request":
			var req protocol.SearchRequest
			json.Unmarshal(raw, &req)
			if !s.browsable() {
				err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "not_allowed", Message: "browsing is turned off on " + s.Name})
				break
			}
			resp := protocol.SearchResponse{Type: "search_response", Results: []protocol.SearchHit{}}
			for _, h := range s.Folders.Search(req.Query, 200) {
				resp.Results = append(resp.Results, protocol.SearchHit{Folder: h.Folder, Path: h.Path, Name: h.Name, Size: h.Size, Modified: h.Modified.UnixMilli()})
			}
			err = protocol.WriteFrame(conn, resp)
		case "mkdir_request":
			var req protocol.MkdirRequest
			json.Unmarshal(raw, &req)
			name, merr := s.Folders.Mkdir(req.Folder, req.Path, req.Name)
			if merr != nil {
				err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "bad_folder", Message: merr.Error()})
			} else {
				err = protocol.WriteFrame(conn, protocol.MkdirOK{Type: "mkdir_ok", Name: name})
			}
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
		case "fetch_request":
			var req protocol.FetchRequest
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			if !s.browsable() {
				err = protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: "not_allowed", Message: "browsing is turned off on " + s.Name})
				break
			}
			s.track(req.TransferID, c)
			err = s.serveFetch(conn, peerID, peer.Name, req)
			s.track(req.TransferID, nil)
			if err != nil {
				s.emit(Event{Kind: EventFailed, DeviceID: peerID, Device: peer.Name, Transfer: req.TransferID, Err: err})
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
	sizes := make([]int64, len(offer.Files))
	for i, f := range offer.Files {
		n, err := files.SanitizeName(f.Name)
		if err != nil || f.Size < 0 {
			return reject("bad_name")
		}
		names[i], sizes[i] = n, f.Size
	}
	if s.Approve != nil && !s.Approve(peerID, from, offer) {
		return reject("declined")
	}
	dirs := make([]string, len(offer.Files))
	for i, f := range offer.Files {
		if dirs[i], err = files.SubDir(dir, f.Path); err != nil {
			return reject("bad_name")
		}
	}
	// Big files from an earlier, interrupted try continue where they stopped.
	resume := make([]transfer.Resume, len(names))
	accept := protocol.FileAccept{Type: "file_accept", TransferID: offer.TransferID}
	resuming := false
	for i, f := range offer.Files {
		if s.PartialDir == "" || f.Size < transfer.MinResume {
			continue
		}
		p := filepath.Join(s.PartialDir, transfer.ResumeKey(peerID, offer.Folder, offer.Path, f.Path, names[i], f.Size))
		resume[i].Path = p
		if st, err := os.Stat(p); err == nil && st.Size() < f.Size {
			resume[i].Offset = st.Size()
			resuming = resuming || st.Size() > 0
		}
	}
	if resuming {
		accept.Offsets = make([]int64, len(names))
		for i := range resume {
			accept.Offsets[i] = resume[i].Offset
		}
	}
	if err := protocol.WriteFrame(conn, accept); err != nil {
		return err
	}
	var src io.Reader = conn
	if s.Throttle > 0 {
		src = &slowReader{r: conn, rate: s.Throttle}
	}
	defer func() {
		s.mu.Lock()
		gone := s.cancelled[offer.TransferID]
		delete(s.cancelled, offer.TransferID)
		s.mu.Unlock()
		if gone { // cancelled here: nothing is kept
			for _, r := range resume {
				if r.Path != "" {
					os.Remove(r.Path)
				}
			}
		}
	}()
	saved, err := transfer.ReceiveResume(conn, src, dirs, names, sizes, resume,
		func(done, total int64, i int) {
			s.emit(Event{Kind: EventProgress, DeviceID: peerID, Device: from, Transfer: offer.TransferID,
				Name: names[i], Bytes: done, Total: total, Index: i, Count: len(names)})
		},
		func(sv transfer.Saved) {
			s.emit(Event{Kind: EventReceived, DeviceID: peerID, Device: from, Path: sv.Path, Bytes: sv.Size, Duration: sv.Took,
				Folder: offer.Folder, Transfer: offer.TransferID, Done: sv.Index == len(names)-1})
		})
	if err != nil {
		return err
	}
	return protocol.WriteFrame(conn, protocol.TransferDone{Type: "transfer_done", TransferID: offer.TransferID, Saved: saved})
}

// serveFetch sends files a paired device asked for (fetch_request), from a shared
// folder only. It reports progress as EventSending and the end as EventSent.
func (s *Server) serveFetch(conn io.ReadWriter, peerID, from string, req protocol.FetchRequest) error {
	fail := func(code, msg string) error {
		return protocol.WriteFrame(conn, protocol.Error{Type: "error", Code: code, Message: msg})
	}
	dir, err := s.Folders.Resolve(req.Folder, req.Path)
	if err != nil {
		return fail("bad_folder", "not a shared folder")
	}
	paths, sizes, err := transfer.Check(dir, req.Files, files.IsHidden)
	if err != nil {
		return fail("not_found", err.Error())
	}
	offer := protocol.FileOffer{Type: "file_offer", TransferID: req.TransferID, Folder: req.Folder, Path: req.Path}
	for i := range paths {
		offer.Files = append(offer.Files, protocol.OfferFile{Name: req.Files[i], Size: sizes[i]})
	}
	if err := protocol.WriteFrame(conn, offer); err != nil {
		return err
	}
	typ, _, err := protocol.ReadFrame(conn)
	if err != nil {
		return err
	}
	if typ != "file_accept" {
		return nil // the device changed its mind
	}
	start := time.Now()
	var total int64
	for _, sz := range sizes {
		total += sz
	}
	var last time.Time
	err = transfer.Stream(conn, paths, sizes, func(sent, total int64, i int) {
		if time.Since(last) > 150*time.Millisecond || sent == total {
			last = time.Now()
			s.emit(Event{Kind: EventSending, DeviceID: peerID, Device: from, Transfer: req.TransferID,
				Name: req.Files[i], Bytes: sent, Total: total, Index: i, Count: len(paths)})
		}
	})
	if err != nil {
		return err
	}
	typ, _, err = protocol.ReadFrame(conn)
	if err != nil {
		return err
	}
	if typ != "transfer_done" {
		return errors.New("the device didn't confirm the files")
	}
	s.emit(Event{Kind: EventSent, DeviceID: peerID, Device: from, Transfer: req.TransferID, Folder: req.Folder,
		Name: req.Files[0], Bytes: total, Count: len(paths), Duration: time.Since(start), Done: true})
	return nil
}

func (s *Server) platform() string {
	if s.Platform == "" {
		return "windows"
	}
	return s.Platform
}

func (s *Server) browsable() bool { return s.AllowBrowse == nil || s.AllowBrowse() }

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
