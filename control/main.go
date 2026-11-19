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

	_ "github.com/jackc/pgx/v5/stdlib"
)

var version = "dev"

func main() {
	var (
		addr   = flag.String("addr", ":8080", "listen address")
		dsn    = flag.String("dsn", os.Getenv("DATABASE_URL"), "postgres connection string")
		domain = flag.String("domain", "gliph.chat", "teams are created at <slug>.<domain>")
		// Secrets never come in as arguments, which any process listing shows:
		// the signing key from a file (or GLIPH_CONTROL_KEY), the admin secret
		// and the mail token from the environment.
		keyFile = flag.String("key-file", "", "file holding the base64 ed25519 private key for signing single sign-on assertions (or GLIPH_CONTROL_KEY)")
		genKey  = flag.Bool("genkey", false, "print a fresh signing keypair and exit")
		proxies = flag.String("trusted-proxy", "127.0.0.0/8,::1/128", "CIDRs whose X-Forwarded-For is believed for rate limits")
		from    = flag.String("mail-from", "Gliph <noreply@gliph.sh>", "From address for verification codes")
		move    = flag.String("move", "", "move a team to another shard and exit: -move <slug>:<shard>")
	)
	flag.Parse()
	trustedProxies = mustCIDRs(*proxies)

	if *genKey {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("control plane key (GLIPH_CONTROL_KEY or -key-file):  %s\n", base64.StdEncoding.EncodeToString(priv))
		fmt.Printf("shard -control-key:  %s\n", base64.StdEncoding.EncodeToString(pub))
		return
	}

	if *dsn == "" {
		log.Fatal("no -dsn and no DATABASE_URL")
	}
	st, err := OpenStore(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	admin := os.Getenv("GLIPH_ADMIN_SECRET")
	api := &API{St: st, Domain: *domain, AdminSecret: admin, Mail: pushleftMailer(os.Getenv("MAIL_TOKEN"), *from)}
	if *move != "" {
		slug, to, ok := strings.Cut(*move, ":")
		if !ok {
			log.Fatal("usage: -move <slug>:<shard>")
		}
		if err := api.Move(slug, to); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s now runs on %s\n", slug, to)
		return
	}
	key := os.Getenv("GLIPH_CONTROL_KEY")
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			log.Fatalf("-key-file: %v", err)
		}
		key = strings.TrimSpace(string(b))
	}
	if key != "" {
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			log.Fatalf("the signing key must be a base64 ed25519 private key (%d bytes) — see -genkey",
				ed25519.PrivateKeySize)
		}
		api.Priv = ed25519.PrivateKey(raw)
	} else {
		// Loud, because a control plane with no key runs perfectly and cannot
		// sign anybody in.
		log.Printf("warning: no signing key (-key-file or GLIPH_CONTROL_KEY); single sign-on will refuse every request")
	}
	if admin == "" {
		log.Printf("warning: no GLIPH_ADMIN_SECRET; the shard registry is closed")
	}

	go func() {
		for range time.Tick(time.Minute) {
			api.Reconcile()
		}
	}()

	// Every request here is a small JSON body: 64 KiB is generous, and the
	// timeouts stop a slow client from holding a connection open.
	hs := &http.Server{Addr: *addr, Handler: http.MaxBytesHandler(api.Routes(), 64<<10),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Printf("gliph control (%s) listening on %s, teams at *.%s", version, *addr, *domain)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	hs.Close()
}
