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
tap_text() {
  shade_closed
  A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1
  A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1 || return 1
  local xy
  xy=$(python3 - "$1" "$WORK/ui.xml" <<'PY'
import re, sys, xml.etree.ElementTree as ET
want, path = sys.argv[1], sys.argv[2]
for n in ET.parse(path).iter("node"):
    if want in (n.get("text") or "") or n.get("resource-id", "").endswith("/" + want):
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", n.get("bounds")))
        print((x1 + x2) // 2, (y1 + y2) // 2); break
PY
)
  [ -n "$xy" ] || return 1
  A shell input tap $xy
}
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
rm -f "$HOME/Downloads/Quarterly report.pdf"
mark
api /api/fetch "{\"device\":\"$DEV\",\"folder\":\"downloads\",\"folderLabel\":\"Downloads\",\"path\":\"\",\"names\":[\"Quarterly report.pdf\"]}" >/dev/null
until_ok 30 "[ \"\$(stat -c %s '$HOME/Downloads/Quarterly report.pdf' 2>/dev/null)\" = 3000000 ]" \
  && pass "laptop got a file from the phone, intact size" || fail "laptop got a file from the phone"
expect 10 'served 1 files to DESKTOP-NIKHIL' "phone logs what it served"

# ---------------------------------------------------------------- 4b. folders, new folder, asking, browsing off
mkdir -p "$WORK/Trip/Day 2"; head -c 70000 /dev/urandom > "$WORK/Trip/a.jpg"; head -c 80000 /dev/urandom > "$WORK/Trip/Day 2/b.jpg"
timeout 60 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Trip" > "$OUT/send-folder.log" 2>&1
[ "$(phone_size '/sdcard/Download/Trip/Day 2/b.jpg')" = 80000 ] && [ "$(phone_size '/sdcard/Download/Trip/a.jpg')" = 70000 ] \
  && pass "laptop sent a whole folder; layout kept on the phone" || fail "whole folder on the phone ($(tail -1 "$OUT/send-folder.log"))"
api /api/remote/mkdir "{\"device\":\"$DEV\",\"folder\":\"downloads\",\"path\":\"\",\"name\":\"Projects\"}" > "$WORK/mkdir.json"
get "/api/remote/list?device=$DEV&folder=downloads&path=" | grep -q '"Projects"' \
  && pass "laptop made a new folder on the phone" || fail "new folder on the phone ($(cat "$WORK/mkdir.json"))"
mark; hook demo.txt ask_on; app 3
head -c 90000 /dev/urandom > "$WORK/Contract.pdf"
( timeout 70 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Contract.pdf" > "$OUT/send-ask.log" 2>&1 & )
expect 20 'approval: asking about Contract.pdf' "phone asks before receiving"
shot 05b-asking
until_ok 10 "tap_text Accept"
expect 15 'approval: accepted Contract.pdf' "accepting on the phone"
until_ok 20 "[ \"\$(phone_size '/sdcard/Download/Contract.pdf')\" = 90000 ]" && pass "accepted file arrived" || fail "accepted file arrived"
hook demo.txt ask_off; app 3; hook demo.txt browse_off; app 3
get "/api/remote/list?device=$DEV&folder=downloads&path=" | grep -q "allow browsing" \
  && pass "browsing off: laptop is told no" || fail "browsing off was not enforced"
hook demo.txt browse_on; app 3
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
    mark; until_ok 10 "tap_text Cancel"
    expect 15 'transfer out CANCELLED: Big video.mp4' "cancel stops the transfer"
    shot 08c-cancelled
    until_ok 10 "! ls '$HOME/Downloads' | grep -q 'Big video'" && pass "cancel leaves nothing on the laptop" \
      || fail "cancel leaves nothing on the laptop ($(ls "$HOME/Downloads" | tr '\n' ' '))"
    mark; until_ok 10 "tap_text Retry"
    until_ok 90 "[ \"\$(stat -c %s '$HOME/Downloads/Big video.mp4' 2>/dev/null)\" = 209715200 ]" \
      && pass "retry sends it again, intact size" || fail "retry sends it again"
  else
    fail "big transfer started"
  fi
else
  fail "folder sheet for the big file"
fi
bail_if_crashed "cancel/retry"
app 6
until_ok 15 "screen_has 'Received Quarterly report.pdf'" && pass "history survives an app restart" || fail "history survives an app restart"

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
expect 15 'bubble shown' "bubble appears over other apps"
A shell input keyevent KEYCODE_HOME; sleep 2; shot 17-bubble-on-home
# Where the bubble sits by default: right edge, 240 dp down.
W=$(A shell wm size | grep -oE '[0-9]+x[0-9]+' | tail -1 | cut -dx -f1)
D=$(A shell wm density | grep -oE '[0-9]+' | tail -1)
X=$(( W - (36 * D / 160) )); Y=$(( 268 * D / 160 ))
mark; A shell input tap $X $Y
expect 10 'bubble tapped' "tapping the bubble"
expect 10 'assistant: ready' "bubble opens the assistant"
shot 18-assistant-from-bubble
A shell input keyevent KEYCODE_BACK
bail_if_crashed "bubble"

# ---------------------------------------------------------------- 9. screens with test data
hook demo.txt add_offline; app 8; shot 09-devices-light
night yes; app 8; shot 09-devices-dark; night no
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

# ---------------------------------------------------------------- 12. no crashes
A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
if grep -q "com.nikhil.lanshare" "$OUT/crashes.txt"; then fail "app crashed (see crashes.txt)"; else pass "no crashes"; fi

log "done: $FAILS failure(s)"
exit $((FAILS > 0))
