# Product Plan: Gliph

> Text-first, E2E-encrypted, self-hostable team chat with mIRC's soul: channels, ops, slash commands, monospace type, soft colors. Generated 2026-08-25. Market researched live on 2026-08-25.

## The product

Gliph is a standalone chat server (Go, custom protocol) plus web, desktop, and mobile clients, for teams and groups who run their own server. It is a Slack alternative shaped like the best of old IRC: everything happens in channels, governance is legible (ops, modes, invitations), the server operator can read nothing, and the whole thing is themeable monospace beauty.

Target: technical teams and communities, 5 to 500 people, worldwide, English-first. The buyer is the person who already resents Slack's pricing and misses mIRC.

## Market landscape

| Competitor | Target | Pricing model | One-line take |
|---|---|---|---|
| Slack | Companies of all sizes | $7.25 to $28/user/mo, free tier gated at 90-day history | The incumbent. History hostage-taking and forced AI bundling are actively burning goodwill (Hack Club incident, Business+ hikes). |
| Discord | Communities and gamers | Consumer Nitro/Boosts, unlimited free history | Proves free unlimited history is viable, but zero org story: no SSO, SCIM, audit, compliance, and a 2025 UI backlash plus privacy anxiety. |
| Mattermost | Enterprises, gov | Open core, ~$10 to $30/user/mo, free tier capped at 10k messages | Single Go binary + Postgres deployment is the bar to beat. No E2EE. Per-channel moderation is enterprise-paywalled. Moving upmarket to defense. |
| Rocket.Chat | Enterprises, gov/defense | Open core; free tiers gutted in 2025-26 (100 users, "core messaging"), Pro retired Apr 2026 | The only mainstream self-hosted alt with shipping E2EE, and its docs honestly list the E2EE tax: no search, no audit, bots blind. MongoDB replica set requirement is hated. |
| Zulip | Technical teams, OSS | 100% open source, self-hosted free with all features; sells hosted push relay and support | The freest and best-governed. Topic-threading is loved and hated. Best-in-class per-channel permissions (Server 10, free). No E2EE, no roadmap for it. Dated UI. |
| Element/Matrix | Sovereignty buyers (Bundeswehr, French state) | Open protocol; ESS Pro contact-sales | The E2EE reference and the cautionary tale: a decade of "unable to decrypt" bugs, verification friction, and ops pain. Migrating itself from Megolm toward MLS, still "not yet" per arewemlsyet.com. |
| IRCCloud Teams | IRC holdouts | Per-user hosted | Closest existing "IRC for teams" product. Validates the category, but IRC-first rather than product-first. |
| Campfire (37signals) | Small teams | Was $299 once, MIT open-sourced Aug 2025 | Proof of appetite for own-it simple chat. Deliberately minimal, no threads, no E2EE. |

Also-rans: Stoat (ex-Revolt, Discord clone, self-hosted clients crippled), Wire (MLS pioneer, enterprise sales motion), Twist, Keybase (zombie since the Zoom acquihire; its device-key UX and "open source as acquisition insurance" lessons live on).

Per-competitor flow notes and every source URL are in the Sources section.

## Table stakes and conventions

Users will assume, because every competitor has them: unlimited searchable history (Discord set this bar for free), reactions, message edit/delete, typing indicators, read state sync across devices, @mentions, notification keywords/mute controls, link previews, image/file sharing inline, code blocks with syntax highlighting, emoji, mobile push notifications that actually work (the universal self-hosting choke point), SSO for team buyers, and an admin console.

Common flow shape: sign up or get invited by email, land in a default channel, browse/join channels, DM people. Deviating from ad-hoc group DMs (which we deliberately do) needs the invite-to-channel flow to be frictionless.

## Assumption check

1. **Text-first monospace UI is a differentiator.** Confirmed as positioning. Notification fatigue is the number one Slack complaint on G2/Workvivo; mIRC nostalgia threads (HN, 174 points, 2025) mourn exactly what Gliph revives: flat chronological logs, legible governance, ownership. Niche by design, and the niche is loud.
2. **Custom protocol, not IRC/Matrix/XMPP.** Confirmed for the wire protocol, challenged for the crypto. IRCv3 has spent 10 years dragging IRC toward parity and history/reactions/redaction are still drafts; Matrix carries federation baggage Gliph does not need. But do NOT invent group encryption: build message crypto on MLS (RFC 9420), the settled winner shipped in production by Wire, Discord (DAVE), and Google/Apple RCS. Matrix, the flagship of roll-your-own (Megolm), is itself migrating away after a decade of decryption bugs.
3. **Go server.** Confirmed. Mattermost and Ergo prove the single-Go-binary chat server; the key libraries are Go-native (tusd uploads, Pion WebRTC, quic-go). The MLS client libraries are Rust (OpenMLS, mls-rs, Wire core-crypto), which pushes toward a shared Rust client-core with bindings; the server barely touches MLS internals (an MLS Delivery Service is ordered broadcast plus a key-package directory).
4. **Channels-only, no ad-hoc group DMs.** Confirmed as a deliberate opinion. Zulip proves an opinionated structure model can be the product. 1:1 DMs exist; three people wanting to talk make a channel and invite, which takes one slash command.
5. **Per-channel admin, scoped to that channel.** Confirmed, and it is a competitive wedge: Zulip's per-channel administrators (free) is best-in-class; Mattermost paywalls channel moderation into enterprise tiers. Gliph makes channel ops a visible, first-class, free primitive with @ sigils.
6. **Private rooms, invite by slash command.** Confirmed. Also needs a GUI affordance for non-keyboard users.
7. **E2EE by default, server reads nothing.** Confirmed as THE differentiator: nobody owns "E2EE + self-hosted + Slack-shaped." Rocket.Chat has opt-in E2EE but is abandoning small teams; Matrix has E2EE with brutal UX; Mattermost and Zulip have none. The tax must be designed in from day one: search becomes a client-side index, bots/agents and compliance exporters must be visible cryptographic members, push is ID-only, and new-member history access is a per-channel policy (member-mediated encrypted key handoff, gated on verified join; MSC3061's vulnerability is the lesson).
8. **Arbitrary files, 1 GB+, expiring.** Confirmed. Slack caps at 1 GB per file on all plans, so "at least a gigabyte" matches the ceiling of the incumbent; resumable upload (tus) makes it survivable. Expiry-by-default turns the storage cost problem into a feature ("the server is a conveyor, not an archive") and is honest about E2E blobs.
9. **P2P file transfer in DMs, magic-wormhole style.** Confirmed with a correction: wormhole's PAKE code words exist for strangers; Gliph DM peers already share an authenticated encrypted channel, so keys come from the DM and the code-phrase ceremony becomes optional flavor. Transport is WebRTC data channels (the only browser P2P primitive), Pion on Go/desktop, signaling over the existing chat WebSocket, self-hosted STUN/TURN as the 10 to 30 percent fallback (libp2p hole punching measures ~70 percent direct success; Tailscale-style birthday-paradox tricks push higher).
10. **Full offline capability.** Confirmed. The sync model is a server-ordered append-only event log per channel with per-device cursors: catch up, then stream. Slack and Discord both work this way. CRDTs are overkill for chat and buy nothing (messages are immutable inserts).
11. **Proximity mesh when the server is unreachable.** Challenged on scope, confirmed on concept. Cross-platform multi-hop BLE mesh (bitchat-style) is a mobile-native project on its own. v1 ships single-hop proximity: mDNS/UDP-multicast discovery on LAN for desktop (trivial in Go, croc's peerdiscovery proves it), Wi-Fi Aware for mobile (iOS 26 finally shipped it, Android has had it since 8.0). Browsers cannot mesh at all (Web Bluetooth is Chromium-only, client-only); web clients join via a native peer acting as bridge, or not at all. Ad-hoc channels reuse the same event-log code with one peer as sequencer; no admin, history evaporates when the last peer leaves.
12. **First-class embeds: images, YouTube, video, documents, code.** Confirmed table stakes, with an E2E twist: link unfurling must happen client-side (the server cannot read URLs), which is also a privacy feature (no server-side fetches leaking what your team reads).
13. **Agents linked into channels.** Confirmed differentiator. Slack has bet the company on AI-in-chat (Slackbot as MCP client, Agent Browser); Mattermost and Rocket.Chat sell BYO-LLM agents to enterprises. None of them can do it honestly under E2EE. Gliph's model: an agent is a visible cryptographic member of the channel, listed with a ◆ sigil and a +v voice mode, with its own device keys. The member list is the audit UI. MCP-native from day one.
14. **Themeable, soft colors, best console font with Nerd Font support.** Confirmed. Font call: JetBrains Mono as the shipping default (highest x-height, consensus best free default, official Nerd Font build, Google Fonts). Bundled alternates: Commit Mono (smart kerning, best for reading prose in monospace), Iosevka (narrow density, plus its quasi-proportional Aile sibling for settings/marketing surfaces), Monaspace (five metrics-compatible voices: map body/action/system message types to different voices), Cascadia Code (1,140 legacy-computing glyphs, the sleeper pick for ASCII art). All OFL, all with official Nerd Font builds. Berkeley Mono stays out (paid license, incompatible with an open-source client).
15. **Inbox2 architecture: one shared Rust core, individual per-platform frontends.** Confirmed by Waseem. The research independently converged on the same shape: the MLS libraries that matter (OpenMLS, mls-rs, Wire core-crypto) are Rust, so the crypto, event-log sync, offline cache, and search index all live in the shared core, exposed via uniffi to Swift/Kotlin and wasm to the web frontend. Each platform builds only its own thin UI.

## Gaps and opportunities

### What the competition has that the brief was missing (now in scope)

Table stakes: reactions, edits/deletes (as new events on the log), typing indicators, read markers, notification keyword controls and mutes, search (client-side index under E2EE), emoji, syntax-highlighted code blocks, mobile push (ID-only, decrypt locally), SSO (OIDC, free: Slack gates SSO behind paid and it is a famous resentment), admin console, data export (your own logs, always: mIRC users still open 16-year-old logs), invite/onboarding flow, empty and error states.

Worth having: message replies (lightweight quote-reply, not Slack's forced thread model, which is widely disliked at scale), channel discovery (/list with topics and member counts, made joyful), a compliance/export story for business buyers (an org-admin archiver that joins channels as a visible member; the only honest model under E2EE), per-channel history policy at creation (joiners-see-history vs sealed).

### What the competition does badly (the openings)

- History as hostage (Slack 90-day wall, Hack Club shakedown). Gliph: your history lives on your server and in every client's local cache, exportable forever.
- Notification fatigue. Gliph: quiet by default; you own your attention via highlight words, notify lists, and /ignore (Slack famously has no per-person mute).
- E2EE with terrible UX (Matrix UISIs, verification friction). Gliph: MLS, mandatory device signing at onboarding, recovery key created during setup, TOFU with pinning. Every Matrix UISI class is a design-out checklist item.
- Deployment pain (Rocket.Chat's MongoDB replica set, Zulip's five services, Matrix's Kubernetes). Gliph: one Go binary, SQLite by default, Postgres when big. Beat Mattermost.
- Free-tier clawbacks and trust burn (Rocket.Chat, Mattermost, mIRC's own lifetime-license walkback). Gliph: whatever is promised free stays free; open-source the server and clients as acquisition insurance (the Keybase lesson).
- Threads confusion. Gliph: one flat, chronological, timestamped stream per channel, with quote-replies. The flat log is the product.

### What NOT to build

- Voice/video calls and huddles. Different product, giant surface, park it. Teams needing calls paste a meet link.
- Federation. Matrix's complexity lives there. One server is one community.
- Ad-hoc group DMs. Deliberate (assumption 4).
- An integrations marketplace. Webhooks in/out plus MCP agents cover it; a marketplace is a company, not a feature.
- Algorithmic anything: no ranking, no unread AI triage, no engagement mechanics. The flat log is sacred.
- Multi-hop BLE mesh routing (parked, see assumption 11).

### The retro pack (features that used to exist, reborn)

The wink is a feature set, not a skin. Each of these was loved, is gone from modern chat, and is cheap on top of Gliph's primitives:

1. **/me actions**, rendered as first-class narration lines (• robin deploys the fix), not mere italics.
2. **/slap**, shipping with the trout, extensible with custom action templates.
3. **Away messages with text** (/away food, back 20:00), shown in whois, member list, and as an auto-context line when someone pings you. Auto-away optional.
4. **/whois dossier**: pronouns, timezone, idle time, channels shared, device key status. Idle time was social signal; bring it back (per-user privacy toggle).
5. **Channel modes with sigils**: +i invite-only, +m moderated (announcement mode), +o op (@), +v voice (+). Governance you can see in the nicklist, one glance.
6. **Kick/ban with reasons, announced to the channel.** Public accountability instead of silent disappearance. Ban list visible to members.
7. **Topics with ceremony**: op-set, change broadcast as a system line, shown on join.
8. **Channel keys (+k)**: join a channel by knowing the passphrase. Speakeasy energy, and it maps cleanly onto invite-plus-shared-secret under MLS.
9. **Notify list**: friend-online as an event ("✓ robin is online"), the AIM-door-sound era, opt-in per person.
10. **/ignore**: client-side per-person mute, silent, no admin needed. Slack does not have this. Ship it with glee.
11. **/list**: browse all public channels with topics and member counts. Serendipity.
12. **Text color codes and formatting**, tasteful modern take: a small palette of theme-safe author colors, bold/underline, rendered from the theme, never hex chaos.
13. **ASCII/ANSI art that works**, because everything is monospace already. Multi-line paste renders aligned; Cascadia's legacy-computing glyphs available via the font picker.
14. **/sound**: send a named sound event to a channel (rate-limited, per-channel mode +s to allow, per-user opt-out). The mIRC connect sound is the memory everyone kept.
15. **Aliases and user scripts**: mSL's spiritual successor. Client-local aliases (/gm expands to a greeting), custom slash commands, and a sandboxed script hook API (on join, on highlight, on message pattern). User-local, instant, shareable as text. This was a generation's first programming language; make it the gateway drug again, and make it the on-ramp to writing agents.
16. **Netsplit lore**: honest connection weather. When the server drops, the client says so in-stream the way netsplits did, and mesh mode offers itself.
17. **Nick registration ritual**: your handle on the server is yours, registered and defended (admin-provisioned accounts still pick a nick). CTCP-style playful client pings (/ctcp robin version) for flavor.
18. **Highlight words**: per-user keyword list, line highlight plus optional beep. You control your attention.
19. **Local logs you own**: every client can export its channels as plain text/JSONL at any time. A 16-year-old Gliph log should still open in 2042.

## Production completeness

- **Auth and accounts**: email+password with TOTP 2FA, magic-link invites, OIDC SSO free (wedge vs Slack/Mattermost). Roles: server admin, member, agent. Per-channel: op, voice, member. Nick registration on top of accounts.
- **Unhappy paths**: offline queue-and-send, reconnect catch-up, upload resume, empty channels ("no one has spoken yet, set a topic"), failed-decrypt recovery flow (restore from recovery key), rate limiting and flood protection (an IRC classic), abuse: kick/ban plus server-level suspend.
- **Admin/back office**: web admin console: users (invite, suspend, delete), channels (list, archive, transfer ownership), storage quotas and expiry policy, server settings, audit log of admin actions. Channel-level moderation stays with channel ops, not server admins (scoped admin is the product).
- **Money**: none in-product. Self-hosted and free; if a hosted offering ever exists it is a separate billing surface. Not needed because the v1 business is open source plus optional paid hosting later.
- **Notifications**: mobile/desktop push, ID-only payload, client decrypts and renders locally (Zulip 12 E2E-push is the state of the art to match). Keyword highlights, per-channel mute, DND schedule. Self-hosters get APNs/FCM via an optional hosted push relay (the Zulip model, and the honest monetization hook).
- **Data**: local log export (per user), server-side encrypted-blob backup/restore for operators, upload expiry sweeper, account deletion with key revocation.
- **Legal/compliance**: self-hosted GDPR posture (operator is controller), a privacy page that explains "the server stores ciphertext", optional org compliance archiver as visible channel member for regulated buyers. Terms/privacy templates shipped with the server.
- **Analytics and observability**: server: Prometheus metrics, structured logs, health endpoint. No message-content analytics, ever. Client: opt-in crash reporting only.
- **Onboarding**: server setup wizard (one binary, prints an invite link), first-run client tour in the voice of an IRC MOTD, seeded #general with topic set, slash-command autocomplete as discoverable UI (type / and learn).
- **i18n**: parked. English v1; unpark on first non-English community demand. Timestamps/formats localized from day one.
- **Mobile/responsive**: native-feel mobile apps are in scope (see build plan); web client responsive down to tablet.
- **SEO/landing**: one marketing page (gliph.chat) with the story, screenshots, install one-liner. Acquisition is HN/word-of-mouth; the landing page is the pitch to the person your teammate linked.

## Build plan

One-shot scope. Stack decisions, made:

- **Server**: Go, one binary. SQLite default, Postgres optional. Embedded tusd for resumable uploads with expiry. WebSocket transport (coder/websocket), JSON envelopes `{type, seq, payload}` versioned for CBOR later. MLS Delivery Service semantics: ordered per-channel event log, key-package directory, welcome delivery. Self-hosted STUN/TURN (pion/turn) for P2P. Prometheus metrics. Admin console served by the same binary.
- **Crypto**: MLS (RFC 9420) via OpenMLS/mls-rs in a shared Rust client-core (uniffi bindings to Swift/Kotlin, wasm for web). Mandatory device signing at onboarding, recovery key in setup flow, TOFU identity pinning. Per-channel history policy at creation. ID-only push.
- **Clients**: inbox2 architecture. One shared Rust core (MLS, event-log sync, local encrypted cache and search index) under individual frontends: web (TS + wasm core), desktop (Tauri frontend with LAN mesh and full P2P), mobile (native Swift/Kotlin frontends via uniffi bindings). One theme system (design tokens) across all; themes are user-installable token files. Fonts bundled: JetBrains Mono NF (default), Commit Mono, Iosevka, Monaspace, Cascadia Code.
- **Sync**: append-only per-channel event log, server-assigned seq, per-device cursor, catch-up then stream. Edits/reactions/deletes/mode-changes are events. Full offline read and queued send.
- **P2P DM transfer**: WebRTC data channels everywhere (browser native, Pion elsewhere), signaling over the chat socket, direct when hole-punching lands, TURN relay fallback, keys from the DM's MLS state. Progress UI with the wormhole-style verify phrase as optional flavor.
- **Nearby mesh**: mDNS/UDP-multicast discovery (desktop LAN) and Wi-Fi Aware (mobile), single-hop, ad-hoc channels with a peer as sequencer, no admin, evaporates when empty; merges nothing back to the server (a nearby channel is its own ephemeral world).
- **Embeds**: client-side unfurl and render for images, YouTube, video files, PDFs/docs (first page preview), code snippets with syntax highlighting. All fetching client-side.
- **Agents**: agent accounts with device keys, joined as visible members (◆ sigil, +v default), MCP client in the agent runtime, webhooks in/out for dumb integrations. An agent SDK ships with the server.
- **The retro pack**: all 19 items above, v1. They are small on top of the primitives and they ARE the brand.
- **Admin console, push relay option, onboarding wizard, landing page**: per completeness section.

Data model implications: accounts, devices (keys), nicks, channels (modes, topic, key, history policy), memberships (per-channel role), event log (seq, type, ciphertext), uploads (expiry), agent registrations, notify lists, ignore lists (client-side), themes (client-side token files).

### Parked

- Voice/video calls. Unpark: sustained demand from teams otherwise churning; consider MLS-keyed WebRTC rooms then.
- Multi-hop BLE mesh routing (bitchat-style). Unpark: v1 nearby mode proves usage and mobile BLE background story improves.
- Federation/server-to-server. Unpark: probably never; revisit only if MIMI standardizes something adoptable for hub-based rooms.
- Marketplace/app directory. Unpark: when third-party agent count makes discovery a real problem.
- i18n UI translations. Unpark: first non-English community adoption.
- Signal-style ZK encrypted membership (server cannot even see who is in a channel). Unpark: if metadata privacy becomes a sales wedge; encrypted member-list blobs are the v1 posture.
- Web client acting as mesh bridge consumer. Unpark: after native mesh ships.

## Risks and open questions

1. E2EE vs. business reality: some buyers will demand server-side search and retention they cannot have. The visible-compliance-member answer must be built and explained, or those deals walked away from, deliberately.
2. MLS in wasm for the web client (OpenMLS wasm bindings are young). Fallback: web client v1 rides the desktop core via a local companion, or accepts a Rust-to-wasm build risk spike early.
3. Wi-Fi Aware on iOS 26 is brand new; macOS lacks it. Nearby mode on mobile may slip to LAN-only parity; the desktop LAN path is the safe core of the feature.
4. Push relay economics for self-hosters (APNs/FCM require app-owner credentials). The hosted relay is both the solution and the monetization; price it to be loved (the mIRC lifetime-license backlash is the cautionary tale).
5. The retro pack must read as craft, not cosplay. Every item ships working (e.g. /ignore really mutes, +m really moderates) or not at all.
6. Trademark/name check for "Gliph" (a prior Gliph messenger existed circa 2013-2016; verify the mark is clear).

## Sources

Slack pricing and tiers: https://slack.com/pricing · Enterprise real-world pricing: https://www.vendr.com/blog/slack-pricing-what-it-costs-in-2026-real-enterprise-data · Pricing churn/Hack Club: https://retentioncheck.com/blog/slack-churn-analysis · https://costbench.com/software/communication/slack/ · Roles: https://slack.com/help/articles/360018112273 · Guests: https://slack.com/help/articles/202518103 · File limits: https://sto.care/blog/slack-file-sharing-limits/ · Complaints: https://www.workvivo.com/internal-communications/slack-pros-cons/ · https://www.g2.com/products/slack/reviews · Threads at scale: https://news.ycombinator.com/item?id=31323227 · Slackbot agent: https://www.salesforce.com/news/press-releases/2026/01/13/slackbot-announcement/ · TDX 2026 MCP: https://salesforcetrail.com/tdx-2026-slack-front-door-agentic-enterprise/
Discord pricing: https://www.eesel.ai/blog/discord-pricing · https://pumble.com/discord-pricing · Boosts: https://www.boostly.to/blog/discord-server-boost-levels-guide · File limits: https://support.discord.com/hc/en-us/articles/25444343291031 · No SSO/SCIM: https://github.com/discord/discord-api-docs/discussions/6365 · UI backlash: https://support.discord.com/hc/en-us/community/posts/30925819903895 · 2026 direction: https://maileroo.com/blog/discord-is-changing-everything-in-2026-heres-what-you-need-to-know-plus-the-best-alternatives/ · DAVE E2EE AV: https://daveprotocol.com/
Mattermost editions: https://docs.mattermost.com/product-overview/editions-and-offerings.html · Pricing: https://mattermost.com/pricing/ · Channel moderation: https://mattermost.com/blog/mattermost-5-22-read-only-channels-channel-moderation-settings-in-e20/ · Threads perf: https://mattermost.com/blog/scaling-collapsed-reply-threads/ · Search: https://docs.mattermost.com/administration-guide/scale/enterprise-search.html · Encryption position: https://docs.mattermost.com/deployment-guide/encryption-options.html · E2EE thread: https://forum.mattermost.com/t/your-input-needed-end-to-end-encryption-e2ee-in-mattermost/20590 · Deploy: https://docs.mattermost.com/deployment-guide/server/deploy-linux.html · Agents: https://github.com/mattermost/mattermost-plugin-agents
Rocket.Chat plans: https://docs.rocket.chat/docs/our-plans · E2EE guide (the tax): https://docs.rocket.chat/docs/end-to-end-encryption-user-guide · Room roles: https://docs.rocket.chat/docs/roles-in-rocketchat · Search complaints: https://github.com/rocketchat/rocket.chat/issues/37962 · Deploy: https://docs.rocket.chat/docs/deploy-with-docker-docker-compose · Community clawback: https://github.com/orgs/RocketChat/discussions/30144 · Sovereign AI: https://www.rocket.chat/rocket-chat-ai
Zulip plans: https://zulip.com/plans/ · Per-channel permissions: https://blog.zulip.com/2025/03/20/flexible-permissions-management/ · https://zulip.com/help/configure-who-can-administer-a-channel · 12.0 E2E push: https://blog.zulip.com/2026/04/27/zulip-12-0-released/ · E2EE issue: https://github.com/zulip/zulip/issues/6096 · Deploy: https://zulip.readthedocs.io/en/stable/production/requirements.html · Search: https://zulip.readthedocs.io/en/stable/subsystems/full-text-search.html
Element/Matrix UISI taxonomy: https://github.com/element-hq/element-meta/issues/245 · Mandatory verification: https://element.io/blog/verifying-your-devices-is-becoming-mandatory-2/ · History sharing MSC4268: https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/4268-encrypted-history-sharing.md · MLS status: https://arewemlsyet.com/ · ESS: https://element.io/en/server-suite/community · Synapse CVEs: https://advisories.gitlab.com/pkg/pypi/matrix-synapse/CVE-2025-30355/
MLS: RFC 9420 https://datatracker.ietf.org/doc/rfc9420/ · Wire in production: https://wire.com/en/messaging-layer-security · OpenMLS: https://github.com/openmls/openmls · mls-rs: https://awslabs.github.io/mls-rs/ · core-crypto: https://github.com/wireapp/core-crypto · MIMI: https://www.ietf.org/archive/id/draft-ietf-mimi-protocol-04.html · MLS vs Matrix practitioner thread: https://news.ycombinator.com/item?id=46784523
Keybase lessons: https://schulz.dk/2026/04/06/the-cryptographic-zombie-how-keybase-went-from-privacy-darling-to-zooms-cleanup-crew/ · https://decrypt.co/28121/keybase-users-revolt-following-zoom-acquisition
Signal private groups: https://signal.org/blog/signal-private-group-system/ · https://eprint.iacr.org/2019/1416 · E2EE search: https://zerokey.vercel.app/blog/blind-indexing-e2ee-search · E2E push pattern: https://push0.com/blog/end-to-end-encrypted-push-notifications-explained/
magic-wormhole: https://magic-wormhole.readthedocs.io/en/latest/welcome.html · wormhole-william (Go): https://github.com/psanford/wormhole-william · croc: https://pkg.go.dev/github.com/schollz/croc · peerdiscovery: https://github.com/schollz/peerdiscovery · Tailscale NAT traversal: https://tailscale.com/blog/how-nat-traversal-works · DCUtR measurement: https://arxiv.org/abs/2510.27500 · Pion: https://github.com/pion/webrtc · tus/tusd: https://tus.io/protocols/resumable-upload · https://github.com/tus/tusd · Slack RTM architecture: https://slack.engineering/real-time-messaging/ · WebTransport status: https://websocket.org/guides/future-of-websockets/ · Local-first/CRDT position: https://wal.sh/research/local-first
Briar/Berty/bitchat/Wi-Fi Aware: https://berty.tech/blog/bluetooth-low-energy/ · https://en.wikipedia.org/wiki/BitChat · https://www.heise.de/en/news/Peer-to-peer-WLAN-by-order-of-the-EU-Apple-integrates-Wi-Fi-Aware-10446649.html · https://developer.apple.com/documentation/WiFiAware · Web Bluetooth limits: https://caniuse.com/web-bluetooth
IRCv3: https://ircv3.net/irc/ · 2026 roundup: https://www.ilmarilauhakangas.fi/irc_technology_news_from_the_first_half_of_2026/ · Ergo: https://github.com/ergochat/ergo · The Lounge: https://thelounge.chat/ · IRCCloud Teams: https://www.irccloud.com/pricing · soju: https://soju.im/ · Mozilla IRC sunset: https://exple.tive.org/blarg/2019/04/26/synchronous-text/ · CTCP: https://modern.ircdocs.horse/ctcp · mIRC news: https://www.mirc.com/news.html · mIRC pricing/lifetime backlash: https://news.ycombinator.com/item?id=33864660 · mIRC nostalgia: https://news.ycombinator.com/item?id=43715306 · Old logs: https://news.ycombinator.com/item?id=11506349 · mSL: https://en.wikipedia.org/wiki/MIRC_scripting_language
Fonts: Nerd Fonts: https://github.com/ryanoasis/nerd-fonts · JetBrains Mono consensus: https://daily.dev/posts/13-best-coding-fonts-for-developers-in-2026-s1e5r8ena · Iosevka: https://github.com/be5invis/Iosevka · Commit Mono: https://commitmono.com/ · Monaspace: https://monaspace.githubnext.com/ · Cascadia NF: https://devblogs.microsoft.com/commandline/cascadia-code-2404-23/ · Berkeley Mono: https://usgraphics.com/products/berkeley-mono
Campfire: https://once.com/campfire · https://world.hey.com/dhh/once-again-3e99f755 · Stoat: https://github.com/stoatchat · https://docs.rocket.chat/docs/our-plans
