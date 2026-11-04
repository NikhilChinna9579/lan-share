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
  const f = fs.createWriteStream(path.join(out, name + ".log"));
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
setTimeout(() => { log("TIMEOUT: the whole run took over 120 s"); failures++; cleanup(2); }, 120_000).unref();

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
    await Promise.race([fn(), sleep(30_000).then(() => { throw new Error("step took over 30 s"); })]);
    log(`ok    ${name} (${Date.now() - s} ms)`);
  } catch (e) {
    failures++;
    log(`FAIL  ${name}: ${e.message}`);
    throw e;
  }
}

(async () => {
  const laptopData = path.join(work, "laptop");
  const phoneData = path.join(work, "phone");
  const pairFile = path.join(work, "pair.json");
  const phoneInbox = path.join(phoneData, "inbox", "Download");
  const laptopDownloads = path.join(home, "Downloads");
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
    page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });

    await step("window opens and shows the empty state", async () => {
      await page.goto(url);
      await page.getByText("Pair your phone").waitFor();
      await page.getByText("Ready to receive").waitFor();
      await settle(); await page.screenshot({ path: path.join(out, "w01-empty.png") });
    });

    await step("pairing code appears", async () => {
      await page.getByRole("button", { name: "Show pairing code" }).click();
      await page.locator(".qr img").waitFor();
      await waitFor("pairing file", () => fs.existsSync(pairFile));
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
    });

    await step("settings shows shared folders", async () => {
      await page.locator("#settingsBtn").click();
      await page.getByText("Shared folders").waitFor();
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

    await step("phone going away shows offline", async () => {
      for (const p of procs.slice(1)) p.kill("SIGKILL");
      await page.getByText(/^Offline/).first().waitFor({ timeout: 25_000 });
      await settle(); await page.screenshot({ path: path.join(out, "w12-offline.png") });
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
  } catch {
    if (page) await page.screenshot({ path: path.join(out, "failure.png") }).catch(() => {});
  }
  await cleanup(failures ? 1 : 0);
})();
