#!/usr/bin/env bash
# End-to-end check on an Android emulator against the real laptop program (Linux build).
# Every command has a time limit and nothing waits for input, so this can't hang.
#
# Checks wait on the app's own log lines ("LanShare" in logcat) and on real files,
# not on what the screen happens to show at that instant: that is what made earlier
# runs flaky on a slow emulator. Screenshots are still taken, for people to look at.
# Results: check-out/RESULTS.txt (one line per step), screenshots, logs.
set -u
PKG=com.nikhil.lanshare
OUT=$PWD/check-out
WORK=$(mktemp -d)
LAPTOP_DATA=$WORK/laptop
export HOME=$WORK/home                     # the laptop program's home (its Downloads etc.)
mkdir -p "$OUT" "$HOME/Downloads" "$HOME/Documents" "$HOME/Desktop" "$HOME/Pictures"
FAILS=0
T0=$(date +%s)

log()  { echo "[$(( $(date +%s) - T0 ))s] $*" | tee -a "$OUT/RESULTS.txt"; }
pass() { log "ok    $*"; }
fail() { log "FAIL  $*"; FAILS=$((FAILS + 1))
         timeout 20 adb exec-out screencap -p > "$OUT/FAIL-$FAILS.png" 2>/dev/null   # what the screen showed
         A logcat -d -s LanShare:V 2>/dev/null | tail -25 > "$OUT/FAIL-$FAILS-log.txt"; }
A()    { timeout 30 adb "$@"; }                                   # adb, never more than 30 s
shot() { sleep 1; timeout 20 adb exec-out screencap -p > "$OUT/$1.png" 2>/dev/null || true; }
# Close the notification shade if anything left it open (it would cover the app and swallow taps).
shade_closed() { A shell cmd statusbar collapse >/dev/null 2>&1; }
app()  { shade_closed; A shell am force-stop $PKG; A shell am start -n $PKG/.MainActivity >/dev/null; sleep "${1:-4}"; }
# Write a file into the app's private folder (debug builds only), without using stdin.
hook() { A shell "run-as $PKG sh -c 'mkdir -p files && echo $(printf '%s' "$2" | base64 -w0) | base64 -d > files/$1'"; }
night(){ A shell cmd uimode night "$1" >/dev/null; sleep 2; }

# The app's log. mark remembers where it ends; seen finds a line after the mark.
applog() { A logcat -d -s LanShare:V 2>/dev/null; }
mark()   { MARK=$(applog | wc -l); }
seen()   { applog | tail -n +$((MARK + 1)) | grep -qE "$1"; }
# Wait up to $1 seconds for a shell condition.
until_ok() { local n=$1; shift; for _ in $(seq 1 "$n"); do eval "$@" && return 0; sleep 1; done; return 1; }
# expect SECONDS 'log pattern' 'what it means'
expect() { if until_ok "$1" "seen '$2'"; then pass "$3"; else fail "$3 (no log line matching: $2)"; fi; }
phone_size() { A shell stat -c %s "'$1'" 2>/dev/null | tr -d '\r'; }

# Laptop program's local API (the same calls its window makes).
inst() { python3 -c "import json;print(json.load(open('$LAPTOP_DATA/instance.json'))['$1'])"; }
api()  { local body=${2:-'{}'}; timeout 20 curl -s -X POST -H "X-Token: $(inst token)" "http://127.0.0.1:$(inst port)$1" -d "$body"; }
get()  { timeout 20 curl -s -H "X-Token: $(inst token)" "http://127.0.0.1:$(inst port)$1"; }
state(){ get /api/state; }

# Tap the on-screen element whose text (or resource id) contains $1.
# Long-press the element whose text contains $1 (e.g. a device row, to forget it).
long_press_text() {
  shade_closed
  A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1
  A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1 || return 1
  local xy
  xy=$(python3 - "$1" "$WORK/ui.xml" <<'PY'
import re, sys, xml.etree.ElementTree as ET
for n in ET.parse(sys.argv[2]).iter("node"):
    if sys.argv[1] in (n.get("text") or ""):
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", n.get("bounds"))); print((x1 + x2) // 2, (y1 + y2) // 2); break
PY
)
  [ -n "$xy" ] || return 1
  A shell input swipe $xy $xy 900
}
tap_text() {
  [ -n "${KEEP_SHADE:-}" ] || shade_closed   # (except when tapping inside the notification shade)
  A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1
  A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1 || return 1
  local xy
  xy=$(python3 - "$1" "$WORK/ui.xml" <<'PY'
import re, sys, xml.etree.ElementTree as ET
want, path = sys.argv[1], sys.argv[2]
# Exact text first (case doesn't matter: Android shows dialog buttons in capitals, and a
# title like "Forget Office PC?" must not win over the "FORGET" button), then contains.
nodes = list(ET.parse(path).iter("node"))
w = want.lower()
for test in (lambda t: t == w, lambda t: w in t):
    hit = next((n for n in nodes if test((n.get("text") or "").lower()) or n.get("resource-id", "").endswith("/" + want)), None)
    if hit is not None:
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", hit.get("bounds")))
        print((x1 + x2) // 2, (y1 + y2) // 2); break
PY
)
  [ -n "$xy" ] || return 1
  A shell input tap $xy
}
# Tap a button by the position the app logged for it ("ui: Cancel at 540,812"): works
# even while a progress bar keeps the screen from ever being idle.
tap_logged() {
  local xy
  xy=$(applog | tail -n +$((MARK + 1)) | grep -oE "ui: $1 at [0-9]+,[0-9]+" | tail -1 | grep -oE '[0-9]+,[0-9]+$')
  [ -n "$xy" ] || return 1
  shade_closed
  A shell input tap "${xy%,*}" "${xy#*,}"
}
# Run a test hook and wait until the app has actually run it (a slow emulator can take
# several seconds to start).
run_hook() { mark; hook demo.txt "$1"; app 2; until_ok 30 "seen 'test hook: $1'" || log "note  hook $1 didn't run"; }
# Restart the app and wait until it's ready to receive again.
app_ready() { mark; app 2; until_ok 30 "seen 'receiving on port'" || log "note  receiving didn't come back in 30 s"
              sleep 2; until_ok 30 "state | grep -q '\"status\":\"online\"'" || log "note  the laptop doesn't see the phone online"; }
screen_has() { A shell rm -f /sdcard/ui.xml; A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; A shell cat /sdcard/ui.xml 2>/dev/null | grep -q "$1"; }

cleanup() {
  A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
  A logcat -d > "$OUT/logcat-full.txt" 2>&1
  applog > "$OUT/app-log.txt"
  cp "$LAPTOP_DATA/lanshare.log" "$OUT/laptop-lanshare.log" 2>/dev/null
  kill "$LAPTOP_PID" 2>/dev/null
}
trap cleanup EXIT

# ---------------------------------------------------------------- 1. install, first screen
A logcat -G 16M >/dev/null 2>&1   # big log buffer: the checks read it
A install -r android/app/build/outputs/apk/debug/app-debug.apk >/dev/null && pass "install APK" || fail "install APK"
for p in POST_NOTIFICATIONS READ_MEDIA_IMAGES READ_MEDIA_VIDEO READ_MEDIA_AUDIO; do A shell pm grant $PKG android.permission.$p 2>/dev/null; done
# A slow CI emulator sometimes shows "Process system isn't responding" over everything.
A shell settings put global hide_error_dialogs 1
A shell settings put global anr_show_background 0
A forward tcp:47821 tcp:47821   # lets the laptop program reach the phone's receiver
crashed() { A logcat -d -b crash 2>/dev/null | grep -q "Process: $PKG"; }
bail_if_crashed() { if crashed; then fail "app crashed during: $1"; A logcat -d -b crash > "$OUT/crashes.txt"; log "stopping early"; exit 1; fi; }
A logcat -b crash -c
mark; night no; app
expect 30 'screen: unpaired' "first launch shows pairing"
# First start asks the usual Android way: its own dialogs one after another.
# (Notifications and media were granted above, so it goes straight to the next question.)
expect 10 'setup: asking for 0 permission' "first start asks with Android's own dialogs (only what isn't allowed yet)"
expect 15 'setup: background' "…then “always run in the background?”"
sleep 2; shot 00-setup-background
until_ok 10 "tap_text Allow"
expect 15 'setup: tile' "…then “add the Send copied text tile?”"
sleep 2; shot 00-setup-tile
until_ok 10 "tap_text 'Add tile'"
expect 15 'setup: add tile' "…the tile answer comes back"
expect 10 'setup: done' "…and setup is done"
expect 5 'version: [0-9.]+ · built [0-9]+ [A-Z][a-z]+ 20[0-9]{2}' "home screen shows which build this is (build time)"
shot 01-unpaired-light
bail_if_crashed "first launch"
night yes; mark; app; shot 01-unpaired-dark; night no
seen 'setup: shown' && fail "the setup screen came back on the next start" || pass "…only on the first start"
# ---------------------------------------------------------------- 2. laptop program + pairing
./bin/lanshare -data "$LAPTOP_DATA" -port 47900 -name DESKTOP-NIKHIL -no-window -pairing-file "$WORK/pair.json" > "$OUT/laptop-stdout.log" 2>&1 &
LAPTOP_PID=$!
until_ok 10 "[ -f '$LAPTOP_DATA/instance.json' ]" && pass "laptop program started" || fail "laptop program started"
api /api/pair >/dev/null
until_ok 10 "[ -f '$WORK/pair.json' ]" && pass "laptop opened pairing" || fail "laptop opened pairing"
# 10.0.2.2 is how the emulator reaches the machine it runs on.
hook test_pair.txt "$(python3 -c "import json;q=json.load(open('$WORK/pair.json'));q['host']='10.0.2.2';print(json.dumps(q))")"
mark; app
expect 30 'pair: ok with DESKTOP-NIKHIL' "pairing works"
expect 15 'sheet: Shared folders' "shared-folder choice shown after pairing"
shot 02-shared-folders-after-pairing
bail_if_crashed "pairing"
A shell input keyevent KEYCODE_BACK; sleep 1
until_ok 30 "state | grep -q '\"status\":\"online\"'" && pass "laptop sees the phone online" || fail "laptop sees the phone online"
expect 40 'presence: .* -> ONLINE' "phone sees the laptop online"
shot 03-home-online
DEV=$(state | python3 -c "import json,sys;print(json.load(sys.stdin)['devices'][0]['id'])")

# ---------------------------------------------------------------- 3. laptop -> phone
head -c 3000000 /dev/urandom > "$WORK/Quarterly report.pdf"
mark
timeout 60 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Quarterly report.pdf" > "$OUT/send-to-phone.log" 2>&1 \
  && pass "laptop sent a file to the phone" || fail "laptop sent a file to the phone ($(tail -1 "$OUT/send-to-phone.log"))"
SIZE=$(phone_size "/sdcard/Download/Quarterly report.pdf")
[ "$SIZE" = "3000000" ] && pass "file arrived on the phone intact size" || fail "file arrived on the phone (size: '$SIZE')"
expect 10 'activity: received: Received Quarterly report.pdf .*in ' "phone shows it, with the time taken"
shot 04-received-on-phone
A shell cmd statusbar expand-notifications; sleep 2; shot 05-notification
shade_closed; sleep 1; A shell input keyevent KEYCODE_BACK; sleep 1; shade_closed

# ---------------------------------------------------------------- 4. laptop gets a file from the phone
LIST=$(get "/api/remote/list?device=$DEV&folder=downloads&path=")
echo "$LIST" | grep -q 'Quarterly report.pdf' && pass "laptop lists the phone's Downloads" || fail "laptop lists the phone's Downloads ($LIST)"
get "/api/remote/search?device=$DEV&q=quarterly" | grep -q 'Quarterly report.pdf' && pass "laptop searches the phone's folders" \
  || fail "laptop searches the phone's folders"
rm -f "$HOME/Downloads/Quarterly report.pdf"
mark
api /api/fetch "{\"device\":\"$DEV\",\"folder\":\"downloads\",\"folderLabel\":\"Downloads\",\"path\":\"\",\"names\":[\"Quarterly report.pdf\"]}" >/dev/null
until_ok 30 "[ \"\$(stat -c %s '$HOME/Downloads/Quarterly report.pdf' 2>/dev/null)\" = 3000000 ]" \
  && pass "laptop got a file from the phone, intact size" || fail "laptop got a file from the phone"
expect 10 'served 1 files to DESKTOP-NIKHIL' "phone logs what it served"

# ---------------------------------------------------------------- 4a. shared clipboard
mark; api /api/clip/copy '{"text":"Meeting at 5pm ✓"}' >/dev/null
expect 15 'clipboard from DESKTOP-NIKHIL: 16 chars, offered' "copied on the laptop: a notification with a Copy button (the default)"
shade_open() { A shell cmd statusbar expand-notifications; sleep 2; }
shade_open; shot 06b-clip-notification
until_ok 10 "KEEP_SHADE=1 tap_text Copy"; sleep 1; shade_closed
expect 10 'clipboard: copied what came from DESKTOP-NIKHIL' "…Copy puts it on the phone's clipboard"
MS=$(applog | tail -n +$((MARK + 1)) | grep -oE 'Copy took [0-9]+ ms' | tail -1 | grep -oE '[0-9]+' || true)
[ -n "$MS" ] && [ "$MS" -lt 500 ] && pass "…at once (${MS} ms), no repeated tapping" || fail "Copy button was slow (${MS:-?} ms)"
run_hook ask_clip_off
mark; api /api/clip/copy '{"text":"Straight in 9921"}' >/dev/null
expect 15 'clipboard from DESKTOP-NIKHIL: 16 chars \(on the clipboard\)' "with asking off: straight onto the phone's clipboard"
run_hook ask_clip_on
mark; hook demo.txt send_clip; app 3
expect 20 'clipboard sent to DESKTOP-NIKHIL: [0-9]+ chars' "the phone sends what's copied"
until_ok 10 "api /api/clip | grep -q 'Hello from the phone'" && pass "…and it's on the laptop's clipboard" || fail "phone's text on the laptop's clipboard"
api /api/clip/set '{"text":"From the laptop 💻"}' >/dev/null
mark; hook demo.txt paste_from; app 3
expect 20 'clipboard pasted from DESKTOP-NIKHIL' "paste from the laptop"
R=$(api /api/remote/clip "{\"device\":\"$DEV\"}")
echo "$R" | grep -q '"chars"' && pass "laptop pastes from the phone while LAN Share is open there" || fail "laptop paste from phone ($R)"
api /api/clip | grep -q 'From the laptop' && pass "…the phone's clipboard came over" || fail "phone's clipboard on the laptop"
A shell input keyevent KEYCODE_HOME; sleep 4
api /api/remote/clip "{\"device\":\"$DEV\"}" | grep -q "isn't open" && pass "…and says why not when it isn't open" || fail "paste from a phone in the background"
A shell am start -a android.intent.action.SEND -t text/plain --es android.intent.extra.TEXT "https://example.com/shared" -n $PKG/.MainActivity >/dev/null
until_ok 20 "api /api/clip | grep -q 'example.com/shared'" && pass "Share → LAN Share with text puts it on the laptop's clipboard" || fail "shared text to the laptop"
# Copied on the phone, then LAN Share opened: it goes across by itself.
run_hook "copy_only:Copied on the phone 4417"; sleep 2
mark; app 3
expect 15 'clipboard auto: [0-9]+ chars to .*DESKTOP-NIKHIL' "copied on the phone: sent by itself when LAN Share opens"
until_ok 15 "get /api/state | grep -q 'Copied on the phone 4417'" && pass "…and the laptop offers it (Copy button)" || fail "auto-sent copy offered on the laptop"
api /api/clip | grep -q 'Copied on the phone 4417' && fail "the laptop replaced its clipboard without asking" || pass "…without replacing the laptop's clipboard"
api /api/clip/again >/dev/null
api /api/clip | grep -q 'Copied on the phone 4417' && pass "…Copy on the laptop puts it there" || fail "laptop Copy of the offered text"
mark; app 3; sleep 3
seen 'clipboard auto:' && fail "the same copy was sent again" || pass "…only once, not on every open"
# Quick Settings tile "Send copied text": copy in another app, tap the tile.
run_hook "copy_only:Tile copy 5150"; sleep 2
A shell cmd statusbar add-tile $PKG/.ClipTile >/dev/null 2>&1
until_ok 10 "A shell settings get secure sysui_qs_tiles | grep -q ClipTile" || log "note  the tile wasn't added: $(A shell settings get secure sysui_qs_tiles)"
# Android delivers a tile's tap only while Quick Settings is open (as when a person taps it).
A shell cmd statusbar expand-settings; sleep 3
mark; A shell cmd statusbar click-tile $PKG/.ClipTile 2>&1 | head -2
expect 15 'clip send: shown, [1-9][0-9]* device\(s\), 14 chars' "the tile opens a small window with what's copied and the devices"
sleep 1; shot 06d-clip-send-window
# In the middle of the screen (not a sheet from the bottom).
A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1
H=$(A shell wm size | grep -oE '[0-9]+x[0-9]+' | tail -1 | cut -dx -f2)
MID=$(python3 - "$WORK/ui.xml" "$H" <<'PY'
import re, sys, xml.etree.ElementTree as ET
h = int(sys.argv[2]); ys = []
for n in ET.parse(sys.argv[1]).iter("node"):
    if n.get("text") in ("Send copied text", "Cancel") and n.get("package") == "com.nikhil.lanshare":
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", n.get("bounds"))); ys += [y1, y2]
print("yes" if ys and min(ys) > h * 0.2 and max(ys) < h * 0.8 else "no %s of %d" % (ys, h))
PY
)
[ "$MID" = yes ] && pass "…in the middle of the screen" || fail "Send copied text window position ($MID)"
until_ok 10 "tap_logged 'Send DESKTOP-NIKHIL'"
expect 15 'clip send: to DESKTOP-NIKHIL' "Send beside the laptop sends it"
until_ok 10 "! A shell dumpsys activity activities 2>/dev/null | grep -q 'ClipSendActivity'" && pass "…and the window closes" || fail "Send copied text window stayed open"
until_ok 15 "get /api/state | grep -q 'Tile copy 5150'" && pass "…and the laptop shows it in its Copy box" || fail "tile copy offered on the laptop"
api /api/clip | grep -q 'Tile copy 5150' && fail "the laptop's clipboard was replaced without Copy" || pass "…its clipboard untouched until Copy"
A shell dumpsys notification --noredact 2>/dev/null | grep -q 'Send copied text' \
  && pass "LAN Share's notification has a “Send copied text” button" || fail "“Send copied text” on the notification"
A shell cmd statusbar remove-tile $PKG/.ClipTile >/dev/null 2>&1; shade_closed
# LAN Share's notification ("Send copied text") can be swiped away on Android 14+; the next
# unlock brings it back.
note_up() { A shell dumpsys notification --noredact 2>/dev/null | grep -q 'Ready to receive files'; }
A shell cmd statusbar expand-notifications; sleep 2
A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1
XY=$(python3 - "$WORK/ui.xml" <<'PY'
import re, sys, xml.etree.ElementTree as ET
for n in ET.parse(sys.argv[1]).iter("node"):
    if (n.get("text") or "") == "Ready to receive files":
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", n.get("bounds"))); print(x1 + 10, (y1 + y2) // 2); break
PY
)
[ -n "$XY" ] && A shell input swipe $XY $(( ${XY% *} + 700 )) ${XY#* } 250
sleep 2; shade_closed
if note_up; then
  log "note  couldn't swipe the notification away here; unlock check skipped"
else
  pass "LAN Share's notification can be swiped away"
  mark; A shell input keyevent KEYCODE_SLEEP; sleep 2; A shell input keyevent KEYCODE_WAKEUP; sleep 1
  A shell wm dismiss-keyguard; sleep 3
  expect 15 'ready note: shown again \(unlock\)' "…and it's back after the next unlock"
  note_up && A shell dumpsys notification --noredact 2>/dev/null | grep -q 'Send copied text' \
    && pass "…with its “Send copied text” button" || fail "notification back with Send copied text"
fi
bail_if_crashed "clipboard"

# ---------------------------------------------------------------- 4b. folders, new folder, asking, browsing off
mkdir -p "$WORK/Trip/Day 2"; head -c 70000 /dev/urandom > "$WORK/Trip/a.jpg"; head -c 80000 /dev/urandom > "$WORK/Trip/Day 2/b.jpg"
timeout 60 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Trip" > "$OUT/send-folder.log" 2>&1
[ "$(phone_size '/sdcard/Download/Trip/Day 2/b.jpg')" = 80000 ] && [ "$(phone_size '/sdcard/Download/Trip/a.jpg')" = 70000 ] \
  && pass "laptop sent a whole folder; layout kept on the phone" || fail "whole folder on the phone ($(tail -1 "$OUT/send-folder.log"))"
api /api/remote/mkdir "{\"device\":\"$DEV\",\"folder\":\"downloads\",\"path\":\"\",\"name\":\"Projects\"}" > "$WORK/mkdir.json"
get "/api/remote/list?device=$DEV&folder=downloads&path=" | grep -q '"Projects"' \
  && pass "laptop made a new folder on the phone" || fail "new folder on the phone ($(cat "$WORK/mkdir.json"))"
hook demo.txt ask_on; app_ready
head -c 90000 /dev/urandom > "$WORK/Contract.pdf"
( for _ in 1 2 3 4; do
    timeout 70 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Contract.pdf" >> "$OUT/send-ask.log" 2>&1 && break
    grep -q "no phone online" "$OUT/send-ask.log" || break; sleep 5
  done & )
expect 20 'approval: asking about Contract.pdf' "phone asks before receiving"
shot 05b-asking
until_ok 10 "tap_logged Accept"
expect 15 'approval: accepted Contract.pdf' "accepting on the phone"
until_ok 20 "[ \"\$(phone_size '/sdcard/Download/Contract.pdf')\" = 90000 ]" && pass "accepted file arrived" || fail "accepted file arrived"
run_hook ask_off; run_hook browse_off
get "/api/remote/list?device=$DEV&folder=downloads&path=" | grep -q "allow browsing" \
  && pass "browsing off: laptop is told no" || fail "browsing off was not enforced"
run_hook browse_on
mark; hook demo.txt send_folder; app 6
if until_ok 30 "seen 'row: Downloads'"; then
  until_ok 10 "tap_text Downloads"; sleep 2
  until_ok 10 "tap_text 'Save here'"
  until_ok 30 "[ \"\$(stat -c %s '$HOME/Downloads/Trip/Day 2/c.jpg' 2>/dev/null)\" = 100000 ]" \
    && pass "phone sent a whole folder; layout kept on the laptop" || fail "phone sent a whole folder"
else
  fail "folder sheet for sending a folder"
fi
bail_if_crashed "folders and asking"

# ---------------------------------------------------------------- 5. phone -> laptop, through the app's send flow
mark; hook demo.txt send_test; app 6
if until_ok 30 "seen 'row: Pictures'"; then
  shot 06-save-on-laptop
  until_ok 10 "tap_text Pictures"; sleep 2; shot 07-browse-pictures
  until_ok 10 "tap_text 'Save here'" && pass "picked a folder on the laptop" || fail "tap Save here"
  until_ok 25 "[ -f '$HOME/Pictures/IMG_2041.jpg' ]" && pass "phone sent a file to the laptop" || fail "phone sent a file to the laptop"
  SZ=$(stat -c %s "$HOME/Pictures/IMG_2041.jpg" 2>/dev/null)
  [ "$SZ" = "2097152" ] && pass "file arrived on the laptop intact size" || fail "file size on laptop: '$SZ'"
  expect 10 'activity: sent: Sent IMG_2041.jpg to DESKTOP-NIKHIL .*MB in ' "phone shows the time taken"
  shot 08-sent-from-phone
else
  fail "folder sheet listed the laptop's Pictures"
fi
bail_if_crashed "sending"

# ---------------------------------------------------------------- 6. cancel midway, then retry
mark; hook demo.txt send_big; app 10
if until_ok 40 "seen 'row: Downloads'"; then
  until_ok 10 "tap_text Downloads"; sleep 2
  until_ok 10 "tap_text 'Save here'"
  if until_ok 20 "seen 'transfer out started: Big video.mp4'"; then
    sleep 2; shot 08b-sending-timer
    until_ok 10 "tap_logged Cancel"
    expect 15 'transfer out CANCELLED: Big video.mp4' "cancel stops the transfer"
    shot 08c-cancelled
    until_ok 10 "! ls '$HOME/Downloads' | grep -q 'Big video'" && pass "cancel leaves nothing on the laptop" \
      || fail "cancel leaves nothing on the laptop ($(ls "$HOME/Downloads" | tr '\n' ' '))"
    until_ok 10 "tap_logged Retry"
    expect 20 'continuing Big video.mp4 from [0-9]' "retry continues where it stopped (doesn't start over)"
    until_ok 180 "[ \"\$(stat -c %s '$HOME/Downloads/Big video.mp4' 2>/dev/null)\" = 209715200 ]" \
      && pass "retry sends it again, intact size" || fail "retry sends it again"
  else
    fail "big transfer started"
  fi
else
  fail "folder sheet for the big file"
fi
bail_if_crashed "cancel/retry"
mark; app 6
expect 15 'history: loaded [1-9][0-9]* items' "history survives an app restart"

# ---------------------------------------------------------------- 8b. phone <-> phone
# This phone pairs with another "phone" (the simulator, which shows a code), then sends it
# a file.
./bin/fakephone -data "$WORK/phone2" -name "Test phone" -port 47840 -allow-pair "$WORK/p2.json" > "$OUT/phone2.log" 2>&1 &
P2=$!
until_ok 10 "[ -f '$WORK/p2.json' ]"
P2ID=$(python3 -c "import json;print(json.load(open('$WORK/p2.json'))['id'])"); P2CODE=$(python3 -c "import json;print(json.load(open('$WORK/p2.json'))['code'])")
mark; hook demo.txt "pair_phone:10.0.2.2:47840:$P2ID:$P2CODE"; app 3
expect 20 'pair phone: ok with Test phone' "paired with another phone using its code"
mark; hook demo.txt send_to_phone; app 3
until_ok 30 "[ \"\$(stat -c %s '$WORK/phone2/inbox/Download/Shared note.txt' 2>/dev/null)\" = 200000 ]" \
  && pass "file arrived on the other phone" || fail "file arrived on the other phone"
# Direct Share: paired devices are one-tap targets in every app's Share menu. With two
# paired now, a Share aimed at the laptop goes straight there, without asking which.
A shell dumpsys shortcut > "$WORK/shortcuts.txt" 2>/dev/null
LAPID=$(grep -oE 'dev:[0-9a-f]{16,}' "$WORK/shortcuts.txt" | grep -v "$P2ID" | head -1)
grep -q "dev:$P2ID" "$WORK/shortcuts.txt" && [ -n "$LAPID" ] \
  && pass "paired laptop and phone appear as targets in the Share menu" || fail "Direct Share targets ($(grep -c 'dev:' "$WORK/shortcuts.txt") found)"
if [ -n "$LAPID" ]; then
  mark; A shell am start -a android.intent.action.SEND -t text/plain --es android.intent.extra.TEXT "direct-share-check" \
    --es android.intent.extra.shortcut.ID "$LAPID" -n $PKG/.MainActivity >/dev/null
  expect 15 'direct share to DESKTOP-NIKHIL' "Share → the laptop's target picks it without asking"
  until_ok 20 "api /api/clip | grep -q 'direct-share-check'" && pass "…and the shared text is on that laptop" || fail "direct-shared text on the laptop"
fi
kill $P2 2>/dev/null
# And the other way: this phone shows a code, the other phone pairs with it.
mark; hook demo.txt allow_pair; app 3
if until_ok 15 "seen 'phone pairing open: code [0-9]{6} id [0-9a-f]{64}'"; then
  L=$(applog | tail -n +$((MARK + 1)) | grep -oE 'phone pairing open: code [0-9]{6} id [0-9a-f]{64}' | tail -1)
  CODE=$(echo "$L" | awk '{print $5}'); PID_=$(echo "$L" | awk '{print $7}')
  printf '{"v":1,"host":"127.0.0.1","port":47821,"id":"%s","name":"Emulator","code":"%s"}' "$PID_" "$CODE" > "$WORK/p3.json"
  timeout 30 ./bin/fakephone -data "$WORK/phone3" -name "Third phone" -port 47841 -pair "$WORK/p3.json" > "$OUT/phone3.log" 2>&1 &
  P3=$!
  expect 20 'phone pairing: paired with Third phone' "another phone paired with this one using the code"
  kill $P3 2>/dev/null
else
  fail "phone shows a pairing code"
fi
bail_if_crashed "phone to phone"

# ---------------------------------------------------------------- 8c2. Browse button on the main screen
mark; app 3
if until_ok 10 "tap_logged 'Browse DESKTOP-NIKHIL'"; then
  expect 10 'sheet: Browse DESKTOP-NIKHIL' "Browse on the main screen opens the laptop's folders"
  shot 08c2-browse
  A shell input keyevent KEYCODE_BACK
else
  fail "Browse button on the main screen"
fi

# ---------------------------------------------------------------- 8d. the phone's own "Get files" screen
# Tap a file = open it (fetched into a temporary cache); again = straight from the cache;
# tick + Get = saved in Downloads (copied from the cache, no second download).
# A long folder (40 photos), so the Get button must stay on screen without scrolling.
for i in $(seq -w 1 40); do head -c 2000 /dev/urandom > "$HOME/Documents/IMG_20261101_$i.jpg"; done
sleep 1
head -c 50000 /dev/urandom > "$HOME/Documents/Plan.pdf"   # newest: listed first
get_docs() { mark; hook demo.txt get_sheet; app 4
  until_ok 30 "seen 'row: Documents'" && until_ok 10 "tap_text Documents" && until_ok 20 "seen 'row: Plan.pdf'"; }
primary_on_screen() {
  local line h; h=$(A shell wm size | grep -oE '[0-9]+x[0-9]+' | tail -1 | cut -dx -f2)
  line=$(applog | tail -n +$((MARK + 1)) | grep -oE 'ui: primary bottom [0-9]+' | tail -1 | grep -oE '[0-9]+$')
  [ -n "$line" ] && [ -n "$h" ] && [ "$line" -le "$h" ]
}
if get_docs; then
  until_ok 15 primary_on_screen && pass "a folder of 40+ files: the Get button stays on screen" \
    || fail "Get button pushed off screen by a long list ($(applog | grep -oE 'ui: primary bottom [0-9]+ of [0-9]+' | tail -1))"
  shot 08d-long-folder
  until_ok 10 "tap_text Plan.pdf"
  expect 30 'open: Plan.pdf from .*: fetched 50000 bytes' "tap a laptop's file: fetched to open it"
  expect 15 'open: (showing|no app for) Plan.pdf' "…and handed to a viewer (or a clear note if there's none)"
  [ "$(phone_size '/sdcard/Download/Plan.pdf')" = 50000 ] && fail "opening also saved it in Downloads" || pass "opening doesn't fill Downloads"
  if get_docs; then
    until_ok 10 "tap_text Plan.pdf"
    expect 20 'open: Plan.pdf from the cache' "opening it again needs no download"
  else fail "Get files screen again"; fi
  if get_docs; then
    until_ok 10 "tap_logged 'tick Plan.pdf'"
    until_ok 10 "tap_text 'Get Plan.pdf'"
    until_ok 30 "[ \"\$(phone_size '/sdcard/Download/Plan.pdf')\" = 50000 ]" \
      && pass "phone browsed the laptop, ticked a file and got it" || fail "phone's Get files screen"
    expect 10 'save from cache: Plan.pdf ok' "…copied from the opened file, not downloaded again"
  else fail "Get files screen for ticking"; fi
else
  fail "Get files screen lists the laptop's folders"
fi

# ---------------------------------------------------------------- 8e. renames show up on the other side
mark; hook demo.txt "rename:Nikhil's Pixel"; app 4
until_ok 40 "state | grep -q \"Nikhil's Pixel\"" && pass "phone renamed: the laptop shows the new name" || fail "phone rename reached the laptop"
mark; api /api/settings '{"name":"NIKHIL-PC"}' >/dev/null
expect 40 'presence: .* is now called NIKHIL-PC' "laptop renamed: the phone shows the new name"
api /api/settings '{"name":"DESKTOP-NIKHIL"}' >/dev/null
hook demo.txt "rename:"; app 3   # back to the phone's own name
bail_if_crashed "renames"

# ---------------------------------------------------------------- 9. screens with test data
hook demo.txt add_offline; app 8; shot 09-devices-light
night yes; app 8; shot 09-devices-dark; night no
# Forget a laptop on the phone (long-press it).
mark; app 6
until_ok 10 "long_press_text 'Office PC'" && until_ok 10 "tap_text Forget"
expect 10 'forgot Office PC' "forgetting a laptop on the phone"
hook demo.txt receiving; app 4; shot 10-receiving
hook demo.txt sheet;     app 4; shot 11-subfolder-sheet
mark; hook demo.txt folders; app 4; shot 12-shared-folders
expect 10 'row: Stay reachable' "settings offer 'stay reachable'"
night yes; hook demo.txt folders; app 4; shot 12-shared-folders-dark; night no

# ---------------------------------------------------------------- 10. firewall-like one-way problem
# Restart the laptop program so it refuses incoming connections (as a blocking Windows
# Firewall would) while it can still reach the phone. Both sides should say so precisely.
kill "$LAPTOP_PID"; sleep 1
./bin/lanshare -data "$LAPTOP_DATA" -port 47900 -name DESKTOP-NIKHIL -no-window -block-incoming > "$OUT/laptop-stdout-2.log" 2>&1 &
LAPTOP_PID=$!
mark; app 6
expect 45 'presence: .* -> ONE_WAY' "phone explains one-way (firewall) problem"
shot 13-one-way
until_ok 30 "state | grep -q '\"cantReachUs\":true'" && pass "laptop shows the firewall warning" || fail "laptop shows the firewall warning"

# ---------------------------------------------------------------- 11. receiving restarts after a phone restart
A shell am force-stop $PKG
if timeout 30 adb root >/dev/null 2>&1; then
  sleep 3; timeout 60 adb wait-for-device
  A shell am broadcast -a android.intent.action.BOOT_COMPLETED -p $PKG -f 32 >/dev/null
  until_ok 20 "A shell dumpsys activity services $PKG | grep -q ReceiveService" \
    && pass "receiving starts by itself after a phone restart" || fail "receiving starts by itself after a phone restart"
else
  log "skip  restart check (no adb root here)"
fi

# ---------------------------------------------------------------- 11b. the signed release build starts
REL=android/app/build/outputs/apk/release/app-release.apk
if [ -f "$REL" ]; then
  A uninstall $PKG >/dev/null   # different key from the debug build
  A install "$REL" >/dev/null && pass "signed release APK installs" || fail "signed release APK installs"
  A logcat -b crash -c
  mark; A shell am start -n $PKG/.MainActivity >/dev/null
  expect 30 'screen: unpaired' "signed release APK starts (shrunk code works)"
  shot 19-release
  crashed && fail "release build crashed" || pass "release build: no crash"
else
  log "skip  release check (signing key not set up yet)"
fi

# ---------------------------------------------------------------- 11c. Clear on Recent activity
mark; app 3
if until_ok 10 "tap_text Clear"; then
  expect 10 'history: cleared' "Clear empties Recent activity"
  mark; app 3
  expect 15 'history: loaded 0 items' "…and it stays empty after a restart"
else
  fail "Clear button on Recent activity"
fi

# ---------------------------------------------------------------- 12. no crashes
A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
if grep -q "com.nikhil.lanshare" "$OUT/crashes.txt"; then fail "app crashed (see crashes.txt)"; else pass "no crashes"; fi

log "done: $FAILS failure(s)"
exit $((FAILS > 0))
