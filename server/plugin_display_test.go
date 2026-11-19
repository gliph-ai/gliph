package main

import (
	"strings"
	"testing"
)

// A plugin declares its display name in `register` (docs/protocol.md), and that
// name labels everything it posts. The server used to unmarshal the field and
// throw it away, so every agent line was attributed to whatever name the token
// happened to be minted with.
func TestPluginDisplayComesFromRegister(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "admin")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	dc := fakeConn(s, dana)

	// minted as "ci", renamed to "buildbot" at register time
	meta, err := s.St.CreatePlugin("ci", "ci", "agent")
	if err != nil {
		t.Fatal(err)
	}
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 64)}
	s.mu.Lock()
	s.plugins[meta.ID] = p
	s.mu.Unlock()
	s.St.PluginJoin(meta.ID, ch.ID)

	reg := `{"t":"register","d":{"display":"buildbot","kind":"agent","commands":[]}}`
	if err := s.handlePluginFrame(p, []byte(reg)); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if got := p.meta.Display; got != "buildbot" {
		t.Fatalf("display is %q, want buildbot", got)
	}
	for _, info := range s.ConnectedPlugins() {
		if info.ID == "ci" && info.Display != "buildbot" {
			t.Fatalf("member list shows %q, want buildbot", info.Display)
		}
	}

	// what it posts is attributed to the registered name
	drain(dc)
	say := `{"t":"say","d":{"channel":"` + ch.ID + `","body":"build 412 green"}}`
	if err := s.handlePluginFrame(p, []byte(say)); err != nil {
		t.Fatalf("say failed: %v", err)
	}
	var actor string
	for _, f := range drain(dc) {
		if f.T == "event" {
			if e, ok := f.D.(*Event); ok && e.Kind == "agent" {
				actor = e.Actor
			}
		}
	}
	if actor != "buildbot" {
		t.Fatalf("agent event attributed to %q, want buildbot", actor)
	}
}

// The display name labels messages, so a plugin must not be able to wear a real
// account's nick — that is the one thing making it plugin-controlled costs us.
func TestPluginCannotClaimAMembersNick(t *testing.T) {
	s := testServer(t)
	if _, err := s.St.CreateUser("waseem", "w@x.io", "hunter2hunter2", "admin"); err != nil {
		t.Fatal(err)
	}
	meta, _ := s.St.CreatePlugin("imposter", "imposter", "bot")
	p := &PluginConn{srv: s, meta: meta, out: make(chan Frame, 64)}

	err := s.handlePluginFrame(p, []byte(`{"t":"register","d":{"display":"waseem","commands":[]}}`))
	if err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("register error = %v, want it refused as taken", err)
	}
	if p.meta.Display != "imposter" {
		t.Fatalf("display changed to %q despite the refusal", p.meta.Display)
	}
}
