# Self-hosting gliph

A self-hosted server is one binary, one team, one directory: `gliph.db`
(SQLite), `uploads/`, `roomnotes/`, and `acme/` (certificates) under `-data`.
Nothing on this page needs PostgreSQL, a bucket or the control plane.

## Listening: TLS 1.3, or say why not

The server will not start until it is told how to listen (`server/listen.go`):

```sh
gliph -tls-domain chat.example.com          # Let's Encrypt; :443, and :80 redirects
gliph -tls-cert cert.pem -tls-key key.pem   # your own certificate; :443, :80 redirects
gliph -plaintext -addr :8787                # plain HTTP, ONLY behind a TLS-terminating proxy
```

The two TLS modes accept TLS 1.3 and nothing older and send HSTS. Port 80
serves only the redirect (and, with `-tls-domain`, the ACME HTTP challenge);
`-http-addr ""` turns it off, `-addr` moves the HTTPS port. Binding 443 and 80
as a normal user needs `CAP_NET_BIND_SERVICE` (`server/install.sh`'s unit grants
it). Certificates are cached in `<data>/acme` and renew on their own.

`-plaintext` is for a proxy that already terminates TLS: gliph.sh runs it
behind Pushleft's edge (`.pl.yaml`), and `bin/run server` runs it on a dev box.
Put nothing else in front of it.

## First run: the setup wizard

Start a fresh server on a terminal with no listener flags and it asks three
things (`server/setup.go`):

1. **TLS**: the domain, then Let's Encrypt, your own certificate files, or a
   proxy in front.
2. **Server name** shown to clients (the domain by default).
3. **Admin account**: nick, email, password (Enter makes one up and prints it).
   It is the first account on an empty server, so it is admin by the same rule
   as any first registration.

It writes the answers to `<data>/gliph.conf`, one flag per line (flags on the
command line override them), prints an invite link
`https://<domain>/?invite=<token>` for the next person, and starts serving.
After that, `gliph -data <dir>` is the whole command line.

`gliph -data <dir> -setup` runs the same wizard and exits, for a server that a
service manager starts (it has no terminal):

```sh
curl -fsSL https://gliph.sh/install.sh | sudo sh -s chat.example.com
sudo -u gliph /opt/gliph/gliph -data /var/lib/gliph -setup
sudo systemctl restart gliph
```

## Backup and restore

```sh
export GLIPH_BACKUP_PASSPHRASE='a long passphrase'   # or type it when asked
gliph -data /var/lib/gliph -backup gliph-2026-11-18.backup
# on the new host, with the same domain pointed at it:
gliph -data /var/lib/gliph -restore gliph-2026-11-18.backup
```

The backup runs beside a live server (`VACUUM INTO` snapshot). The archive
holds the database (accounts, sessions, devices, channels, ciphertext history),
`uploads/`, `roomnotes/`, `gliph.conf` and the certificate cache, as a tar.gz
sealed with AES-256-GCM in 64 KiB chunks under a scrypt key
(`server/backup.go`). A wrong passphrase, or an archive that was cut, edited
or reordered, restores nothing.

Restore needs an empty data dir. Sessions come back with the database, so a
client pointed at the same address reconnects with the token it has and no
one signs in again. Keep the passphrase somewhere apart from the archive:
without it the archive cannot be opened.

A server on PostgreSQL (`DATABASE_URL`, gliph.sh's solo team) writes the same
archive: its tables are copied into the archive's `gliph.db` and its bucket
objects become files. `-restore` with `DATABASE_URL` set loads them back
through the `-import-sqlite` path, into a database with no accounts, and the
files into the bucket. Teams on a shard are not covered; they live in the
managed PostgreSQL and the bucket, and those have their own backups.

## Mobile push through the relay

A self-hosted server has no APNs key or FCM account for the gliph apps, so
mobile push is off unless it points at a relay:

```sh
gliph -data /var/lib/gliph -push-relay https://push.gliph.sh/
```

Without it everything except phone wakes works. The relay receives a device
token and the time of the wake (`server/push.go` has the full note).

### Running the relay

The relay is the same binary in another mode (`server/relay.go`). It keeps no
state and opens no database:

```sh
gliph -relay -tls-domain push.gliph.sh \
  -apns-key AuthKey.p8 -apns-key-id <id> -apns-team-id <team> \
  -fcm-credentials firebase-service-account.json
```

`POST /` takes `{"wake":[{"platform":"ios","token":"..."}]}` (what
`-push-relay` sends), any other field is a 400, and `GET /healthz` answers
for a load balancer. Limits, in memory per process: 300 wakes then 2 a second
per sending address (a /64 on IPv6), 3 then one per 20 seconds per device, 100
wakes per request. Over the budget the extra wakes get a 429.

Not deployed. On Pushleft it would be one more role in `.pl.yaml`, with the
APNs key gliph.sh already has and the Firebase service account as secrets:

```yaml
  relay:
    kind: web
    port: 8788
    health: /healthz
    command: >-
      exec gliph -relay -plaintext -addr :8788 -trusted-proxy 10.128.0.0/9
      -apns-key /secrets/apns.p8 -apns-key-id 9HMKWDYFKP -apns-team-id KF53B4S5HD
      -fcm-credentials /secrets/fcm.json
    secrets:
      - { name: APNS_KEY, path: /secrets/apns.p8 }
      - { name: FCM_CREDENTIALS, path: /secrets/fcm.json }
    resources: { cpu: 0.25, memory: 128MiB }
    route:
      domains: [push.gliph.sh]
```

plus an A record for `push.gliph.sh` at Porkbun. `-trusted-proxy` matters:
the limits count the address in `X-Forwarded-For` only from Pushleft's router.
