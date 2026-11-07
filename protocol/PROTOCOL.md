# LAN Share protocol — version 1

The contract both apps implement. If the Android and Windows code ever disagree,
**this file is right** and the code is a bug.

## 1. Identity

- On first launch a device generates an **ECDSA P-256** key pair and a self-signed
  X.509 certificate (validity 20 years, CN = device name).
- **Device ID / fingerprint** = lowercase hex SHA-256 of the certificate's DER-encoded
  `SubjectPublicKeyInfo`. Hashing the key (not the whole cert) means the ID stays the same
  if the certificate is ever re-issued with the same key.
- The private key never leaves the device (Android Keystore / Windows protected storage).

## 2. Discovery (mDNS / DNS-SD)

| Field | Value |
| --- | --- |
| Service type | `_lanshare._tcp` |
| Domain | `local.` |
| Instance name | device's friendly name, e.g. `Laptop` |
| Port | TCP port the device listens on (default 47821) |
| TXT `v` | protocol version, `1` |
| TXT `id` | device fingerprint (64 hex chars) |
| TXT `p` | platform: `android` or `windows` |

A device only acts on announcements whose `id` is in its trust store (or, during
pairing, the `id` from the QR code).

## 3. Transport

- TCP, then **TLS 1.3 only**. Both sides present their certificate (mutual TLS).
- Normal verification of CA chains is turned off (certs are self-signed). Instead each
  side computes the peer's fingerprint and **rejects the connection unless it is in the
  trust store**. This check runs during the handshake, before any message is read.
- Exception: a device in **pairing mode** accepts an unknown client certificate, but only
  `pair_request` is allowed on that connection, and it must carry the valid one-time code.
- One connection per action (ping, folder list, one transfer). Close when done.
- Idle timeout: 15 s without a frame → close.

## 4. Framing

Every control message is a frame:

```
+----------------------+---------------------------------+
| length: uint32, BE   | JSON object, UTF-8, `length` B  |
+----------------------+---------------------------------+
```

- Maximum frame length: 1 MiB. Larger → protocol error, close.
- Every JSON object has a `"type"` field. Unknown types → reply `error` with
  code `unknown_type`, keep the connection.
- File contents are **not** framed: after `file_accept`, the sender writes exactly
  `size` raw bytes per file, followed by a `file_end` frame.

## 5. Messages

### Session

| Type | Direction | Fields | Notes |
| --- | --- | --- | --- |
| `hello` | both, first frame | `v` (int), `name` (string), `platform` | Version mismatch → `error` `version`, close |
| `ping` | client → server | — | |
| `pong` | server → client | `reach` (bool, optional), `battery` (int 1–100, optional), `charging` (bool, with `battery`) | `reach: false` = "I can't connect back to you" (usually a firewall on the pinging device). Phones add their battery level, which the laptop shows beside the phone's name |
| `error` | either | `code` (string), `message` (string) | See codes below |

### Pairing

| Type | Direction | Fields |
| --- | --- | --- |
| `pair_request` | phone → laptop | `code` (6 digits), `name` |
| `pair_ok` | laptop → phone | `name` |
| `pair_fail` | laptop → phone | `reason`: `bad_code`, `expired`, `not_pairing` |

The QR code encodes this JSON (as text):

```json
{"v":1,"host":"192.168.1.20","port":47821,"id":"<fingerprint>","name":"Laptop","code":"481920"}
```

The one-time code is random, valid for **2 minutes**, and dies after one use or
**5 wrong attempts**.

### Shared folders

Each device decides which of its folders it shares. Only those (and their subfolders)
can be saved into or browsed; nothing else on the device is reachable, so system
folders never appear.

| Type | Direction | Fields |
| --- | --- | --- |
| `folders_request` | either | — |
| `folders_response` | reply | `default` (folder key), `folders`: list of `{key, label}` |
| `list_request` | either | `folder` (key), `path` (subfolder, `/`-separated, optional) |
| `list_response` | reply | `folder`, `path`, `dirs`: names, `files`: list of `{name, size, modified}` (ms) |

Folder **keys** (e.g. `downloads`, `documents`, `f-3a9c12be`) are what travel on the wire,
never full paths. The receiver maps keys to real paths itself and rejects any `path` that
would leave the shared folder (`..`, links, junctions) with `error` `bad_folder`. Hidden
and system items are not listed. A device may share folders it can't list subfolders of
(for example Android's standard folders); it then returns empty `dirs`.

### Transfer

| Type | Direction | Fields |
| --- | --- | --- |
| `file_offer` | sender → receiver | `transfer_id` (UUID), `folder` (key), `path` (subfolder, optional), `files`: list of `{name, size, path?}`; a file's own `path` is its subfolder when a whole folder is sent ("Trip/Day 2") |
| `file_accept` | receiver → sender | `transfer_id`, `offsets` (optional): per file, bytes the receiver already has from an interrupted try |
| `file_reject` | receiver → sender | `transfer_id`, `reason`: `declined` (the person said no, or didn't answer within 50 s), `no_space`, `bad_folder`, `bad_name` |
| *(raw bytes)* | sender → receiver | exactly `size` bytes of the current file |
| `file_end` | sender → receiver | `index` (int), `sha256` (hex) |
| `transfer_done` | receiver → sender | `transfer_id`, `saved`: list of final file names |

Receiver rules:

1. **File names** are sanitised: strip any path parts (`/`, `\`, `..`), drop control
   characters, forbid Windows reserved names (`CON`, `NUL`, …). Empty after cleaning →
   `file_reject` `bad_name`.
2. Write to `<name>.part`; on `file_end` compare SHA-256. Match → rename to the final
   name (`name (2).ext` if taken). Mismatch → delete the `.part`, send `error` `checksum`.
3. Connection drops mid-transfer → delete every `.part` from that transfer.

A receiver may ask its user before answering `file_offer` ("Ask before receiving");
the sender waits up to 60 s for `file_accept` / `file_reject`. Each part of a file's
`path` is cleaned like a file name; a part that is `..` (or cleans to nothing) rejects
the offer with `bad_name`.

**Resuming.** A receiver may keep a big file (≥ 4 MB) that was cut off by a dropped
connection, out of sight (its own private folder; on Android a hidden pending
MediaStore item), keyed by sender, place, name and size. When the same file is offered
again it answers with `offsets`; the sender reads those bytes only for the checksum
and sends the rest. The checksum still covers the whole file. A transfer cancelled on
the receiving side keeps nothing; a checksum mismatch deletes the kept part.
Unfinished files are deleted after 3 days. Senders must honour `offsets`.

### Search

| Type | Direction | Fields |
| --- | --- | --- |
| `search_request` | either | `query`: words that must all appear in the file name (any case) |
| `search_response` | reply | `results`: list of `{folder, path, name, size, modified}`, newest first, at most 200 |

Searches every shared folder and its subfolders (hidden items skipped), for at most
about 2 seconds. Refused with `not_allowed` when browsing is off. A hit can be fetched
with `fetch_request` using its `folder` and `path`.

### Shared clipboard

| Type | Direction | Fields |
| --- | --- | --- |
| `clip_push` | either | `text` (≤ 256 KB): put it on the receiver's clipboard |
| `clip_ok` | reply | — (or `error` `not_allowed` when clipboard sharing is off) |
| `clip_request` | either | — ("paste from that device") |
| `clip_response` | reply | `available` (bool), `text`, or `reason` (sharing off, nothing copied, marked private, or, on Android, LAN Share not on screen) |

Laptops push what the person copies to their online phones as it's copied. Android
apps can only read the clipboard while on screen, so phones push only when the person
asks (assistant, Send sheet, Share → LAN Share with text). Copies that password
managers mark as private (Windows `ExcludeClipboardContentFromMonitorProcessing` /
`CanIncludeInClipboardHistory`=0, Android `EXTRA_IS_SENSITIVE`) are never sent.

### New folder

| Type | Direction | Fields |
| --- | --- | --- |
| `mkdir_request` | either | `folder` (key), `path` (optional), `name` |
| `mkdir_ok` | reply | `name` as created (cleaned). Existing folder is fine. Errors: `bad_folder` |

### Fetch (getting files from the other device)

| Type | Direction | Fields |
| --- | --- | --- |
| `fetch_request` | requester → owner | `transfer_id`, `folder` (key), `path` (optional), `files`: list of file names in that folder |

The owner answers like a sender: `file_offer` (same `transfer_id`, with sizes), and after
the requester's `file_accept` the raw bytes and `file_end` frames; the requester ends
with `transfer_done`. The requester follows the receiver rules above and saves into a
folder of its own choosing (its default). The owner refuses with `error` `bad_folder`
(not a shared folder, or the path leaves it) or `not_found` (a name with path parts, a
hidden or system file, a folder, or a file that isn't there). Only files directly in
the requested folder can be fetched. A device whose user turned browsing off answers
`list_request` and `fetch_request` with `error` `not_allowed`.

## 6. Error codes

`version`, `unknown_type`, `bad_frame`, `not_trusted`, `bad_folder`, `not_found`, `not_allowed`, `checksum`,
`io`, `internal`.

## 7. Versioning

`v` is an integer. Adding an optional field or a new message type does **not** bump it
(unknown fields are ignored, unknown types get `error unknown_type`). Changing the
meaning of an existing field or message **does**.
