package main

// Admin console API (spec F14). Every route here is registered through the
// admin-only wrapper that Routes() passes in, so no handler in this file does
// its own auth — there is exactly one place that decides who is an operator.
//
// PRIVACY — gliph is end-to-end encrypted and this is the widest-reading API on
// the server. An operator gets identities and counts: nicks, emails, channel
// NAMES, member counts, file names and sizes. Nothing here returns a message
// body, a ciphertext, a password hash, a session token, or a wrapped channel
// key, and nothing added later may either. If a query needs a column from
// `events` other than COUNT(*), it does not belong in this file.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var startedAt = time.Now()

// Every list endpoint here is capped. The audit log always was; the others read
// the whole table, which is fine at ten users and a slow page at ten thousand —
// and the console is the one place that reads them all at once. If a server ever
// outgrows this, the fix is paging, not a bigger number.
const adminListMax = 500

// adminWrap is the auth wrapper Routes() owns; adminRoutes takes it as an
// argument rather than re-deriving admin-ness here.
type adminWrap func(func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc

// audit records an operator action, to the server log (what a self-host ships
// to journald) and to the audit_log table the console reads back.
//
// The target is written as text — a nick, a #channel — and never as a foreign
// key, because half of what gets audited is a deletion: the row has to still
// name its subject after the subject is gone.
//
// A failed write is logged and swallowed. An audit row records an action that
// already happened; failing the operator's request afterwards would tell them
// it did not.
func (s *Server) audit(actor *User, action, target, detail string) {
	line := action + " " + target
	if detail != "" {
		line += " (" + detail + ")"
	}
	log.Printf("admin %s: %s", actor.Nick, line)
	if err := s.St.AppendAudit(actor.Nick, action, target, detail); err != nil {
		log.Printf("audit write failed: %v", err)
	}
}

func (s *Server) adminRoutes(mux *http.ServeMux, admin adminWrap) {
	// ---- users ----

	mux.HandleFunc("GET /api/admin/users", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		// email is included: an operator needs it to contact or identify an
		// account. pass_hash is not selected here and must never be.
		rows, err := s.St.r.Query(`SELECT u.id,u.nick,u.email,u.role,u.suspended,u.created_at,
		  (SELECT COUNT(*) FROM devices d WHERE d.user_id=u.id),
		  (SELECT COUNT(*) FROM members m WHERE m.user_id=u.id)
		  FROM users u ORDER BY u.created_at LIMIT ?`, adminListMax)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var uid, nick, email, role, created string
			var susp, devices, channels int
			if err := rows.Scan(&uid, &nick, &email, &role, &susp, &created, &devices, &channels); err != nil {
				fail(w, 500, err.Error())
				return
			}
			out = append(out, map[string]any{"id": uid, "nick": nick, "email": email, "role": role,
				"suspended": susp == 1, "created": created, "devices": devices,
				"channels": channels, "online": s.IsOnline(uid)})
		}
		writeJSON(w, 200, out)
	}))

	// Provisioning an account by hand is the other half of invites: a self-host
	// that already knows who is joining should not have to send anyone a link.
	// The password is chosen by the operator and handed over out of band — there
	// is no mailer here (see the invite note below).
	mux.HandleFunc("POST /api/admin/users", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Nick, Email, Password, Role string }
		json.NewDecoder(r.Body).Decode(&d)
		if d.Role != "admin" {
			d.Role = "member"
		}
		if d.Email == "" {
			fail(w, 400, "email is required")
			return
		}
		u, err := s.St.CreateUser(d.Nick, d.Email, d.Password, d.Role)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		// Same landing as self-registration, so a provisioned account is not a
		// second kind of account with its own empty state.
		if ch, err := s.St.ChannelByName("general"); err == nil {
			s.St.Join(ch.ID, u.ID, "member")
			s.Post(ch, &Event{Kind: "join", Actor: u.Nick, Body: "joined the server", Meta: map[string]any{"new": true}}, "")
			s.PushChannelState(ch.ID)
		}
		s.audit(actor, "create-user", u.Nick, d.Role)
		writeJSON(w, 200, map[string]any{"id": u.ID, "nick": u.Nick, "role": u.Role})
	}))

	// A reset ends every session the account has. That is the point of one: the
	// reason to reset is usually that somebody else might be holding the old
	// password, and leaving their live tokens alone would reset nothing.
	mux.HandleFunc("POST /api/admin/users/{id}/password", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Password string }
		json.NewDecoder(r.Body).Decode(&d)
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		if err := s.St.SetPassword(u.Nick, d.Password); err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.kick(u.ID)
		s.audit(actor, "reset-password", u.Nick, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /api/admin/users/{id}/suspend", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		if _, err := s.St.db.Exec(`UPDATE users SET suspended=1 WHERE id=?`, u.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.kick(u.ID)
		s.audit(actor, "suspend", u.Nick, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /api/admin/users/{id}/unsuspend", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		// Look the account up first so the audit row names a nick like every
		// other row does — an audit trail of opaque ids reads as nothing.
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		if _, err := s.St.db.Exec(`UPDATE users SET suspended=0 WHERE id=?`, u.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.audit(actor, "unsuspend", u.Nick, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// Disconnect drops an account's live sockets and sessions without touching
	// the account: the sign-in-again button for a stuck client or a device the
	// operator wants off the server right now.
	mux.HandleFunc("POST /api/admin/users/{id}/disconnect", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		s.kick(u.ID)
		s.audit(actor, "disconnect", u.Nick, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("POST /api/admin/users/{id}/role", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Role string }
		json.NewDecoder(r.Body).Decode(&d)
		if d.Role != "member" && d.Role != "admin" {
			fail(w, 400, "role must be member or admin")
			return
		}
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		if d.Role == "member" {
			if err := s.canLoseAdmin(u, actor, "demote"); err != nil {
				fail(w, 409, err.Error())
				return
			}
		}
		if _, err := s.St.db.Exec(`UPDATE users SET role=? WHERE id=?`, d.Role, u.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.audit(actor, "role", u.Nick, d.Role)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("DELETE /api/admin/users/{id}", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		u, err := s.St.UserByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such user")
			return
		}
		if err := s.canLoseAdmin(u, actor, "delete"); err != nil {
			fail(w, 409, err.Error())
			return
		}
		// sessions, members, devices all cascade on users(id). A DM is a
		// channel of two; with one gone it is a rail entry named after a dead
		// id, so it goes with the user.
		s.St.db.Exec(`DELETE FROM channels WHERE is_dm=1 AND id IN (SELECT channel_id FROM members WHERE user_id=?)`, u.ID)
		if _, err := s.St.db.Exec(`DELETE FROM users WHERE id=?`, u.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.kick(u.ID)
		s.audit(actor, "delete-user", u.Nick, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// ---- invites ----
	//
	// An invite is a token, not an email: gliph has no mailer and a self-host
	// should not need one. The console shows the redeem link and the operator
	// sends it however they already talk to the person.

	mux.HandleFunc("GET /api/admin/invites", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		// A redeemed invite stays redeemed after its account is deleted: the
		// nick is gone, but used_by is not, and Register and revoke both key
		// off used_by. Reporting '' here showed such an invite as open.
		rows, err := s.St.r.Query(`SELECT i.token,i.role,i.created_at,
		  CASE WHEN i.used_by IS NULL THEN '' ELSE COALESCE(u.nick,'a deleted account') END
		  FROM invites i LEFT JOIN users u ON u.id=i.used_by ORDER BY i.created_at DESC LIMIT ?`, adminListMax)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var tok, role, created, usedBy string
			if err := rows.Scan(&tok, &role, &created, &usedBy); err != nil {
				fail(w, 500, err.Error())
				return
			}
			out = append(out, map[string]any{"token": tok, "role": role, "created": created, "used_by": usedBy})
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("POST /api/admin/invites", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Role string }
		json.NewDecoder(r.Body).Decode(&d)
		if d.Role != "admin" {
			d.Role = "member"
		}
		t, err := s.St.CreateInvite(d.Role)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.audit(actor, "invite", d.Role, "")
		writeJSON(w, 200, map[string]string{"invite": t, "role": d.Role})
	}))

	// Revoking is a delete, and only of an unused token: a redeemed invite is
	// the audit trail of how an account got here.
	mux.HandleFunc("DELETE /api/admin/invites/{token}", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		res, err := s.St.db.Exec(`DELETE FROM invites WHERE token=? AND used_by IS NULL`, r.PathValue("token"))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(w, 409, "already redeemed or gone")
			return
		}
		s.audit(actor, "revoke-invite", r.PathValue("token")[:8]+"…", "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// ---- plugins ----
	//
	// The token is returned once, at registration, and never listed: it is a
	// bot credential like a session token (see the privacy note above). An
	// operator who loses it revokes the plugin and registers it again.

	mux.HandleFunc("GET /api/admin/plugins", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		live := map[string]int{}
		for _, p := range s.ConnectedPlugins() {
			live[p.ID] = p.NCommands
		}
		rows, err := s.St.r.Query(`SELECT p.id,p.display,p.kind,p.created_at,
		  (SELECT COUNT(*) FROM plugin_channels pc WHERE pc.plugin_id=p.id) FROM plugins p ORDER BY p.created_at LIMIT ?`, adminListMax)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var pid, display, kind, created string
			var channels int
			if err := rows.Scan(&pid, &display, &kind, &created, &channels); err != nil {
				fail(w, 500, err.Error())
				return
			}
			cmds, online := live[pid]
			out = append(out, map[string]any{"id": pid, "display": display, "kind": kind,
				"created": created, "channels": channels, "online": online, "commands": cmds})
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("POST /api/admin/plugins", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ ID, Display, Kind string }
		json.NewDecoder(r.Body).Decode(&d)
		if d.ID == "" {
			fail(w, 400, "id is required")
			return
		}
		if d.Kind != "agent" {
			d.Kind = "bot"
		}
		if d.Display == "" {
			d.Display = d.ID
		}
		p, err := s.St.CreatePlugin(d.ID, d.Display, d.Kind)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.audit(actor, "register-plugin", p.ID, p.Kind)
		writeJSON(w, 200, p)
	}))

	// Revoking kills the token and the channel memberships; a connected plugin
	// keeps its socket until it reconnects, so drop it from the live map too.
	mux.HandleFunc("DELETE /api/admin/plugins/{id}", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		res, err := s.St.db.Exec(`DELETE FROM plugins WHERE id=?`, r.PathValue("id"))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(w, 404, "no such plugin")
			return
		}
		s.dropPlugin(r.PathValue("id"))
		s.audit(actor, "revoke-plugin", r.PathValue("id"), "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// ---- channels ----

	mux.HandleFunc("GET /api/admin/channels", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		// Names, modes and counts only. Message bodies are ciphertext the server
		// cannot read and must not surface even when they are not — an operator
		// moderating a channel is not a reason to hand them the log.
		rows, err := s.St.r.Query(`SELECT c.id,c.name,c.modes,c.is_dm,c.created_at,
		  (SELECT COUNT(*) FROM members m WHERE m.channel_id=c.id)
		  FROM channels c ORDER BY c.is_dm, c.name LIMIT ?`, adminListMax)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var cid, name, modes, created string
			var dm, members int
			if err := rows.Scan(&cid, &name, &modes, &dm, &created, &members); err != nil {
				fail(w, 500, err.Error())
				return
			}
			out = append(out, map[string]any{"id": cid, "name": name, "modes": modes,
				"is_dm": dm == 1, "created": created, "members": members})
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("POST /api/admin/channels", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Name, Topic string }
		json.NewDecoder(r.Body).Decode(&d)
		c, err := s.St.CreateChannel(d.Name, d.Topic, false)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		// No members and no op: it opens as an ordinary public channel anyone can
		// /join. Seeding the operator as op would make every admin-made channel
		// theirs, which is not what "set up #support for the team" means.
		s.audit(actor, "create-channel", "#"+c.Name, "")
		writeJSON(w, 200, map[string]any{"id": c.ID, "name": c.Name})
	}))

	// Archive is soft and reversible by hand: every member is removed and the
	// channel is set invite-only (+i) so nobody can /join their way back in.
	// History is left untouched — the events are ciphertext an operator cannot
	// read anyway, and destroying them would make "archive" a data-loss button
	// one confirm away. Un-archiving is `/mode -i` plus re-inviting.
	mux.HandleFunc("POST /api/admin/channels/{id}/archive", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		c, err := s.St.ChannelByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such channel")
			return
		}
		if _, err := s.St.db.Exec(`DELETE FROM members WHERE channel_id=?`, c.ID); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if !c.HasMode('i') {
			if err := s.St.SetModes(c.ID, c.Modes+"i"); err != nil {
				fail(w, 500, err.Error())
				return
			}
		}
		s.audit(actor, "archive", "#"+c.Name, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// Transfer hands a dead channel to a live owner: the nick is joined (if
	// needed) and made op.
	mux.HandleFunc("POST /api/admin/channels/{id}/transfer", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ Nick string }
		json.NewDecoder(r.Body).Decode(&d)
		c, err := s.St.ChannelByID(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such channel")
			return
		}
		u, err := s.St.UserByNick(d.Nick)
		if err != nil {
			fail(w, 404, "no such user: "+d.Nick)
			return
		}
		if err := s.St.Join(c.ID, u.ID, "op"); err != nil {
			fail(w, 500, err.Error())
			return
		}
		// Join is a no-op when they are already a member, so set the role too.
		if err := s.St.SetMemberRole(c.ID, u.ID, "op"); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.audit(actor, "transfer", "#"+c.Name, "op to "+u.Nick)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// ---- settings (the MOTD and the org icon it carries) ----

	mux.HandleFunc("GET /api/admin/settings", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		writeJSON(w, 200, map[string]any{
			"motd": s.St.Setting(settingMOTD),
			"icon": s.St.Setting(settingIcon),
		})
	}))

	mux.HandleFunc("POST /api/admin/settings", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		var d struct{ MOTD, Icon string }
		json.NewDecoder(r.Body).Decode(&d)
		if !validIcon(d.Icon) {
			fail(w, 400, "the icon must be an inline image under 96 KB")
			return
		}
		if err := s.St.SetSetting(settingMOTD, strings.TrimSpace(d.MOTD)); err != nil {
			fail(w, 500, err.Error())
			return
		}
		if err := s.St.SetSetting(settingIcon, d.Icon); err != nil {
			fail(w, 500, err.Error())
			return
		}
		s.audit(actor, "motd", "", firstLine(d.MOTD))
		s.BroadcastMOTD()
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// ---- stats ----

	mux.HandleFunc("GET /api/admin/audit", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		rows, err := s.St.AuditLog(200)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, rows)
	}))

	mux.HandleFunc("GET /api/admin/stats", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		s.mu.RLock()
		conns := 0
		for _, list := range s.conns {
			conns += len(list)
		}
		plugins := len(s.plugins)
		s.mu.RUnlock()
		db := s.St.dbBytes()
		uploads := s.St.metricCount(`SELECT COALESCE(SUM(received),0) FROM uploads`)
		writeJSON(w, 200, map[string]any{
			"server":         s.Name,
			"users":          s.St.CountUsers(),
			"channels":       s.St.metricCount(`SELECT COUNT(*) FROM channels`),
			"events":         s.St.metricCount(`SELECT COUNT(*) FROM events`),
			"db_bytes":       db,
			"upload_bytes":   uploads,
			"storage_bytes":  db + uploads,
			"uptime_seconds": int64(time.Since(startedAt).Seconds()),
			"connections":    conns,
			"plugins":        plugins,
		})
	}))

	// series is the dashboard's charts: one count per UTC day, zero-filled, for
	// the last N days. COUNT(*) only — see the privacy note at the top of this
	// file. A day with no activity is a real zero and has to be plotted as one,
	// which is why the day list is built here rather than left to the SQL.
	mux.HandleFunc("GET /api/admin/series", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		days := 30
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n >= 2 && n <= 365 {
			days = n
		}
		start := time.Now().UTC().AddDate(0, 0, -(days - 1))
		labels := make([]string, days)
		for i := range labels {
			labels[i] = start.AddDate(0, 0, i).Format("2006-01-02")
		}
		since := labels[0]
		// ponytail: GROUP BY substr() is a table scan. Fine for a self-host's
		// event log; if one ever outgrows it, add a generated day column + index.
		fill := func(q string) []int64 {
			by := s.St.dailyCounts(q, since)
			out := make([]int64, days)
			for i, d := range labels {
				out[i] = by[d]
			}
			return out
		}
		writeJSON(w, 200, map[string]any{
			"days":     labels,
			"signups":  fill(`SELECT substr(created_at,1,10) d,COUNT(*) FROM users WHERE created_at>=? GROUP BY d`),
			"messages": fill(`SELECT substr(ts,1,10) d,COUNT(*) FROM events WHERE ts>=? AND kind='message' GROUP BY d`),
			"files":    fill(`SELECT substr(ts,1,10) d,COUNT(*) FROM events WHERE ts>=? AND kind='file' GROUP BY d`),
			"actions":  fill(`SELECT substr(created_at,1,10) d,COUNT(*) FROM audit_log WHERE created_at>=? GROUP BY d`),
		})
	}))

	// ---- uploads ----

	mux.HandleFunc("GET /api/admin/uploads", admin(func(w http.ResponseWriter, r *http.Request, _ *User) {
		rows, err := s.St.r.Query(`SELECT u.id,u.owner,u.name,u.size,u.received,u.expires_at,u.complete,
		  COALESCE(c.name,u.channel_id) FROM uploads u
		  LEFT JOIN channels c ON c.id=u.channel_id ORDER BY u.expires_at LIMIT ?`, adminListMax)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var fid, owner, name, expires, channel string
			var size, received int64
			var complete int
			if err := rows.Scan(&fid, &owner, &name, &size, &received, &expires, &complete, &channel); err != nil {
				fail(w, 500, err.Error())
				return
			}
			out = append(out, map[string]any{"id": fid, "owner": owner, "name": name, "size": size,
				"received": received, "expires": expires, "complete": complete == 1, "channel": channel})
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("DELETE /api/admin/uploads/{id}", admin(func(w http.ResponseWriter, r *http.Request, actor *User) {
		up, err := s.St.Upload(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "no such upload")
			return
		}
		s.Files.remove(up.Path) // the row is the only pointer to the blob; drop both
		s.St.DeleteUpload(up.ID)
		s.audit(actor, "delete-upload", up.ID, up.Name)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
}

// dailyCounts runs a `SELECT day, COUNT(*) ... GROUP BY day` and returns it as a
// map. The query is a constant from this file — the only bound value is the
// start date. A failure is an empty map, not an error: a chart that cannot draw
// is a flat line, never a 500 on the whole dashboard.
func (s *Store) dailyCounts(q, since string) map[string]int64 {
	out := map[string]int64{}
	rows, err := s.r.Query(q, since)
	if err != nil {
		log.Printf("series query failed: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var n int64
		if rows.Scan(&day, &n) == nil {
			out[day] = n
		}
	}
	return out
}

// canLoseAdmin guards the two ways an operator can lock everyone out: demoting
// or deleting themselves, and removing the last admin account.
func (s *Server) canLoseAdmin(target, actor *User, verb string) error {
	if target.Role != "admin" {
		return nil
	}
	if target.ID == actor.ID && verb == "demote" {
		return fmt.Errorf("you cannot demote yourself — ask another admin")
	}
	if s.St.metricCount(`SELECT COUNT(*) FROM users WHERE role='admin'`) <= 1 {
		return fmt.Errorf("cannot %s the last admin — promote someone else first", verb)
	}
	return nil
}

// kick makes a suspension or deletion take effect on the live server. Login
// already refuses a suspended account and UserByToken rejects its tokens, so
// the durable half is dropping the sessions; closing the sockets shuts the
// window where an already-connected client keeps streaming.
//
// This is the live-disconnect hook: it lives here rather than in server.go so
// the admin console owns it end to end. Any other code path that suspends or
// removes an account (a CLI flag, a future moderation command) must call it.
// dropAll closes every live socket on this server, for a team the shard has
// suspended. Sessions stay: suspension can be lifted, and while it holds the
// shard refuses every request before it reaches a handler.
func (s *Server) dropAll() {
	s.mu.RLock()
	var list []*Conn
	for _, cs := range s.conns {
		list = append(list, cs...)
	}
	s.mu.RUnlock()
	for _, c := range list {
		if c.ws != nil {
			c.ws.CloseNow()
		}
	}
}

func (s *Server) kick(uid string) {
	s.St.db.Exec(`DELETE FROM sessions WHERE user_id=?`, uid)
	s.mu.RLock()
	list := append([]*Conn(nil), s.conns[uid]...)
	s.mu.RUnlock()
	for _, c := range list {
		if c.ws != nil {
			// CloseNow, not Close: a writer goroutine may be mid-Write, and this
			// only needs the read loop to error out so removeConn runs.
			c.ws.CloseNow()
		}
	}
}
