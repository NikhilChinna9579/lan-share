// Package client connects to a paired device and talks to it: ping, ask for its
// folders, send files (PROTOCOL.md §3–§5). It is the mirror of package server.
package client

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
	"github.com/NikhilChinna9579/lan-share/windows/internal/transfer"
)

// ErrWrongDevice means something answered at that address, but not the device we paired with.
var ErrWrongDevice = errors.New("a different device answered at that address")

// Conn is one connection to a peer, hello already exchanged.
type Conn struct {
	c        *tls.Conn
	raw      net.Conn
	PeerName string
}

// Abort cuts the connection at once (used for Cancel). Close is polite and waits for
// the chunk in flight; Abort doesn't, so the other side stops and cleans up right away.
func (c *Conn) Abort() {
	if tcp, ok := c.raw.(*net.TCPConn); ok {
		tcp.SetLinger(0) // send a reset: data still in flight is thrown away
	}
	c.raw.Close()
}

// Dial connects to addr and accepts only the device whose fingerprint is pinnedID.
func Dial(addr, pinnedID string, me *identity.Identity, myName string, timeout time.Duration) (*Conn, error) {
	raw, err := (&net.Dialer{Timeout: timeout}).Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := tls.Client(raw, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{me.Cert},
		InsecureSkipVerify: true, // no CA chain to check; VerifyPeerCertificate pins the key instead
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return ErrWrongDevice
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil || identity.Fingerprint(cert) != pinnedID {
				return ErrWrongDevice
			}
			return nil
		},
	})
	c.SetDeadline(time.Now().Add(timeout + 5*time.Second))
	if err := c.Handshake(); err != nil {
		raw.Close()
		var recErr tls.RecordHeaderError
		if errors.As(err, &recErr) {
			return nil, ErrWrongDevice
		}
		return nil, err
	}
	conn := &Conn{c: c, raw: raw}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if err := protocol.WriteFrame(c, protocol.Hello{Type: "hello", V: protocol.Version, Name: myName, Platform: "windows"}); err != nil {
		c.Close()
		return nil, err
	}
	var hello protocol.Hello
	if err := conn.expect(&hello, "hello"); err != nil {
		c.Close()
		return nil, err
	}
	conn.PeerName = hello.Name
	return conn, nil
}

// Close ends the connection.
func (c *Conn) Close() error { return c.c.Close() }

// expect reads one frame, fails unless its type is in types, and decodes it into v.
func (c *Conn) expect(v any, types ...string) error {
	c.c.SetDeadline(time.Now().Add(15 * time.Second))
	typ, raw, err := protocol.ReadFrame(c.c)
	if err != nil {
		return err
	}
	for _, t := range types {
		if typ == t {
			if v != nil {
				return json.Unmarshal(raw, v)
			}
			return nil
		}
	}
	if typ == "error" {
		var e protocol.Error
		json.Unmarshal(raw, &e)
		if e.Code == "not_allowed" && strings.Contains(e.Message, "clipboard") {
			return fmt.Errorf("%s has clipboard sharing turned off (LAN Share's settings there)", c.PeerName)
		}
		if e.Code == "not_allowed" {
			return fmt.Errorf("%s doesn't allow browsing its files. Turn it on in LAN Share's settings there", c.PeerName)
		}
		return fmt.Errorf("device said: %s %s", e.Code, e.Message)
	}
	return fmt.Errorf("unexpected reply %q", typ)
}

func (c *Conn) send(msg any) error {
	c.c.SetDeadline(time.Now().Add(15 * time.Second))
	return protocol.WriteFrame(c.c, msg)
}

// Ping checks the device is alive and answering.
func (c *Conn) Ping() error {
	_, err := c.PingInfo()
	return err
}

// Pong is what a device says when pinged.
type Pong struct {
	// Reach: whether it can connect back to us (nil if it didn't say). false usually
	// means a firewall on this computer blocks it.
	Reach *bool `json:"reach"`
	// Battery: its battery level, 1-100 (0 if it didn't say), and whether it's charging.
	Battery  int  `json:"battery"`
	Charging bool `json:"charging"`
}

// PingInfo pings and returns what the device said about itself.
func (c *Conn) PingInfo() (Pong, error) {
	var pong Pong
	if err := c.send(protocol.Message{Type: "ping"}); err != nil {
		return pong, err
	}
	err := c.expect(&pong, "pong")
	if pong.Battery < 0 || pong.Battery > 100 {
		pong.Battery = 0
	}
	return pong, err
}

// Pair sends the one-time code from the QR (only the fake phone uses this; the real
// pairing client is the Android app).
func (c *Conn) Pair(code, myName string) error {
	if err := c.send(protocol.PairRequest{Type: "pair_request", Code: code, Name: myName}); err != nil {
		return err
	}
	var fail protocol.PairFail
	c.c.SetDeadline(time.Now().Add(15 * time.Second))
	typ, raw, err := protocol.ReadFrame(c.c)
	if err != nil {
		return err
	}
	if typ == "pair_ok" {
		return nil
	}
	json.Unmarshal(raw, &fail)
	return fmt.Errorf("pairing refused: %s", fail.Reason)
}

// Folders asks which folders the device offers.
func (c *Conn) Folders() (protocol.FoldersResponse, error) {
	var resp protocol.FoldersResponse
	if err := c.send(protocol.Message{Type: "folders_request"}); err != nil {
		return resp, err
	}
	err := c.expect(&resp, "folders_response")
	return resp, err
}

// List asks what is inside a shared folder (path "" = its top level).
func (c *Conn) List(folder, path string) (protocol.ListResponse, error) {
	var resp protocol.ListResponse
	if err := c.send(protocol.ListRequest{Type: "list_request", Folder: folder, Path: path}); err != nil {
		return resp, err
	}
	err := c.expect(&resp, "list_response")
	return resp, err
}

// Progress is called while sending: bytes sent so far across all files, total bytes,
// and the index of the file being sent.
type Progress func(sent, total int64, file int)

// SendFiles streams local files into folderKey (and subfolder subPath) on the device and returns the saved names.
func (c *Conn) SendFiles(transferID, folderKey, subPath string, paths []string, progress Progress) ([]string, error) {
	items := make([]Item, len(paths))
	for i, p := range paths {
		items[i] = Item{Path: p}
	}
	return c.SendItems(transferID, folderKey, subPath, items, progress)
}

// Item is a file to send; Rel is its subfolder when it's part of a whole folder.
type Item struct {
	Path string
	Rel  string // "/"-separated, e.g. "Holiday/Day 1"; "" for a loose file
}

// SendItems is SendFiles with subfolders: whole folders keep their layout.
func (c *Conn) SendItems(transferID, folderKey, subPath string, items []Item, progress Progress) ([]string, error) {
	offer := protocol.FileOffer{Type: "file_offer", TransferID: transferID, Folder: folderKey, Path: subPath}
	var total int64
	paths := make([]string, len(items))
	for i, it := range items {
		p := it.Path
		paths[i] = p
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("%s is a folder; folders can't be sent yet", filepath.Base(p))
		}
		offer.Files = append(offer.Files, protocol.OfferFile{Name: filepath.Base(p), Size: st.Size(), Path: it.Rel})
		total += st.Size()
	}
	if err := c.send(offer); err != nil {
		return nil, err
	}
	var reject protocol.FileReject
	c.c.SetDeadline(time.Now().Add(60 * time.Second)) // the user may be asked to accept
	typ, raw, err := protocol.ReadFrame(c.c)
	if err != nil {
		return nil, err
	}
	var accept protocol.FileAccept
	switch typ {
	case "file_accept":
		json.Unmarshal(raw, &accept) // may say how much of each file it already has
		for i, off := range accept.Offsets {
			if off > 0 && i < len(items) {
				log.Printf("continuing %s from %d bytes (kept from an interrupted try)", filepath.Base(items[i].Path), off)
			}
		}
	case "file_reject":
		json.Unmarshal(raw, &reject)
		if reject.Reason == "declined" {
			return nil, fmt.Errorf("%s declined the files", c.PeerName)
		}
		return nil, fmt.Errorf("the device refused the files (%s)", reject.Reason)
	default:
		return nil, fmt.Errorf("unexpected reply %q", typ)
	}

	sizes := make([]int64, len(paths))
	for i := range paths {
		sizes[i] = offer.Files[i].Size
	}
	if err := transfer.StreamFrom(&deadlineConn{c.c}, paths, sizes, accept.Offsets, progress); err != nil {
		return nil, err
	}
	var done protocol.TransferDone
	if err := c.expect(&done, "transfer_done"); err != nil {
		return nil, err
	}
	return done.Saved, nil
}

// FetchFiles asks the device for files from one of its shared folders and saves
// them in saveDir. progress gets bytes received so far, the total and the file index.
func (c *Conn) FetchFiles(transferID, folder, path string, names []string, saveDir string, progress Progress) ([]string, error) {
	if err := c.send(protocol.FetchRequest{Type: "fetch_request", TransferID: transferID, Folder: folder, Path: path, Files: names}); err != nil {
		return nil, err
	}
	var offer protocol.FileOffer
	if err := c.expect(&offer, "file_offer"); err != nil {
		return nil, err
	}
	if len(offer.Files) != len(names) {
		return nil, errors.New("the device offered different files")
	}
	clean := make([]string, len(names))
	sizes := make([]int64, len(names))
	for i, f := range offer.Files {
		n, err := files.SanitizeName(f.Name)
		if err != nil || f.Size < 0 {
			return nil, fmt.Errorf("bad file name from the device: %q", f.Name)
		}
		clean[i], sizes[i] = n, f.Size
	}
	if err := c.send(protocol.FileAccept{Type: "file_accept", TransferID: transferID}); err != nil {
		return nil, err
	}
	dc := &deadlineConn{c.c}
	dirs := make([]string, len(clean))
	for i := range dirs {
		dirs[i] = saveDir
	}
	saved, err := transfer.Receive(dc, dc, dirs, clean, sizes, func(done, total int64, i int) {
		if progress != nil {
			progress(done, total, i)
		}
	}, nil)
	if err != nil {
		return nil, err
	}
	if err := c.send(protocol.TransferDone{Type: "transfer_done", TransferID: transferID, Saved: saved}); err != nil {
		return nil, err
	}
	return saved, nil
}

// deadlineConn pushes the deadline forward on every read and write, so a big
// transfer can take as long as it needs while a stalled one fails after 15 s.
type deadlineConn struct{ c net.Conn }

func (d *deadlineConn) Read(p []byte) (int, error) {
	d.c.SetDeadline(time.Now().Add(15 * time.Second))
	return d.c.Read(p)
}

func (d *deadlineConn) Write(p []byte) (int, error) {
	d.c.SetDeadline(time.Now().Add(15 * time.Second))
	return d.c.Write(p)
}

// Mkdir creates a subfolder in one of the device's shared folders and returns its name.
func (c *Conn) Mkdir(folder, path, name string) (string, error) {
	if err := c.send(protocol.MkdirRequest{Type: "mkdir_request", Folder: folder, Path: path, Name: name}); err != nil {
		return "", err
	}
	var ok protocol.MkdirOK
	err := c.expect(&ok, "mkdir_ok")
	return ok.Name, err
}

// Search finds files by name in all of the device's shared folders.
func (c *Conn) Search(query string) ([]protocol.SearchHit, error) {
	if err := c.send(protocol.SearchRequest{Type: "search_request", Query: query}); err != nil {
		return nil, err
	}
	var resp protocol.SearchResponse
	err := c.expect(&resp, "search_response")
	return resp.Results, err
}

// PushClip puts text on the device's clipboard (a deliberate send).
func (c *Conn) PushClip(text string) error { return c.pushClip(text, false) }

// PushClipAuto sends text copied here by itself; the device may offer it with a Copy button.
func (c *Conn) PushClipAuto(text string) error { return c.pushClip(text, true) }

func (c *Conn) pushClip(text string, auto bool) error {
	if err := c.send(protocol.ClipPush{Type: "clip_push", Text: text, Auto: auto}); err != nil {
		return err
	}
	return c.expect(nil, "clip_ok")
}

// RequestClip gets the device's clipboard text ("paste from …").
func (c *Conn) RequestClip() (string, error) {
	if err := c.send(protocol.ClipRequest{Type: "clip_request"}); err != nil {
		return "", err
	}
	var r protocol.ClipResponse
	if err := c.expect(&r, "clip_response"); err != nil {
		return "", err
	}
	if !r.Available {
		return "", fmt.Errorf("%s: %s", c.PeerName, r.Reason)
	}
	return r.Text, nil
}
