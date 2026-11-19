package main

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type API struct {
	St *Store
	// Priv signs single sign-on assertions. Its public half goes to every
	// shard as -control-key.
	Priv ed25519.PrivateKey
	// Domain is what a team's address is built under: acme + gliph.chat.
	Domain string
	// AdminSecret guards the shard registry. Operations, not customers.
	AdminSecret string
	// Mail sends the address-verification code.
	Mail mailer

	limitOnce sync.Once
	lim       *limiter
}

// tooMany counts a hit on every key and answers 429 when any is over its
// budget. Each rule is {key, n, per}.
func (a *API) tooMany(w http.ResponseWriter, rules ...rule) bool {
	a.limitOnce.Do(func() { a.lim = newLimiter() })
	for _, r := range rules {
		if !a.lim.allow(r.key, r.n, r.per) {
			fail(w, 429, "too many attempts; try again later")
			return true
		}
	}
	return false
}

// maxTeamsPerAccount is how many teams one account may start on shared shards.
const maxTeamsPerAccount = 3

type rule struct {
	key string
	n   int
	per time.Duration
}

// pushStatus tells the team's shard its status and marks it synced.
func (a *API) pushStatus(slug, status string, version int64) error {
	sh, err := a.St.ShardFor(slug)
	if err == nil {
		err = setShardStatus(sh, slug, status == StatusSuspended, version)
	}
	if err == nil {
		a.St.MarkSynced(slug, version)
	}
	return err
}

// Reconcile pushes every status a shard has not confirmed. One pass; main runs
// it on a timer.
func (a *API) Reconcile() {
	pending, err := a.St.Unsynced()
	if err != nil {
		return
	}
	for slug, p := range pending {
		if err := a.pushStatus(slug, p.Status, p.Version); err != nil {
			log.Printf("status for %s still not on its shard: %v", slug, err)
		}
	}
}

// sendCode mails a fresh verification code to the account's address.
func (a *API) sendCode(acc *Account) error {
	code, err := a.St.NewVerifyCode(acc.ID)
	if err != nil {
		return err
	}
	return a.Mail(acc.Email, "Your Gliph code",
		"Your code is "+code+"\n\nEnter it in Gliph to confirm this is your address. It works for a day.\n")
}

// verified is an account that has proved its address. Everything that trusts
// the email (a team, a membership, an assertion) asks for one.
func (a *API) verified(w http.ResponseWriter, r *http.Request) *Account {
	acc, err := a.account(r)
	if err != nil {
		fail(w, 401, "unauthorized")
		return nil
	}
	if !acc.Verified {
		fail(w, 403, "confirm your email address first")
		return nil
	}
	return acc
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (a *API) account(r *http.Request) (*Account, error) { return a.St.AccountByToken(bearer(r)) }

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// ---- the account ----

	// One answer whether or not the address already has an account, so signup
	// is no way to find out who uses Gliph. The mail is what differs: a new
	// account gets its code, an existing one is told to sign in. Either way
	// the next step is POST /api/login, then /api/verify with the code.
	mux.HandleFunc("POST /api/signup", func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Email, Password string }
		json.NewDecoder(r.Body).Decode(&d)
		if a.tooMany(w, rule{"signup:" + clientIP(r), 10, time.Hour}) {
			return
		}
		// An unconfirmed account with this address is taken over by Signup;
		// ErrTaken means a confirmed one.
		acc, err := a.St.Signup(d.Email, d.Password)
		switch {
		case errors.Is(err, ErrTaken):
			err = a.Mail(normaliseEmail(d.Email), "Your Gliph account",
				"Someone, probably you, tried to make a Gliph account with this address.\n\n"+
					"There already is one. Sign in with it. If that was not you, nothing has changed.\n")
		case err != nil:
			fail(w, 400, err.Error()) // a bad address or a short password, not who exists
			return
		default:
			err = a.sendCode(acc)
		}
		if err != nil {
			fail(w, 502, "the mail did not go: "+err.Error())
			return
		}
		writeJSON(w, 202, map[string]any{"ok": true, "next": "check your mail, then sign in"})
	})

	// The code from the mail proves the address.
	mux.HandleFunc("POST /api/verify", func(w http.ResponseWriter, r *http.Request) {
		acc, err := a.account(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		var d struct{ Code string }
		json.NewDecoder(r.Body).Decode(&d)
		if a.tooMany(w, rule{"verify:" + acc.ID, 10, 10 * time.Minute}) {
			return
		}
		if err := a.St.Verify(acc.ID, d.Code); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/verify/resend", func(w http.ResponseWriter, r *http.Request) {
		acc, err := a.account(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		// A minute between mails, and a day's ceiling.
		if a.tooMany(w, rule{"resend:" + acc.ID, 1, time.Minute}, rule{"resend-day:" + acc.ID, 10, 24 * time.Hour}) {
			return
		}
		if err := a.sendCode(acc); err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Email, Password string }
		json.NewDecoder(r.Body).Decode(&d)
		if a.tooMany(w, rule{"login-ip:" + clientIP(r), 30, 10 * time.Minute},
			rule{"login:" + normaliseEmail(d.Email), 10, 10 * time.Minute}) {
			return
		}
		tok, acc, err := a.St.Login(d.Email, d.Password)
		if err != nil {
			fail(w, 401, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"token": tok, "account": acc})
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		a.St.Logout(bearer(r))
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// ---- the directory ----

	// What a fresh device asks for: every team this account is in, with the
	// address to reach it. This is what makes "one account, every server" more
	// than a slogan — the client no longer needs anyone to retype an address.
	mux.HandleFunc("GET /api/servers", func(w http.ResponseWriter, r *http.Request) {
		acc, err := a.account(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		servers, err := a.St.ServersFor(acc.ID)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"servers": servers})
	})

	mux.HandleFunc("POST /api/servers", func(w http.ResponseWriter, r *http.Request) {
		acc := a.verified(w, r)
		if acc == nil {
			return
		}
		var d struct{ Slug, Name, Nick string }
		json.NewDecoder(r.Body).Decode(&d)
		if !validSlug(d.Slug) {
			fail(w, 400, "the address must be 1-40 characters of a-z, 0-9 and -")
			return
		}
		// A customer never picks a pool: a dedicated pool is somebody's paid-for
		// capacity, and letting a request name it would let anyone spend it.
		// Operations place a dedicated team; this route always uses shared.
		const pool = "shared"
		host := d.Slug + "." + a.Domain
		name := d.Name
		if name == "" {
			name = d.Slug
		}
		// The row lands first. A crash between here and the shard leaves a team
		// that exists and is not yet provisioned; the same caller retrying
		// finishes it (StartedBy), and provisioning is idempotent on the
		// shard. The other order leaves a live team nothing knows about.
		var sh *Shard
		v, err := a.St.StartedBy(d.Slug, acc.ID)
		if err == nil {
			sh, err = a.St.ShardFor(v.Slug)
		} else {
			// Shared capacity is everyone's: an account starts at most
			// maxTeamsPerAccount teams (counted under the placement lock),
			// and not in a burst.
			// ponytail: a flat ceiling until billing sets real entitlements.
			if a.tooMany(w, rule{"create:" + acc.ID, 5, 24 * time.Hour}) {
				return
			}
			v, sh, err = a.St.PlaceAndCreate(pool, d.Slug, name, host, acc.ID, maxTeamsPerAccount)
			if errors.Is(err, ErrTooManyTeams) {
				fail(w, 403, fmt.Sprintf("an account can start %d teams", maxTeamsPerAccount))
				return
			}
			if err != nil && !errors.Is(err, ErrTaken) {
				// Out of room is an operations problem, not the customer's.
				// Say so plainly rather than pretending the name was bad.
				fail(w, 503, err.Error())
				return
			}
		}
		if err != nil {
			if errors.Is(err, ErrTaken) {
				fail(w, 409, "that address is taken")
				return
			}
			fail(w, 500, err.Error())
			return
		}
		// The shard's owner invite stays with the shard (an operations-only way
		// back in, behind its secret). The owner comes in the one way every
		// member of a hosted team does: single sign-on, where the "owner" role
		// makes them the team's admin. A password account made from the invite
		// would be one single sign-on then refuses to touch.
		_, err = provision(sh, v.Slug, v.Name, v.Host)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		if err := a.St.AddMember(acc.ID, v.Slug, d.Nick, "owner"); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"server": v})
	})

	// Adding somebody to a team, which is how a hosted team grows: the owner
	// names an account, and that account sees the team in its own list from
	// then on. Their user row on the shard is created the first time they
	// actually sign in, not here.
	mux.HandleFunc("POST /api/servers/{slug}/members", func(w http.ResponseWriter, r *http.Request) {
		acc, err := a.account(r)
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		slug := r.PathValue("slug")
		mine, err := a.St.Membership(acc.ID, slug)
		if err != nil || mine.Role != "owner" {
			// Same answer for "not yours" and "not an owner": otherwise this
			// reports which teams exist.
			fail(w, 403, "only an owner can add people to this team")
			return
		}
		var d struct{ Email, Nick string }
		json.NewDecoder(r.Body).Decode(&d)
		target, err := a.St.AccountByEmail(d.Email)
		if err != nil || !target.Verified {
			// Unverified is "no account": whoever signed up with that address
			// has not shown it is theirs, and this grants the team to it.
			fail(w, 404, "no confirmed gliph account with that email")
			return
		}
		if err := a.St.AddMember(target.ID, slug, d.Nick, "member"); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// ---- the way in ----

	// Trade an account session for a short-lived assertion the team's own
	// server will accept. The membership row is the authority: no row, no
	// assertion, whatever the caller claims.
	mux.HandleFunc("POST /api/servers/{slug}/sso", func(w http.ResponseWriter, r *http.Request) {
		acc := a.verified(w, r)
		if acc == nil {
			return
		}
		v, err := a.St.Membership(acc.ID, r.PathValue("slug"))
		if err != nil {
			fail(w, 403, "you are not in that team")
			return
		}
		// Billing lives here and only here. A suspended team is not reachable;
		// past_due still lets people in, because locking a team out of its own
		// history over a failed card is a thing to threaten, not to do.
		if v.Status == StatusSuspended {
			fail(w, 402, "this team is suspended — sort the subscription out to get back in")
			return
		}
		if len(a.Priv) == 0 {
			fail(w, 500, "this control plane has no signing key")
			return
		}
		assertion, err := mintAssertion(a.Priv, v.Slug, acc.Email, v.Nick, v.Role, token(), time.Now())
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"assertion": assertion, "host": "https://" + v.Host, "status": v.Status,
		})
	})

	// ---- operations ----

	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := bearer(r)
			if a.AdminSecret == "" || got == "" ||
				subtle.ConstantTimeCompare([]byte(got), []byte(a.AdminSecret)) != 1 {
				fail(w, 403, "admin only")
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/shards", admin(func(w http.ResponseWriter, r *http.Request) {
		shards, err := a.St.Shards()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"shards": shards})
	}))

	mux.HandleFunc("POST /api/shards", admin(func(w http.ResponseWriter, r *http.Request) {
		// Its own shape rather than Shard: the secret is `json:"-"` there so it
		// can never appear in a listing, which also means it never decodes.
		var d struct {
			Name      string `json:"name"`
			Pool      string `json:"pool"`
			BaseURL   string `json:"base_url"`
			Secret    string `json:"secret"`
			Capacity  int    `json:"capacity"`
			Accepting bool   `json:"accepting"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		sh := Shard{Name: d.Name, Pool: d.Pool, BaseURL: d.BaseURL, Secret: d.Secret,
			Capacity: d.Capacity, Accepting: d.Accepting}
		if sh.Name == "" || sh.BaseURL == "" || sh.Secret == "" {
			fail(w, 400, "a shard needs a name, a base_url and a secret")
			return
		}
		if sh.Pool == "" {
			sh.Pool = "shared"
		}
		if sh.Capacity <= 0 {
			sh.Capacity = 50
		}
		if err := a.St.AddShard(sh); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// Whatever ends up deciding it — a provider's webhook, an invoice run, a
	// person — writes the answer here, and nothing else in Gliph learns how it
	// was decided.
	mux.HandleFunc("POST /api/servers/{slug}/status", admin(func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Status, BillingRef string }
		json.NewDecoder(r.Body).Decode(&d)
		switch d.Status {
		case StatusActive, StatusPastDue, StatusSuspended:
		default:
			fail(w, 400, fmt.Sprintf("status must be %s, %s or %s",
				StatusActive, StatusPastDue, StatusSuspended))
			return
		}
		slug := r.PathValue("slug")
		version, err := a.St.SetStatus(slug, d.Status, d.BillingRef)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		// Refusing new assertions is not enough: a session minted before the
		// suspension would keep working. The shard is told, and refuses every
		// request for the team while it lasts. A failed push is not lost: the
		// row stays unsynced and Reconcile keeps trying.
		if err := a.pushStatus(slug, d.Status, version); err != nil {
			writeJSON(w, 202, map[string]any{"ok": true, "pending": "the shard was not reached; retrying"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// Drain, move, repoint, resume (move.go).
	mux.HandleFunc("POST /api/servers/{slug}/move", admin(func(w http.ResponseWriter, r *http.Request) {
		var d struct{ Shard string }
		json.NewDecoder(r.Body).Decode(&d)
		if err := a.Move(r.PathValue("slug"), d.Shard); err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"slug": r.PathValue("slug"), "shard": d.Shard})
	}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	return mux
}

// validSlug is deliberately identical to the shard's: an address the control
// plane accepts and the shard refuses is a team that half exists.
// validSlug is a DNS label: a-z, 0-9 and "-", 1-40 long, never starting or
// ending with "-" (-.gliph.chat is not a hostname).
func validSlug(s string) bool {
	if s == "" || len(s) > 40 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
