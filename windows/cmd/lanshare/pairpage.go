package main

import (
	"encoding/base64"
	"html/template"
	"os"
	"path/filepath"
	"time"
)

// The pairing page is a plain HTML file that reloads itself every 2 seconds. When the
// phone pairs, we rewrite the file with a "Paired" message and the open page updates:
// a live page without running a web server.
var pairTmpl = template.Must(template.New("pair").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
{{if not .Done}}<meta http-equiv="refresh" content="2">{{end}}
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pair your phone · LAN Share</title>
<style>
  :root { --bg:#f4f6fb; --card:#fff; --ink:#12141a; --mute:#5b6170; --line:#e3e6ee; --brand:#2563eb; --ok:#16a34a; --on-brand:#fff; }
  @media (prefers-color-scheme: dark) {
    :root { --bg:#0f1115; --card:#1a1d24; --ink:#eceef3; --mute:#a0a6b3; --line:#2a2e37; --brand:#8ab4ff; --ok:#4ade80; --on-brand:#0a1f4d; }
  }
  * { box-sizing: border-box; }
  body { margin:0; min-height:100vh; display:grid; place-items:center; background:var(--bg);
         color:var(--ink); font: 15px/1.5 "Segoe UI Variable Text","Segoe UI",system-ui,sans-serif; }
  .card { width:min(420px, calc(100vw - 32px)); background:var(--card); border:1px solid var(--line);
          border-radius:24px; padding:32px; text-align:center; box-shadow:0 8px 30px rgba(0,0,0,.06); }
  .brand { display:inline-flex; align-items:center; gap:10px; font-weight:600; color:var(--mute); font-size:13px;
           letter-spacing:.06em; text-transform:uppercase; }
  .logo { width:26px; height:26px; border-radius:8px; background:var(--brand); display:grid; place-items:center; }
  h1 { font-size:22px; margin:16px 0 4px; font-weight:600; }
  p { margin:0; color:var(--mute); }
  .qr { margin:24px auto 16px; width:264px; height:264px; padding:12px; background:#fff; border-radius:16px;
        border:1px solid var(--line); }
  .qr img { width:100%; height:100%; image-rendering:pixelated; display:block; }
  .code { font: 600 22px/1 ui-monospace,"Cascadia Mono",Consolas,monospace; letter-spacing:.25em; margin-top:4px; }
  .timer { font-size:13px; color:var(--mute); margin-top:8px; }
  .done { width:72px; height:72px; border-radius:50%; margin:28px auto 8px; display:grid; place-items:center;
          background:color-mix(in srgb, var(--ok) 15%, transparent); color:var(--ok); font-size:36px; }
  ol { text-align:left; color:var(--mute); padding-left:20px; margin:16px 0 0; font-size:14px; }
</style></head>
<body><main class="card">
  <div class="brand"><span class="logo"><svg width="16" height="16" viewBox="0 0 24 24" style="fill:var(--on-brand)"><path d="M6.99 11 3 15l3.99 4v-3H14v-2H6.99v-3zM21 9l-3.99-4v3H10v2h7.01v3L21 9z"/></svg></span>LAN Share</div>
  {{if .Done}}
    <div class="done">✓</div>
    <h1>Paired with {{.Phone}}</h1>
    <p>You can close this page. The phone and {{.Name}} will find each other from now on, even if Wi-Fi addresses change.</p>
  {{else}}
    <h1>Pair your phone</h1>
    <p>with <b>{{.Name}}</b></p>
    <div class="qr"><img alt="Pairing QR code" src="{{.QR}}"></div>
    <div class="code">{{.Code}}</div>
    <div class="timer" id="t">Valid for 2 minutes</div>
    <ol>
      <li>Open <b>LAN Share</b> on your phone.</li>
      <li>Tap <b>Pair a laptop</b> and scan this code.</li>
    </ol>
    <script>
      const end = new Date("{{.Expires}}").getTime();
      const t = document.getElementById("t");
      function tick() {
        const s = Math.max(0, Math.round((end - Date.now()) / 1000));
        t.textContent = s > 0 ? "Expires in " + Math.floor(s/60) + ":" + String(s%60).padStart(2,"0")
                              : "Expired. Press P in the LAN Share window for a new code.";
      }
      tick(); setInterval(tick, 1000);
    </script>
  {{end}}
</main></body></html>`))

type pairPage struct {
	Name, Code, Phone string
	QR                template.URL // data: URL of the PNG (trusted: we made it)
	Expires           string       // RFC 3339
	Done              bool
}

func pairPagePath() string { return filepath.Join(os.TempDir(), "lanshare-pair.html") }

func writePairPage(p pairPage, png []byte) error {
	p.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
	f, err := os.Create(pairPagePath())
	if err != nil {
		return err
	}
	defer f.Close()
	return pairTmpl.Execute(f, p)
}

func writePairedPage(name, phone string) {
	writePairPage(pairPage{Name: name, Phone: phone, Done: true, Expires: time.Now().Format(time.RFC3339)}, nil)
}
