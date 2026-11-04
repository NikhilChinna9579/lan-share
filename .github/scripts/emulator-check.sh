#!/usr/bin/env bash
# End-to-end check on an Android emulator against the real laptop program (Linux build).
# Every command has a time limit and nothing waits for input, so this can't hang.
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
fail() { log "FAIL  $*"; FAILS=$((FAILS + 1)); }
A()    { timeout 30 adb "$@"; }                                   # adb, never more than 30 s
shot() { sleep 1; timeout 20 adb exec-out screencap -p > "$OUT/$1.png" 2>/dev/null || true; }
app()  { A shell am force-stop $PKG; A shell am start -n $PKG/.MainActivity >/dev/null; sleep "${1:-5}"; }
# Write a file into the app's private folder (debug builds only), without using stdin.
hook() { A shell "run-as $PKG sh -c 'mkdir -p files && echo $(printf '%s' "$2" | base64 -w0) | base64 -d > files/$1'"; }
night(){ A shell cmd uimode night "$1" >/dev/null; sleep 2; }
# Laptop program's local API (the same calls its window makes).
api()  { local port tok; port=$(python3 -c "import json;print(json.load(open('$LAPTOP_DATA/instance.json'))['port'])");
         tok=$(python3 -c "import json;print(json.load(open('$LAPTOP_DATA/instance.json'))['token'])");
         local body=${2:-'{}'}
         timeout 20 curl -s -X POST -H "X-Token: $tok" "http://127.0.0.1:$port$1" -d "$body"; }
state(){ local port tok; port=$(python3 -c "import json;print(json.load(open('$LAPTOP_DATA/instance.json'))['port'])");
         tok=$(python3 -c "import json;print(json.load(open('$LAPTOP_DATA/instance.json'))['token'])");
         timeout 20 curl -s -H "X-Token: $tok" "http://127.0.0.1:$port/api/state"; }
# Wait up to $1 seconds for a shell condition.
until_ok() { local n=$1; shift; for _ in $(seq 1 "$n"); do eval "$@" && return 0; sleep 1; done; return 1; }
# Tap the on-screen element whose text contains $1 (Android's UI automation dump).
tap_text() {
  A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1
  A pull /sdcard/ui.xml "$WORK/ui.xml" >/dev/null 2>&1 || return 1
  local xy
  xy=$(python3 - "$1" "$WORK/ui.xml" <<'PY'
import re, sys, xml.etree.ElementTree as ET
want, path = sys.argv[1], sys.argv[2]
for n in ET.parse(path).iter("node"):
    if want in (n.get("text") or ""):
        x1, y1, x2, y2 = map(int, re.findall(r"\d+", n.get("bounds")))
        print((x1 + x2) // 2, (y1 + y2) // 2); break
PY
)
  [ -n "$xy" ] || return 1
  A shell input tap $xy
}
screen_has() { A shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; A shell cat /sdcard/ui.xml 2>/dev/null | grep -q "$1"; }

cleanup() {
  A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
  A logcat -d > "$OUT/logcat-full.txt" 2>&1
  grep -E "lanshare|AndroidRuntime" "$OUT/logcat-full.txt" | tail -400 > "$OUT/logcat.txt"
  cp "$LAPTOP_DATA/lanshare.log" "$OUT/laptop-lanshare.log" 2>/dev/null
  kill "$LAPTOP_PID" 2>/dev/null
}
trap cleanup EXIT

# ---------------------------------------------------------------- 1. install, first screen
A install -r android/app/build/outputs/apk/debug/app-debug.apk >/dev/null && pass "install APK" || fail "install APK"
A shell pm grant $PKG android.permission.POST_NOTIFICATIONS 2>/dev/null
A forward tcp:47821 tcp:47821   # lets the laptop program reach the phone's receiver
night no;  app; shot 01-unpaired-light
screen_has "Pair your laptop" && pass "first launch shows pairing" || fail "first launch shows pairing"
night yes; app; shot 01-unpaired-dark; night no

# ---------------------------------------------------------------- 2. laptop program + pairing
./bin/lanshare -data "$LAPTOP_DATA" -port 47900 -name DESKTOP-NIKHIL -no-window -pairing-file "$WORK/pair.json" > "$OUT/laptop-stdout.log" 2>&1 &
LAPTOP_PID=$!
until_ok 10 "[ -f '$LAPTOP_DATA/instance.json' ]" && pass "laptop program started" || fail "laptop program started"
api /api/pair >/dev/null
until_ok 10 "[ -f '$WORK/pair.json' ]" && pass "laptop opened pairing" || fail "laptop opened pairing"
# 10.0.2.2 is how the emulator reaches the machine it runs on.
hook test_pair.txt "$(python3 -c "import json;q=json.load(open('$WORK/pair.json'));q['host']='10.0.2.2';print(json.dumps(q))")"
app 8
shot 02-shared-folders-after-pairing
screen_has "Shared folders" && pass "pairing done; shared-folder choice shown" || fail "pairing done; shared-folder choice shown"
A shell input keyevent KEYCODE_BACK; sleep 1
until_ok 20 "state | grep -q '\"status\":\"online\"'" && pass "laptop sees the phone online" || fail "laptop sees the phone online"
until_ok 20 "screen_has 'Online · 10.0.2.2'" && pass "phone sees the laptop online" || fail "phone sees the laptop online"
shot 03-home-online

# ---------------------------------------------------------------- 3. laptop -> phone, for real
head -c 3000000 /dev/urandom > "$WORK/Quarterly report.pdf"
timeout 60 ./bin/lanshare -data "$LAPTOP_DATA" -folder downloads send "$WORK/Quarterly report.pdf" > "$OUT/send-to-phone.log" 2>&1 \
  && pass "laptop sent a file to the phone" || fail "laptop sent a file to the phone ($(tail -1 "$OUT/send-to-phone.log"))"
SIZE=$(A shell stat -c %s "'/sdcard/Download/Quarterly report.pdf'" 2>/dev/null | tr -d '\r')
[ "$SIZE" = "3000000" ] && pass "file arrived on the phone intact size" || fail "file arrived on the phone (size: '$SIZE')"
sleep 1; shot 04-received-on-phone
screen_has "Received Quarterly report.pdf" && pass "phone shows it live" || fail "phone shows it live"
A shell cmd statusbar expand-notifications; sleep 2; shot 05-notification; A shell cmd statusbar collapse

# ---------------------------------------------------------------- 4. phone -> laptop through the Share menu
head -c 2000000 /dev/urandom > "$WORK/IMG_2041.jpg"
A push "$WORK/IMG_2041.jpg" /sdcard/Download/IMG_2041.jpg >/dev/null
A shell am broadcast -a android.intent.action.MEDIA_SCANNER_SCAN_FILE -d file:///sdcard/Download/IMG_2041.jpg >/dev/null 2>&1
media_id() { A shell content query --uri content://media/external/file --projection _id:_display_name 2>/dev/null \
             | grep "_display_name=IMG_2041.jpg" | sed -E 's/.*_id=([0-9]+).*/\1/' | head -1 | tr -d '\r'; }
URI=""
for _ in $(seq 1 15); do URI=$(media_id); [ -n "$URI" ] && break; sleep 1; done
if [ -n "$URI" ]; then
  A shell am start -a android.intent.action.SEND -t image/jpeg --grant-read-uri-permission \
    --eu android.intent.extra.STREAM "content://media/external/file/$URI" -n $PKG/.MainActivity >/dev/null
  sleep 4; shot 06-save-on-laptop
  if tap_text "Pictures"; then
    sleep 3; shot 07-browse-pictures
    tap_text "Save here" && pass "picked a folder on the laptop" || fail "tap Save here"
    until_ok 20 "[ -f '$HOME/Pictures/IMG_2041.jpg' ]" && pass "phone sent a file to the laptop" || fail "phone sent a file to the laptop"
    sleep 1; shot 08-sent-from-phone
  else
    fail "folder sheet listed the laptop's Pictures"
  fi
else
  fail "couldn't prepare a shared image (no MediaStore id)"
fi

# ---------------------------------------------------------------- 5. screens with test data
hook demo.txt add_offline; app 10; shot 09-devices-light
night yes; app 10; shot 09-devices-dark; night no
hook demo.txt receiving; app 4; shot 10-receiving
hook demo.txt sheet;     app 4; shot 11-subfolder-sheet
hook demo.txt folders;   app 4; shot 12-shared-folders
night yes; hook demo.txt folders; app 4; shot 12-shared-folders-dark; night no

# ---------------------------------------------------------------- 6. firewall-like one-way problem
# Restart the laptop program on another port: the phone can no longer reach it, but the
# laptop still reaches the phone. The phone should say so precisely.
kill "$LAPTOP_PID"; sleep 1
./bin/lanshare -data "$LAPTOP_DATA" -port 47950 -name DESKTOP-NIKHIL -no-window > "$OUT/laptop-stdout-2.log" 2>&1 &
LAPTOP_PID=$!
app 6
until_ok 40 "screen_has \"Can't connect to it\"" && pass "phone explains one-way (firewall) problem" || fail "phone explains one-way problem"
shot 13-one-way
until_ok 20 "state | grep -q '\"cantReachUs\":true'" && pass "laptop shows the firewall warning" || fail "laptop shows the firewall warning"

# ---------------------------------------------------------------- 7. no crashes
A logcat -d -b crash > "$OUT/crashes.txt" 2>&1
if grep -q "com.nikhil.lanshare" "$OUT/crashes.txt"; then fail "app crashed (see crashes.txt)"; else pass "no crashes"; fi

log "done: $FAILS failure(s)"
exit $((FAILS > 0))
