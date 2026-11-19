package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"time"
)

// Minting the assertion a shard will trade for a session.
//
// The shard verifies this with the public half and nothing else: no callback,
// no shared database, no network hop on the sign-in path. See server/sso.go for
// the other side, which is the one that has to be careful.

// assertionTTL is short on purpose. The assertion crosses one redirect and is
// spent immediately, so a minute is generous and a stolen one is worth almost
// nothing.
const assertionTTL = time.Minute

type claims struct {
	// Aud names the team. Without it an assertion for one team would be a
	// session on any team on the same shard.
	Aud   string `json:"aud"`
	Email string `json:"email"`
	Nick  string `json:"nick"`
	Exp   int64  `json:"exp"`
	// Jti makes the assertion single-use: the shard records it on first
	// redemption and refuses it after, so a copied one mints nothing.
	Jti string `json:"jti"`
	// Role is the team role here, "owner" or "member". The shard makes an
	// owner an admin: whoever created the team runs it there too.
	Role string `json:"role,omitempty"`
}

func mintAssertion(priv ed25519.PrivateKey, slug, email, nick, role, jti string, now time.Time) (string, error) {
	raw, err := json.Marshal(claims{
		Aud: slug, Email: email, Nick: nick, Exp: now.Add(assertionTTL).Unix(), Jti: jti, Role: role,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, raw)), nil
}
