package main

// The first-run setup wizard (spec F14): TLS, server name, admin account, then
// an invite link. It runs on its own when a fresh self-hosted server starts on
// a terminal with no listener flags, and again on `gliph -setup`.
//
// What it decides it writes to <data>/gliph.conf, one flag per line, so the
// next start is plain `gliph -data <dir>`. Flags on the command line still win
// over the file.

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

const confName = "gliph.conf"

// loadConf re-parses the flags with the conf file's lines first, so anything
// given on the command line overrides them.
func loadConf(dataDir string) error {
	raw, err := os.ReadFile(filepath.Join(dataDir, confName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var args []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			args = append(args, l)
		}
	}
	return flag.CommandLine.Parse(append(args, os.Args[1:]...))
}

// firstRun is when the wizard starts by itself: a self-hosted server with no
// config, no accounts, no listener chosen, and a person at the terminal.
func firstRun(dataDir string, st *Store, lo listenOpts) bool {
	if lo.configured() || st.CountUsers() > 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dataDir, confName)); err == nil {
		return false
	}
	return stdinIsTerminal()
}

// A service manager hands the process /dev/null, which is a character device
// too; only a real terminal gets the wizard.
func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

type wizard struct {
	in  *bufio.Reader
	out io.Writer
	tty bool // hide the password as it is typed
}

func (w *wizard) ask(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(w.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(w.out, "%s: ", q)
	}
	l, err := w.in.ReadString('\n')
	if err != nil && (err != io.EOF || l == "") {
		return "", errors.New("setup: input ended")
	}
	if l = strings.TrimSpace(l); l == "" {
		return def, nil
	}
	return l, nil
}

func (w *wizard) secret(q string) (string, error) {
	if !w.tty {
		return w.ask(q, "")
	}
	fmt.Fprintf(w.out, "%s: ", q)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(w.out)
	return strings.TrimSpace(string(b)), err
}

// setupResult is what main needs to carry on and listen.
type setupResult struct {
	lo     listenOpts
	name   string
	invite string // the link to hand to the first person after the admin
}

// runSetup walks the three steps against the solo team t and writes the
// config. The admin account goes through Register, so it is admin by the same
// rule as any first account on an empty server.
func runSetup(in io.Reader, out io.Writer, tty bool, dataDir string, t *Tenant, lo listenOpts) (*setupResult, error) {
	w := &wizard{in: bufio.NewReader(in), out: out, tty: tty}
	fmt.Fprintln(out, "gliph setup: TLS, server name, admin account. Enter keeps the [default].")

	// 1. TLS
	domain := ""
	for domain == "" {
		var err error
		if domain, err = w.ask("\n1/3 TLS\nDomain people will reach this server at (e.g. chat.example.com)", ""); err != nil {
			return nil, err
		}
		domain = hostKey(domain)
	}
	conf := []string{}
	lo.domains, lo.cert, lo.key, lo.plaintext = "", "", "", false
	mode, err := w.ask("Certificate: 1 Let's Encrypt (ports 80 and 443 reach this host), 2 my own files, 3 a proxy in front terminates TLS", "1")
	if err != nil {
		return nil, err
	}
	public := "https://" + domain
	switch mode {
	case "2":
		if lo.cert, err = w.ask("Certificate file (PEM)", ""); err != nil {
			return nil, err
		}
		if lo.key, err = w.ask("Private key file (PEM)", ""); err != nil {
			return nil, err
		}
		if _, err := tls.LoadX509KeyPair(lo.cert, lo.key); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		conf = append(conf, "-tls-cert="+lo.cert, "-tls-key="+lo.key)
	case "3":
		if lo.addr, err = w.ask("Address the proxy forwards to", or(lo.addr, "127.0.0.1:8787")); err != nil {
			return nil, err
		}
		lo.plaintext = true
		conf = append(conf, "-plaintext", "-addr="+lo.addr)
	default:
		lo.domains = domain
		conf = append(conf, "-tls-domain="+domain)
	}

	// 2. name
	name, err := w.ask("\n2/3 Server name shown to clients", domain)
	if err != nil {
		return nil, err
	}
	conf = append(conf, "-name="+name)
	t.Name, t.Srv.Name = name, name

	// 3. admin
	if t.St.CountUsers() > 0 {
		fmt.Fprintln(out, "\n3/3 This server already has accounts; no new admin.")
	} else {
		fmt.Fprintln(out, "\n3/3 Admin account")
		nick, err := w.ask("Nick", "admin")
		if err != nil {
			return nil, err
		}
		email, err := w.ask("Email", "")
		if err != nil {
			return nil, err
		}
		pass, err := w.secret("Password (8 or more characters; Enter makes one up)")
		if err != nil {
			return nil, err
		}
		made := pass == ""
		if made {
			b := make([]byte, 15)
			rand.Read(b)
			pass = strings.ToLower(base32.StdEncoding.EncodeToString(b))
		}
		u, err := t.St.Register(nick, email, pass, "")
		if err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		if ch, err := t.St.ChannelByName("general"); err == nil {
			t.St.Join(ch.ID, u.ID, "member")
			t.Srv.Post(ch, &Event{Kind: "join", Actor: u.Nick, Body: "joined the server", Meta: map[string]any{"new": true}}, "")
		}
		if made {
			fmt.Fprintf(out, "Password for %s: %s\n", u.Nick, pass)
		}
		fmt.Fprintf(out, "%s is the admin.\n", u.Nick)
	}

	body := "# written by gliph -setup; flags on the command line override these\n" + strings.Join(conf, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dataDir, confName), []byte(body), 0o600); err != nil {
		return nil, err
	}
	inv, err := t.St.CreateInvite("member")
	if err != nil {
		return nil, err
	}
	link := public + "/?invite=" + inv
	fmt.Fprintf(out, "\nDone. Config in %s; next time run: gliph -data %s\n", filepath.Join(dataDir, confName), dataDir)
	fmt.Fprintf(out, "Invite the first person with:\n\n  %s\n\nMore invites: the admin console at %s/admin.\n", link, public)
	return &setupResult{lo: lo, name: name, invite: link}, nil
}
