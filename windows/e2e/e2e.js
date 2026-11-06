// End-to-end test of the Windows app (built for Linux) with the phone simulator.
//
//   node e2e/e2e.js <lanshare binary> <fakephone binary> <output dir>
//
// Starts LAN Share, opens its window in a headless browser, pairs the simulated phone,
// sends files both ways (including into a subfolder), checks the firewall warning,
// and saves screenshots. Every step has a time limit and the whole run is capped, so
// it can never hang; all processes are killed on exit.
const { chromium } = require("playwright");
const { spawn, execFileSync } = require("child_process");
const fs = require("fs");
const path = require("path");
const os = require("os");
const crypto = require("crypto");

const [lanshare, fakephone, out] = process.argv.slice(2).map((p) => path.resolve(p));
const work = fs.mkdtempSync(path.join(os.tmpdir(), "lanshare-e2e-"));
const home = path.join(work, "home");
fs.mkdirSync(out, { recursive: true });
fs.mkdirSync(home, { recursive: true });
for (const d of ["Downloads", "Documents", "Desktop", "Pictures"]) fs.mkdirSync(path.join(home, d), { recursive: true });

const procs = [];
let browser;
let failures = 0;
const t0 = Date.now();
const log = (...a) => console.log(`[${((Date.now() - t0) / 1000).toFixed(1).padStart(5)}s]`, ...a);

function start(name, cmd, args) {
  const p = spawn(cmd, args, { env: { ...process.env, HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") } });
  // Opened right away (not lazily), so the log exists even if the process says nothing.
  const f = fs.createWriteStream(null, { fd: fs.openSync(path.join(out, name + ".log"), "w") });
  p.stdout.pipe(f);
  p.stderr.pipe(f);
  procs.push(p);
  return p;
}

async function cleanup(code) {
  for (const p of procs) try { p.kill("SIGKILL"); } catch {}
  if (browser) await browser.close().catch(() => {});
  log(code === 0 ? "ALL PASSED" : `FAILED (${failures} problem(s))`);
  process.exit(code);
}
setTimeout(() => { log("TIMEOUT: the whole run took over 300 s"); failures++; cleanup(2); }, 300_000).unref();

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const settle = () => sleep(400); // let sheet animations finish before a screenshot
async function waitFor(what, fn, ms = 10_000) {
  const end = Date.now() + ms;
  let last;
  while (Date.now() < end) {
    try { const v = await fn(); if (v) return v; } catch (e) { last = e; }
    await sleep(200);
  }
  throw new Error(`timed out after ${ms / 1000}s waiting for ${what}` + (last ? `: ${last.message}` : ""));
}
async function step(name, fn) {
  const s = Date.now();
  try {
    await Promise.race([fn(), sleep(60_000).then(() => { throw new Error("step took over 60 s"); })]);
    log(`ok    ${name} (${Date.now() - s} ms)`);
  } catch (e) {
    failures++;
    log(`FAIL  ${name}: ${e.message}`);
    throw e;
  }
}

(async () => {
  const laptopData = path.join(work, "laptop");
  const openLog = path.join(work, "opened.txt"); // what the laptop "opened" (instead of launching apps)
  process.env.LANSHARE_OPEN_LOG = openLog;
  process.env.LANSHARE_FAKE_HEARD = "get the latest photo from my phone"; // stands in for the microphone
  const win = process.platform === "win32";
  if (win) {
    // Real Windows: the real speech recogniser listens to this sentence, spoken by Windows'
    // own text-to-speech into a WAV file (there's no microphone on a test machine).
    const wav = path.join(work, "said.wav");
    try {
      execFileSync("powershell", ["-NoProfile", "-Command",
        `Add-Type -AssemblyName System.Speech; $s = New-Object System.Speech.Synthesis.SpeechSynthesizer; $s.Rate = -1; ` +
        `$s.SetOutputToWaveFile('${wav}'); $s.Speak('get the latest photo from my phone'); $s.Dispose()`], { timeout: 60_000 });
      process.env.LANSHARE_SPEECH_WAV = wav;
    } catch (e) { console.log("note: couldn't make the test recording: " + e.message); }
  }
  // Windows: is the bubble window on screen?
  let bubbleSaw = "";
  const bubbleVisible = () => (bubbleSaw = execFileSync("powershell", ["-NoProfile", "-Command",
    `Add-Type -Name W -Namespace U -MemberDefinition '[DllImport("user32.dll")] public static extern System.IntPtr FindWindow(string c, string t); [DllImport("user32.dll")] public static extern bool IsWindowVisible(System.IntPtr h);'; ` +
    `$h = [U.W]::FindWindow('LanShareBubble', [NullString]::Value); if ($h -ne [System.IntPtr]::Zero -and [U.W]::IsWindowVisible($h)) { 'yes' } else { 'no ' + $h }`], { timeout: 30_000 }).toString().trim()) === "yes";
  const phoneData = path.join(work, "phone");
  const pairFile = path.join(work, "pair.json");
  const phoneInbox = path.join(phoneData, "inbox", "Download");
  let laptopDownloads = path.join(home, "Downloads"); // replaced by the app's real Downloads below
  let page, url;

  try {
    await step("laptop app starts and prints its window address", async () => {
      const p = start("laptop", lanshare, ["-data", laptopData, "-port", "47911", "-name", "DESKTOP-NIKHIL", "-no-window", "-pairing-file", pairFile]);
      url = await waitFor("UI address", () => {
        const txt = fs.existsSync(path.join(out, "laptop.log")) ? fs.readFileSync(path.join(out, "laptop.log"), "utf8") : "";
        const m = txt.match(/UI: (http\S+)/);
        return m && m[1];
      });
      if (p.exitCode !== null) throw new Error("laptop app exited");
    });

    browser = await chromium.launch();
    const ctx = await browser.newContext({ viewport: { width: 480, height: 820 }, deviceScaleFactor: 1 });
    page = await ctx.newPage();
    page.setDefaultTimeout(10_000);
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    // Expected API refusals (shown to the person as messages, e.g. "isn't open on the phone")
    // appear as failed requests; real script errors still count.
    page.on("console", (m) => { if (m.type() === "error" && !/status of 400/.test(m.text())) errors.push(m.text()); });

    await step("window opens and shows the empty state", async () => {
      await page.goto(url);
      await page.getByText("Pair your phone").waitFor();
      await page.getByText("Ready to receive").waitFor();
      // Where this laptop saves received files (on Windows: the real Downloads folder).
      laptopDownloads = await page.evaluate(() => S.folders.find((f) => f.key === "downloads").path);
      log(`      laptop Downloads: ${laptopDownloads}`);
      await settle(); await page.screenshot({ path: path.join(out, "w01-empty.png") });
    });

    await step("pairing code appears", async () => {
      await page.getByRole("button", { name: "Show pairing code" }).click();
      await page.locator(".qr img").waitFor();
      await waitFor("pairing file", () => fs.existsSync(pairFile));
      // The simulated phone runs on this same machine: talk over loopback, so a
      // firewall on the test machine can't get in the way.
      const qr = JSON.parse(fs.readFileSync(pairFile, "utf8"));
      qr.host = "127.0.0.1";
      fs.writeFileSync(pairFile, JSON.stringify(qr));
      await settle(); await page.screenshot({ path: path.join(out, "w02-pair.png") });
    });

    // A subfolder on the phone to save into.
    fs.mkdirSync(path.join(phoneInbox, "Work", "Reports"), { recursive: true });

    await step("phone pairs; window shows success, then the phone online", async () => {
      start("phone", fakephone, ["-data", phoneData, "-pair", pairFile, "-name", "Nikhil's phone", "-port", "47821"]);
      await page.getByText("Paired with Nikhil's phone").first().waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w03-paired.png") });
      await page.getByRole("button", { name: "Done" }).click();
      await page.getByText(/Online · /).waitFor({ timeout: 15_000 });
      await settle(); await page.screenshot({ path: path.join(out, "w04-online.png") });
    });

    const big = path.join(work, "Holiday photos.zip");
    fs.writeFileSync(big, crypto.randomBytes(24 * 1024 * 1024));

    await step('"Send to" from Explorer opens the send sheet with the file', async () => {
      execFileSync(lanshare, ["-data", laptopData, "send", big], { env: { ...process.env, HOME: home }, timeout: 10_000 });
      await page.getByText("Holiday photos.zip").first().waitFor();
      await page.getByText("Downloads").first().waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w05-send-sheet.png") });
    });

    await step("browse into a subfolder on the phone and send there", async () => {
      await page.locator('[data-act="root"]', { hasText: "Downloads" }).click();
      await page.locator('[data-act="sub"]', { hasText: "Work" }).click();
      await page.locator('[data-act="sub"]', { hasText: "Reports" }).click();
      await page.locator(".crumbs", { hasText: "Reports" }).waitFor();
      await page.getByText("No subfolders here.").waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w06-subfolder.png") });
      await page.locator('[data-act="dosend"]').click();
      await page.getByText(/Sending|Sent Holiday/).first().waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w07-sending.png") });
      const dest = path.join(phoneInbox, "Work", "Reports", "Holiday photos.zip");
      await waitFor("file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === fs.statSync(big).size, 20_000);
      const same = crypto.createHash("sha256").update(fs.readFileSync(dest)).digest("hex") ===
                   crypto.createHash("sha256").update(fs.readFileSync(big)).digest("hex");
      if (!same) throw new Error("file on the phone differs");
      await page.getByText(/Sent Holiday photos\.zip to Nikhil's phone/).first().waitFor();
      // Time taken and speed are shown.
      await page.getByText(/24\.0 MB in (under )?[\d.]+ s · [\d.]+ MB\/s/).first().waitFor();
    });

    await step("phone sends to the laptop; window shows it live", async () => {
      fs.rmSync(path.join(laptopDownloads, "Scan 2026-11-04.pdf"), { force: true }); // from a previous run on this machine
      const f = path.join(work, "Scan 2026-11-04.pdf");
      fs.writeFileSync(f, crypto.randomBytes(6 * 1024 * 1024));
      // A second simulated phone process just for sending (same identity, different port).
      start("phone-send", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47822", "-send", f]);
      await page.getByText("Received Scan 2026-11-04.pdf").first().waitFor({ timeout: 15_000 });
      await page.getByText(/6\.0 MB in (under )?[\d.]+ s/).first().waitFor();
      const got = path.join(laptopDownloads, "Scan 2026-11-04.pdf");
      await waitFor("file on the laptop", () => fs.existsSync(got) && fs.statSync(got).size === fs.statSync(f).size);
      await settle(); await page.screenshot({ path: path.join(out, "w08-received.png") });
    });

    await step("cancel a transfer midway, then retry it", async () => {
      procs[1].kill("SIGKILL");
      await sleep(300);
      // A slow phone (4 MB/s) so there's time to press Cancel.
      procs[1] = start("phone-slow", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47821", "-slow", "4"]);
      const f = path.join(work, "Big video.mp4");
      fs.writeFileSync(f, crypto.randomBytes(16 * 1024 * 1024));
      await sleep(1500);
      execFileSync(lanshare, ["-data", laptopData, "send", f], { env: { ...process.env, HOME: home }, timeout: 10_000 });
      await page.locator('[data-act="root"]', { hasText: "Downloads" }).click();
      await page.locator('[data-act="dosend"]').click();
      await page.getByText(/Sending Big video\.mp4/).waitFor();
      await page.getByText(/ · \d+\.\d s$/).first().waitFor(); // live timer
      await settle(); await page.screenshot({ path: path.join(out, "w08b-sending-timer.png") });
      await page.locator('[data-act="cancel"]').click();
      await page.getByText(/Cancelled sending to Nikhil's phone/).waitFor();
      const dest = path.join(phoneInbox, "Big video.mp4");
      // The phone deletes the partial file as soon as it notices; allow it a moment.
      await waitFor("partial file removed on the phone",
        () => fs.readdirSync(phoneInbox).filter((n) => n.startsWith("Big video")).length === 0, 5_000);
      await settle(); await page.screenshot({ path: path.join(out, "w08c-cancelled.png") });
      await page.locator('[data-act="retry"]').click();
      await page.getByText(/Sending Big video\.mp4/).waitFor();
      await waitFor("retried file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === 16 * 1024 * 1024, 20_000);
      await page.getByText(/Sent Big video\.mp4 to Nikhil's phone/).first().waitFor();
      // The retry continued from what the phone kept, instead of starting over.
      if (!/continuing Big video\.mp4 from [1-9]\d* bytes/.test(fs.readFileSync(path.join(out, "laptop.log"), "utf8")))
        throw new Error("retry started over instead of continuing");
    });

    await step("drag and drop a file onto the window opens the send sheet", async () => {
      await page.evaluate(() => {
        const dt = new DataTransfer();
        dt.items.add(new File([new Uint8Array(70_000)], "Dropped notes.txt", { type: "text/plain" }));
        for (const type of ["dragenter", "dragover", "drop"]) {
          window.dispatchEvent(new DragEvent(type, { dataTransfer: dt, bubbles: true, cancelable: true }));
        }
      });
      await page.locator(".sheet .r", { hasText: "Dropped notes.txt" }).waitFor();
      await page.locator('[data-act="root"]', { hasText: "Downloads" }).click();
      await page.locator('[data-act="dosend"]').click();
      const dest = path.join(phoneInbox, "Dropped notes.txt");
      await waitFor("dropped file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === 70_000, 15_000);
    });

    await step("get a file from the phone (browse its folder, tick, Get)", async () => {
      const name = "Meeting notes.txt";
      fs.rmSync(path.join(laptopDownloads, name), { force: true });
      const src = path.join(phoneData, "inbox", "Documents", name);
      fs.mkdirSync(path.dirname(src), { recursive: true });
      for (let i = 1; i <= 40; i++) fs.writeFileSync(path.join(path.dirname(src), `IMG_20261101_${String(i).padStart(2, "0")}.jpg`), crypto.randomBytes(3000));
      await sleep(50);
      fs.writeFileSync(src, crypto.randomBytes(300_000)); // newest: listed first
      await page.locator('.dev [data-act="get"]').first().click();
      await page.locator('[data-act="root"]', { hasText: "Documents" }).click();
      await page.locator(".r", { hasText: name }).locator('[data-act="pickfile"]').click();
      await page.locator('[data-act="dofetch"]', { hasText: /Get Meeting notes\.txt .* into Downloads/ }).waitFor();
      // However long the folder, the Get button is on screen without scrolling.
      const inView = await page.locator('[data-act="dofetch"]').evaluate((b) => {
        const r = b.getBoundingClientRect(), s = b.closest(".sheet").getBoundingClientRect();
        return r.bottom <= Math.min(window.innerHeight, s.bottom) + 1 && r.top >= s.top;
      });
      if (!inView) throw new Error("Get button is below the fold in a long folder");
      await settle(); await page.screenshot({ path: path.join(out, "w08d-get-sheet.png") });
      await page.locator('[data-act="dofetch"]').click();
      await page.getByText(`Got ${name} from Nikhil's phone`).first().waitFor({ timeout: 15_000 });
      const got = path.join(laptopDownloads, name);
      await waitFor("fetched file on the laptop", () => fs.existsSync(got) && fs.statSync(got).size === 300_000);
      await settle(); await page.screenshot({ path: path.join(out, "w08e-got.png") });
    });

    await step("tap a phone's file to open it; again from the cache; Save a copy", async () => {
      const name = "Holiday photo.jpg";
      const src = path.join(phoneData, "inbox", "Documents", name);
      fs.writeFileSync(src, crypto.randomBytes(450_000));
      fs.rmSync(openLog, { force: true });
      fs.rmSync(path.join(laptopDownloads, name), { force: true });
      await page.locator('.dev [data-act="get"]').first().click();
      await page.locator('[data-act="root"]', { hasText: "Documents" }).click();
      await page.locator('[data-act="openfile"]', { hasText: name }).click();
      const opened = () => fs.existsSync(openLog) ? fs.readFileSync(openLog, "utf8").trim().split("\n").filter(Boolean) : [];
      await waitFor("the file opened", () => opened().length === 1, 15_000);
      const cached = opened()[0];
      if (!cached.startsWith(path.join(laptopData, "cache"))) throw new Error("opened outside the cache: " + cached);
      if (!fs.readFileSync(cached).equals(fs.readFileSync(src))) throw new Error("opened file differs from the phone's");
      if (fs.existsSync(path.join(laptopDownloads, name))) throw new Error("opening also saved it in Downloads");
      // Again: straight from the cache, no second fetch.
      await page.locator('[data-act="openfile"]', { hasText: name }).click();
      await waitFor("opened again", () => opened().length === 2);
      const lg = fs.readFileSync(path.join(out, "laptop.log"), "utf8");
      if ((lg.match(/open: fetched Holiday photo\.jpg/g) || []).length !== 1 || !/open: Holiday photo\.jpg from the cache/.test(lg))
        throw new Error("the second open fetched it again");
      // Changed on the phone: fetched afresh.
      await page.locator('[data-act="close"]').first().click();
      fs.writeFileSync(src, crypto.randomBytes(460_000));
      await page.locator('.dev [data-act="get"]').first().click();
      await page.locator('[data-act="root"]', { hasText: "Documents" }).click();
      await page.locator('[data-act="openfile"]', { hasText: name }).click();
      await waitFor("changed file opened", () => opened().length === 3 && fs.statSync(opened()[2]).size === 460_000, 15_000);
      if (opened()[2] !== cached) throw new Error("changed file landed elsewhere: " + opened()[2]);
      await page.locator('[data-act="close"]').first().click();
      await page.locator(".tr", { hasText: `Opened ${name}` }).first().waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w08e2-opened.png") });
      await page.locator(".tr", { hasText: `Opened ${name}` }).first().locator('[data-act="savecopy"]').click();
      const saved = path.join(laptopDownloads, name);
      await waitFor("saved copy in Downloads", () => fs.existsSync(saved) && fs.statSync(saved).size === 460_000);
    });

    await step("“Send what's open”: the file in the window in front goes to the send sheet", async () => {
      const f = path.join(work, "Trip plan.docx");
      fs.writeFileSync(f, crypto.randomBytes(40_000));
      // As if Word showed "Trip plan - Word" and Windows listed the file as recently opened.
      await page.evaluate((f) => api("/api/sendopen", {title: "Trip plan - Word", recent: [f]}), f);
      await page.locator(".sheet .r", { hasText: "Trip plan.docx" }).waitFor();
      await page.locator('[data-act="root"]', { hasText: "Downloads" }).click();
      await page.locator('[data-act="dosend"]').click();
      const dest = path.join(phoneInbox, "Trip plan.docx");
      await waitFor("the open file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === 40_000, 15_000);
      // Can't tell: says so instead of sending something else.
      await page.evaluate(() => api("/api/sendopen", {title: "Untitled - Paint"}).catch(() => {}));
      await page.getByText("Couldn't tell which file is open").first().waitFor();
    });

    await step("assistant: typed “send this to my phone” sends the file in front (after 2 s)", async () => {
      const f = path.join(work, "Trip notes.txt");
      fs.writeFileSync(f, crypto.randomBytes(12_000));
      await page.evaluate((f) => api("/api/assistant/say", {text: "send this to my phone", title: "Trip notes.txt - Notepad", recent: [f]}), f);
      const card = page.locator(".card.assist");
      await card.getByText("Sending Trip notes.txt to Nikhil's phone").first().waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w08j-assistant.png") });
      const dest = path.join(phoneInbox, "Trip notes.txt");
      await waitFor("the file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === 12_000, 15_000);
    });

    await step("assistant: Cancel within 2 s stops it", async () => {
      const f = path.join(work, "Not this.txt");
      fs.writeFileSync(f, "x".repeat(500));
      await page.evaluate((f) => api("/api/assistant/say", {text: "send this to my phone", title: "Not this.txt - Notepad", recent: [f]}), f);
      await page.locator('.card.assist [data-act="acancel"]').click();
      await sleep(3000);
      if (fs.existsSync(path.join(phoneInbox, "Not this.txt"))) throw new Error("sent despite Cancel");
    });

    await step("assistant: the 🎤 button, “get the latest photo from my phone”", async () => {
      const pics = path.join(phoneData, "inbox", "Pictures");
      fs.mkdirSync(pics, { recursive: true });
      fs.writeFileSync(path.join(pics, "IMG_old.jpg"), crypto.randomBytes(5_000));
      await sleep(30);
      fs.writeFileSync(path.join(pics, "IMG_newest.jpg"), crypto.randomBytes(7_000));
      fs.rmSync(path.join(laptopDownloads, "IMG_newest.jpg"), { force: true });
      await page.locator("#micBtn").click();
      const card = page.locator(".card.assist");
      if (win) { // the real recogniser: allow for its start-up, and say so if Windows has none
        await card.getByText(/Getting IMG_newest\.jpg|Can't listen|Didn't catch/).first().waitFor({ timeout: 45_000 });
        const t = await card.innerText();
        if (/isn't installed/.test(t)) { log("      note: this Windows has no English speech recogniser; voice not tested here"); return; }
        if (!/Getting IMG_newest\.jpg/.test(t)) throw new Error("voice: " + t.replace(/\s+/g, " "));
      } else {
        await card.getByText("Getting IMG_newest.jpg from Nikhil's phone").first().waitFor({ timeout: 10_000 });
      }
      const got = path.join(laptopDownloads, "IMG_newest.jpg");
      await waitFor("the newest photo on the laptop", () => fs.existsSync(got) && fs.statSync(got).size === 7_000, 15_000);
    });

    await step("assistant: nonsense gets a helpful answer", async () => {
      await page.evaluate(() => api("/api/assistant/say", {text: "what's the weather"}));
      await page.locator(".card.assist").getByText("I didn't understand that").waitFor();
      await page.locator('.card.assist [data-act="adismiss"]').click();
    });

    await step("search the phone's folders and get a file from a subfolder", async () => {
      const name = "Budget 2026.xlsx";
      fs.rmSync(path.join(laptopDownloads, name), { force: true });
      const src = path.join(phoneData, "inbox", "Documents", "Work", "Finance", name);
      fs.mkdirSync(path.dirname(src), { recursive: true });
      fs.writeFileSync(src, crypto.randomBytes(120_000));
      await page.locator('.dev [data-act="get"]').first().click();
      await page.locator("#search").fill("budget");
      await page.locator("#search").press("Enter");
      await page.locator(".r", { hasText: name }).getByText("Documents / Work / Finance").waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w08h-search.png") });
      await page.locator('[data-act="gethit"]').first().click();
      const got = path.join(laptopDownloads, name);
      await waitFor("found file on the laptop", () => fs.existsSync(got) && fs.statSync(got).size === 120_000, 15_000);
    });

    await step("phone gets a file from the laptop", async () => {
      const name = "Slides.pptx";
      fs.writeFileSync(path.join(laptopDownloads, name), crypto.randomBytes(2_000_000));
      const dest = path.join(phoneData, "inbox", "Fetched", name);
      fs.rmSync(dest, { force: true });
      start("phone-fetch", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47823", "-fetch", "downloads:" + name]);
      await page.getByText(`Nikhil's phone got ${name}`).first().waitFor({ timeout: 15_000 });
      await waitFor("file on the phone", () => fs.existsSync(dest) && fs.statSync(dest).size === 2_000_000);
    });

    await step("shared clipboard: copy on the laptop, it's on the phone", async () => {
      const phoneLog = () => fs.readFileSync(path.join(out, "phone-slow.log"), "utf8");
      await page.evaluate(() => api("/api/clip/copy", {text: "Meeting at 5pm ✓"}));
      await waitFor("phone got the clipboard", () => phoneLog().includes('clipboard from DESKTOP-NIKHIL: "Meeting at 5pm ✓"'), 10_000);
      await page.getByText(/is ready to paste on Nikhil's phone/).waitFor();
      // Turned off: nothing is shared.
      await page.evaluate(() => api("/api/settings", {clip: false}));
      await page.evaluate(() => api("/api/clip/copy", {text: "private note"}));
      await sleep(2000);
      if (phoneLog().includes("private note")) throw new Error("clipboard shared while turned off");
      await page.evaluate(() => api("/api/settings", {clip: true}));
    });

    await step("shared clipboard: the phone sends text, ready for Ctrl+V here", async () => {
      start("phone-clip-push", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47861", "-clip-push", "https://example.com/from-phone"]);
      await waitFor("on the laptop's clipboard", async () => (await page.evaluate(() => api("/api/clip", {}))).text === "https://example.com/from-phone", 10_000);
      await page.locator(".card.clip", { hasText: "Copied from Nikhil's phone" }).getByText("https://example.com/from-phone").waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w08i-clipboard.png") });
      // Copied something else since: the card's Copy button puts the phone's text back.
      await page.evaluate(() => api("/api/clip/set", {text: "something else"}));
      await page.locator('[data-act="clipcopy"]').click();
      await waitFor("restored", async () => (await page.evaluate(() => api("/api/clip", {}))).text === "https://example.com/from-phone");
    });

    await step("shared clipboard: copied on the phone by itself: offered with Copy, not pasted over", async () => {
      await page.evaluate(() => api("/api/clip/set", {text: "keep me"}));
      start("phone-clip-auto", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47862", "-clip-push", "Auto copy 5512", "-clip-auto"]);
      const card = page.locator(".card.clip", { hasText: "Copied on Nikhil's phone" });
      await card.getByText("Auto copy 5512").waitFor({ timeout: 10_000 });
      if ((await page.evaluate(() => api("/api/clip", {}))).text !== "keep me") throw new Error("replaced the clipboard without asking");
      await settle(); await page.screenshot({ path: path.join(out, "w08i2-clip-offered.png") });
      await card.locator('[data-act="clipcopy"]').click();
      await waitFor("copied after clicking Copy", async () => (await page.evaluate(() => api("/api/clip", {}))).text === "Auto copy 5512");
      // "Paste right away" setting: straight in.
      await page.evaluate(() => api("/api/settings", {clipNow: true}));
      start("phone-clip-auto2", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47863", "-clip-push", "Auto copy 6623", "-clip-auto"]);
      await waitFor("pasted right away", async () => (await page.evaluate(() => api("/api/clip", {}))).text === "Auto copy 6623", 10_000);
      await page.evaluate(() => api("/api/settings", {clipNow: false}));
    });

    await step('shared clipboard: "Paste from this phone"', async () => {
      // LAN Share isn't on screen on the phone: a clear message.
      await page.locator('.dev [data-act="devmenu"]').first().click();
      await page.locator('[data-act="pastefrom"]').click();
      await page.getByText(/isn't open on the phone/).waitFor();
      // It is: its clipboard comes over.
      procs[1].kill("SIGKILL");
      await sleep(300);
      procs[1] = start("phone-clip", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47821", "-clip-text", "Copied on the phone 📱"]);
      await sleep(1500);
      await page.locator('.dev [data-act="devmenu"]').first().click();
      await page.locator('[data-act="pastefrom"]').click();
      await page.getByText(/Copied \d+ characters from Nikhil's phone/).waitFor();
      const r = await page.evaluate(() => api("/api/clip", {}));
      if (r.text !== "Copied on the phone 📱") throw new Error("laptop clipboard: " + r.text);
    });

    await step('"Send to" a whole folder, into a new folder made on the phone', async () => {
      const trip = path.join(work, "Trip");
      fs.mkdirSync(path.join(trip, "Day 2"), { recursive: true });
      fs.writeFileSync(path.join(trip, "a.jpg"), crypto.randomBytes(50_000));
      fs.writeFileSync(path.join(trip, "Day 2", "b.jpg"), crypto.randomBytes(60_000));
      fs.writeFileSync(path.join(trip, ".hidden"), "x");
      execFileSync(lanshare, ["-data", laptopData, "send", trip], { env: { ...process.env, HOME: home }, timeout: 10_000 });
      await page.locator(".sheet .r", { hasText: "Trip" }).getByText("2 files").waitFor();
      await page.locator('[data-act="root"]', { hasText: "Downloads" }).click();
      await page.locator('[data-act="newdir"]').click();
      await page.locator("#newdir").fill("Holiday");
      await page.locator('[data-act="mkdir"]').click();
      await page.locator(".crumbs", { hasText: "Holiday" }).waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w08f-folder-send.png") });
      await page.locator('[data-act="dosend"]').click();
      await page.getByText(/Sent folder Trip \(2 files\) to Nikhil's phone/).first().waitFor({ timeout: 15_000 });
      for (const [p, n] of [["Holiday/Trip/a.jpg", 50_000], ["Holiday/Trip/Day 2/b.jpg", 60_000]]) {
        const f = path.join(phoneInbox, ...p.split("/"));
        if (!fs.existsSync(f) || fs.statSync(f).size !== n) throw new Error("missing on the phone: " + p);
      }
      if (fs.existsSync(path.join(phoneInbox, "Holiday", "Trip", ".hidden"))) throw new Error("hidden file was sent");
    });

    await step("ask before receiving: decline, then accept", async () => {
      await page.locator("#settingsBtn").click();
      await page.locator('[data-act="toggleask"]').click();
      await waitFor("setting on", () => page.evaluate(() => S.me.ask));
      await page.keyboard.press("Escape");
      const f = path.join(work, "Contract.pdf");
      fs.writeFileSync(f, crypto.randomBytes(200_000));
      fs.rmSync(path.join(laptopDownloads, "Contract.pdf"), { force: true });
      const ask = async (n) => {
        procs.push(start("phone-ask" + n, fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", String(47830 + n), "-send", f]));
        await page.getByText("Nikhil's phone wants to send Contract.pdf").waitFor({ timeout: 15_000 });
      };
      await ask(1);
      await settle(); await page.screenshot({ path: path.join(out, "w08g-ask.png") });
      await page.locator('[data-act="decline"]').click();
      await waitFor("phone told it was declined", () => /declined the files/.test(fs.readFileSync(path.join(out, "phone-ask1.log"), "utf8")));
      if (fs.existsSync(path.join(laptopDownloads, "Contract.pdf"))) throw new Error("declined file was saved");
      await ask(2);
      await page.locator('[data-act="accept"]').click();
      await waitFor("accepted file saved", () => fs.existsSync(path.join(laptopDownloads, "Contract.pdf")) &&
        fs.statSync(path.join(laptopDownloads, "Contract.pdf")).size === 200_000, 15_000);
      await page.locator("#settingsBtn").click();
      await page.locator('[data-act="toggleask"]').click();
      await waitFor("setting off", () => page.evaluate(() => !S.me.ask));
      await page.keyboard.press("Escape");
    });

    await step("settings: make another folder the default, rename the laptop", async () => {
      await page.locator("#settingsBtn").click();
      await page.locator('.r', { hasText: "Documents" }).locator('[data-act="setdefault"]').click();
      await waitFor("Documents is default", () => page.evaluate(() => S.folders.find((f) => f.key === "documents").default));
      await page.locator('.r', { hasText: "Downloads" }).locator('[data-act="setdefault"]').click();
      await page.locator('[data-act="rename"]').click();
      await page.locator("#newname").fill("NIKHIL-PC");
      await page.locator('[data-act="dorename"]').click();
      await page.locator("h1", { hasText: "NIKHIL-PC" }).waitFor().catch(async () => {
        if (!(await page.evaluate(() => S.me.name === "NIKHIL-PC"))) throw new Error("name not changed");
      });
      await page.keyboard.press("Escape");
    });

    await step("settings shows shared folders", async () => {
      await page.locator("#settingsBtn").click();
      await page.getByText("Shared folders", { exact: true }).waitFor();
      await page.getByText("Add a folder…").waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w09-settings.png"), fullPage: true });
      await page.keyboard.press("Escape");
    });

    await step("dark mode renders", async () => {
      await page.emulateMedia({ colorScheme: "dark" });
      await sleep(300);
      await settle(); await page.screenshot({ path: path.join(out, "w10-dark.png") });
      await page.emulateMedia({ colorScheme: "light" });
    });

    await step("phone that can't reach the laptop triggers the firewall warning", async () => {
      // Restart the simulated phone so it reports it can't connect back.
      procs[1].kill("SIGKILL");
      await sleep(300);
      start("phone-blocked", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47821", "-cant-reach"]);
      await page.getByText("can't connect to this laptop").first().waitFor({ timeout: 20_000 });
      await settle(); await page.screenshot({ path: path.join(out, "w11-firewall.png") });
    });

    await step("a phone renamed on the phone shows its new name here", async () => {
      for (const p of procs.slice(1)) p.kill("SIGKILL");
      await sleep(300);
      procs.push(start("phone-renamed", fakephone, ["-data", phoneData, "-name", "Nikhil's Pixel", "-port", "47821"]));
      await page.locator(".dev .t1", { hasText: "Nikhil's Pixel" }).waitFor({ timeout: 20_000 });
    });

    await step("phone going away shows offline", async () => {
      for (const p of procs.slice(1)) p.kill("SIGKILL");
      await page.getByText(/^Offline/).first().waitFor({ timeout: 25_000 });
      await settle(); await page.screenshot({ path: path.join(out, "w12-offline.png") });
    });

    await step("shared clipboard: a phone that was away gets the last copy when it's back", async () => {
      await page.evaluate(() => api("/api/clip/copy", {text: "Copied while the phone slept"}));
      await sleep(1500);
      start("phone-back", fakephone, ["-data", phoneData, "-name", "Nikhil's phone", "-port", "47821"]);
      const lg = () => fs.readFileSync(path.join(out, "phone-back.log"), "utf8");
      await waitFor("the missed copy on the phone", () => /clipboard from [^:]+: "Copied while the phone slept"/.test(lg()), 25_000);
      await page.getByText(/^Online/).first().waitFor();
      procs[procs.length - 1].kill("SIGKILL");
      await page.getByText(/^Offline/).first().waitFor({ timeout: 25_000 });
    });

    await step("forget a phone: it's gone, and can't connect any more", async () => {
      await page.locator('.dev [data-act="devmenu"]').first().click();
      await page.locator('[data-act="forget"]').click();
      await page.locator('[data-act="confirmok"]').click();
      await page.getByText("Pair your phone").waitFor();
      // The forgotten phone is refused now.
      const f = path.join(work, "After forget.txt");
      fs.writeFileSync(f, "x");
      start("phone-forgotten", fakephone, ["-data", phoneData, "-name", "Nikhil's Pixel", "-port", "47851", "-send", f]);
      const lg = () => fs.readFileSync(path.join(out, "phone-forgotten.log"), "utf8");
      await waitFor("refused", () => /send: .*(bad certificate|not_trusted)|sent to .*: \[\] .+/.test(lg()), 30_000)
        .catch((e) => { throw new Error(e.message + " (phone said: " + lg().trim().split("\n").slice(-3).join(" | ") + ")"); });
      if (fs.existsSync(path.join(laptopDownloads, "After forget.txt"))) throw new Error("a forgotten phone could still send");
    });

    await step("log file has the story", async () => {
      const logTxt = fs.readFileSync(path.join(laptopData, "lanshare.log"), "utf8");
      for (const want of ["Paired with Nikhil's phone", "Received Scan", "Sent Holiday photos.zip"]) {
        if (!logTxt.includes(want)) throw new Error(`log is missing "${want}"`);
      }
      fs.copyFileSync(path.join(laptopData, "lanshare.log"), path.join(out, "lanshare.log"));
    });

    await step("no errors in the window's console", async () => {
      if (errors.length) throw new Error(errors.join(" | "));
    });

    await step("second launch hands over instead of starting twice", async () => {
      execFileSync(lanshare, ["-data", laptopData, "-no-window"], { env: { ...process.env, HOME: home }, timeout: 5_000 });
      if (procs[0].exitCode !== null) throw new Error("first copy died");
    });

    if (win) await step("Windows: the floating bubble is on screen, and hides from Settings", async () => {
      await waitFor("the bubble", () => bubbleVisible(), 15_000).catch((e) => { throw new Error(e.message + " (saw: " + bubbleSaw + ")"); });
      try {
        execFileSync("powershell", ["-NoProfile", "-Command",
          `Add-Type -AssemblyName System.Windows.Forms,System.Drawing; $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds; ` +
          `$i = New-Object System.Drawing.Bitmap $b.Width, $b.Height; $g = [System.Drawing.Graphics]::FromImage($i); ` +
          `$g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size); $i.Save('${path.join(out, "w20-desktop-bubble.png")}')`], { timeout: 30_000 });
      } catch {}
      await page.evaluate(() => api("/api/settings", {bubble: false}));
      await waitFor("the bubble hidden", () => !bubbleVisible(), 10_000);
      await page.evaluate(() => api("/api/settings", {bubble: true}));
      await waitFor("the bubble back", () => bubbleVisible(), 10_000);
    });

    await step("Quit stops the app", async () => {
      await page.locator("#settingsBtn").click();
      await page.getByRole("button", { name: "Quit LAN Share" }).click();
      await waitFor("process exit", () => procs[0].exitCode !== null, 5_000);
    });

    await step("history is still there after restarting", async () => {
      fs.rmSync(path.join(out, "laptop.log"), { force: true });
      procs[0] = start("laptop", lanshare, ["-data", laptopData, "-port", "47911", "-name", "DESKTOP-NIKHIL", "-no-window"]);
      const url2 = await waitFor("UI address", () => {
        const txt = fs.existsSync(path.join(out, "laptop.log")) ? fs.readFileSync(path.join(out, "laptop.log"), "utf8") : "";
        const m = txt.match(/UI: (http\S+)/);
        return m && m[1];
      });
      await page.goto(url2);
      await page.getByText("Sent Holiday photos.zip to Nikhil's phone").first().waitFor();
    });
  } catch (e) {
    if (!failures) { failures++; log(`FAIL  ${e.message}`); } // a crash outside any step still fails the run
    if (page) await page.screenshot({ path: path.join(out, "failure.png") }).catch(() => {});
  }
  await cleanup(failures ? 1 : 0);
})();
