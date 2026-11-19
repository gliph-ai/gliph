package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is stamped by the build (`-ldflags "-X main.version=$(bin/version --long)"`);
// a plain `go run` reports "dev".
var version = "dev"

func main() {
	var (
		addr     = flag.String("addr", "", "listen address (default :443 with TLS, :8787 with -plaintext)")
		dataDir  = flag.String("data", "./data", "data directory")
		webDir   = flag.String("web", "", "directory of built web client to serve (optional)")
		name     = flag.String("name", "gliph.local", "server name shown to clients")
		mkInvite = flag.Bool("invite", false, "print a new invite token and exit")
		mkPlugin = flag.String("plugin", "", "create a plugin token: -plugin id:display:kind")
		passwd   = flag.String("passwd", "", "reset a password and exit: -passwd nick:newpassword")
		// F14: one config value turns mobile push on for a self-hosted server
		// that has no APNs/FCM credentials of its own. Unset, mobile push is
		// simply unavailable and nothing else changes. See push.go for what the
		// relay does and does not learn.
		pushRelay = flag.String("push-relay", "", "hosted push relay endpoint for mobile wakes (optional)")
		// F5: direct APNs for iOS, with the team's own auth key. Beats the relay
		// for the ios platform when both are set. -fcm-credentials is its Android twin.
		apnsKey    = flag.String("apns-key", "", "APNs auth key (.p8) for direct iOS push (optional)")
		apnsKeyID  = flag.String("apns-key-id", "", "the key's id from the developer portal")
		apnsTeamID = flag.String("apns-team-id", "", "Apple developer team id")
		apnsTopic  = flag.String("apns-topic", "chat.gliph.mobile", "the iOS app's bundle id")
		// F5, Android: direct FCM with the Firebase project's service account.
		fcmCreds  = flag.String("fcm-credentials", "", "Firebase service-account JSON for direct Android push (optional)")
		downloads = flag.String("downloads", "", "directory of published desktop releases to serve (optional)")
		// Outbound email, off unless credentials are given. Only the missed-message
		// digest uses it, and it carries counts and channel names — never message
		// content, which the server does not have. See email.go.
		sesKeyID  = flag.String("ses-key-id", "", "Amazon SES access key id (optional; enables outbound email)")
		sesSecret = flag.String("ses-secret", "", "Amazon SES secret access key")
		sesRegion = flag.String("ses-region", "eu-west-1", "Amazon SES region")
		mailFrom  = flag.String("mail-from", "", "From address for outbound email; must be an identity verified in SES")
		// F7: without a TURN relay, the peer pairs behind symmetric NAT (a fifth
		// to a third of them) cannot connect at all. The secret is coturn's
		// static-auth-secret; the server mints a short-lived credential per
		// request rather than shipping a standing password to browsers.
		stunURL    = flag.String("stun", "", "STUN server(s) for p2p transfers, comma-separated")
		turnURL    = flag.String("turn", "", "TURN relay(s) for p2p transfers, comma-separated")
		turnSecret = flag.String("turn-secret", "", "coturn static-auth-secret used to mint short-lived TURN credentials")
		// A shard serves many teams, routed by Host: hosted only, on
		// PostgreSQL and the bucket. A self-hosted server is one team.
		shardMode   = flag.Bool("shard", false, "serve many teams, routed by Host (needs DATABASE_URL and S3_BUCKET)")
		shardSecret = flag.String("shard-secret", "", "bearer token for the /shard provisioning API")
		shardName   = flag.String("shard-name", "", "this shard's stable name in the team registry (required with -shard)")
		controlKey  = flag.String("control-key", "", "control plane ed25519 public key (base64) that may assert single sign-on")
		proxies     = flag.String("trusted-proxy", "127.0.0.0/8,::1/128", "CIDRs whose X-Forwarded-For is believed for rate limits (the reverse proxy in front)")
		importDB    = flag.String("import-sqlite", "", "copy this SQLite database into an empty PostgreSQL one at boot (one-time move)")
		// How it listens (listen.go). One of the three is required.
		tlsDomain = flag.String("tls-domain", "", "serve TLS 1.3 with a Let's Encrypt certificate for these hosts (comma-separated)")
		tlsCert   = flag.String("tls-cert", "", "serve TLS 1.3 with this certificate (PEM; needs -tls-key)")
		tlsKey    = flag.String("tls-key", "", "the -tls-cert private key (PEM)")
		httpAddr  = flag.String("http-addr", ":80", "plain-HTTP listener that redirects to HTTPS (\"\" for none)")
		plaintext = flag.Bool("plaintext", false, "serve plain HTTP on -addr: ONLY behind a proxy that terminates TLS")
		setup     = flag.Bool("setup", false, "walk TLS, server name and the admin account, print an invite link, and exit")
		backupTo  = flag.String("backup", "", "write an encrypted backup of this server to the file and exit (GLIPH_BACKUP_PASSPHRASE)")
		relayMode = flag.Bool("relay", false, "run as the hosted push relay for -push-relay servers (needs -apns-key and/or -fcm-credentials)")
		restore   = flag.String("restore", "", "restore an encrypted backup into an empty -data (or DATABASE_URL) and exit")
		// "Sign in with Gliph" on Substrate (docs/substrate.md): the one
		// non-loopback redirect the device-link approval will deliver a code to.
		subCallback = flag.String("substrate-callback", "", "Substrate's Sign in with Gliph callback URL (optional)")
	)
	flag.Parse()
	// What -setup wrote; the command line overrides it (setup.go).
	if err := loadConf(*dataDir); err != nil {
		log.Fatalf("%s: %v", confName, err)
	}
	lo := listenOpts{addr: *addr, httpAddr: *httpAddr, domains: *tlsDomain, cert: *tlsCert, key: *tlsKey,
		plaintext: *plaintext, dataDir: *dataDir}
	trustedProxies = mustCIDRs(*proxies)

	relay := NewRelaySender(*pushRelay)
	senders := byPlatform{"ios": relay, "android": relay}
	mailer, err := NewSESMailer(*sesKeyID, *sesSecret, *sesRegion, *mailFrom)
	if err != nil {
		log.Fatalf("email: %v", err)
	}
	if MailerConfigured(mailer) {
		log.Printf("email: sending through SES (%s) as %s", *sesRegion, *mailFrom)
	}
	if *turnURL != "" && *turnSecret == "" {
		log.Printf("warning: -turn is set with no -turn-secret; the relay will refuse every client")
	}
	if *pushRelay != "" {
		log.Printf("mobile push: waking devices through %s", *pushRelay)
	}

	// DATABASE_URL (PostgreSQL, as Pushleft injects it) or a SQLite file in
	// -data; the S3_* bucket, or files in -data.
	pg := os.Getenv("DATABASE_URL")
	bucket, _ := filesFromEnv(*dataDir).(*s3Files)
	if *shardMode && *shardName == "" {
		log.Fatal("-shard needs -shard-name: the registry is shared, and a shard loads only the teams naming it")
	}
	if *shardMode && (pg == "" || bucket == nil) {
		log.Fatal("-shard needs DATABASE_URL (PostgreSQL) and the S3 bucket: a team's data has to outlive this instance")
	}
	if bucket != nil {
		log.Printf("files: uploads and voice notes in bucket %s", bucket.bucket)
	}
	if *backupTo != "" || *restore != "" {
		if err := backupOrRestore(*backupTo, *restore, *dataDir, pg, bucket); err != nil {
			log.Fatal(err)
		}
		return
	}

	shard := NewShard(ShardOpts{
		WebDir:      *webDir,
		DownloadDir: *downloads,
		Version:     version,
		Started:     time.Now(),
		Senders:     senders,
		Mail:        mailer,
		ICE:         ICEConfig{STUN: *stunURL, TURN: *turnURL, Secret: *turnSecret},
		Secret:      *shardSecret,
		DataDir:     *dataDir,
		PG:          pg,
		Bucket:      bucket,
		Name:        *shardName,
		ControlKey:  mustControlKey(*controlKey),
	})
	defer shard.Close()

	if *apnsKey != "" {
		// One APNs connection for the whole process: the auth key belongs to
		// the app, not to a team. A token Apple rejects lives in exactly one
		// team's database, so the drop is offered to all of them.
		apns, err := NewAPNSSender(func(token string) {
			for _, t := range shard.Tenants() {
				t.St.DeletePushToken(token)
			}
		}, *apnsKey, *apnsKeyID, *apnsTeamID, *apnsTopic)
		if err != nil {
			log.Fatalf("apns: %v", err)
		}
		senders["ios"] = apns
		log.Printf("apns: direct iOS push on, topic %s", *apnsTopic)
	}
	if *fcmCreds != "" {
		fcm, err := NewFCMSender(func(token string) {
			for _, t := range shard.Tenants() {
				t.St.DeletePushToken(token)
			}
		}, *fcmCreds)
		if err != nil {
			log.Fatalf("fcm: %v", err)
		}
		senders["android"] = fcm
		log.Printf("fcm: direct Android push on, project in %s", *fcmCreds)
	}
	if *relayMode {
		if *apnsKey == "" && *fcmCreds == "" {
			log.Fatal("-relay needs -apns-key and/or -fcm-credentials: it has nothing to forward to")
		}
		if !lo.configured() {
			log.Fatal("no listener: pass -tls-domain, -tls-cert/-tls-key, or -plaintext behind a TLS proxy")
		}
		waitAndClose(runRelay(senders, lo))
		return
	}

	// Two shapes, one binary. Without -shard this is a single team at -data
	// answering whatever Host arrives: a self-hosted server, and every
	// deployment that existed before shards. With it, this process serves the
	// teams under <data>/tenants and routes by Host.
	var st *Store
	if *shardMode {
		if err := shard.Load(); err != nil {
			log.Fatal(err)
		}
		if *shardSecret == "" {
			log.Printf("warning: -shard with no -shard-secret; this shard accepts no provisioning")
		}
		if ts := shard.Tenants(); len(ts) > 0 {
			st = ts[0].St // what the one-off CLI flags below operate on
		}
	} else {
		if *importDB != "" {
			// Before the solo team opens and listens (see importSQLite).
			if err := importIntoPG(pg, bucket, *importDB); err != nil {
				log.Fatal(err)
			}
		}
		t, err := shard.Open("", *name, nil)
		if err != nil {
			log.Fatal(err)
		}
		shard.Solo(t)
		st = t.St
		t.Srv.SubstrateCallback = *subCallback
		if *setup && pg != "" {
			log.Fatal("-setup is for a self-hosted server on SQLite")
		}
		if pg == "" && (*setup || firstRun(*dataDir, st, lo)) {
			res, err := runSetup(os.Stdin, os.Stdout, stdinIsTerminal(), *dataDir, t, lo)
			if err != nil {
				log.Fatal(err)
			}
			if *setup {
				return // a service manager starts it with the config just written
			}
			lo = res.lo
		}
	}

	if (*mkInvite || *passwd != "" || *mkPlugin != "") && st == nil {
		log.Fatal("this shard has no teams yet; provision one through /shard/tenants first")
	}
	if *mkInvite {
		t, err := st.CreateInvite("member")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(t)
		return
	}
	if *passwd != "" {
		nick, pass, found := strings.Cut(*passwd, ":")
		if !found || nick == "" || pass == "" {
			log.Fatal("usage: -passwd nick:newpassword")
		}
		if err := st.SetPassword(nick, pass); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("password updated for %s\n", nick)
		return
	}
	if *mkPlugin != "" {
		var pid, disp, kind string
		parts := splitN(*mkPlugin, ':', 3)
		pid = parts[0]
		disp, kind = parts[1], parts[2]
		if disp == "" {
			disp = pid
		}
		if kind == "" {
			kind = "bot"
		}
		p, err := st.CreatePlugin(pid, disp, kind)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("plugin %s (%s)\ntoken: %s\n", p.ID, p.Kind, p.Token)
		return
	}

	go shard.Housekeeping()

	if !lo.configured() {
		log.Fatal("no listener: run gliph -setup, or pass -tls-domain <host> (Let's Encrypt), -tls-cert/-tls-key, " +
			"or -plaintext behind a proxy that terminates TLS")
	}
	if *shardMode {
		log.Printf("gliph shard (%s): %d teams", version, len(shard.Tenants()))
	} else {
		log.Printf("gliph %s (%s)", *name, version)
	}
	if st != nil && st.CountUsers() == 0 {
		log.Printf("no accounts yet — the first account created becomes admin")
	}
	waitAndClose(listen(shard, lo))
}

func waitAndClose(servers []*http.Server) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	for _, hs := range servers {
		hs.Close()
	}
}

func splitN(s string, sep rune, n int) []string {
	out := make([]string, n)
	i := 0
	for _, r := range s {
		if r == sep && i < n-1 {
			i++
			continue
		}
		out[i] += string(r)
	}
	return out
}

// mustControlKey decodes the control plane's public key, or dies. A key that is
// present but unreadable must not start a server that silently refuses every
// sign-on instead.
func mustControlKey(b64 string) ed25519.PublicKey {
	if b64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		log.Fatalf("-control-key must be a base64 ed25519 public key (%d bytes)", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw)
}
