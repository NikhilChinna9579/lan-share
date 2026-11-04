package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NikhilChinna9579/lan-share/windows/internal/files"
)

//go:embed web/index.html
var indexHTML []byte

// ---------------------------------------------------------------- live updates

// hub pushes the whole app state to every open window whenever something changes
// (Server-Sent Events). At most ~7 pushes a second, however busy things get.
type hub struct {
	state func() []byte
	mu    sync.Mutex
	subs  map[chan [2]string]struct{}
	dirty chan struct{}
}

func newHub(state func() []byte) *hub {
	h := &hub{state: state, subs: map[chan [2]string]struct{}{}, dirty: make(chan struct{}, 1)}
	go func() {
		for range h.dirty {
			h.send("state", string(h.state()))
			time.Sleep(150 * time.Millisecond)
		}
	}()
	return h
}

func (h *hub) changed() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

func (h *hub) send(event, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- [2]string{event, data}:
		default: // a stuck window doesn't hold up everyone else
		}
	}
}

func (h *hub) clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// ---------------------------------------------------------------- server

func serveAPI(a *app, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		w.Write(indexHTML)
	})
	h := func(path string, fn func(*http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !a.authorized(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			out, err := fn(r)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if out == nil {
				out = map[string]bool{"ok": true}
			}
			json.NewEncoder(w).Encode(out)
		})
	}

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		c := make(chan [2]string, 16)
		a.hub.mu.Lock()
		a.hub.subs[c] = struct{}{}
		a.hub.mu.Unlock()
		defer func() {
			a.hub.mu.Lock()
			delete(a.hub.subs, c)
			a.hub.mu.Unlock()
		}()
		fmt.Fprintf(w, "event: state\ndata: %s\n\n", a.stateJSON())
		fl.Flush()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case m := <-c:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m[0], m[1])
				fl.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": keep-alive\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			case <-a.quit:
				return
			}
		}
	})

	h("/api/ping", func(*http.Request) (any, error) { return nil, nil })
	h("/api/state", func(*http.Request) (any, error) { return json.RawMessage(a.stateJSON()), nil })
	h("/api/show", func(*http.Request) (any, error) { a.showWindow(); return nil, nil })
	h("/api/quit", func(*http.Request) (any, error) {
		go func() { time.Sleep(200 * time.Millisecond); close(a.quit) }()
		return nil, nil
	})

	h("/api/pair", func(*http.Request) (any, error) { return nil, a.startPairing() })
	h("/api/pair/close", func(*http.Request) (any, error) {
		a.mu.Lock()
		a.pairing = nil
		a.mu.Unlock()
		a.hub.changed()
		return nil, nil
	})
	h("/api/forget", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		d, _ := a.trust.Get(in.ID)
		if err := a.trust.Remove(in.ID); err != nil {
			return nil, err
		}
		a.note("info", "Forgot "+d.Name, "It can no longer connect to this laptop.", "")
		return nil, nil
	})

	// Shared folders on this laptop.
	h("/api/folders/add", func(r *http.Request) (any, error) {
		var in struct{ Path string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		root, err := a.folders.AddFolder(in.Path)
		if err == nil {
			a.note("info", "Sharing "+root.Label, root.Path, "")
		}
		return root, err
	})
	h("/api/folders/remove", func(r *http.Request) (any, error) {
		var in struct{ Key string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		defer a.hub.changed()
		return nil, a.folders.Remove(in.Key)
	})
	h("/api/pick/files", func(*http.Request) (any, error) {
		paths, err := pickFiles()
		if err != nil {
			return nil, err
		}
		return statFiles(paths), nil
	})
	h("/api/pick/folder", func(*http.Request) (any, error) {
		p, err := pickFolder()
		return map[string]string{"path": p}, err
	})
	h("/api/upload", func(r *http.Request) (any, error) { return a.upload(r) })
	h("/api/pending/clear", func(*http.Request) (any, error) {
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()
		a.hub.changed()
		return nil, nil
	})
	h("/api/handoff", func(r *http.Request) (any, error) {
		var in struct{ Files []string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		a.handoff(in.Files)
		return nil, nil
	})

	// A phone's shared folders.
	h("/api/remote/folders", func(r *http.Request) (any, error) {
		c, _, err := a.dial(r.URL.Query().Get("device"))
		if err != nil {
			return nil, err
		}
		defer c.Close()
		return c.Folders()
	})
	h("/api/remote/list", func(r *http.Request) (any, error) {
		q := r.URL.Query()
		c, _, err := a.dial(q.Get("device"))
		if err != nil {
			return nil, err
		}
		defer c.Close()
		return c.List(q.Get("folder"), q.Get("path"))
	})
	h("/api/send", func(r *http.Request) (any, error) {
		var in struct {
			Device, Folder, FolderLabel, Path string
			Files                             []fileInfo
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		if len(in.Files) == 0 {
			return nil, fmt.Errorf("no files to send")
		}
		in.Files = a.checkedFiles(in.Files)
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()
		return map[string]string{"id": a.send(in.Device, in.Folder, in.FolderLabel, in.Path, in.Files)}, nil
	})
	// Testing: send to the only online phone and wait for the result.
	h("/api/send-now", func(r *http.Request) (any, error) {
		var in struct {
			Files  []string
			Folder string
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		for _, s := range a.tracker.Snapshot() {
			if s.Status.String() == "online" {
				tid := "test-" + fmt.Sprint(time.Now().UnixNano())
				err := a.sendNowList(tid, s.ID, in.Folder, "", statFiles(in.Files))
				if err == nil {
					a.note("sent", "Sent "+fileCount(len(in.Files))+" to "+s.Name, "test", "")
				}
				return map[string]string{"sentTo": s.Name}, err
			}
		}
		return nil, fmt.Errorf("no phone online")
	})

	// Settings and system integration.
	h("/api/firewall", func(*http.Request) (any, error) {
		err := allowFirewall()
		if err == nil {
			a.note("info", "Allowed LAN Share through Windows Firewall", "Phones can connect to this laptop now.", "")
			a.tracker.CheckNow()
		}
		return nil, err
	})
	h("/api/autostart", func(r *http.Request) (any, error) {
		var in struct{ On bool }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		defer a.hub.changed()
		return nil, setAutostart(in.On)
	})
	h("/api/open", func(r *http.Request) (any, error) {
		var in struct{ What, Key, Path string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		switch in.What {
		case "log":
			return nil, openPath(a.logPath, false)
		case "folder":
			p, ok := a.folders.Path(in.Key)
			if !ok {
				return nil, fmt.Errorf("unknown folder")
			}
			return nil, openPath(p, false)
		case "file": // "Show in folder" for a received file; only inside shared folders
			if !a.insideShared(in.Path) {
				return nil, fmt.Errorf("not in a shared folder")
			}
			return nil, openPath(in.Path, true)
		}
		return nil, fmt.Errorf("unknown")
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		log.Printf("api: %v", err)
	}
}

// authorized: requests must come to 127.0.0.1 (blocks DNS-rebinding tricks from web
// pages) and carry the token that only our window and our own .exe know.
func (a *app) authorized(r *http.Request) bool {
	host := r.Host
	if host != fmt.Sprintf("127.0.0.1:%d", a.apiPort) && host != fmt.Sprintf("localhost:%d", a.apiPort) {
		return false
	}
	tok := r.Header.Get("X-Token")
	if tok == "" {
		tok = r.URL.Query().Get("t")
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
		return false
	}
	// Changes only by POST, so a link can't trigger anything.
	return r.Method == http.MethodPost || r.URL.Path == "/api/events" || r.URL.Path == "/api/state" ||
		strings.HasPrefix(r.URL.Path, "/api/remote/")
}

// upload stores a file dropped onto the window in our outbox, to be sent and deleted.
func (a *app) upload(r *http.Request) (any, error) {
	name, err := files.SanitizeName(r.URL.Query().Get("name"))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(a.dataDir, "outbox", fmt.Sprint(time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return fileInfo{Path: p, Name: name, Size: n, Temp: true}, nil
}

// checkedFiles re-reads sizes from disk and drops anything that isn't a readable file.
func (a *app) checkedFiles(in []fileInfo) []fileInfo {
	var out []fileInfo
	outbox := filepath.Join(a.dataDir, "outbox") + string(filepath.Separator)
	for _, f := range in {
		st, err := os.Stat(f.Path)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, fileInfo{Path: f.Path, Name: filepath.Base(f.Path), Size: st.Size(), Temp: strings.HasPrefix(f.Path, outbox)})
	}
	return out
}

func (a *app) insideShared(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	for _, r := range a.folders.Roots() {
		if rel, err := filepath.Rel(r.Path, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

func statFiles(paths []string) []fileInfo {
	out := []fileInfo{}
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, fileInfo{Path: p, Name: filepath.Base(p), Size: st.Size()})
		}
	}
	return out
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
