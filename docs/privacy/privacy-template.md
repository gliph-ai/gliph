# Privacy notice for [SERVER NAME]

Template for Gliph operators. Replace every `[BRACKET]`, delete the lines
that do not apply, and have it checked before you publish it. The facts behind
each line are in [operators.md](operators.md).

---

**Controller:** [ORGANISATION], [ADDRESS], [CONTACT EMAIL].
[Data protection officer: NAME, EMAIL. Delete if you have none.]

**Last updated:** [DATE]

## The data we keep

- **Your account:** nick, email address, a hash of your password, your role,
  your away text and the last time you were connected.
- **Your devices:** a name you give each device, its public encryption key,
  and its push token if you allow notifications.
- **Your channels:** the channels you are in, your role in each, and
  invitations and bans.
- **Your messages:** in an encrypted channel (mode `+e`), we store only
  ciphertext and cannot read it. In other channels we store the text you post.
  For every message we store the channel, the time and your nick.
- **Your files:** files and voice notes you upload. We delete uploads after
  [7 DAYS].
- **Security records:** session tokens, an audit log of admin actions, and
  your IP address in memory for rate limits. We do not write IP addresses to
  disk.

We use no analytics, no advertising and no crash reporting.

## Purpose and legal basis

We use this data to run the chat service you signed up for (GDPR Art. 6(1)(b),
contract). We use the audit log and rate limits to keep the service secure
(Art. 6(1)(f), legitimate interest).

## Processors and transfers

[List the services your server uses. For example:]

- [HOSTING PROVIDER, COUNTRY]: runs the server, the database and file storage.
- [EMAIL PROVIDER, REGION]: sends account mail and unread digests. A digest
  holds counts and channel names, and no message text.
- Apple Push Notification service, USA: delivers a wake to your iPhone. The
  wake holds no message content. [Transfers rely on the EU-US Data Privacy
  Framework.]

## Retention

- Account data: until you or an admin deletes the account.
- Messages: until [the channel is deleted | DAYS]. Messages you posted in
  shared channels stay after your account is deleted, unless you ask us to
  delete them.
- Uploads: [7 DAYS].
- Sessions: 90 days at most.
- Backups: [DAYS].

## Your rights

You can ask for a copy of your data, a correction, deletion, a restriction or
a portable export. You can object to processing based on legitimate interest.
Write to [CONTACT EMAIL]. We answer within one month.

You can export the history on your own device at any time with `/export`.

You can complain to your data protection authority. Ours is [AUTHORITY].
