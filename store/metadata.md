# Gliph App Store listing

Source of truth for the App Store text. Edit here, then push with
`mobile/tools/asc-listing.rb`. Written through the voice-dna skill; keep it that
way (no em-dashes, no banned words, no rule-of-three anaphora).

## Subtitle (30 max)

Team chat that needs no server

## Promotional text (170 max)

Nearby mode puts everyone on the same Wi-Fi into one channel. There is no
account to make and no server to reach. Pick a nick and talk, at 30,000 feet or
anywhere else.

## Keywords (100 max)

irc,mesh,offline,wifi,peer,team,encrypted,nearby,selfhosted,nick,channel,privacy,local,p2p,lan

## Description (4000 max)

Gliph is team chat that reads like a terminal and behaves like one. Channels,
nicks, slash commands, a member list down the side, and text that stays text.
Anyone who spent their teenage years in an IRC window will know where they are
inside ten seconds.

Then there is the part nobody else does.

Open Gliph, tap "chat with people nearby", pick a nick, and you are in a channel
with everyone else running Gliph on the same Wi-Fi. There is no account to make
and no server anywhere in the path. The phones find each other and talk
directly. When the last person leaves, the channel and everything said in it are
gone, because none of it was ever written down.

That is useful exactly where group chat usually dies. A plane at 30,000 feet. A
conference basement with two bars of nothing. A house with the router down. A
field at a festival. Just as Skype made the phone company optional for a call,
nearby mode makes the server optional for a conversation.

Most chat apps stop working the moment their servers do. That is a strange thing
to accept from software whose only job is to carry a sentence across a room.

## When you do want a server

Gliph also connects to one, and it can be yours. The server is a single Go
binary with SQLite underneath, small enough to run on the cheapest box you can
rent. Channels can be end to end encrypted, so the server carries ciphertext it
cannot read. Invites are explicit. There is an admin console for the person who
runs it, and nobody else.

Slash commands come from the server, which means a plugin can add one and it
appears in your palette. The client never hardcodes a command list, so your
server decides what your team can do.

## What Gliph does not do

No ads. No tracking. Nothing is ranked, summarised or reordered by a model
before you see it. Messages arrive in the order they were sent, from the person
who sent them.

Text first, on purpose. There are no voice calls, no video, no stories.

## Free

Gliph is free, and the server is yours to run.
