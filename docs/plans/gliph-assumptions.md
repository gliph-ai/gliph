# Gliph — Step 1 frame and assumptions

> Working notes for the product plan. 2026-08-25.

## The product, one sentence

Gliph is a text-first, themeable, E2E-encrypted team chat (web, desktop, mobile) with a Go server and a custom protocol, built as a modern love letter to mIRC: channels, ops, slash commands, monospace type, soft colors.

## Who it's for

B2B-ish: teams and groups of people who run their own server (self-hosted Slack alternative). Global, English-first.

## Stated assumptions (to verify/challenge in Step 3)

1. Text-first monospace UI with nerd-font support is a real differentiator vs Slack's rich UI.
2. A custom protocol (not IRC, not Matrix, not XMPP) is the right call for a standalone team server.
3. Go is a good server language for this.
4. Structure is channels-only: no ad-hoc group DMs; people are invited into channels. 1:1 DMs exist.
5. Per-channel admin model: every channel has at least one admin; admins can promote others, but admin rights are scoped to that channel. Server-level admin manages user access.
6. Private rooms are invite-only via slash commands (mIRC style).
7. E2E encryption by default; the server operator can read nothing.
8. Arbitrary file sending, large uploads (>= 1 GB), uploads expire after a set period.
9. 1:1 DMs can transfer files peer-to-peer (magic-wormhole style: NAT traversal, direct, encrypted).
10. Clients are fully offline-capable (local cache of history).
11. When the server is unreachable, physically-nearby clients can find each other (proximity mesh) and form an ad-hoc channel with no admin, which ceases to exist when everyone leaves.
12. First-class rich embeds: images, YouTube links, embedded video, document previews, code snippets.
13. AI agents can be linked into a channel to monitor the stream and act.
14. Themeable app; soft color palettes; most-readable console font (not Courier) with Nerd Font support.
15. Architecture: **inbox2 architecture** (confirmed by Waseem 2026-08-25): one shared Rust core used by all apps, with individual per-platform frontends.

## Platforms

Web, desktop, mobile. One shared core if possible.

## Reference products named by Waseem

mIRC (spiritual ancestor), Slack (the incumbent to replace), magic-wormhole (P2P transfer model).
