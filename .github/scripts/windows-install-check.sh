#!/usr/bin/env bash
# Install / uninstall check on a real Windows machine (Git Bash on the CI runner).
# Uses the default data folder, like a real user. Every wait has a limit.
set -u
EXE=$1
DATA="$APPDATA/LanShare"
INST="$LOCALAPPDATA/Programs/LanShare/lanshare.exe"
LNK="$APPDATA/Microsoft/Windows/Start Menu/Programs/LAN Share.lnk"
FAILS=0
pass() { echo "ok    $*"; }
fail() { echo "FAIL  $*"; FAILS=$((FAILS + 1)); }
until_ok() { local n=$1; shift; for _ in $(seq 1 "$n"); do eval "$@" && return 0; sleep 1; done; return 1; }
field() { python -c "import json,os;print(json.load(open(os.path.join(os.environ['APPDATA'],'LanShare','instance.json')))['$1'])" 2>/dev/null; }
api() { curl -s -m 20 -X POST -H "X-Token: $(field token)" "http://127.0.0.1:$(field port)$1" -d '{}'; }

"$EXE" -no-window > start.log 2>&1 &
until_ok 15 "[ -n \"\$(field port)\" ]" && pass "started" || fail "started"
PID1=$(field pid)
api /api/install; echo
until_ok 20 "[ -f '$INST' ]" && pass "copied to Programs\\LanShare" || fail "copied to Programs\\LanShare"
until_ok 20 "[ -f '$LNK' ]" && pass "Start menu shortcut" || fail "Start menu shortcut"
until_ok 20 "[ \"\$(field pid)\" != '$PID1' ] && [ -n \"\$(field pid)\" ]" && pass "installed copy took over" || fail "installed copy took over"
until_ok 10 "reg query 'HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run' //v LanShare >/dev/null 2>&1" \
  && pass "starts with Windows" || fail "starts with Windows"
api /api/uninstall; echo
until_ok 20 "[ ! -f '$INST' ]" && pass "uninstall removed the program" || fail "uninstall removed the program"
[ ! -f "$LNK" ] && pass "uninstall removed the shortcut" || fail "uninstall removed the shortcut"
reg query 'HKCU\Software\Microsoft\Windows\CurrentVersion\Run' //v LanShare >/dev/null 2>&1 && fail "still starts with Windows" || pass "no longer starts with Windows"
[ -f "$DATA/identity.pem" ] && pass "pairings kept" || fail "pairings kept"
taskkill //F //IM lanshare.exe >/dev/null 2>&1; taskkill //F //IM msedge.exe >/dev/null 2>&1
echo "done: $FAILS failure(s)"
exit $((FAILS > 0))
