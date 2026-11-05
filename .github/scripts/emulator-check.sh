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
  shade_closed
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
shot 01-unpaired-light
bail_if_crashed "first launch"
night yes; app; shot 01-unpaired-dark; night no
# The bubble before pairing: allowing "Display over other apps" alone brings it up (it
# used to need a paired laptop, because it lives in the receiving service).
mark; A shell appops set $PKG SYSTEM_ALERT_WINDOW allow; app 3
expect 20 'bubble shown' "bubble appears before pairing, once the permission is allowed"
shot 01b-bubble-unpaired
# Taken away while LAN Share runs, then back to LAN Share (as when returning from Settings).
mark; A shell appops set $PKG SYSTEM_ALERT_WINDOW deny; A shell am start -n $PKG/.MainActivity >/dev/null
expect 15 'bubble hidden' "…and goes when the permission is taken away"

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
expect 15 'clipboard from DESKTOP-NIKHIL: 16 chars' "copied on the laptop: it's on the phone's clipboard"
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

# ---------------------------------------------------------------- 7. the assistant
# a) Everything said at once: "send this to my laptop in downloads".
mark; hook demo.txt assistant; app 3
expect 15 'assistant: plan send Voice note.m4a to DESKTOP-NIKHIL in Downloads' "assistant understood the command"
shot 14-assistant-confirm
until_ok 30 "[ \"\$(stat -c %s '$HOME/Downloads/Voice note.m4a' 2>/dev/null)\" = 524288 ]" \
  && pass "assistant sent the file (after the 2 s pause)" || fail "assistant sent the file"
expect 10 'assistant: Sent Voice note.m4a' "assistant shows it was sent"
bail_if_crashed "assistant send"
# b) A follow-up question, answered by typing: "send this to my laptop" → "Which folder?" → "documents".
mark; hook demo.txt assistant_ask; app 3
expect 20 'assistant: asking: Which folder on DESKTOP-NIKHIL' "assistant asks which folder"
shot 15-assistant-asks
until_ok 10 "tap_text input" && A shell input text documents && A shell input keyevent 66
expect 15 'assistant: plan send Voice memo 2.m4a to DESKTOP-NIKHIL in Documents' "assistant took the answer"
until_ok 30 "[ \"\$(stat -c %s '$HOME/Documents/Voice memo 2.m4a' 2>/dev/null)\" = 300000 ]" \
  && pass "follow-up send arrived in Documents" || fail "follow-up send arrived in Documents"
# c) Getting a file by name: "get meeting notes from my laptop".
head -c 400000 /dev/urandom > "$HOME/Documents/Meeting notes.txt"
mark; hook demo.txt assistant_get; app 3
expect 25 'assistant: plan get Meeting notes.txt from DESKTOP-NIKHIL' "assistant found the file on the laptop"
until_ok 30 "[ \"\$(phone_size '/sdcard/Download/Meeting notes.txt')\" = 400000 ]" \
  && pass "phone got the file from the laptop, intact size" || fail "phone got the file from the laptop"
shot 16-assistant-got
bail_if_crashed "assistant"

# ---------------------------------------------------------------- 8. floating bubble
A shell appops set $PKG SYSTEM_ALERT_WINDOW allow
mark; hook demo.txt bubble; app 3
A shell input keyevent KEYCODE_HOME   # the bubble is for other apps: it hides on LAN Share's own screen
expect 15 'bubble shown' "bubble appears over other apps"
sleep 1; shot 17-bubble-on-home
until_ok 10 "applog | tail -n +$((MARK + 1)) | grep -qE 'ui: bubble at '"
BXY=$(applog | tail -n +$((MARK + 1)) | grep -oE 'ui: bubble at [0-9]+,[0-9]+' | tail -1 | grep -oE '[0-9]+,[0-9]+$')
mark; A shell input tap "${BXY%,*}" "${BXY#*,}"
expect 10 'bubble tapped' "tapping the bubble"
expect 10 'assistant: ready' "bubble opens the assistant"
shot 18-assistant-from-bubble
A shell input keyevent KEYCODE_BACK
# Drag the bubble into the bottom corner: it must stay above the navigation bar.
SH=$(A shell wm size | grep -oE '[0-9]+x[0-9]+' | tail -1 | cut -dx -f2)
sleep 2; mark; A shell input swipe "${BXY%,*}" "${BXY#*,}" 20 $((SH - 5)) 900
expect 10 'bubble moved to ' "dragging the bubble"
L=$(applog | tail -n +$((MARK + 1)) | grep -oE 'bubble moved to [0-9]+,[0-9]+ \(allowed y [0-9]+\.\.[0-9]+\)' | tail -1)
Y=$(echo "$L" | sed -E 's/.*,([0-9]+) \(.*/\1/'); MAXY=$(echo "$L" | sed -E 's/.*\.\.([0-9]+)\)/\1/')
[ -n "$Y" ] && [ "$Y" -le "$MAXY" ] && pass "…it stays clear of the navigation bar ($L)" || fail "bubble went under the navigation bar ($L)"
# Drag it onto the ✕ at the bottom: hidden; opening LAN Share brings it back.
P=$(applog | grep -oE 'ui: bubble at [0-9]+,[0-9]+' | tail -1 | grep -oE '[0-9]+,[0-9]+$')
T=$(applog | grep -oE 'ui: bubble target at [0-9]+,[0-9]+' | tail -1 | grep -oE '[0-9]+,[0-9]+$')
mark; A shell input swipe "${P%,*}" "${P#*,}" "${T%,*}" "${T#*,}" 1200
expect 10 'bubble dismissed' "dragging the bubble onto the ✕ puts it away"
mark; app 3
expect 15 'bubble back \(LAN Share opened\)' "opening LAN Share brings it back"
expect 10 'bubble shown' "…and it's on screen again"
bail_if_crashed "bubble"

# ---------------------------------------------------------------- 8b. phone <-> phone
# This phone pairs with another "phone" (the simulator, which shows a code), then sends it
# a file through the assistant.
./bin/fakephone -data "$WORK/phone2" -name "Test phone" -port 47840 -allow-pair "$WORK/p2.json" > "$OUT/phone2.log" 2>&1 &
P2=$!
until_ok 10 "[ -f '$WORK/p2.json' ]"
P2ID=$(python3 -c "import json;print(json.load(open('$WORK/p2.json'))['id'])"); P2CODE=$(python3 -c "import json;print(json.load(open('$WORK/p2.json'))['code'])")
mark; hook demo.txt "pair_phone:10.0.2.2:47840:$P2ID:$P2CODE"; app 3
expect 20 'pair phone: ok with Test phone' "paired with another phone using its code"
mark; hook demo.txt "say:send this to test phone in downloads"; app 3
expect 20 'assistant: plan send Shared note.txt to Test phone in Downloads' "assistant sends to the other phone"
until_ok 30 "[ \"\$(stat -c %s '$WORK/phone2/inbox/Download/Shared note.txt' 2>/dev/null)\" = 200000 ]" \
  && pass "file arrived on the other phone" || fail "file arrived on the other phone"
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

# ---------------------------------------------------------------- 8c. "Hey Jarvis" wake word
# The real models and TensorFlow Lite on the phone, fed recordings (the emulator has no
# microphone): "hey jarvis" must score high, similar-sounding phrases low.
A shell "run-as $PKG mkdir -p files/wav"
for f in .github/testdata/*.wav; do
  b=$(basename "$f"); A push "$f" "/data/local/tmp/$b" >/dev/null; A shell chmod 644 "/data/local/tmp/$b"
  A shell "run-as $PKG cp /data/local/tmp/$b files/wav/$b"
done
mark; hook demo.txt wake_files; app 3
expect 60 'wake test: jarvis_a.wav score (0\.[5-9]|1\.)' "“Hey Jarvis” is recognised"
expect 10 'wake test: neg_travis.wav score 0\.[0-4]' "no false alarm on “hey travis”"
expect 10 'wake test: neg_send.wav score 0\.[0-4]' "no false alarm on ordinary speech"
A shell pm grant $PKG android.permission.RECORD_AUDIO 2>/dev/null
mark; hook demo.txt wake_on; app 4
expect 30 'wake word: (listening|microphone unavailable)' "listening for “Hey Jarvis” starts in the background"
mark; hook demo.txt wake_off; app 3
expect 15 'wake word: off' "turning it off stops listening"
bail_if_crashed "wake word"

# ---------------------------------------------------------------- 8d. the phone's own "Get files" screen
head -c 50000 /dev/urandom > "$HOME/Documents/Plan.pdf"
mark; hook demo.txt get_sheet; app 4
if until_ok 30 "seen 'row: Documents'"; then
  until_ok 10 "tap_text Documents"
  until_ok 20 "seen 'row: Plan.pdf'" && until_ok 10 "tap_text Plan.pdf"
  until_ok 10 "tap_text 'Get Plan.pdf'"
  until_ok 30 "[ \"\$(phone_size '/sdcard/Download/Plan.pdf')\" = 50000 ]" \
    && pass "phone browsed the laptop, ticked a file and got it" || fail "phone's Get files screen"
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
expect 5 'row: Floating assistant bubble' "settings offer the bubble"
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

# ---------------------------------------------------------------- 12. no crashes
A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
if grep -q "com.nikhil.lanshare" "$OUT/crashes.txt"; then fail "app crashed (see crashes.txt)"; else pass "no crashes"; fi

log "done: $FAILS failure(s)"
exit $((FAILS > 0))
