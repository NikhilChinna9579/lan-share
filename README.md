# lan-share

Send files between an Android phone and a Windows laptop over home Wi-Fi — by voice,
from a floating bubble, with no cloud, no login and no messaging app in between.

**Status:** design done, project skeletons in place. Next: the pairing + transfer spike.

## Where things are

| Folder | What |
| --- | --- |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How the system fits together, and why each decision was made |
| [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md) | The wire contract both apps implement |
| [`windows/`](windows) | Windows app, Go |
| [`android/`](android) | Android app, Kotlin |

## Getting the apps

Every push builds both apps on GitHub Actions:

1. Open the **Actions** tab, click the latest green **build** run.
2. Scroll to **Artifacts** and download `lanshare-windows` (the .exe) and
   `lanshare-android` (the APK).
3. Phone: open the APK and allow "install unknown apps" for your browser or file manager.
4. Laptop: run `lanshare.exe`. Windows SmartScreen may warn because the app isn't signed —
   choose **More info → Run anyway**.

## Building locally (optional)

- Windows app: install Go 1.22+, then `cd windows && go build ./cmd/lanshare`.
- Android app: open the `android` folder in Android Studio and press Run.
