PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS users (
  id         TEXT PRIMARY KEY,
  nick       TEXT UNIQUE NOT NULL,
  email      TEXT UNIQUE NOT NULL,
  pass_hash  TEXT NOT NULL,
  role       TEXT NOT NULL DEFAULT 'member',  -- member | admin
  away       TEXT NOT NULL DEFAULT '',
  suspended  INTEGER NOT NULL DEFAULT 0,
  -- What the missed-message digest runs on (email.go): the last moment the
  -- account was connected, and the last time it was mailed. Empty until each
  -- first happens, and '' sorts below every RFC3339 stamp by design.
  last_seen   TEXT NOT NULL DEFAULT '',
  last_digest TEXT NOT NULL DEFAULT '',
  -- The account master signing key (base64 SPKI). It signs each device; a
  -- device without its signature is sealed no channel keys (crypto.go).
  master_key  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  token       TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  -- Set only for a session minted by the device-link flow, so the account can
  -- list what is signed in and sign out one device. A browser login leaves
  -- both empty.
  device_name TEXT NOT NULL DEFAULT '',
  platform    TEXT NOT NULL DEFAULT ''
);
-- Every "sign this account out" path filters by user_id — an admin kick, a
-- password reset, the device list, revoking one device. Without this the only
-- index is the token primary key, so each of them scans the whole table.
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS invites (
  token      TEXT PRIMARY KEY,
  role       TEXT NOT NULL DEFAULT 'member',
  used_by    TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS channels (
  id         TEXT PRIMARY KEY,
  name       TEXT UNIQUE NOT NULL,
  topic      TEXT NOT NULL DEFAULT '',
  modes      TEXT NOT NULL DEFAULT 'nt',      -- i invite-only, m moderated, n no-external, t op-topic, s sounds
  chan_key   TEXT NOT NULL DEFAULT '',        -- +k
  is_dm      INTEGER NOT NULL DEFAULT 0,
  history    TEXT NOT NULL DEFAULT 'open',    -- open | sealed
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS members (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL DEFAULT 'member',  -- member | voice | op
  cursor     INTEGER NOT NULL DEFAULT 0,   -- how far we have DELIVERED
  read_seq   INTEGER NOT NULL DEFAULT 0,   -- how far the user has READ, synced across their devices
  joined_seq INTEGER NOT NULL DEFAULT 0,
  joined_epoch INTEGER NOT NULL DEFAULT 0,  -- newest key epoch at join; a sealed channel seals nothing older
  PRIMARY KEY (channel_id, user_id)
);

CREATE TABLE IF NOT EXISTS channel_invites (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  by_nick    TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (channel_id, user_id)
);

CREATE TABLE IF NOT EXISTS bans (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  nick       TEXT NOT NULL,
  reason     TEXT NOT NULL DEFAULT '',
  by_nick    TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (channel_id, nick)
);

CREATE TABLE IF NOT EXISTS events (
  id         TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  actor      TEXT NOT NULL,
  ts         TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  meta       TEXT NOT NULL DEFAULT '{}',
  nonce      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS events_chan_seq ON events(channel_id, seq);
CREATE INDEX IF NOT EXISTS events_chan_ts ON events(channel_id, ts);
CREATE UNIQUE INDEX IF NOT EXISTS events_nonce ON events(channel_id, nonce) WHERE nonce <> '';
-- The devlog blob's only query (Store.DevlogEvents). Without it, a channel with
-- few commits walks its whole history backwards looking for the newest fifty.
-- Partial, so it costs nothing on the chat messages that are ~all of the table;
-- the WHERE has to stay character-identical to the query's or SQLite will not
-- prove the implication and will ignore the index.
CREATE INDEX IF NOT EXISTS events_devlog ON events(channel_id, seq)
  WHERE kind IN ('commit','log');

CREATE TABLE IF NOT EXISTS plugins (
  id         TEXT PRIMARY KEY,
  token      TEXT UNIQUE NOT NULL,
  display    TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'bot',     -- bot | agent
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_channels (
  plugin_id  TEXT NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  PRIMARY KEY (plugin_id, channel_id)
);

-- A channel op removed this plugin (/kick): its own subscribe list no longer
-- brings it back. Only an op's /invite does, which clears the row.
CREATE TABLE IF NOT EXISTS plugin_kicks (
  plugin_id  TEXT NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  PRIMARY KEY (plugin_id, channel_id)
);

CREATE TABLE IF NOT EXISTS uploads (
  id         TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL,
  owner      TEXT NOT NULL,
  name       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  received   INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  complete   INTEGER NOT NULL DEFAULT 0
);

-- ---- end-to-end encryption ----
-- The server stores public keys and opaque wrapped blobs only. It never holds
-- a private key and never sees a plaintext message body in a +e channel.

CREATE TABLE IF NOT EXISTS devices (
  id         TEXT PRIMARY KEY,               -- client-generated, stable per install
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL DEFAULT '',
  pubkey     TEXT NOT NULL,                  -- base64 SPKI, P-256 ECDH public key
  sig        TEXT NOT NULL DEFAULT '',       -- master key's signature over this device; '' = unapproved
  sealed_master TEXT NOT NULL DEFAULT '',    -- the master secret sealed to this device by its approver
  legacy     INTEGER NOT NULL DEFAULT 0,     -- registered before signing existed
  created_at TEXT NOT NULL,
  last_seen  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS devices_user ON devices(user_id);

-- One row per (channel, epoch, recipient device): the channel key sealed to
-- that device. Only the holder of the device private key can open it.
CREATE TABLE IF NOT EXISTS channel_keys (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  epoch      INTEGER NOT NULL,
  device_id  TEXT NOT NULL,
  wrapped    TEXT NOT NULL,                  -- base64: ephemeral pubkey ‖ iv ‖ ciphertext
  by_device  TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (channel_id, epoch, device_id)
);
CREATE INDEX IF NOT EXISTS channel_keys_dev ON channel_keys(device_id);

-- ---- blobs (channel mini-apps) ----

-- One stored document per shared blob, replaced on every publish.
CREATE TABLE IF NOT EXISTS blob_docs (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  blob_id    TEXT NOT NULL,
  doc        TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (channel_id, blob_id)
);

CREATE TABLE IF NOT EXISTS todos (
  id         TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  author     TEXT NOT NULL,
  assignee   TEXT NOT NULL DEFAULT '',
  done       INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  done_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS todos_chan ON todos(channel_id, done);

CREATE TABLE IF NOT EXISTS pins (
  id         TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  event_id   TEXT NOT NULL DEFAULT '',
  author     TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS pins_chan ON pins(channel_id);

-- Daily check-ins: one row per member per day, upserted.
CREATE TABLE IF NOT EXISTS checkins (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  day        TEXT NOT NULL,
  body       TEXT NOT NULL,
  ts         TEXT NOT NULL,
  PRIMARY KEY (channel_id, user_id, day)
);

-- Devlog webhooks. The token is both the URL path segment and the shared
-- secret (GitHub HMAC key, GitLab token header), so deleting the row revokes
-- the address and the secret at once. There is no devlog table: commits and
-- log lines are ordinary rows in events.
CREATE TABLE IF NOT EXISTS hooks (
  id         TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  token      TEXT UNIQUE NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'git',      -- git (github + gitlab pushes)
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS hooks_chan ON hooks(channel_id);

-- ---- admin audit log (F14) ----
-- The target is stored as free text, never a foreign key: an audit row has to
-- outlive the thing it records. "deleted user robin" must still read as that
-- after robin's row is gone, and a FK with ON DELETE CASCADE would erase the
-- evidence along with the evidence's subject.
CREATE TABLE IF NOT EXISTS audit_log (
  id         TEXT PRIMARY KEY,
  actor      TEXT NOT NULL,                  -- nick at the time of the action
  action     TEXT NOT NULL,                  -- suspend | role | delete | archive | ...
  target     TEXT NOT NULL DEFAULT '',       -- nick or #channel, as text
  detail     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_ts ON audit_log(created_at DESC);

-- ---- mobile push (F5, F14) ----
-- Registration only. There is no notification table and no queue: the payload
-- carries nothing, so there is nothing to store. See push.go.
CREATE TABLE IF NOT EXISTS push_devices (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  platform   TEXT NOT NULL,                  -- ios | android
  token      TEXT UNIQUE NOT NULL,           -- write-only through the API
  created_at TEXT NOT NULL,
  last_seen  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS push_devices_user ON push_devices(user_id);

-- ---- 3D room (see room.go) ----
-- Presence, movement and chat are relayed and never stored. What persists is
-- exactly what outlives a session: the shared boards and the voice-note wall.

-- One row per (channel, kind): the whole board document, replaced on save.
CREATE TABLE IF NOT EXISTS room_boards (
  channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,                  -- tasks | drawing
  data       TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (channel_id, kind)
);

-- Voice notes left in a sub-room. The audio bytes live on disk at
-- <data>/roomnotes/<id>, the way uploads live at <data>/uploads/blob_<id>.
CREATE TABLE IF NOT EXISTS room_notes (
  id           TEXT PRIMARY KEY,
  channel_id   TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  room         TEXT NOT NULL,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  duration_ms  INTEGER NOT NULL,
  size_bytes   INTEGER NOT NULL,
  content_type TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
-- The wall's only two queries: newest-first per (channel, room), and the reap.
CREATE INDEX IF NOT EXISTS room_notes_wall ON room_notes(channel_id, room, created_at DESC);

-- ---- device link (PKCE loopback) ----
-- A pending authorization code. Stored as a digest, never in the clear: for its
-- five minutes a code IS a credential, so it is kept the way a password is.
CREATE TABLE IF NOT EXISTS device_links (
  id             TEXT PRIMARY KEY,
  code_digest    TEXT UNIQUE NOT NULL,
  user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_challenge TEXT NOT NULL,                -- PKCE S256
  redirect_uri   TEXT NOT NULL,                -- validated loopback-only
  device_name    TEXT NOT NULL DEFAULT '',
  platform       TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  expires_at     TEXT NOT NULL
);

-- ---- server settings (motd, org icon) ----
-- A key/value table because there are two rows and no queries: a settings
-- table with a column per setting needs a migration every time an operator
-- gets a new knob.
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);

-- ---- single sign-on (sso.go) ----
-- Assertion ids already redeemed, so each assertion signs in once. Kept until
-- the assertion would have expired anyway.
CREATE TABLE IF NOT EXISTS sso_spent (
  jti TEXT PRIMARY KEY,
  exp INTEGER NOT NULL
);
