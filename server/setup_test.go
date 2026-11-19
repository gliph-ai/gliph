package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The wizard on a fresh server: TLS, name, admin, then an invite link that
// really admits the next person, and a config the next start reads.
func TestSetupWizardMakesAdminConfigAndInvite(t *testing.T) {
	dir := t.TempDir()
	sh := NewShard(ShardOpts{DataDir: dir})
	defer sh.Close()
	team, err := sh.Open("", "gliph.local", nil)
	if err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("Chat.Example.com\n\nAcme chat\nboss\nboss@example.com\nhunter2hunter2\n")
	var out bytes.Buffer
	res, err := runSetup(in, &out, false, dir, team, listenOpts{dataDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.lo.domains != "chat.example.com" || !res.lo.configured() || team.Srv.Name != "Acme chat" {
		t.Fatalf("listen %+v, name %q", res.lo, team.Srv.Name)
	}
	conf, _ := os.ReadFile(filepath.Join(dir, confName))
	for _, want := range []string{"-tls-domain=chat.example.com", "-name=Acme chat"} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("conf lacks %q:\n%s", want, conf)
		}
	}
	if _, u, err := team.St.Login("boss", "hunter2hunter2"); err != nil || u.Role != "admin" {
		t.Fatalf("admin: %v %+v", err, u)
	}
	link := regexp.MustCompile(`https://chat\.example\.com/\?invite=(\w+)`).FindStringSubmatch(out.String())
	if link == nil || link[0] != res.invite {
		t.Fatalf("no invite link in:\n%s", out.String())
	}
	if u, err := team.St.Register("dana", "d@x.io", "hunter2hunter2", link[1]); err != nil || u.Role != "member" {
		t.Fatalf("invite: %v %+v", err, u)
	}
}

// Behind a proxy, and with a password left blank: plaintext on the address
// given, and a made-up password that logs in.
func TestSetupWizardBehindAProxy(t *testing.T) {
	dir := t.TempDir()
	sh := NewShard(ShardOpts{DataDir: dir})
	defer sh.Close()
	team, _ := sh.Open("", "", nil)
	var out bytes.Buffer
	res, err := runSetup(strings.NewReader("chat.example.com\n3\n\n\n\n\n\n"), &out, false, dir, team, listenOpts{dataDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !res.lo.plaintext || res.lo.addr != "127.0.0.1:8787" {
		t.Fatalf("listen %+v", res.lo)
	}
	pass := regexp.MustCompile(`Password for admin: (\w+)`).FindStringSubmatch(out.String())
	if pass == nil {
		t.Fatalf("no password printed:\n%s", out.String())
	}
	if _, _, err := team.St.Login("admin", pass[1]); err != nil {
		t.Fatal(err)
	}
}
