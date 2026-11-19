package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// roomEvents pulls the room.event payloads out of a drained frame list.
func roomEvents(frames []Frame) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f.T == "room.event" {
			out = append(out, f.D.(map[string]any))
		}
	}
	return out
}

// roomPair is a channel with two members, both connected and joined-into the
// room, drained to a clean slate.
func roomPair(t *testing.T) (*Server, *Channel, *Conn, *Conn) {
	t.Helper()
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "op")
	s.St.Join(ch.ID, robin.ID, "member")
	dc, rc := fakeConn(s, dana), fakeConn(s, robin)
	for _, c := range []*Conn{dc, rc} {
		if !s.handleRoomFrame(c, "room.join", raw(t, map[string]any{"channel": ch.ID, "room": "office"})) {
			t.Fatal("room.join not handled")
		}
	}
	drain(dc)
	drain(rc)
	return s, ch, dc, rc
}

func TestRoomCommandOpensOnlyTheAskingConnection(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "member")
	laptop, phone := fakeConn(s, dana), fakeConn(s, dana)
	drain(laptop)
	drain(phone)

	for _, name := range []string{"room0", "3d"} {
		if err := s.RunCommand(laptop, ch.ID, name, ""); err != nil {
			t.Fatalf("/%s: %v", name, err)
		}
		var opened bool
		for _, f := range drain(laptop) {
			if f.T == "room.open" && f.D.(map[string]any)["channel"] == ch.ID {
				opened = true
			}
		}
		if !opened {
			t.Fatalf("/%s sent no room.open to the caller", name)
		}
		// You typed it on the laptop; the phone must not change surface.
		for _, f := range drain(phone) {
			if f.T == "room.open" {
				t.Fatalf("/%s opened the room on another device", name)
			}
		}
	}
}

func TestRoomJoinBroadcastsServerTruth(t *testing.T) {
	s := testServer(t)
	dana, _ := s.St.CreateUser("dana", "d@x.io", "hunter2hunter2", "member")
	robin, _ := s.St.CreateUser("robin", "r@x.io", "hunter2hunter2", "member")
	mallory, _ := s.St.CreateUser("mallory", "m@x.io", "hunter2hunter2", "member")
	ch, _ := s.St.ChannelByName("general")
	s.St.Join(ch.ID, dana.ID, "member")
	s.St.Join(ch.ID, robin.ID, "member")
	dc, rc, mc := fakeConn(s, dana), fakeConn(s, robin), fakeConn(s, mallory)
	drain(dc)
	drain(rc)

	// The join carries the SERVER's name for the user and capped/clamped state,
	// whatever the client claimed.
	if !s.handleRoomFrame(dc, "room.join", raw(t, map[string]any{
		"channel": ch.ID, "room": strings.Repeat("r", 99), "x": 5000.0, "y": -5000.0, "h": 2.0,
		"anim": strings.Repeat("a", 99), "look": strings.Repeat("l", 99), "name": "impostor"})) {
		t.Fatal("room.join not handled")
	}
	evs := roomEvents(drain(rc))
	if len(evs) != 1 {
		t.Fatalf("robin saw %d events, want 1", len(evs))
	}
	e := evs[0]
	if e["type"] != "join" || e["user_id"] != dana.ID || e["name"] != "dana" {
		t.Fatalf("join event = %v", e)
	}
	if len(e["room"].(string)) != roomMaxRoom || len(e["anim"].(string)) != roomMaxAnim || len(e["look"].(string)) != roomMaxLook {
		t.Fatalf("strings not capped: %v", e)
	}
	if e["x"].(float64) != 1000 || e["y"].(float64) != -1000 || e["h"].(float64) != 2 {
		t.Fatalf("coords not clamped: %v", e)
	}
	// sender included — clients filter their own echo by user_id
	if len(roomEvents(drain(dc))) != 1 {
		t.Fatal("sender did not get their own join echo")
	}

	// a non-member cannot join the room or be broadcast into it
	if !s.handleRoomFrame(mc, "room.join", raw(t, map[string]any{"channel": ch.ID})) {
		t.Fatal("frame not handled")
	}
	if evs := roomEvents(drain(rc)); len(evs) != 0 {
		t.Fatalf("non-member join was broadcast: %v", evs)
	}
	if !strings.Contains(noticeText(drain(mc)), "not in that channel") {
		t.Fatal("non-member got no refusal")
	}

	// room.send before room.join is refused
	if !s.handleRoomFrame(rc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "move", "room": "office"})) {
		t.Fatal("frame not handled")
	}
	if !strings.Contains(noticeText(drain(rc)), "join the room first") {
		t.Fatal("send before join was not refused")
	}
	if evs := roomEvents(drain(dc)); len(evs) != 0 {
		t.Fatalf("unjoined send was broadcast: %v", evs)
	}
}

func TestRoomMoveAnnounceAndFieldRebuild(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	// announce relays as move; carry is capped; a client's extra keys die at
	// the field-by-field rebuild.
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
		"channel": ch.ID, "type": "announce", "room": "garden", "x": 1.0, "y": 2.0, "h": 0.5,
		"anim": "walk", "carry": strings.Repeat("c", 99), "moving": true, "voice": true,
		"look": "suit", "evil": "yes"}))
	evs := roomEvents(drain(rc))
	if len(evs) != 1 || evs[0]["type"] != "move" {
		t.Fatalf("announce did not relay as move: %v", evs)
	}
	e := evs[0]
	if e["name"] != "dana" || e["room"] != "garden" || e["moving"] != true || e["voice"] != true {
		t.Fatalf("move fields: %v", e)
	}
	if len(e["carry"].(string)) != roomMaxCarry {
		t.Fatalf("carry not capped: %v", e["carry"])
	}
	if _, leaked := e["evil"]; leaked {
		t.Fatal("client-invented key passed through the relay")
	}

	// no carry means no carry key, not an empty string
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "move", "room": "office"}))
	e = roomEvents(drain(rc))[0]
	if _, has := e["carry"]; has {
		t.Fatalf("empty carry was sent: %v", e)
	}

	// an unknown type is refused, not relayed
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "teleport"}))
	if evs := roomEvents(drain(rc)); len(evs) != 0 {
		t.Fatalf("unknown type relayed: %v", evs)
	}
	if !strings.Contains(noticeText(drain(dc)), "unknown room event") {
		t.Fatal("unknown type got no refusal")
	}
}

func TestRoomObjectHolderRewriteAndRtc(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	// holder "me" is rewritten to the sender's real id
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
		"channel": ch.ID, "type": "object", "id": "ball", "room": "garden",
		"x": 1.0, "y": 2.0, "h": 0.4, "vx": 9999.0, "vy": -9999.0, "holder": "me"}))
	e := roomEvents(drain(rc))[0]
	if e["type"] != "object" || e["id"] != "ball" || e["holder"] != dc.user.ID {
		t.Fatalf("object event: %v", e)
	}
	if e["h"].(float64) != 0.4 {
		t.Fatalf("object h not relayed: %v", e)
	}
	if e["vx"].(float64) != 1000 || e["vy"].(float64) != -1000 {
		t.Fatalf("velocity not clamped: %v", e)
	}

	// other holder values pass through, capped
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
		"channel": ch.ID, "type": "object", "id": "ball", "holder": strings.Repeat("h", 99)}))
	if e := roomEvents(drain(rc))[0]; len(e["holder"].(string)) != roomMaxCarry {
		t.Fatalf("holder not capped: %v", e["holder"])
	}

	// an object with no id is refused
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "object"}))
	if evs := roomEvents(drain(rc)); len(evs) != 0 {
		t.Fatalf("id-less object relayed: %v", evs)
	}

	// rtc: to/kind capped, payload carried opaque and byte-identical
	sdp := json.RawMessage(`{"sdp":"v=0 o=- 42","nested":{"deep":true}}`)
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
		"channel": ch.ID, "type": "rtc", "to": rc.user.ID, "kind": "offer", "payload": sdp}))
	e = roomEvents(drain(rc))[0]
	if e["type"] != "rtc" || e["to"] != rc.user.ID || e["kind"] != "offer" {
		t.Fatalf("rtc event: %v", e)
	}
	if !bytes.Equal(e["payload"].(json.RawMessage), sdp) {
		t.Fatalf("rtc payload was not carried opaque: %s", e["payload"])
	}

	// emote
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "emote", "kind": "wave"}))
	if e := roomEvents(drain(rc))[0]; e["type"] != "emote" || e["kind"] != "wave" || e["name"] != "dana" {
		t.Fatalf("emote event: %v", e)
	}
}

func TestRoomLeaveAndDisconnectBroadcastLeave(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	s.handleRoomFrame(dc, "room.leave", raw(t, map[string]any{"channel": ch.ID}))
	e := roomEvents(drain(rc))[0]
	if e["type"] != "leave" || e["user_id"] != dc.user.ID {
		t.Fatalf("leave event: %v", e)
	}
	// a second leave has nothing to announce
	s.handleRoomFrame(dc, "room.leave", raw(t, map[string]any{"channel": ch.ID}))
	if evs := roomEvents(drain(rc)); len(evs) != 0 {
		t.Fatalf("double leave broadcast: %v", evs)
	}
	// ...and having left, sending is refused again
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{"channel": ch.ID, "type": "move", "room": "office"}))
	if evs := roomEvents(drain(rc)); len(evs) != 0 {
		t.Fatalf("post-leave send relayed: %v", evs)
	}

	// a dropped socket is a leave too, for every room the conn stood in
	s.handleRoomFrame(dc, "room.join", raw(t, map[string]any{"channel": ch.ID, "room": "office"}))
	drain(rc)
	s.removeConn(dc)
	e = roomEvents(drain(rc))[0]
	if e["type"] != "leave" || e["user_id"] != dc.user.ID {
		t.Fatalf("disconnect leave: %v", e)
	}
	s.roomMu.Lock()
	_, still := s.roomJoined[dc]
	s.roomMu.Unlock()
	if still {
		t.Fatal("disconnected conn still tracked in roomJoined")
	}
}

func TestRoomFramesAreThrottled(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	for i := 0; i < 200; i++ {
		s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
			"channel": ch.ID, "type": "move", "room": "office"}))
	}
	got := len(roomEvents(drain(rc)))
	if got >= 200 {
		t.Fatalf("all %d frames relayed — the room kind is unmetered", got)
	}
	if !strings.Contains(noticeText(drain(dc)), "too fast") {
		t.Fatal("throttled sender was not told")
	}
}

// ---- HTTP: boards ----

func roomHTTP(t *testing.T) (*Server, http.Handler, *Channel, string, map[string]string) {
	t.Helper()
	s := testServer(t)
	base := t.TempDir()
	h := s.Routes("", filepath.Join(base, "uploads"))
	ch, _ := s.St.ChannelByName("general")
	toks := map[string]string{}
	for nick, role := range map[string]string{"dana": "op", "robin": "member", "mika": "member"} {
		u, err := s.St.CreateUser(nick, nick+"@x.io", "hunter2hunter2", "member")
		if err != nil {
			t.Fatal(err)
		}
		s.St.Join(ch.ID, u.ID, role)
		tok, _, err := s.St.Login(nick, "hunter2hunter2")
		if err != nil {
			t.Fatal(err)
		}
		toks[nick] = tok
	}
	// outsider: an account, but not a member of the channel
	if _, err := s.St.CreateUser("sam", "s@x.io", "hunter2hunter2", "member"); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := s.St.Login("sam", "hunter2hunter2")
	toks["sam"] = tok
	return s, h, ch, filepath.Join(base, "roomnotes"), toks
}

func roomReq(h http.Handler, method, url, token string, body []byte, ctype string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRoomBoardUpsertAndCaps(t *testing.T) {
	_, h, ch, _, toks := roomHTTP(t)
	url := "/api/room/board?channel=" + ch.ID + "&kind=tasks"

	// a board nobody saved yet is an empty object, not a 404
	w := roomReq(h, "GET", url, toks["robin"], nil, "")
	if w.Code != 200 {
		t.Fatalf("fresh board: %d %s", w.Code, w.Body)
	}
	var got struct {
		Board struct {
			Kind      string          `json:"kind"`
			Data      json.RawMessage `json:"data"`
			UpdatedAt string          `json:"updated_at"`
		} `json:"board"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if string(got.Board.Data) != "{}" || got.Board.Kind != "tasks" {
		t.Fatalf("fresh board = %s", w.Body)
	}

	// save, read back, overwrite
	for i, data := range []string{`{"items":["a"]}`, `{"items":["a","b"]}`} {
		w = roomReq(h, "PUT", url, toks["robin"], []byte(`{"data":`+data+`}`), "application/json")
		if w.Code != 200 {
			t.Fatalf("put %d: %d %s", i, w.Code, w.Body)
		}
	}
	w = roomReq(h, "GET", url, toks["dana"], nil, "")
	json.Unmarshal(w.Body.Bytes(), &got)
	if string(got.Board.Data) != `{"items":["a","b"]}` || got.Board.UpdatedAt == "" {
		t.Fatalf("board after upsert = %s", w.Body)
	}

	// data must be a JSON object
	w = roomReq(h, "PUT", url, toks["robin"], []byte(`{"data":[1,2]}`), "application/json")
	if w.Code != 400 {
		t.Fatalf("array data: %d", w.Code)
	}
	// a board just over the cap is refused with an honest 413
	big := `{"data":{"blob":"` + strings.Repeat("a", roomBoardMax) + `"}}`
	w = roomReq(h, "PUT", url, toks["robin"], []byte(big), "application/json")
	if w.Code != 413 {
		t.Fatalf("oversized board: %d", w.Code)
	}
	// unknown kind and non-member
	if w = roomReq(h, "GET", "/api/room/board?channel="+ch.ID+"&kind=weather", toks["robin"], nil, ""); w.Code != 400 {
		t.Fatalf("bad kind: %d", w.Code)
	}
	if w = roomReq(h, "GET", url, toks["sam"], nil, ""); w.Code != 403 {
		t.Fatalf("outsider read a board: %d", w.Code)
	}
}

// ---- HTTP: voice notes ----

func postNote(t *testing.T, h http.Handler, token, cid, room, dur, ctype string, audio []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("channel", cid)
	mw.WriteField("room", room)
	mw.WriteField("duration_ms", dur)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="note.webm"`)
	hdr.Set("Content-Type", ctype)
	pw, _ := mw.CreatePart(hdr)
	pw.Write(audio)
	mw.Close()
	return roomReq(h, "POST", "/api/room/notes", token, b.Bytes(), mw.FormDataContentType())
}

func noteID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var res struct {
		Note map[string]any `json:"voice_note"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	nid, _ := res.Note["id"].(string)
	if nid == "" {
		t.Fatalf("no note id in %s", w.Body)
	}
	return nid
}

func TestRoomNoteCreateListenAndValidate(t *testing.T) {
	s, h, ch, notesDir, toks := roomHTTP(t)
	dana, _ := s.St.UserByNick("dana")
	dc := fakeConn(s, dana)
	drain(dc)

	w := postNote(t, h, toks["robin"], ch.ID, "lounge", "4200", "audio/webm;codecs=opus", []byte("OggS-ish bytes"))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	nid := noteID(t, w)
	var res struct {
		Note map[string]any `json:"voice_note"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Note["author_name"] != "robin" || res.Note["room"] != "lounge" ||
		res.Note["duration_ms"] != float64(4200) ||
		res.Note["audio_url"] != "/api/room/notes/"+nid+"/audio" {
		t.Fatalf("note shape: %s", w.Body)
	}

	// every member was told a note landed
	var vnote map[string]any
	for _, e := range roomEvents(drain(dc)) {
		if e["type"] == "vnote" {
			vnote = e
		}
	}
	if vnote == nil || vnote["op"] != "new" || vnote["note"] == nil {
		t.Fatalf("vnote broadcast: %v", vnote)
	}

	// the wall lists it, and the audio plays back with the stored type
	w = roomReq(h, "GET", "/api/room/notes?channel="+ch.ID+"&room=lounge", toks["dana"], nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), nid) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	w = roomReq(h, "GET", "/api/room/notes/"+nid+"/audio", toks["dana"], nil, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/webm" ||
		w.Header().Get("Content-Disposition") != "inline" || w.Body.String() != "OggS-ish bytes" {
		t.Fatalf("audio: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	// ?token= works the way /api/file's does — an <audio> tag has no headers
	req := httptest.NewRequest("GET", "/api/room/notes/"+nid+"/audio?token="+toks["dana"], nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("audio via ?token=: %d", rec.Code)
	}
	// but not for an outsider
	if w = roomReq(h, "GET", "/api/room/notes/"+nid+"/audio", toks["sam"], nil, ""); w.Code != 403 {
		t.Fatalf("outsider heard a note: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(notesDir, nid)); err != nil {
		t.Fatalf("audio bytes not on disk: %v", err)
	}

	// the validations, each its own refusal
	for name, w := range map[string]*httptest.ResponseRecorder{
		"bad room":       postNote(t, h, toks["robin"], ch.ID, "moon", "4200", "audio/webm", []byte("x")),
		"bad type":       postNote(t, h, toks["robin"], ch.ID, "lounge", "4200", "text/html", []byte("x")),
		"zero duration":  postNote(t, h, toks["robin"], ch.ID, "lounge", "0", "audio/webm", []byte("x")),
		"long duration":  postNote(t, h, toks["robin"], ch.ID, "lounge", "999999", "audio/webm", []byte("x")),
		"outsider":       postNote(t, h, toks["sam"], ch.ID, "lounge", "4200", "audio/webm", []byte("x")),
		"bad list room":  roomReq(h, "GET", "/api/room/notes?channel="+ch.ID+"&room=moon", toks["robin"], nil, ""),
		"no such note":   roomReq(h, "GET", "/api/room/notes/vn_nope/audio", toks["robin"], nil, ""),
		"delete unknown": roomReq(h, "DELETE", "/api/room/notes/vn_nope", toks["robin"], nil, ""),
	} {
		if w.Code < 400 {
			t.Fatalf("%s: got %d, want a refusal", name, w.Code)
		}
	}
}

func TestRoomNoteWallReapsAtFifty(t *testing.T) {
	_, h, ch, notesDir, toks := roomHTTP(t)

	ids := make([]string, 0, roomNoteKeep+2)
	for i := 0; i < roomNoteKeep+2; i++ {
		w := postNote(t, h, toks["robin"], ch.ID, "office", "1000", "audio/webm",
			[]byte(fmt.Sprintf("note-%d", i)))
		if w.Code != 201 {
			t.Fatalf("create %d: %d %s", i, w.Code, w.Body)
		}
		ids = append(ids, noteID(t, w))
	}

	w := roomReq(h, "GET", "/api/room/notes?channel="+ch.ID+"&room=office", toks["robin"], nil, "")
	var res struct {
		Notes []map[string]any `json:"voice_notes"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Notes) != roomNoteKeep {
		t.Fatalf("wall holds %d notes, want %d", len(res.Notes), roomNoteKeep)
	}
	if res.Notes[0]["id"] != ids[len(ids)-1] {
		t.Fatalf("wall is not newest-first: %v", res.Notes[0]["id"])
	}
	// the two oldest are gone — row AND file
	for _, old := range ids[:2] {
		if w := roomReq(h, "GET", "/api/room/notes/"+old+"/audio", toks["robin"], nil, ""); w.Code != 404 {
			t.Fatalf("reaped note %s still served: %d", old, w.Code)
		}
		if _, err := os.Stat(filepath.Join(notesDir, old)); !os.IsNotExist(err) {
			t.Fatalf("reaped note %s left its file behind", old)
		}
	}
}

func TestRoomNoteDeletePermissions(t *testing.T) {
	_, h, ch, notesDir, toks := roomHTTP(t)

	nid := noteID(t, postNote(t, h, toks["robin"], ch.ID, "kitchen", "1000", "audio/webm", []byte("mine")))

	// another plain member may not delete it
	if w := roomReq(h, "DELETE", "/api/room/notes/"+nid, toks["mika"], nil, ""); w.Code != 403 {
		t.Fatalf("member deleted someone else's note: %d", w.Code)
	}
	// the author may
	if w := roomReq(h, "DELETE", "/api/room/notes/"+nid, toks["robin"], nil, ""); w.Code != 204 {
		t.Fatalf("author delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(notesDir, nid)); !os.IsNotExist(err) {
		t.Fatal("deleted note left its file behind")
	}
	// ...and so may a channel op, for someone else's note
	nid = noteID(t, postNote(t, h, toks["robin"], ch.ID, "kitchen", "1000", "audio/webm", []byte("mine too")))
	if w := roomReq(h, "DELETE", "/api/room/notes/"+nid, toks["dana"], nil, ""); w.Code != 204 {
		t.Fatalf("op delete: %d %s", w.Code, w.Body)
	}
}

// ---- the bridge: room chat is a channel message ----

func TestRoomMessageStampsRoomAndQuiet(t *testing.T) {
	s, ch, dc, _ := roomPair(t)

	if err := s.handleSend(dc, ch.ID, "east wall now", "", false, 0, "", "office", false); err != nil {
		t.Fatalf("room message: %v", err)
	}
	if err := s.handleSend(dc, ch.ID, "side note", "", false, 0, "", "lounge", true); err != nil {
		t.Fatalf("quiet room message: %v", err)
	}
	evs, _ := s.St.Since(ch.ID, 0, 10)
	if n := len(evs); n != 2 {
		t.Fatalf("posted %d events, want 2", n)
	}
	if evs[0].Meta["room"] != "office" || evs[0].Meta["quiet"] != nil {
		t.Fatalf("loud line's meta: %v", evs[0].Meta)
	}
	if evs[1].Meta["room"] != "lounge" || evs[1].Meta["quiet"] != true {
		t.Fatalf("quiet line's meta: %v", evs[1].Meta)
	}

	// A room nobody can stand in would be rendered as a mark all the same.
	if err := s.handleSend(dc, ch.ID, "nowhere", "", false, 0, "", "atlantis", false); err == nil {
		t.Fatal("an invented room was stamped")
	}
}

func TestChannelStateCarriesWhoIsInTheRoom(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	v := s.channelView(ch, dc.user.ID)
	room, ok := v["room"].(map[string]any)
	if !ok {
		t.Fatal("an occupied room is missing from the channel view")
	}
	if room["since"] == "" {
		t.Fatal("the room reports no start time")
	}
	users := room["users"].([]roomOccupant)
	if len(users) != 2 || users[0].Name != "dana" || users[0].Room != "office" {
		t.Fatalf("occupants: %v", users)
	}

	// voice rides the move that reports it
	s.handleRoomFrame(dc, "room.send", raw(t, map[string]any{
		"channel": ch.ID, "type": "move", "room": "lounge", "voice": true}))
	users = s.channelView(ch, dc.user.ID)["room"].(map[string]any)["users"].([]roomOccupant)
	if !users[0].Voice || users[0].Room != "lounge" {
		t.Fatalf("the seat did not follow the move: %v", users[0])
	}

	// last one out and the channel view forgets the room entirely
	s.handleRoomFrame(dc, "room.leave", raw(t, map[string]any{"channel": ch.ID}))
	s.handleRoomFrame(rc, "room.leave", raw(t, map[string]any{"channel": ch.ID}))
	if _, still := s.channelView(ch, dc.user.ID)["room"]; still {
		t.Fatal("an empty room is still reported")
	}
	s.roomMu.Lock()
	since := s.roomSince[ch.ID]
	s.roomMu.Unlock()
	if since != "" {
		t.Fatal("an empty room kept its start time")
	}
}

func TestRoomInviteChannelPersonAndQuiet(t *testing.T) {
	s, ch, dc, rc := roomPair(t)

	// bare: one line for the whole channel, naming where the inviter stands
	if err := s.RunCommand(dc, ch.ID, "room0", "invite"); err != nil {
		t.Fatalf("/room0 invite: %v", err)
	}
	evs, _ := s.St.Since(ch.ID, 0, 10)
	last := evs[len(evs)-1]
	if last.Kind != "room_invite" || last.Body != "office" || last.Meta["target"] != nil {
		t.Fatalf("channel invite: %+v", last)
	}

	// a nick: the same event in the DM, addressed
	if err := s.RunCommand(dc, ch.ID, "room0", "invite robin"); err != nil {
		t.Fatalf("/room0 invite robin: %v", err)
	}
	dm, err := s.DMChannel(dc.user, rc.user)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ = s.St.Since(dm.ID, 0, 10)
	last = evs[len(evs)-1]
	if last.Kind != "room_invite" || last.Meta["target"] != rc.user.ID {
		t.Fatalf("personal invite: %+v", last)
	}
	// A DM has no room of its own, so the invite has to name the channel whose
	// room was opened — that is what the join button walks into.
	if last.Meta["channel"] != ch.ID {
		t.Fatalf("personal invite points at no channel: %+v", last.Meta)
	}
	if last.Meta["target_nick"] != "robin" {
		t.Fatalf("personal invite cannot name who was asked: %+v", last.Meta)
	}

	if err := s.RunCommand(dc, ch.ID, "room0", "invite nobody"); err == nil {
		t.Fatal("invited a user who does not exist")
	}

	// quiet is the client's switch; the server only asks it to flip
	drain(dc)
	if err := s.RunCommand(dc, ch.ID, "room0", "quiet"); err != nil {
		t.Fatalf("/room0 quiet: %v", err)
	}
	var flipped bool
	for _, f := range drain(dc) {
		if f.T == "room.quiet" && f.D.(map[string]any)["channel"] == ch.ID {
			flipped = true
		}
	}
	if !flipped {
		t.Fatal("/room0 quiet sent no room.quiet to the caller")
	}
}
