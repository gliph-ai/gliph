# Data protection for Gliph operators

You run a Gliph server, so you are the controller of the personal data on it.
This page lists the data the server stores, the data it can read, and the
steps for an access or erasure request. Two templates sit next to it:
[privacy-template.md](privacy-template.md) and
[terms-template.md](terms-template.md). Fill in the brackets, check them with
your own counsel, and publish them where your users sign up.

## Ciphertext storage

All cryptography runs on the client (`core/src/crypto.rs`). The server
(`server/crypto.go`) stores public keys and relays sealed data.

1. Each device makes a P-256 key pair. The private key stays on the device.
   The server stores the public key and the account's signature over it.
2. A channel with mode `+e` has one AES-256 key per epoch. A client seals that
   key to each member device (ECDH-ES, HKDF-SHA256, AES-GCM). The server
   stores the sealed copies and gives each device only its own copy.
3. Message bodies in a `+e` channel are AES-256-GCM ciphertext. The server
   stores the ciphertext and the epoch number. It has no key that opens them,
   even with full database access.
4. Removing a member starts a new epoch. Later messages use a key that the
   removed member's devices never get.

Mode `+e` is a channel setting. New channels open with modes `nt`, so a
channel that nobody set to `+e` stores its message bodies in plain text. Tell
your users which of your channels are encrypted.

The scheme is sender keys. A stolen device key opens every message that the
device could already read. It does not give per-message forward secrecy.

### Data the server can read

| Data | Readable by the operator |
|---|---|
| Message bodies in a `+e` channel, including reply quotes | No |
| Message bodies in a channel without `+e` | Yes |
| Uploaded files and room voice notes | Yes. Uploads are stored as sent, and expire after 7 days by default (`UploadTTL`) |
| Account: nick, email, password hash (bcrypt), role, away text, last-seen time | Yes |
| Channel names, topics, modes, member lists, roles, bans, invites | Yes |
| Event metadata: channel, sequence number, kind, sender nick, time | Yes, for every channel |
| Device names, device public keys, sealed channel keys | Names and public keys yes. Sealed keys no |
| Sessions: token, creation time, device name, platform | Yes. A session ends after 90 days |
| Push tokens (APNs or FCM) per device | Yes |
| Todos, pins, check-ins, room boards, blob documents | Yes |
| Audit log of admin actions | Yes |
| IP addresses | In memory only, for rate limits |

Push notifications and email digests carry no message content. A push is a
content-free wake (`server/push.go`). A digest says how many messages are
unread and in which channels (`server/email.go`).

## Data-subject requests

You have one month to answer an access or erasure request (GDPR Art. 12(3)).
Check the identity of the person first, for example with a message from the
account itself.

### Access and portability

1. Ask the person to run `/export` in each channel and DM. The client writes
   the history it holds as text and JSONL. Only their own device can do this
   for a `+e` channel, because only the device has the keys.
2. Export the server-side rows for the account. With SQLite:

   ```bash
   sqlite3 -json data/gliph.db "SELECT id,nick,email,role,away,last_seen,created_at FROM users WHERE nick='NICK'"
   ```

   Then run the same query, with `user_id = '<id>'`, on `sessions`, `members`,
   `devices`, `push_devices`, `checkins` and `room_notes`. Use
   `actor = 'NICK'` on `events` and `audit_log`, and `owner_id = '<id>'` on
   `uploads`. With PostgreSQL, use `psql` and the same queries.
3. Send the person the files. Event bodies from a `+e` channel are ciphertext
   in this export. Tell the person that step 1 is the readable copy.

### Erasure

1. Open the admin console, find the account under Users, and delete it
   (`DELETE /api/admin/users/{id}`). This deletes the account, its sessions,
   memberships, channel invites, devices, push tokens, check-ins, voice-note
   rows and pending device links. It also deletes the person's DMs.
2. Messages the person posted in shared channels stay, under their old nick.
   Other members have a right to their own history. If the person asks for
   their messages to go too, delete them:

   ```sql
   DELETE FROM events WHERE actor = 'NICK' AND kind IN ('message','action');
   ```

   Members who already synced those messages keep them on their devices. The
   server cannot reach into a client.
3. Delete their uploads from the files directory or bucket, using the `path`
   column of `uploads WHERE owner_id = '<id>'`, then delete those rows.
4. Leave the `audit_log` rows. They record admin actions, and you may need them
   to show that you handled the request.
5. Backups made with `gliph -backup` still hold the data until they expire.
   Say so in your answer, and give the date the last such backup expires.

### Rectification and restriction

The person changes their own nick with `/nick`. An admin changes a role in
the console. To correct an email address, run
`UPDATE users SET email = '<new>' WHERE id = '<id>'`. Suspend an account to
stop all processing for it while a complaint is open. A suspended account
cannot sign in, and its data stays unchanged.

## gliph.sh, the hosted service

gliph.sh runs on Pushleft, on Hetzner servers. It uses these processors and
stores:

| Store or processor | Holds |
|---|---|
| PostgreSQL (Pushleft) | Every table listed above |
| `files` bucket (Pushleft) | Uploads, room voice notes, desktop releases |
| Amazon SES, eu-west-1 | Email address, digest counts and channel names. Sends as `noreply@gliph.sh` |
| Apple Push Notification service | The device push token and a content-free wake |
| coturn TURN relay (Pushleft) | Relayed bytes of a direct file transfer, sealed by DTLS. Nothing is stored |

The web, desktop and iOS clients send no analytics and no crash reports. A
client talks to the server you signed in to. The desktop app also checks
`gliph.sh/downloads` for updates, on every server, self-hosted ones included.
By default, link previews, YouTube embeds and the room whiteboard load a
third-party page only after a click. A user can set previews to load always.
