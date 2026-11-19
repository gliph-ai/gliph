-- The control plane. Accounts, which teams they are in, where those teams run
-- and whether they are paid up. No message ever touches this database.

CREATE TABLE IF NOT EXISTS accounts (
  id         text PRIMARY KEY,
  email      text UNIQUE NOT NULL,
  pass_hash  text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- An address is proved by a code mailed to it. Until then the account can sign
-- in and nothing else: no team, no membership, no assertion, because every one
-- of those trusts the email.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS verified boolean NOT NULL DEFAULT false;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS verify_digest text NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS verify_expires timestamptz;

-- Bumped when an unconfirmed account is taken over (Store.Signup). A session
-- is good only while it carries the account's current generation, so a login
-- that read the old password and finished after the takeover is dead on
-- arrival.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS session_gen bigint NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS account_sessions (
  token      text PRIMARY KEY,
  account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS account_sessions_account ON account_sessions(account_id);
ALTER TABLE account_sessions ADD COLUMN IF NOT EXISTS gen bigint NOT NULL DEFAULT 0;

-- A shard is a process serving up to `capacity` teams. `pool` is what groups
-- them: shared shards take anybody, and a dedicated customer gets a pool of
-- their own with capacity 1 — same table, no special case.
CREATE TABLE IF NOT EXISTS shards (
  name      text PRIMARY KEY,
  pool      text NOT NULL DEFAULT 'shared',
  base_url  text NOT NULL,
  secret    text NOT NULL,
  capacity  integer NOT NULL DEFAULT 50,
  accepting boolean NOT NULL DEFAULT true
);

-- A team. `host` is what the customer types and is deliberately NOT derived
-- from the shard: naming a customer's address after the machine it happens to
-- sit on is what makes a team impossible to move.
CREATE TABLE IF NOT EXISTS servers (
  slug        text PRIMARY KEY,
  name        text NOT NULL,
  host        text UNIQUE NOT NULL,
  shard       text NOT NULL REFERENCES shards(name),
  plan        text NOT NULL DEFAULT 'shared',
  status      text NOT NULL DEFAULT 'active',   -- active | past_due | suspended
  billing_ref text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS servers_shard ON servers(shard);
-- Who asked for the team, so a creation that failed after this row landed can
-- be retried by the same account instead of answering "taken" for ever.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS created_by text NOT NULL DEFAULT '';
-- false from a status change until the team's shard has confirmed it. The
-- reconciler (API.Reconcile) keeps pushing until it does, so a suspension
-- cannot be lost to one failed call.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS shard_synced boolean NOT NULL DEFAULT true;
-- Bumped by every status change and sent with the push; the shard ignores a
-- push older than what it has, so two pushes arriving out of order cannot
-- leave it on the stale one.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS status_version bigint NOT NULL DEFAULT 0;

-- Who is in which team. This is the authority for single sign-on: an assertion
-- is only ever minted for a row that exists here.
CREATE TABLE IF NOT EXISTS memberships (
  account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  slug       text NOT NULL REFERENCES servers(slug) ON DELETE CASCADE,
  nick       text NOT NULL DEFAULT '',
  role       text NOT NULL DEFAULT 'member',    -- owner | member
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, slug)
);
