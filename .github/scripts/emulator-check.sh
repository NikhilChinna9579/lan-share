#!/usr/bin/env bash
# Runs on the Android emulator in CI: installs the app, pairs it with the real Windows
# program (built for Linux) running on the CI machine, sends a file laptop → phone, and
# takes screenshots of each screen in light and dark mode. Output goes to check-out/.
set -x
PKG=com.nikhil.lanshare
OUT=$PWD/check-out
D=$(mktemp -d)
mkdir -p "$OUT"

shot() { sleep 1; adb exec-out screencap -p > "$OUT/$1.png"; }
app() { adb shell am force-stop $PKG; adb shell am start -n $PKG/.MainActivity; sleep "${1:-5}"; }
hook() { echo "$2" | adb shell "run-as $PKG sh -c 'mkdir -p files; cat > files/$1'"; }
night() { adb shell cmd uimode night "$1"; sleep 2; }

adb install -r android/app/build/outputs/apk/debug/app-debug.apk
adb shell pm grant $PKG android.permission.POST_NOTIFICATIONS || true

# 1. First launch: not paired.
night no;  app; shot 01-unpaired-light
night yes; app; shot 01-unpaired-dark
night no

# 2. Start the laptop program; pair the phone with it (10.0.2.2 = the CI machine).
(sleep 3600 | ./bin/lanshare -data "$D/laptop" -port 47900 -name DESKTOP-NIKHIL -pairing-file "$D/pair.json" > "$OUT/laptop.log" 2>&1) &
sleep 3
python3 -c "import json; q=json.load(open('$D/pair.json')); q['host']='10.0.2.2'; print(json.dumps(q))" > "$D/pair-emu.json"
hook test_pair.txt "$(cat "$D/pair-emu.json")"
app 10
shot 02-paired-light
hook demo.txt add_offline; app 12
shot 03-devices-light
night yes; app 12; shot 03-devices-dark; night no

# 3. Screens that need a transfer in flight: shown with test data.
hook demo.txt sending; app 4; shot 04-sending-light
hook demo.txt done;    app 4; shot 05-done-light
hook demo.txt error;   app 4; shot 06-error-light
hook demo.txt sheet;   app 4; shot 07-sheet-light
night yes; hook demo.txt sheet; app 4; shot 07-sheet-dark; night no

# 4. Laptop → phone, for real. The phone listens on 47821; forward it to the CI machine.
app 6
adb forward tcp:47821 tcp:47821
head -c 2500000 /dev/urandom > "$D/Quarterly report.pdf"
timeout 60 ./bin/lanshare -data "$D/laptop" -name DESKTOP-NIKHIL -folder downloads send "$D/Quarterly report.pdf" < /dev/null > "$OUT/send-to-phone.log" 2>&1
echo "exit code $?" >> "$OUT/send-to-phone.log"
adb shell ls -l /sdcard/Download/ > "$OUT/phone-downloads.txt" 2>&1
adb shell cmd statusbar expand-notifications; sleep 2; shot 08-notification; adb shell cmd statusbar collapse

adb logcat -d -b crash > "$OUT/crashes.txt" 2>&1
adb logcat -d | grep -i -E "lanshare|AndroidRuntime" | tail -300 > "$OUT/logcat.txt"
kill %1 2>/dev/null
true
