package main

// Mobile push, spec F5 ("ID-only push notifications") and F14 ("optional hosted
// push relay configuration").
//
// THE WHOLE POINT, AND THE ONLY THING THIS FILE MAY EVER DO: wake a device.
//
// gliph is end-to-end encrypted, so the server is structurally incapable of
// writing a useful notification — in a +e channel it holds ciphertext and does
// not have, and must never have, the key. That is not a limitation to work
// around; it is the feature. So the push payload carries no body, no sender
// nick, no channel name, no channel id, and no count. It says "something
// happened, come look". The device wakes, reconnects, catches up from its own
// cursor, decrypts with its own key, and renders the notification locally
// (web/src/notify.ts). Every word the user reads is produced on their phone.
//
// Adding a field here to make the notification "nicer" would move that text
// onto the wire, into APNs' and Google's logs, and into the relay operator's
// reach. Do not. The client already renders the good notification; it just
// needs to be awake.
//
// WHAT THE RELAY LEARNS, STATED HONESTLY. A self-hosted server has no APNs or
// FCM credentials, so F14 allows pointing it at a hosted relay with one config
// value. The relay sees a device token and the time it was woken. That is a
// traffic-analysis surface: it can tell that a given device is in a busy
// conversation, and roughly when. It cannot tell with whom, in what channel, or
// about what. Running without `-push-relay` (the default) leaks nothing at all
// and costs only mobile push; everything else works.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// ---- store ----

type PushDevice struct {
	ID       string `json:"id"`
	Platform string `json:"platform"` // ios | android
	Created  string `json:"created_at"`
	// Token is deliberately absent from the JSON: it is write-only through the
	// API. A client registers by token and deletes by token; nothing reads one
	// back out, so a stolen session cannot enumerate a user's devices' push
	// addresses.
	Token string `json:"-"`
}

var pushPlatforms = map[string]bool{"ios": true, "android": true}

// RegisterPushDevice upserts by token: the same device re-registering after a
// reinstall or an OS token rotation must not accumulate rows, and any device
// may register idempotently on every launch.
func (s *Store) RegisterPushDevice(uid, platform, tok string) error {
	_, err := s.db.Exec(`
		INSERT INTO push_devices (id,user_id,platform,token,created_at,last_seen)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(token) DO UPDATE SET user_id=excluded.user_id,
		  platform=excluded.platform, last_seen=excluded.last_seen`,
		id("pdev"), uid, platform, tok, now(), now())
	return err
}

// DeletePushDevice removes a registration by its token. Deleting by token
// rather than by id means a client that has forgotten everything except what
// the OS just handed it can still unregister.
func (s *Store) DeletePushDevice(uid, tok string) error {
	_, err := s.db.Exec(`DELETE FROM push_devices WHERE user_id=? AND token=?`, uid, tok)
	return err
}

func (s *Store) PushDevices(uid string) ([]PushDevice, error) {
	rows, err := s.db.Query(
		`SELECT id,platform,token,created_at FROM push_devices WHERE user_id=?`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PushDevice{}
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.ID, &d.Platform, &d.Token, &d.Created); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PrunePushDevices drops registrations nothing has touched in a long time. A
// token for an app that was deleted months ago is dead weight the relay would
// keep being asked to deliver to.
func (s *Store) PrunePushDevices(olderThan time.Duration) {
	cut := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	s.db.Exec(`DELETE FROM push_devices WHERE last_seen < ?`, cut)
}

func (s *Store) touchPushDevices(uid string) {
	s.db.Exec(`UPDATE push_devices SET last_seen=? WHERE user_id=?`, now(), uid)
}

// ---- the wake sender ----

// Wake is one content-free nudge to one device. There is no body field, and
// there is no room to add one: the struct IS the contract.
type wake struct {
	Platform string `json:"platform"`
	Token    string `json:"token"`
}

// PushSender delivers wakes. Two implementations exist and a third should not
// be needed: nothing (the default) and a relay.
type PushSender interface {
	Wake(devices []PushDevice)
}

type noPush struct{}

func (noPush) Wake([]PushDevice) {}

// relaySender forwards wakes to a hosted relay that holds the APNs/FCM
// credentials a self-hoster does not have. The relay is handed tokens and
// nothing else — see the honesty note at the top of this file.
type relaySender struct {
	url    string
	client *http.Client
}

func NewRelaySender(url string) PushSender {
	if url == "" {
		return noPush{}
	}
	return &relaySender{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

func (r *relaySender) Wake(devices []PushDevice) {
	if len(devices) == 0 {
		return
	}
	ws := make([]wake, 0, len(devices))
	for _, d := range devices {
		ws = append(ws, wake{Platform: d.Platform, Token: d.Token})
	}
	body, err := json.Marshal(map[string]any{"wake": ws})
	if err != nil {
		return
	}
	// Fire and forget on a bounded context. A push that fails is a phone that
	// finds out on its next foreground sync; it is never worth blocking or
	// failing the message that triggered it.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", r.url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := r.client.Do(req)
		if err != nil {
			log.Printf("push relay: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("push relay: HTTP %d", resp.StatusCode)
		}
	}()
}

// ---- coalescing ----

// Wakes are coalesced per user. A burst of forty messages in a busy channel is
// still one reason to pick up the phone, and both platforms punish a server
// that says otherwise: iOS silently throttles background pushes from an app
// that sends too many, so spending them on every single event is how you end up
// with none delivered when it matters.
const wakeInterval = 30 * time.Second

type pushHub struct {
	sender PushSender
	st     *Store

	mu   sync.Mutex
	last map[string]time.Time // user id -> last wake sent
}

func newPushHub(st *Store, sender PushSender) *pushHub {
	return &pushHub{sender: sender, st: st, last: map[string]time.Time{}}
}

// wakeUser sends at most one wake per user per wakeInterval. Returns whether a
// wake was actually dispatched, which is what the tests assert on.
func (h *pushHub) wakeUser(uid string) bool {
	if h == nil || h.sender == nil {
		return false
	}
	h.mu.Lock()
	if t, ok := h.last[uid]; ok && time.Since(t) < wakeInterval {
		h.mu.Unlock()
		return false
	}
	h.last[uid] = time.Now()
	h.mu.Unlock()

	devices, err := h.st.PushDevices(uid)
	if err != nil || len(devices) == 0 {
		return false
	}
	h.sender.Wake(devices)
	return true
}

// prune keeps the coalescing map from growing with every user who ever
// received a wake.
func (h *pushHub) prune() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for uid, t := range h.last {
		if time.Since(t) > 10*wakeInterval {
			delete(h.last, uid)
		}
	}
}

// ---- fan-out ----

// wakePhones nudges every member's registered phones except the sender's own.
//
// It used to skip anyone with a live socket, which sounds right and is not: a
// push device is only ever a phone, and "this account has a socket somewhere"
// is usually the desktop app being open on another desk. That check meant a DM
// arriving while the desktop was open woke nothing at all — the bug this
// replaces. A phone that is awake and connected gets a redundant wake at most
// once per wakeInterval, a cheap price for a notification that arrives.
//
// It deliberately does not care what the event said — it cannot read a +e body
// anyway, and treating encrypted and plaintext channels differently here would
// make the push pattern itself leak which channels are encrypted.
// The member list is passed in rather than re-read: Post already has it, and
// asking twice per message doubled the cost of the busiest call on the server.
func (s *Server) wakePhones(members []string, e *Event) {
	if s.Push == nil {
		return
	}
	// Only a human utterance is worth a phone buzzing. Joins, mode changes and
	// topic edits are not.
	if e.Kind != "message" && e.Kind != "action" && e.Kind != "file" {
		return
	}
	// Who NOT to wake, resolved once. This used to load every member of the
	// channel to compare their nick against the actor's — a query per member,
	// on the hot path, that also got the answer wrong the moment anyone
	// renamed. Comparing ids costs at most one lookup for the whole fan-out.
	self := e.actorID
	if self == "" {
		if u, err := s.St.UserByNick(e.Actor); err == nil {
			self = u.ID
		}
	}
	for _, uid := range members {
		if uid == self {
			continue // never wake someone for their own message on another device
		}
		s.Push.wakeUser(uid)
	}
}

// ---- routes ----

func (s *Server) pushRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/push/devices", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		var d struct{ Platform, Token string }
		json.NewDecoder(r.Body).Decode(&d)
		if !pushPlatforms[d.Platform] {
			fail(w, 400, "platform must be ios or android")
			return
		}
		if len(d.Token) == 0 || len(d.Token) > 512 {
			fail(w, 400, "bad token")
			return
		}
		if err := s.St.RegisterPushDevice(u.ID, d.Platform, d.Token); err != nil {
			fail(w, 500, err.Error())
			return
		}
		// Registering says nothing back about the other devices on the account:
		// the count alone would tell a stolen session how many phones to expect.
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("DELETE /api/push/devices", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.authUser(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		var d struct{ Token string }
		json.NewDecoder(r.Body).Decode(&d)
		if d.Token == "" {
			fail(w, 400, "token required")
			return
		}
		if err := s.St.DeletePushDevice(u.ID, d.Token); err != nil && err != sql.ErrNoRows {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}
