package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// markSigned stands in for an approval where a test is about the courier, not
// the signature: the gate only looks at whether a signature is on file.
func markSigned(t *testing.T, s *Server, devs ...string) {
	t.Helper()
	for _, d := range devs {
		if _, err := s.St.db.Exec(`UPDATE devices SET sig='test' WHERE id=?`, d); err != nil {
			t.Fatal(err)
		}
	}
}

type testKey struct{ k *ecdsa.PrivateKey }

func newKey(t *testing.T) testKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{k}
}

func (k testKey) pub() string {
	der, _ := x509.MarshalPKIXPublicKey(&k.k.PublicKey)
	return base64.StdEncoding.EncodeToString(der)
}

func (k testKey) sign(uid, dev, pubkey string) string {
	h := sha256.Sum256([]byte(deviceStatement(uid, dev, pubkey)))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.k, h[:])
	return base64.StdEncoding.EncodeToString(sig)
}

func frame(c *Conn, s *Server, typ string, d any) []Frame {
	raw, _ := json.Marshal(d)
	drain(c)
	s.handleCryptoFrame(c, typ, raw)
	return drain(c)
}

func trustOf(frames []Frame) map[string]any {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].T == "device.trust" {
			return frames[i].D.(map[string]any)
		}
	}
	return nil
}

func hello(t *testing.T, s *Server, u *User, dev string) (*Conn, string) {
	t.Helper()
	c := fakeConn(s, u)
	pk := newKey(t).pub() // any P-256 SPKI does for a device key here
	frame(c, s, "device.hello", map[string]string{"device": dev, "name": dev, "pubkey": pk})
	return c, pk
}

func memberFlag(t *testing.T, s *Server, cid, uid string) bool {
	ms, err := s.St.Members(cid)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.UserID == uid {
			return m.Unverified
		}
	}
	t.Fatal("not a member")
	return false
}

// The F2 acceptance: a second device is cross-signed from the first before it
// can be sealed a key, and until then peers see it flagged.
func TestASecondDeviceGetsNothingUntilTheFirstSignsIt(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	master := newKey(t)

	// First device: no master on file, so it may establish one.
	laptop, lpk := hello(t, s, dana, "dev-laptop")
	if tr := trustOf(frame(laptop, s, "device.hello", map[string]string{"device": "dev-laptop", "pubkey": lpk})); tr["can_establish"] != true {
		t.Fatalf("the first device may mint the master key: %+v", tr)
	}
	// A signature that does not verify is refused.
	if n := noticeText(frame(laptop, s, "device.sign", map[string]any{"master": master.pub(),
		"sigs": map[string]string{"dev-laptop": newKey(t).sign(dana.ID, "dev-laptop", lpk)}})); !strings.Contains(n, "does not verify") {
		t.Fatalf("a junk signature was accepted: %q", n)
	}
	frame(laptop, s, "device.sign", map[string]any{"master": master.pub(),
		"sigs": map[string]string{"dev-laptop": master.sign(dana.ID, "dev-laptop", lpk)}})
	if s.St.MasterKey(dana.ID) != master.pub() {
		t.Fatal("master key not recorded")
	}
	if memberFlag(t, s, ch.ID, dana.ID) {
		t.Fatal("a fully signed account is flagged")
	}

	// Second device: waits. It cannot mint a master of its own.
	phone, ppk := hello(t, s, dana, "dev-phone")
	tr := trustOf(frame(phone, s, "device.hello", map[string]string{"device": "dev-phone", "pubkey": ppk}))
	if tr["signed"] != false || tr["can_establish"] != false {
		t.Fatalf("a second device is born unsigned: %+v", tr)
	}
	if n := noticeText(frame(phone, s, "device.sign", map[string]any{"master": newKey(t).pub(), "reset": false,
		"sigs": map[string]string{"dev-phone": "x"}})); n == "" {
		t.Fatal("a second device replaced the master key")
	}
	if !memberFlag(t, s, ch.ID, dana.ID) {
		t.Fatal("peers must see the unsigned device")
	}

	// Peers are not told about it, a publication to it is dropped, and it
	// fetches nothing.
	frame(laptop, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 1,
		"entries": map[string]string{"dev-laptop": "L", "dev-phone": "P"}})
	if got, _ := s.St.ChannelKeysFor(ch.ID, "dev-phone"); len(got) != 0 {
		t.Fatal("a key was sealed to an unsigned device")
	}
	peers, _ := s.St.DevicesInChannel(ch.ID)
	for _, d := range peers {
		if d.ID == "dev-phone" {
			t.Fatal("an unsigned device is listed as a peer")
		}
	}
	for _, f := range frame(phone, s, "keys.fetch", map[string]string{"channel": ch.ID}) {
		if f.T == "keys" && len(f.D.(map[string]any)["keys"].([]any)) != 0 {
			t.Fatal("an unsigned device fetched keys")
		}
	}
	if n := noticeText(frame(phone, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 2,
		"entries": map[string]string{"dev-laptop": "L"}})); !strings.Contains(n, "not approved") {
		t.Fatal("an unsigned device published a key")
	}

	// The first device signs it: now it is a peer, unflagged, and holds the
	// master key its approver sealed to it.
	frame(laptop, s, "device.sign", map[string]any{
		"sigs":   map[string]string{"dev-phone": master.sign(dana.ID, "dev-phone", ppk)},
		"sealed": map[string]string{"dev-phone": "SEALED-MASTER"}})
	tr = trustOf(drain(phone))
	if tr["signed"] != true || tr["sealed_master"] != "SEALED-MASTER" {
		t.Fatalf("the phone was not told it is signed: %+v", tr)
	}
	if memberFlag(t, s, ch.ID, dana.ID) {
		t.Fatal("still flagged after approval")
	}
	frame(laptop, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 1,
		"entries": map[string]string{"dev-phone": "P"}})
	if got, _ := s.St.ChannelKeysFor(ch.ID, "dev-phone"); len(got) != 1 {
		t.Fatal("a signed device could not be sealed a key")
	}

	// A reinstall is a new key: its signature no longer covers it.
	s.St.UpsertDevice(dana.ID, "dev-phone", "phone", newKey(t).pub())
	if sig, _, _ := s.St.deviceTrust("dev-phone"); sig != "" {
		t.Fatal("a signature survived a device key change")
	}
}

func TestATrustResetIsRefusedWhileAnApprovedDeviceIsOnline(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	master := newKey(t)
	laptop, lpk := hello(t, s, dana, "dev-laptop")
	frame(laptop, s, "device.sign", map[string]any{"master": master.pub(),
		"sigs": map[string]string{"dev-laptop": master.sign(dana.ID, "dev-laptop", lpk)}})
	phone, ppk := hello(t, s, dana, "dev-phone")
	fresh := newKey(t)
	reset := map[string]any{"master": fresh.pub(), "reset": true,
		"sigs": map[string]string{"dev-phone": fresh.sign(dana.ID, "dev-phone", ppk)}}
	if n := noticeText(frame(phone, s, "device.sign", reset)); !strings.Contains(n, "approve this one from there") {
		t.Fatalf("reset went through with the laptop online: %q", n)
	}
	s.removeConn(laptop)
	frame(phone, s, "device.sign", reset)
	if s.St.MasterKey(dana.ID) != fresh.pub() {
		t.Fatal("reset did not take")
	}
	if sig, _, _ := s.St.deviceTrust("dev-laptop"); sig != "" {
		t.Fatal("the old devices keep their signatures after a reset")
	}
}

func TestADeviceFromBeforeSigningMayEstablishAndVouchForItsPeers(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	a, apk := hello(t, s, dana, "dev-a")
	_, bpk := hello(t, s, dana, "dev-b")
	s.St.db.Exec(`UPDATE devices SET legacy=1`)
	_, newpk := hello(t, s, dana, "dev-new")
	master := newKey(t)
	// It may not vouch for a device that arrived after signing did.
	if n := noticeText(frame(a, s, "device.sign", map[string]any{"master": master.pub(), "sigs": map[string]string{
		"dev-a": master.sign(dana.ID, "dev-a", apk), "dev-new": master.sign(dana.ID, "dev-new", newpk)}})); n == "" {
		t.Fatal("the first signer vouched for a device newer than signing")
	}
	frame(a, s, "device.sign", map[string]any{"master": master.pub(), "sigs": map[string]string{
		"dev-a": master.sign(dana.ID, "dev-a", apk), "dev-b": master.sign(dana.ID, "dev-b", bpk)}})
	if sig, _, _ := s.St.deviceTrust("dev-b"); sig == "" {
		t.Fatal("a legacy device was not carried over")
	}
}

// F3: a sealed channel's history policy, enforced where the keys are handed out.
func TestASealedChannelSealsNoEpochFromBeforeTheJoin(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	dc := fakeConn(s, dana)
	for _, h := range []string{"sealed", "open"} {
		if err := s.RunCommand(dc, "", "create", "#"+h+"room "+h); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RunCommand(dc, "", "create", "#bad nonsense"); err == nil {
		t.Fatal("an unknown history policy was accepted")
	}
	s.St.UpsertDevice(dana.ID, "dev-dana", "mac", "PK1")
	s.St.UpsertDevice(robin.ID, "dev-robin", "phone", "PK2")
	markSigned(t, s, "dev-dana", "dev-robin")
	dc.device = "dev-dana"

	for _, name := range []string{"sealedroom", "openroom"} {
		ch, _ := s.St.ChannelByName(name)
		if s.channelView(ch, dana.ID)["history"] != strings.TrimSuffix(name, "room") {
			t.Fatalf("#%s: the policy is not in channel info", name)
		}
		frame(dc, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 1, "entries": map[string]string{"dev-dana": "D1"}})
		frame(dc, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 2, "entries": map[string]string{"dev-dana": "D2"}})
		s.St.Join(ch.ID, robin.ID, "member") // joins at epoch 2
		frame(dc, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 1, "entries": map[string]string{"dev-robin": "R1"}})
		frame(dc, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 2, "entries": map[string]string{"dev-robin": "R2"}})
		frame(dc, s, "keys.publish", map[string]any{"channel": ch.ID, "epoch": 3, "entries": map[string]string{"dev-robin": "R3"}})
		got, _ := s.St.ChannelKeysFor(ch.ID, "dev-robin")
		want := 3
		if name == "sealedroom" {
			want = 1 // only the epoch minted after the join
		}
		if len(got) != want {
			t.Fatalf("#%s: robin holds %d epochs, want %d: %+v", name, len(got), want, got)
		}
	}
}
