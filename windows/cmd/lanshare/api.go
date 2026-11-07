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

	"github.com/NikhilChinna9579/lan-share/windows/internal/clipboard"
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
		go func() { time.Sleep(200 * time.Millisecond); a.requestQuit() }()
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
	h("/api/pick/sendfolder", func(*http.Request) (any, error) {
		p, err := pickFolder()
		if err != nil {
			return nil, err
		}
		return statFiles([]string{p}), nil
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
	h("/api/fetch", func(r *http.Request) (any, error) {
		var in struct {
			Device, Folder, FolderLabel, Path string
			Names                             []string
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		if len(in.Names) == 0 {
			return nil, fmt.Errorf("no files chosen")
		}
		return map[string]string{"id": a.fetch(in.Device, in.Folder, in.FolderLabel, in.Path, in.Names)}, nil
	})
	h("/api/openremote", func(r *http.Request) (any, error) { // tap a phone's file
		var in struct {
			Device, Folder, Path, Name string
			Size, Modified             int64
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return a.openRemote(in.Device, in.Folder, in.Path, in.Name, in.Size, in.Modified)
	})
	h("/api/history/clear", func(*http.Request) (any, error) { // "Clear" on Recent activity
		a.clearHistory()
		return nil, nil
	})
	h("/api/reopen", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.reopen(in.ID)
	})
	h("/api/savecopy", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		p, err := a.saveCopy(in.ID)
		return map[string]string{"path": p}, err
	})
	h("/api/approve", func(r *http.Request) (any, error) {
		var in struct {
			ID string
			OK bool
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.answer(in.ID, in.OK)
	})
	h("/api/settings", func(r *http.Request) (any, error) {
		var in struct {
			Name    *string
			Ask     *bool
			Browse  *bool
			Clip    *bool
			ClipNow *bool
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		if in.Name != nil {
			if err := a.rename(*in.Name); err != nil {
				return nil, err
			}
		}
		a.mu.Lock()
		if in.Ask != nil {
			a.settings.Ask = *in.Ask
		}
		if in.Browse != nil {
			a.settings.NoBrowse = !*in.Browse
		}
		if in.Clip != nil {
			a.settings.NoClip = !*in.Clip
		}
		if in.ClipNow != nil {
			a.settings.ClipNow = *in.ClipNow
		}
		a.mu.Unlock()
		a.hub.changed()
		return nil, a.saveSettings()
	})
	h("/api/clip", func(*http.Request) (any, error) { // what's on this laptop's clipboard
		text, private, err := clipboard.Read()
		return map[string]any{"text": text, "private": private}, err
	})
	h("/api/clip/set", func(r *http.Request) (any, error) { // the window's Copy button
		var in struct{ Text string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, clipboard.Write(in.Text)
	})
	h("/api/clip/again", func(*http.Request) (any, error) { // the clipboard card's Copy button
		return nil, a.copyOffered()
	})
	h("/api/clip/open", func(*http.Request) (any, error) { // the clipboard card's Open button (a link)
		return nil, a.openClipLink()
	})
	h("/api/clip/copy", func(r *http.Request) (any, error) { // testing: as if the person copied this
		var in struct{ Text string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		clipboard.Copy(in.Text)
		return nil, nil
	})
	h("/api/remote/clip", func(r *http.Request) (any, error) { // "Paste from this phone"
		var in struct{ Device string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		n, err := a.pasteFromPhone(in.Device)
		if err != nil {
			return nil, err
		}
		return map[string]any{"chars": n}, nil
	})
	h("/api/folders/default", func(r *http.Request) (any, error) {
		var in struct{ Key string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		err := a.folders.SetDefault(in.Key)
		a.hub.changed()
		return nil, err
	})
	h("/api/remote/search", func(r *http.Request) (any, error) {
		q := r.URL.Query()
		c, _, err := a.dial(q.Get("device"))
		if err != nil {
			return nil, err
		}
		defer c.Close()
		return c.Search(q.Get("q"))
	})
	h("/api/remote/mkdir", func(r *http.Request) (any, error) {
		var in struct{ Device, Folder, Path, Name string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		c, _, err := a.dial(in.Device)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		name, err := c.Mkdir(in.Folder, in.Path, in.Name)
		return map[string]string{"name": name}, err
	})
	h("/api/cancel", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.cancelTransfer(in.ID)
	})
	h("/api/retry", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		id, err := a.retryTransfer(in.ID)
		return map[string]string{"id": id}, err
	})
	h("/api/dismiss", func(r *http.Request) (any, error) {
		var in struct{ ID string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		a.dismissTransfer(in.ID)
		return nil, nil
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
				list := statFiles(in.Files)
				err := a.sendNowList(tid, s.ID, in.Folder, "", list)
				if err == nil {
					a.note("sent", "Sent "+fileCount(len(list))+" to "+s.Name, "test", "")
				}
				return map[string]string{"sentTo": s.Name}, err
			}
		}
		return nil, fmt.Errorf("no phone online")
	})

	// Settings and system integration.
	h("/api/install", func(*http.Request) (any, error) {
		if err := install(); err != nil {
			return nil, err
		}
		a.note("info", "Installed LAN Share", installedExe(), "")
		go func() { time.Sleep(300 * time.Millisecond); a.requestQuit() }() // the installed copy takes over
		return nil, nil
	})
	h("/api/uninstall", func(r *http.Request) (any, error) {
		var in struct{ RemoveData bool }
		readJSON(r, &in)
		if err := uninstall(a.dataDir, in.RemoveData); err != nil {
			return nil, err
		}
		go func() { time.Sleep(300 * time.Millisecond); a.requestQuit() }()
		return nil, nil
	})
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
		out = append(out, fileInfo{Path: f.Path, Name: filepath.Base(f.Path), Size: st.Size(), Rel: f.Rel, Temp: strings.HasPrefix(f.Path, outbox)})
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

// statFiles turns paths into files to send. A folder becomes all the files inside it
// (hidden and system files skipped), each remembering its subfolder, so the folder
// arrives with the same layout.
func statFiles(paths []string) []fileInfo {
	out := []fileInfo{}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			out = append(out, fileInfo{Path: p, Name: filepath.Base(p), Size: st.Size()})
			continue
		}
		parent := filepath.Dir(p)
		filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil || len(out) >= maxFolderFiles {
				return filepath.SkipDir
			}
			if path != p && files.IsHidden(filepath.Dir(path), d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(parent, filepath.Dir(path))
			out = append(out, fileInfo{Path: path, Name: d.Name(), Size: info.Size(), Rel: filepath.ToSlash(rel)})
			return nil
		})
	}
	return out
}

const maxFolderFiles = 10000

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(v) // big enough for a folder of 10 000 files
}
