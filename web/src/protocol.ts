// Wire types. Mirrors docs/protocol.md — keep the two in step.

export type Frame = { t: string; d?: any };

export type Arg = {
  name: string;
  type: "text" | "nick" | "channel" | "int" | "file" | "rest";
  required?: boolean;
};

/** One palette entry. The client NEVER hardcodes commands: this whole list
 *  arrives from the server and is replaced whenever a plugin comes or goes. */
export type Command = {
  name: string;
  usage: string;
  desc: string;
  args: Arg[] | null;
  scope: "channel" | "dm" | "any" | "global";
  min_role: "member" | "op" | "admin";
  source: string; // "builtin" | plugin id
};

export type Member = {
  user_id: string;
  nick: string;
  role: "op" | "voice" | "member";
  away?: string;
  online?: boolean;
  kind: "user" | "agent" | "bot";
  /** has a device no other device of the account approved: it reads nothing encrypted */
  unverified?: boolean;
};

export type ChannelView = {
  id: string;
  name: string;
  topic: string;
  modes: string;
  /** "open": joiners read what came before. "sealed": they start at their join. */
  history?: "open" | "sealed";
  is_dm: boolean;
  role: string;
  seq: number;
  members: Member[];
  has_key: boolean;
  /** the mini-apps attached to this channel, rendered in the switcher */
  blobs?: { id: string; title: string; mode: string; source: string }[];
  room?: { since: string; users: RoomPerson[] };
};

export type EventKind =
  | "message" | "action" | "join" | "part" | "mode"
  | "topic" | "kick" | "notice" | "agent" | "file" | "room_invite";

/** Who is standing in a channel's 3D room. Sent with the channel while
 *  somebody is inside; the comings and goings after it are room.event
 *  relays, which reach every member whether or not they are in there. */
export type RoomPerson = { user_id: string; name: string; room: string; voice: boolean };

export type Ev = {
  seq: number;
  id: string;
  channel: string;
  kind: EventKind;
  actor: string;
  ts: string;
  /** Plaintext for server-generated events; base64 ciphertext when `enc`. */
  body: string;
  /** Set in a +e channel: the body is ciphertext under the channel key for
   *  `epoch`, and only a member device can read it. */
  enc?: boolean;
  epoch?: number;
  meta?: Record<string, any>;
};

export type User = { id: string; nick: string; role: string; away?: string };

