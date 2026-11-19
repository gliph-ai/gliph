# Deploying gliph

Production is **https://gliph.sh**: the `gliph` project on Pushleft (org
`empire`, environment `production`), described by `.pl.yaml`. It moved there
from the RKE2 node in November 2026; `deploy/prod` (OpenTofu) is what ran it
before and is no longer applied.

## What runs

The Go server is the whole web role: it owns `/api` and `/ws` and serves the
built web client (and `/admin`) off the same origin. A `turn` role runs coturn.

State lives off the instance, so a deploy starts the new instance before it
stops the old one and there is no gap:

- **PostgreSQL** (`data.postgres` in `.pl.yaml`, plan `dev`). The server reads
  `DATABASE_URL`; without it, it opens `gliph.db` (SQLite) in `-data`, which is
  what a self-host does. Every query is written once for both
  (`server/sqldb.go`), and `GLIPH_TEST_PG=postgres://localhost/postgres go test
  ./...` runs the whole suite against PostgreSQL.
- **The `files` bucket** (`pl storage`, private). Uploads under `uploads/`,
  room voice notes under `roomnotes/`, desktop releases under
  `downloads/desktop/` (`server/files.go`). The server gets the bucket's key as
  the `S3_*`/`AWS_*` env secrets and serves everything through itself.

Until 2026-11-17 state was SQLite on a 20 GiB volume (`data`, at `/data`). A
role with a volume is stopped before its replacement starts, so every deploy
was a few seconds of 503s. The move imported `gliph.db` into PostgreSQL at
boot (`-import-sqlite`, `server/import.go`: only into an empty database, in one
transaction, before the server listens). The SQLite file from that day is kept
on Waseem's Mac in `~/Backups/gliph-prod-2026-11-17/`.

The web role runs with `-plaintext`: Pushleft's edge terminates TLS and
forwards plain HTTP, and without the flag the server refuses to listen
(`docs/selfhost.md`).

Shell access: `pl --org empire exec web --production -- sh`.

## DNS

Registered at Porkbun, DNS served by Porkbun (API access is enabled on the
domain, so records can be changed over the API — no dashboard trip needed).

| name | type | value |
|------|------|-------|
| `gliph.sh` | A | 148.251.246.134 (Pushleft, pl-app-2) |
| `www.gliph.sh` | A | 148.251.246.134 |
| `<token>._domainkey.gliph.sh` | CNAME | `<token>.dkim.amazonses.com` (three of them, see below) |

The parking records (apex `ALIAS` and `*.gliph.sh CNAME` → `pixie.porkbun.com`)
were deleted. Both hosts are on the ingress and the certificate.

## Releasing

Pushing to `main` on https://github.com/gliph-ai/gliph is the release.
`.github/workflows/release.yml` runs it. The `deploy` job stamps
`GLIPH_VERSION` into `.pl.yaml` and runs `pl up --production`: Pushleft builds
the image, gates it on its image scan, and rolls it out.

The desktop apps ship with it, on GitHub-hosted runners: a Developer ID-signed,
notarized Mac app (macOS), a Windows installer (Windows) and a Linux `.deb`
(Ubuntu), all signed for the updater. The `publish` job writes `latest.json`
and uploads everything with the `files` bucket's own S3 key to
`downloads/desktop/<version>/`, then `downloads/desktop/latest.json` last,
which is what makes the version live. `tools/release-secrets` sets the
repository secrets. `bin/release-desktop` still builds and publishes the same
release from a Mac by hand.
Testers find them at **https://gliph.sh/downloads**. Running copies poll
`latest.json` every 15 minutes and show a ★ update button; `/status` prints
which build anyone is on. The version is `bin/version`: patch = commit count,
so every push is a strictly newer release with nothing bumped by hand.

Only the phone is still manual:

```sh
bin/ota ios        # builds and installs over Wi-Fi (mobile/tools/deploy-device.sh)
bin/ota desktop    # a working-tree build into /Applications, no updater — for local testing
```

`bin/ota` pins `GLIPH_SERVER=https://gliph.sh` for both — `bin/install-app` on
its own does not, and an unpinned desktop build opens on an empty server field.
TestFlight is the other iOS route (`mobile/tools/testflight.sh`); use it when the
phone is not on this network, and expect Apple's ~10 minutes of processing.

By hand, if the runners are down: `pl up --production` from the repo root.

## The pinned server

A production build bakes its server address into the bundle
(`GLIPH_SERVER=https://gliph.sh`, wired up in `web/vite.config.ts` and read as
`pinnedServer` in `web/src/client.ts`). The iOS app takes the same value as an
`xcodebuild` setting, landing in `Info.plist` as `GliphServer`. This matters for
both: neither has a page origin that is a server, so without the pin they open
on an empty field.

The login screen therefore hides the server field in a pinned build.
**Triple-tap the gliph logo to reveal it** — one tester can point the shipped
app at a staging server without a rebuild, and a self-hoster's own build (no
`GLIPH_SERVER`) shows the field as it always did.

## Accounts

The first account created on a fresh server becomes admin — that is `waseem`,
created 2026-08-26. Everyone else needs an invite:

```sh
curl -sX POST https://gliph.sh/api/admin/invites \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"role":"member"}'
```

or the admin console at https://gliph.sh/admin, which signs in with the
same credentials — its **invites** tab creates the invite, copies the redeem
link (`https://gliph.sh/?invite=<token>`, which opens the signup form with the
code filled in), and revokes any invite nobody has used yet. Its **users** tab
also provisions an account outright (nick, email, password, role) for the case
where you already know who is joining, and resets a password — a reset signs
that account out of every device, which is the point of one. The offline
equivalent, for when nobody can reach the console, is still
`pl --org empire exec web --production -- sh -c 'gliph -passwd nick:newpassword'`
(the role's `DATABASE_URL` is in that shell's environment).

## STUN/TURN (coturn)

`deploy/prod/coturn.tf` runs coturn on the node — hostNetwork (TURN is raw
public UDP, a ClusterIP cannot carry it), riding `gliph.sh` itself on 3478
(the DNS already points at the node), relay range 49160–49200, listeners and
relays pinned to the public IP (`turn_public_ip`) so CNI addresses never leak
into candidates, RFC1918/CGNAT/link-local peers denied. Setting `turn_secret`
in the tfvars deploys it AND hands the server its `-stun`/`-turn`/`-turn-secret`
flags in the same apply; the server mints ephemeral `use-auth-secret`
credentials from that shared secret (`server/ice.go`) and serves them at
`GET /api/ice` to p2p transfers and room0 voice.

UDP 3478 and the relay range 49160–49200 are open in Hetzner Robot's firewall
as of 2026-09-03, so `stun_url` is `stun:gliph.sh:3478` and both TURN
transports are first-class — verified end-to-end against prod with a real
credential over UDP (401 challenge → authed Allocate → relayed address
`168.119.64.32:49197`, inside the range).

Before that the firewall dropped unsolicited inbound UDP while passing all
TCP, which is worth remembering if candidates ever go quiet again: TURN still
worked over its TCP transport, and the relay↔peer UDP leg still got through
because ICE makes the relay send first, opening the stateful pinhole. So a
symptom of "UDP closed" here is a working call with every candidate relayed
over TCP, not a dead one.

## Not deployed yet

No plugins or bots (the console's **plugins** tab registers one and shows its
token once, but nothing is connected). The push relay (`gliph -relay`) is built and not deployed; `docs/selfhost.md` has the role it would be. `-push-relay` is an optional server
flag with a Terraform variable that defaults to empty.

## iOS push (direct APNs)

The server pushes to Apple itself; there is no relay in between. It needs the
team's **APNs auth key**: one per team, shared by every app on it, minted once
in the developer portal (Keys → **+** → *Apple Push Notifications service*;
Apple shows the `.p8` once). gliph.sh uses the existing team key, held in
`terraform.tfvars` as `apns_key` / `apns_key_id` (see `tfvars.example`); a
`tofu apply` carries it over.

The key lands as the `apns` secret mounted at `/secrets/apns.p8`, and the server
starts with `-apns-key … -apns-key-id … -apns-team-id KF53B4S5HD`. The topic is
the bundle id `chat.gliph.mobile`. Production and sandbox gateways are both
tried, so a phone installed from Xcode and one from TestFlight are woken by the
same server. Unset, iOS push is simply off.

## Android push (direct FCM)

The Android twin. The server signs in to Google with a Firebase project's
**service account** and sends a data-only `wake`, the same empty payload. It
needs the service-account JSON (Firebase console → Project settings → Service
accounts → *Generate new private key*), stored as the `FCM_CREDENTIALS` file
secret mounted at `/secrets/fcm.json`, and the server started with
`-fcm-credentials /secrets/fcm.json`. The project id is read from the JSON.
Unset, Android push is simply off. gliph.sh has no Firebase project yet, so the
flag is not in `.pl.yaml`.

## Outbound email (Amazon SES)

Email is off unless the server is given credentials, and only the missed-message
digest uses it. What it may contain is fixed by the same rule that governs push:
gliph holds ciphertext, so a digest carries unread counts and channel names and
never a line of what was said (`server/email.go`).

gliph.sh uses the shared **devops@erip.me** AWS account, eu-west-1, the same
credential husttle, lugha and verna send on. It is out of the SES sandbox
(50,000/day, 15/sec), so it can mail any address, not just verified ones.
Credentials live in `terraform.tfvars` as `ses_access_key_id` /
`ses_secret_access_key`; rotating them means rotating the other services too.

The server starts with `-ses-key-id … -ses-secret … -ses-region eu-west-1
-mail-from …`. There is no AWS SDK: SES is one signed POST, and `server/email.go`
signs it with SigV4 in about forty lines.

**`mail_from` has to be an identity verified in that account.** gliph.sh
verified as a domain identity on 2026-09-10, with its three DKIM CNAMEs
published at Porkbun (the tokens are in the DNS table above, and SES lists them
under the identity if they are ever needed again). Digests go out as
`noreply@gliph.sh`. `devops@erip.me` is verified on the same account and is
what to fall back to if the domain is ever removed.
