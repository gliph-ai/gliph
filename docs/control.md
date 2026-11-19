# The control plane, shards and tenancy

Gliph's unit of tenancy is a **server**. A server IS a team: its own tables,
its own accounts, its own `#general`. Self-hosting never touches anything on
this page: a self-hosted server is one team on one SQLite file, and it has no
shard, no control plane and no single sign-on.

Three pieces, in the order a request meets them. All three are hosted-only.

## 1. The shard (`server/`, `-shard`)

One process, many teams. `server/shard.go` resolves the `Host` header to a
tenant and hands the request to code that has never heard of tenancy. Every
query in `store.go` and every frame in `server.go` is the same single-tenant
code, holding a `*Server` that belongs to one team.

That is the whole security model, and it is why a shard is safe to share: there
is exactly **one** place to get tenancy right, instead of a `WHERE tenant_id` on
each of ~120 queries, each its own chance to leak one team's messages into
another's. `TestATokenFromOneTeamIsWorthlessOnAnother` asserts it.

Where a team's data lives (since 2026-11-17, when gliph.sh left its volume):

```
PostgreSQL schema t_<slug>          the team's tables; its Store connects with
                                    search_path=t_<slug>, so no query names it
bucket prefix tenants/<slug>/       its uploads/ and roomnotes/
public.gliph_tenants                slug, name, hosts: what a restart reloads
```

`TestTeamsAreSeparateSchemas` asserts that an account made on one team is not a
row the other can see. A restart reads `gliph_tenants` and serves what it was
serving, with no control plane in the loop.

`-shard` refuses to start without `DATABASE_URL` and the `S3_*` bucket: on
Pushleft an instance's disk does not outlive a deploy, so a team kept there
would vanish with the next one. Without `-shard` the binary is one team (the
solo team) answering whatever `Host` arrives: PostgreSQL's public schema and
the bucket's root on gliph.sh, SQLite and files in `-data` on a self-host.

```
gliph -shard -shard-secret $SECRET -control-key $PUBKEY   # DATABASE_URL, S3_* in the env
```

- `POST /shard/tenants` provisions a team and returns its owner invite, which
  stays with operations: a hosted team's people, its owner included, come in
  through single sign-on, where the control plane's "owner" role makes the
  creator admin. A hosted team's first *password* account needs an invite (no
  "first to register is admin"); single sign-on creates accounts without one. Idempotent for the same slug and addresses, so the
  control plane can retry.
- `GET /shard/tenants` lists what this shard serves.
- Both need `Authorization: Bearer $SHARD_SECRET`. No secret configured means
  the shard accepts no provisioning at all.
- An unknown `Host` is a **404**, never a team created on the spot: wildcard DNS
  points every name on the domain here.

## 2. The control plane (`control/`)

Its own PostgreSQL database, separate from the teams'. Accounts, which teams they are in,
which shard each team lives on, and whether it is paid up. It knows no message,
channel or nick: those are in the team's own schema, which nothing here reads.

It is deliberately **off the chat path**. A team's server answers every request
from its own schema; putting session lookup in the control plane would add a
hop per request and a shared point of failure in front of every team.

```
control -dsn postgres://... -domain gliph.chat -key $PRIVKEY -admin-secret $OPS
control -genkey     # prints the keypair: -key here, -control-key on every shard
```

**Placement** is a row and an `ORDER BY`, not a service:

```sql
SELECT s.name FROM shards s LEFT JOIN servers v ON v.shard = s.name
WHERE s.accepting AND s.pool = $1
GROUP BY s.name, s.capacity HAVING COUNT(v.slug) < s.capacity
ORDER BY COUNT(v.slug), s.name LIMIT 1
```

A dedicated customer is a pool of their own with capacity 1. Same table, no
special case.

**Billing** lives here and only here. `servers.status` is `active`, `past_due`
or `suspended`; whatever decides it — a provider's webhook, an invoice run, a
person — writes `POST /api/servers/{slug}/status` and nothing else in Gliph
learns how it was decided. A suspended team cannot be signed in to; `past_due`
still can, because locking a team out of its own history over a failed card is a
thing to threaten, not to do. **No payment provider is integrated yet** — the
column and the endpoint are the seam it will attach to.

## Moving a team between shards

```
control -move acme:chuckles-2                       # from a shell, with -dsn
POST /api/servers/acme/move {"shard":"chuckles-2"}  # Authorization: Bearer $GLIPH_ADMIN_SECRET
```

One operation, four steps (`control/move.go`, `server/shardmove.go`):

1. **Drain.** The source shard's `POST /shard/tenants/{slug}/release` closes
   the team's sockets, answers its address with 503 (clients retry), waits
   for queries in flight and closes its connections.
2. **Move.** The same call sets the team's `gliph_tenants.shard` to empty.
   No data moves: the schema and the bucket prefix are where every shard
   finds them.
3. **Resume.** The target's `POST /shard/tenants/{slug}/adopt` claims the row
   (only if no shard holds it) and opens the team, suspension included.
4. **Repoint.** The control plane's `servers.shard` names the target.

A failed adopt is followed by an adopt on the source, so the team stays where
it was. Each step is idempotent, so a move that died halfway finishes when run
again. Sessions are rows in the team's schema, so nobody signs in again.
`TestMovingATeamBetweenShards` (server) and `TestMovingATeamIsOneCommand`
(control) cover both halves.

## 3. Single sign-on

One account, every hosted team, without the control plane being a hop on the
chat path:

1. The client asks `POST /api/servers/{slug}/sso` on the control plane.
2. It answers with a signed assertion — "this email may be `<nick>` on acme" —
   good for one minute.
3. The client posts it to the team's own server at `POST /api/sso`.
4. The shard verifies the Ed25519 signature, the expiry, and that the
   **audience** names *this* team, then finds or creates the local user and
   mints an ordinary session. Downstream, it is indistinguishable from a
   password login, because it is one.

The audience check is what makes a shared shard safe: without it, an assertion
for one team would be a session on its neighbour
(`TestAnAssertionForOneTeamIsWorthlessOnAnother`).

A shard with no `-control-key` refuses every assertion. **Self-hosted servers
keep passwords and invites and never grow a second front door.**

The assertion format is defined in two modules that cannot import each other, so
it is pinned from both sides by the same literal: `control`'s
`TestTheWireFormatIsStable` mints it, `server`'s
`TestTheControlPlaneWireFormatIsStable` verifies it. Change one and the other
fails.

## Naming, and the one rule that matters

A customer's address names the **team**, never the shard: `acme.gliph.chat`, not
`chuckles-1-acme`. Name it after the machine and the team can never move. Shard
hostnames exist for operations only.

Routing is the shard's route on Pushleft with every team host as a domain, under
a wildcard record. With every team in the one managed PostgreSQL and the one
bucket, moving a team between shards moves no data: it is a registry change
and a route change.

## What is not done

- **The clients do not call the control plane yet.** They hold their own list of
  servers (`core/src/servers.rs`) and sign in to each with a password, which is
  exactly what a self-hoster does forever. The account-level sign-in and the
  server directory are built and tested; wiring the login screen to them is the
  remaining step.
- No payment provider.
- No deploy config for `control/` and no `-shard` on gliph.sh yet: gliph.sh is
  still the solo team. `.pl.yaml` needs a `control` role with its own database,
  and gliph.sh's own team needs a home once the web role shards.
- A move repoints the registry and the directory, not the platform's route.
  While every shard is behind one route (one wildcard domain) that is the
  whole move; with shards on separate Pushleft roles, the team's host has to
  be on the target role's `route.domains` too.
- **A connection pooler before real shards.** Every team holds up to one
  writer and four reader connections while busy (idle teams release theirs
  after a minute), against ~100 `max_connections` on the dev plan. Pushleft is
  building a managed PgBouncer (requested 2026-11-17). It has to run
  transaction mode with `max_prepared_statements` (pgx prepares statements)
  and `track_extra_parameters = search_path, default_transaction_read_only`:
  without the second, a pooled connection runs one team's queries in another
  team's schema. Run `TestTeamsAreSeparateSchemas` through it before enabling.
