package main

// End-to-end encryption, server side.
//
// The server's entire job here is to be a dumb, honest key directory and a
// courier for opaque blobs:
//
//   - it stores device PUBLIC keys so clients can seal a channel key to each
//     other's devices,
//   - it stores those sealed blobs (`channel_keys`) and hands each device only
//     its own,
//   - it stores ciphertext message bodies it cannot read.
//
// It never holds a private key, never wraps or unwraps anything, and cannot
// decrypt a message in a +e channel even with full database access. All the
// cryptography lives in the client (web/src/crypto.ts).
//
// ponytail: this is a sender-key scheme (one symmetric key per channel epoch,
// sealed per device with ECDH-ES), not MLS. It gives confidentiality from the
// server and removal-forward-secrecy via epoch rotation, but NOT per-message
// forward secrecy or post-compromise security. MLS is the documented upgrade
// path (docs/plans/gliph.md, assumption 2); the wire shape here — opaque body
// plus an epoch number — is deliberately the same one MLS would use, so
// swapping the client crypto later does not move the server.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---- store ----

type Device struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Nick   string `json:"nick"`
	Name   string `json:"name"`
	Pubkey string `json:"pubkey"`
	// The account master key's signature over this device, and the master
	// public key it verifies against. A client re-checks both before sealing
	// anything to the device; the server only lists signed devices as peers.
	Sig    string `json:"sig"`
	Master string `json:"master"`
	// Registered before device signing existed: the account's first signer
	// may vouch for it.
	Legacy bool `json:"legacy,omitempty"`
	// The epoch the owner joined the channel at; 0 outside a channel query.
	JoinedEpoch int64 `json:"-"`
}

// UpsertDevice registers or refreshes a device's public key. A device may only
// ever belong to the user that created it: a second user claiming the same
// device id is rejected rather than silently rebound.
//
// The bool reports whether this device now needs keys sealed to it — true for a
// brand new device and for one whose key changed (a reinstall). Callers use it
// to nudge the other members into re-keying, because a new device does not
// change channel membership and so would otherwise be invisible to them.
func (s *Store) UpsertDevice(userID, devID, name, pubkey string) (bool, error) {
	if devID == "" || pubkey == "" {
		return false, errors.New("device id and pubkey are required")
	}
	if len(pubkey) > 512 || len(devID) > 128 {
		return false, errors.New("device id or pubkey too long")
	}
	var owner, existing string
	err := s.db.QueryRow(`SELECT user_id, pubkey FROM devices WHERE id=?`, devID).Scan(&owner, &existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO devices(id,user_id,name,pubkey,created_at,last_seen) VALUES(?,?,?,?,?,?)`,
			devID, userID, name, pubkey, now(), now())
		return err == nil, err
	case err != nil:
		return false, err
	case owner != userID:
		return false, errors.New("device id already registered to another account")
	case existing != pubkey:
		// A changed key is a new identity for that device. Allowed (reinstall),
		// but every sealed key for the old public key is now useless, so drop
		// them: the client will notice the gap and ask to be re-keyed. Its
		// signature was over the old key, so it has to be approved again.
		if _, err := s.db.Exec(`DELETE FROM channel_keys WHERE device_id=?`, devID); err != nil {
			return false, err
		}
		_, err = s.db.Exec(`UPDATE devices SET pubkey=?, name=?, last_seen=?, sig='', sealed_master='', legacy=0 WHERE id=?`,
			pubkey, name, now(), devID)
		return err == nil, err
	default:
		_, err = s.db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, now(), devID)
		return false, err
	}
}

const devCols = `d.id,d.user_id,u.nick,d.name,d.pubkey,d.sig,u.master_key,d.legacy`

// DevicesForUser is every device an account owns, signed or not.
func (s *Store) DevicesForUser(userID string) ([]Device, error) {
	return s.devices(`SELECT `+devCols+`,0 FROM devices d
	  JOIN users u ON u.id=d.user_id WHERE d.user_id=? ORDER BY d.created_at`, userID)
}

// DevicesInChannel lists every APPROVED device belonging to every member — the
// exact set a client may seal a channel key to. An unsigned device is nobody's
// peer until another device of its account signs it.
func (s *Store) DevicesInChannel(cid string) ([]Device, error) {
	return s.devices(`SELECT `+devCols+`,m.joined_epoch FROM devices d
	  JOIN users u ON u.id=d.user_id
	  JOIN members m ON m.user_id=d.user_id AND m.channel_id=?
	  WHERE d.sig <> ''
	  ORDER BY u.nick, d.created_at`, cid)
}

func (s *Store) devices(q string, args ...any) ([]Device, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		var legacy int
		if err := rows.Scan(&d.ID, &d.UserID, &d.Nick, &d.Name, &d.Pubkey, &d.Sig, &d.Master, &legacy, &d.JoinedEpoch); err != nil {
			return nil, err
		}
		d.Legacy = legacy == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeviceOwner(devID string) (string, bool) {
	var uid string
	if err := s.db.QueryRow(`SELECT user_id FROM devices WHERE id=?`, devID).Scan(&uid); err != nil {
		return "", false
	}
	return uid, true
}

// PutChannelKeys stores sealed channel keys for one epoch. Callers must already
// have checked that byDevice belongs to a member of the channel.
func (s *Store) PutChannelKeys(cid string, epoch int64, byDevice string, entries map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for dev, wrapped := range entries {
		if len(wrapped) > 4096 {
			return errors.New("wrapped key too large")
		}
		if _, err := tx.Exec(`INSERT INTO channel_keys(channel_id,epoch,device_id,wrapped,by_device,created_at)
		  VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id,epoch,device_id) DO NOTHING`,
			cid, epoch, dev, wrapped, byDevice, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChannelKeysFor returns every epoch sealed to one device, so a client can
// decrypt history as well as new traffic.
func (s *Store) ChannelKeysFor(cid, devID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT epoch, wrapped, by_device FROM channel_keys
	  WHERE channel_id=? AND device_id=? ORDER BY epoch`, cid, devID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var epoch int64
		var wrapped, by string
		if err := rows.Scan(&epoch, &wrapped, &by); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"epoch": epoch, "wrapped": wrapped, "by_device": by})
	}
	return out, rows.Err()
}

// MaxEpoch is the newest epoch anyone has published for a channel.
func (s *Store) MaxEpoch(cid string) int64 {
	var n sql.NullInt64
	s.db.QueryRow(`SELECT MAX(epoch) FROM channel_keys WHERE channel_id=?`, cid).Scan(&n)
	return n.Int64
}

// DevicesMissingEpoch reports member devices that have no sealed copy of an
// epoch — the work list a client uses to re-key after someone joins.
func (s *Store) DevicesMissingEpoch(cid string, epoch int64) ([]Device, error) {
	return s.devices(`SELECT `+devCols+`,m.joined_epoch FROM devices d
	  JOIN users u ON u.id=d.user_id
	  JOIN members m ON m.user_id=d.user_id AND m.channel_id=?
	  WHERE d.sig <> '' AND NOT EXISTS (SELECT 1 FROM channel_keys k
	    WHERE k.channel_id=? AND k.epoch=? AND k.device_id=d.id)
	  ORDER BY u.nick`, cid, cid, epoch)
}

// mayHold is the history policy, applied to one sealed key: in a sealed
// channel a member's devices get no epoch that existed before they joined.
func mayHold(ch *Channel, d Device, epoch int64) bool {
	return ch == nil || ch.History != "sealed" || epoch > d.JoinedEpoch
}

// ---- device trust ----

// deviceStatement is what the account master key signs. core/src/crypto.rs
// builds the same string; change both or neither.
func deviceStatement(uid, devID, pubkey string) string {
	return "gliph/device/v1\n" + uid + "\n" + devID + "\n" + pubkey
}

// verifyDeviceSig checks a base64 DER ECDSA P-256 signature against a base64
// SPKI master key. The server checks so that a client cannot mark a device
// trusted with junk; peers check again so that the server cannot either.
func verifyDeviceSig(master, uid, devID, pubkey, sig string) bool {
	der, err := base64.StdEncoding.DecodeString(master)
	if err != nil {
		return false
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	pk, ok := k.(*ecdsa.PublicKey)
	if !ok || pk.Curve != elliptic.P256() {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	h := sha256.Sum256([]byte(deviceStatement(uid, devID, pubkey)))
	return ecdsa.VerifyASN1(pk, h[:], raw)
}

// deviceCode is a device's approval code, the same string the client shows:
// the first 48 bits of SHA-256 over its SPKI, as abcd-ef01-2345.
func deviceCode(pubkey string) string {
	der, _ := base64.StdEncoding.DecodeString(pubkey)
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:6])
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12]
}

func (s *Store) MasterKey(uid string) string {
	var k string
	s.db.QueryRow(`SELECT master_key FROM users WHERE id=?`, uid).Scan(&k)
	return k
}

func (s *Store) deviceTrust(devID string) (sig, sealed string, legacy bool) {
	var l int
	s.db.QueryRow(`SELECT sig, sealed_master, legacy FROM devices WHERE id=?`, devID).Scan(&sig, &sealed, &l)
	return sig, sealed, l == 1
}

// SignDevices records signatures (and sealed master keys) for an account's
// own devices. With master set it first installs the account master key:
// only when none is on file, unless reset, which also strips every other
// device's signature and the channel keys sealed to them.
func (s *Store) SignDevices(uid, master string, reset bool, keep string, sigs, sealed map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if master != "" {
		q := `UPDATE users SET master_key=? WHERE id=? AND master_key=''`
		if reset {
			q = `UPDATE users SET master_key=? WHERE id=?`
		}
		res, err := tx.Exec(q, master, uid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("this account already has a signing key")
		}
		if reset {
			if _, err := tx.Exec(`DELETE FROM channel_keys WHERE device_id IN
			  (SELECT id FROM devices WHERE user_id=? AND id<>?)`, uid, keep); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE devices SET sig='', sealed_master='' WHERE user_id=?`, uid); err != nil {
				return err
			}
		}
	}
	for dev, sig := range sigs {
		if _, err := tx.Exec(`UPDATE devices SET sig=?, sealed_master=? WHERE id=? AND user_id=?`,
			sig, sealed[dev], dev, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ForgetDevice drops one of an account's devices and every key sealed to it.
func (s *Store) ForgetDevice(uid, devID string) error {
	if _, err := s.db.Exec(`DELETE FROM channel_keys WHERE device_id IN
	  (SELECT id FROM devices WHERE id=? AND user_id=?)`, devID, uid); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM devices WHERE id=? AND user_id=?`, devID, uid)
	return err
}

// trustFrame is one connection's view of its account's devices.
func (s *Server) trustFrame(c *Conn) Frame {
	devs, _ := s.St.DevicesForUser(c.user.ID)
	master := s.St.MasterKey(c.user.ID)
	d := map[string]any{"master": master, "devices": devs, "signed": false}
	if c.device != "" {
		sig, sealed, legacy := s.St.deviceTrust(c.device)
		d["signed"] = sig != "" && master != ""
		if sig != "" {
			d["sealed_master"] = sealed
		}
		// Minting the account master key is for the account's first device,
		// or a device that predates signing. A new device on an account that
		// already has others waits to be approved by one of them.
		d["can_establish"] = master == "" && (legacy || len(devs) <= 1)
	}
	return Frame{T: "device.trust", D: d}
}

// pushTrust tells every connection of an account where its devices stand,
// and refreshes the member lists it appears in so peers see the flag.
func (s *Server) pushTrust(uid string) {
	s.mu.RLock()
	list := append([]*Conn(nil), s.conns[uid]...)
	s.mu.RUnlock()
	for _, c := range list {
		c.Send(s.trustFrame(c))
	}
	chans, _ := s.St.ChannelsFor(uid)
	for _, ch := range chans {
		s.PushChannelState(ch.ID)
	}
}

// signedOnlineElsewhere reports whether another approved device of this
// account holds a socket right now. A reset is refused while one does:
// approving from it is the honest path, and a reset would strip it.
func (s *Server) signedOnlineElsewhere(c *Conn) bool {
	s.mu.RLock()
	list := append([]*Conn(nil), s.conns[c.user.ID]...)
	s.mu.RUnlock()
	for _, o := range list {
		if o.device == "" || o.device == c.device {
			continue
		}
		if sig, _, _ := s.St.deviceTrust(o.device); sig != "" {
			return true
		}
	}
	return false
}

func (s *Server) handleDeviceSign(c *Conn, raw json.RawMessage) error {
	var d struct {
		Master string            `json:"master"`
		Reset  bool              `json:"reset"`
		Sigs   map[string]string `json:"sigs"`
		Sealed map[string]string `json:"sealed"`
	}
	json.Unmarshal(raw, &d)
	if c.device == "" {
		return errors.New("register a device first")
	}
	if len(d.Sigs) == 0 || len(d.Sigs) > 64 {
		return errors.New("nothing to sign")
	}
	uid := c.user.ID
	devs, err := s.St.DevicesForUser(uid)
	if err != nil {
		return err
	}
	byID := map[string]Device{}
	for _, x := range devs {
		byID[x.ID] = x
	}
	current := s.St.MasterKey(uid)
	master := current
	switch {
	case d.Reset:
		if d.Master == "" {
			return errors.New("a reset needs a new signing key")
		}
		if s.signedOnlineElsewhere(c) {
			return errors.New("another approved device of yours is online: approve this one from there")
		}
		// After a reset the only device the new key may sign is this one.
		if len(d.Sigs) != 1 || d.Sigs[c.device] == "" {
			return errors.New("a reset signs this device only")
		}
		master = d.Master
	case current == "":
		if d.Master == "" {
			return errors.New("this account has no signing key yet")
		}
		_, _, legacy := s.St.deviceTrust(c.device)
		if !legacy && len(devs) > 1 {
			return errors.New("approve this device from one you already use")
		}
		// The first signer vouches for itself and for devices that predate
		// signing, never for one that arrived after it.
		for dev := range d.Sigs {
			if dev != c.device && !byID[dev].Legacy {
				return errors.New("only devices from before signing can be signed with a new key")
			}
		}
		master = d.Master
	default:
		if d.Master != "" && d.Master != current {
			return errors.New("this account already has a signing key")
		}
		if sig, _, _ := s.St.deviceTrust(c.device); sig == "" {
			return errors.New("this device is not approved, so it cannot approve another")
		}
		d.Master = ""
	}
	for dev, sig := range d.Sigs {
		x, ok := byID[dev]
		if !ok {
			return errors.New("not one of your devices")
		}
		if !verifyDeviceSig(master, uid, dev, x.Pubkey, sig) {
			return errors.New("that signature does not verify")
		}
		if len(d.Sealed[dev]) > 4096 {
			return errors.New("sealed key too large")
		}
	}
	if err := s.St.SignDevices(uid, d.Master, d.Reset, c.device, d.Sigs, d.Sealed); err != nil {
		return err
	}
	s.pushTrust(uid)
	// A newly approved device changes no membership; nudge whoever holds
	// the keys to seal them to it.
	s.nudgeRekey(uid)
	return nil
}

// ---- commands ----

func registerTrustCommands(add func(name, usage, desc, scope, minRole string, args []Arg, h func(*Ctx) error)) {
	add("devices", "/devices [forget <code> | reset]", "your devices, and which are approved to read encrypted channels", "any", "member",
		[]Arg{{Name: "action", Type: "text"}, {Name: "code", Type: "text"}}, cmdDevices)
	// The core signs on the device and never sends /approve; this handler
	// only answers a client too old to know it.
	add("approve", "/approve <code>", "let another of your devices read encrypted channels", "any", "member",
		[]Arg{{Name: "code", Type: "text", Required: true}},
		func(c *Ctx) error { return errors.New("approving a device needs a newer gliph on this device") })
}

func cmdDevices(c *Ctx) error {
	devs, err := c.Srv.St.DevicesForUser(c.User.ID)
	if err != nil {
		return err
	}
	switch c.Arg(0) {
	case "":
	case "forget":
		want := strings.ReplaceAll(strings.ToLower(c.Arg(1)), "-", "")
		for _, d := range devs {
			if strings.ReplaceAll(deviceCode(d.Pubkey), "-", "") != want || want == "" {
				continue
			}
			if c.Conn != nil && d.ID == c.Conn.device {
				return errors.New("that is this device: sign out instead")
			}
			if err := c.Srv.St.ForgetDevice(c.User.ID, d.ID); err != nil {
				return err
			}
			c.Srv.pushTrust(c.User.ID)
			c.Notice("forgot %s. it can no longer read encrypted channels", d.Name)
			return nil
		}
		return fmt.Errorf("no device of yours has code %s", c.Arg(1))
	case "reset":
		return errors.New("resetting device trust needs a newer gliph on this device")
	default:
		return errors.New("usage: /devices [forget <code> | reset]")
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d devices:", len(devs)))
	for _, d := range devs {
		state := "approved"
		if d.Sig == "" {
			state = "WAITING: /approve it from an approved device"
		}
		this := ""
		if c.Conn != nil && d.ID == c.Conn.device {
			this = " (this one)"
		}
		b.WriteString(fmt.Sprintf("\n  %s  %-10s %s%s", deviceCode(d.Pubkey), d.Name, state, this))
	}
	c.Notice("%s", b.String())
	return nil
}

// ---- frame handling ----

// handleCryptoFrame handles the E2EE frames. Returns false if the frame is not
// one of ours, so the main dispatcher can carry on.
func (s *Server) handleCryptoFrame(c *Conn, typ string, raw json.RawMessage) bool {
	switch typ {

	// A client announces one of its devices and its public key.
	case "device.hello":
		var d struct {
			Device string `json:"device"`
			Name   string `json:"name"`
			Pubkey string `json:"pubkey"`
		}
		json.Unmarshal(raw, &d)
		fresh, err := s.St.UpsertDevice(c.user.ID, d.Device, d.Name, d.Pubkey)
		if err != nil {
			s.errTo(c, "", err)
			return true
		}
		c.device = d.Device
		if fresh {
			// Everyone else on the account learns there is a device waiting.
			s.pushTrust(c.user.ID)
		} else {
			c.Send(s.trustFrame(c))
		}
		if fresh {
			// A new device changes no membership, so nobody would otherwise
			// know to seal it a key. Nudge every member of this user's
			// encrypted channels to re-key; whoever holds the current key
			// answers, and the rest no-op.
			s.nudgeRekey(c.user.ID)
		}
		return true

	case "device.sign":
		if err := s.handleDeviceSign(c, raw); err != nil {
			s.errTo(c, "", err)
			c.Send(s.trustFrame(c))
		}
		return true

	// "Who do I need to seal this channel key to?" Public keys only.
	case "keys.peers":
		var d struct {
			Channel string `json:"channel"`
			Epoch   int64  `json:"epoch"`
		}
		json.Unmarshal(raw, &d)
		if _, err := s.requireMember(d.Channel, c.user.ID); err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		var (
			devs []Device
			err  error
		)
		if d.Epoch > 0 {
			devs, err = s.St.DevicesMissingEpoch(d.Channel, d.Epoch)
		} else {
			devs, err = s.St.DevicesInChannel(d.Channel)
		}
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		if d.Epoch > 0 {
			ch, _ := s.St.ChannelByID(d.Channel)
			kept := devs[:0]
			for _, x := range devs {
				if mayHold(ch, x, d.Epoch) {
					kept = append(kept, x)
				}
			}
			devs = kept
		}
		c.Send(Frame{T: "keys.peers", D: map[string]any{
			"channel": d.Channel, "epoch": d.Epoch, "devices": devs,
			"max_epoch": s.St.MaxEpoch(d.Channel),
		}})
		return true

	// A client publishes the channel key for an epoch, sealed per device.
	case "keys.publish":
		var d struct {
			Channel string            `json:"channel"`
			Epoch   int64             `json:"epoch"`
			Entries map[string]string `json:"entries"`
		}
		json.Unmarshal(raw, &d)
		if _, err := s.requireMember(d.Channel, c.user.ID); err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		if c.device == "" {
			s.errTo(c, d.Channel, errors.New("register a device first"))
			return true
		}
		if d.Epoch <= 0 || len(d.Entries) == 0 || len(d.Entries) > 2000 {
			s.errTo(c, d.Channel, errors.New("bad key publication"))
			return true
		}
		if sig, _, _ := s.St.deviceTrust(c.device); sig == "" {
			s.errTo(c, d.Channel, errors.New("this device is not approved yet"))
			return true
		}
		// Only seal to approved devices of members right now, so a
		// compromised client cannot smuggle a key to an outsider's device —
		// and, in a sealed channel, not an epoch from before they joined.
		ch, _ := s.St.ChannelByID(d.Channel)
		allowed := map[string]bool{}
		if devs, err := s.St.DevicesInChannel(d.Channel); err == nil {
			for _, x := range devs {
				allowed[x.ID] = mayHold(ch, x, d.Epoch)
			}
		}
		clean := map[string]string{}
		for dev, w := range d.Entries {
			if allowed[dev] {
				clean[dev] = w
			}
		}
		if err := s.St.PutChannelKeys(d.Channel, d.Epoch, c.device, clean); err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		// Everyone in the channel is told a new epoch exists; each client then
		// asks for its own sealed copy.
		for _, uid := range s.St.MemberIDs(d.Channel) {
			s.SendToUser(uid, Frame{T: "keys.epoch", D: map[string]any{
				"channel": d.Channel, "epoch": d.Epoch}})
		}
		return true

	// "Give me my sealed keys for this channel." A device only ever receives
	// blobs addressed to itself.
	case "keys.fetch":
		var d struct {
			Channel string `json:"channel"`
		}
		json.Unmarshal(raw, &d)
		if _, err := s.requireMember(d.Channel, c.user.ID); err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		if c.device == "" {
			s.errTo(c, d.Channel, errors.New("register a device first"))
			return true
		}
		if owner, ok := s.St.DeviceOwner(c.device); !ok || owner != c.user.ID {
			s.errTo(c, d.Channel, errors.New("unknown device"))
			return true
		}
		// An unapproved device gets nothing. The client is already showing why.
		if sig, _, _ := s.St.deviceTrust(c.device); sig == "" {
			c.Send(Frame{T: "keys", D: map[string]any{"channel": d.Channel, "keys": []any{}}})
			return true
		}
		keys, err := s.St.ChannelKeysFor(d.Channel, c.device)
		if err != nil {
			s.errTo(c, d.Channel, err)
			return true
		}
		c.Send(Frame{T: "keys", D: map[string]any{"channel": d.Channel, "keys": keys}})
		return true
	}
	return false
}

// nudgeRekey asks members of a user's encrypted channels to seal the current
// key to any device that is missing it.
func (s *Server) nudgeRekey(userID string) {
	chans, err := s.St.ChannelsFor(userID)
	if err != nil {
		return
	}
	for _, ch := range chans {
		if !encryptedChannel(ch) {
			continue
		}
		for _, uid := range s.St.MemberIDs(ch.ID) {
			s.SendToUser(uid, Frame{T: "keys.rekey", D: map[string]any{"channel": ch.ID}})
		}
	}
}

// encryptedChannel reports whether a channel is in E2EE mode (+e).
func encryptedChannel(ch *Channel) bool { return ch != nil && ch.HasMode('e') }

// validateEncrypted checks an incoming encrypted body before it is stored. The
// server cannot read it, so all it can do is insist it is well-formed and that
// the channel's mode and the client's claim agree.
func validateEncrypted(ch *Channel, enc bool, epoch int64, body string) error {
	if encryptedChannel(ch) && !enc {
		return fmt.Errorf("#%s is +e: messages must be encrypted", ch.Name)
	}
	if !encryptedChannel(ch) && enc {
		return fmt.Errorf("#%s is not +e", ch.Name)
	}
	if !enc {
		return nil
	}
	if epoch <= 0 {
		return errors.New("encrypted message needs a key epoch")
	}
	// base64 payload, nothing else: keeps junk out of the log and makes the
	// "server stores ciphertext only" test meaningful.
	if body == "" || len(body) > 64000 || strings.ContainsAny(body, " \n\t") {
		return errors.New("malformed ciphertext")
	}
	return nil
}
