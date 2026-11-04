// Package client connects to a paired device and talks to it: ping, ask for its
// folders, send files (PROTOCOL.md §3–§5). It is the mirror of package server.
package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/identity"
	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
)

// ErrWrongDevice means something answered at that address, but not the device we paired with.
var ErrWrongDevice = errors.New("a different device answered at that address")

// Conn is one connection to a peer, hello already exchanged.
type Conn struct {
	c        *tls.Conn
	PeerName string
}

// Dial connects to addr and accepts only the device whose fingerprint is pinnedID.
func Dial(addr, pinnedID string, me *identity.Identity, myName string, timeout time.Duration) (*Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	c, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
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
	if err != nil {
		var recErr tls.RecordHeaderError
		if errors.As(err, &recErr) {
			return nil, ErrWrongDevice
		}
		return nil, err
	}
	conn := &Conn{c: c}
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
	if err := c.send(protocol.Message{Type: "ping"}); err != nil {
		return err
	}
	return c.expect(nil, "pong")
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

// Progress is called while sending: bytes sent so far across all files, total bytes,
// and the index of the file being sent.
type Progress func(sent, total int64, file int)

// SendFiles streams local files into folderKey on the device and returns the saved names.
func (c *Conn) SendFiles(transferID, folderKey string, paths []string, progress Progress) ([]string, error) {
	offer := protocol.FileOffer{Type: "file_offer", TransferID: transferID, Folder: folderKey}
	var total int64
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return nil, fmt.Errorf("%s is a folder; folders can't be sent yet", filepath.Base(p))
		}
		offer.Files = append(offer.Files, protocol.OfferFile{Name: filepath.Base(p), Size: st.Size()})
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
	switch typ {
	case "file_accept":
	case "file_reject":
		json.Unmarshal(raw, &reject)
		return nil, fmt.Errorf("the device refused the files (%s)", reject.Reason)
	default:
		return nil, fmt.Errorf("unexpected reply %q", typ)
	}

	var sent int64
	buf := make([]byte, 256*1024)
	for i, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		size := offer.Files[i].Size
		for remaining := size; remaining > 0; {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			got, err := io.ReadFull(f, buf[:n])
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s changed while sending: %w", filepath.Base(p), err)
			}
			h.Write(buf[:got])
			c.c.SetDeadline(time.Now().Add(15 * time.Second))
			if _, err := c.c.Write(buf[:got]); err != nil {
				f.Close()
				return nil, err
			}
			remaining -= int64(got)
			sent += int64(got)
			if progress != nil {
				progress(sent, total, i)
			}
		}
		f.Close()
		if err := c.send(protocol.FileEnd{Type: "file_end", Index: i, SHA256: hex.EncodeToString(h.Sum(nil))}); err != nil {
			return nil, err
		}
	}
	var done protocol.TransferDone
	if err := c.expect(&done, "transfer_done"); err != nil {
		return nil, err
	}
	return done.Saved, nil
}
