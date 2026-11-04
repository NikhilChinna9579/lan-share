# lan-share

Send files between an Android phone and a Windows laptop over home Wi-Fi — by voice,
from a floating bubble, with no cloud, no login and no messaging app in between.

**Status:** working on the same Wi-Fi. Internet relay, online AI and wake word are planned.

## What it does

- **Pair once**: scan the laptop's QR code; pair phones with each other using a 6-digit code.
  Devices find each other again by themselves when Wi-Fi addresses change.
- **Send both ways**: files or whole folders (subfolders kept), into any shared folder or
  subfolder, or a new folder made on the spot. Live progress, time taken, speed, Cancel, Retry.
  Big transfers that get cut off **resume** where they stopped.
- **Shared clipboard**: copy on the laptop, paste on the phone; send what you copied on the
  phone (or any shared text) to the laptop in one tap; "paste from laptop/phone" on demand.
- **Get files from the other device**: browse its shared folders, or **search** them all by name.
- **Voice assistant** in a **floating bubble**: "send this to my laptop in Documents",
  "send the last photo to my laptop", "get budget from my laptop". Asks follow-up questions,
  pauses 2 s so you can cancel. Speech is offline-first with an online fallback.
- **Your choices**: which folders are shared, ask before receiving (Accept/Decline),
  whether others may browse your files, device names, the laptop's default folder.
- **Secure**: mutual TLS 1.3 with pinned keys; nothing outside shared folders is reachable;
  the laptop's key is encrypted by Windows (DPAPI).
- **Windows app**: a small window plus tray icon, Explorer "Send to", drag and drop, install,
  start with Windows, firewall fix.

## Where things are

| Folder | What |
| --- | --- |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How the system fits together, and why each decision was made |
| [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md) | The wire contract both apps implement |
| [`windows/`](windows) | Windows app, Go |
| [`android/`](android) | Android app, Kotlin |

## Getting the apps

After a green **build and test** run (started by hand in the Actions tab), the newest
`lanshare.exe` and `lanshare.apk` are on the [`builds`](../../tree/builds) branch.

1. Download both from the `builds` branch.
3. Phone: open the APK and allow "install unknown apps" for your browser or file manager.
4. Laptop: run `lanshare.exe`. Windows SmartScreen may warn because the app isn't signed —
   choose **More info → Run anyway**.

## Building locally (optional)

- Windows app: install Go 1.22+, then `cd windows && go build ./cmd/lanshare`.
- Android app: open the `android` folder in Android Studio and press Run.
