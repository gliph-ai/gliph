package main

// Encrypted backup and restore (spec F14).
//
//	gliph -data <dir> -backup gliph.backup      # GLIPH_BACKUP_PASSPHRASE, or asked
//	gliph -data <dir> -restore gliph.backup     # onto a fresh host
//
// The archive is the whole server: the database (accounts, sessions, devices,
// channels, ciphertext history, keys' public halves), uploads, voice notes,
// gliph.conf and the certificate cache. Sessions are in it, so a restored
// server at the same address takes every client back without a new login.
//
// Inside it is a tar.gz with the data dir's own layout: gliph.db, uploads/<id>,
// roomnotes/<id>. A server on PostgreSQL (gliph.sh) backs up to the same
// layout (its tables copied into a SQLite gliph.db, its bucket objects as
// files) and restores through the -import-sqlite path, so either kind of
// server restores onto either kind.
//
// Encryption: scrypt turns the passphrase into an AES-256 key; the tar.gz is
// sealed in 64 KiB AES-GCM chunks whose nonce is the chunk number plus a
// last-chunk flag, so a reordered, cut or edited archive fails to open rather
// than restoring part of a server.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/term"
)

const (
	backupMagic = "GLIPHBK1"
	backupChunk = 64 << 10
)

var errBadBackup = errors.New("backup: wrong passphrase, or the archive is damaged")

// backupPassphrase is GLIPH_BACKUP_PASSPHRASE, or typed at the terminal
// (twice when making a backup: a typo there is a backup nobody can open).
func backupPassphrase(confirm bool) (string, error) {
	if p := os.Getenv("GLIPH_BACKUP_PASSPHRASE"); p != "" {
		return p, nil
	}
	if !stdinIsTerminal() {
		return "", errors.New("backup: set GLIPH_BACKUP_PASSPHRASE")
	}
	read := func(q string) string {
		fmt.Fprint(os.Stderr, q)
		b, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b)
	}
	p := read("Backup passphrase: ")
	if len(p) < 8 {
		return "", errors.New("backup: the passphrase needs 8 or more characters")
	}
	if confirm && read("Again: ") != p {
		return "", errors.New("backup: the passphrases differ")
	}
	return p, nil
}

func backupKey(pass string, salt []byte) (cipher.AEAD, error) {
	k, err := scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func chunkNonce(n uint64, last bool) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[3:11], n)
	if last {
		nonce[11] = 1
	}
	return nonce
}

// sealer is the encrypting writer. Close seals the last chunk.
type sealer struct {
	w    io.Writer
	aead cipher.AEAD
	hdr  []byte
	buf  []byte
	n    uint64
}

func newSealer(w io.Writer, pass string) (*sealer, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	aead, err := backupKey(pass, salt)
	if err != nil {
		return nil, err
	}
	hdr := append([]byte(backupMagic), salt...)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &sealer{w: w, aead: aead, hdr: hdr}, nil
}

func (s *sealer) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		k := min(backupChunk-len(s.buf), len(p))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
		// A full chunk is sealed only once more data arrives, so the final
		// chunk is always the one Close seals with the last flag.
		if len(s.buf) == backupChunk && len(p) > 0 {
			if err := s.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (s *sealer) flush(last bool) error {
	ct := s.aead.Seal(nil, chunkNonce(s.n, last), s.buf, s.hdr)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := s.w.Write(append(l[:], ct...)); err != nil {
		return err
	}
	s.n++
	s.buf = s.buf[:0]
	return nil
}

func (s *sealer) Close() error { return s.flush(true) }

// opener is the decrypting reader. It refuses to report EOF until it has
// opened the chunk marked last.
type opener struct {
	r    *bufio.Reader
	aead cipher.AEAD
	hdr  []byte
	buf  []byte
	n    uint64
	done bool
}

func newOpener(r io.Reader, pass string) (*opener, error) {
	br := bufio.NewReader(r)
	hdr := make([]byte, len(backupMagic)+16)
	if _, err := io.ReadFull(br, hdr); err != nil || string(hdr[:len(backupMagic)]) != backupMagic {
		return nil, errors.New("backup: not a gliph backup")
	}
	aead, err := backupKey(pass, hdr[len(backupMagic):])
	if err != nil {
		return nil, err
	}
	return &opener{r: br, aead: aead, hdr: hdr}, nil
}

func (o *opener) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(o.r, l[:]); err != nil {
			return 0, errBadBackup // cut short before the last chunk
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > backupChunk+uint32(o.aead.Overhead()) {
			return 0, errBadBackup
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(o.r, ct); err != nil {
			return 0, errBadBackup
		}
		pt, err := o.aead.Open(nil, chunkNonce(o.n, false), ct, o.hdr)
		if err != nil {
			if pt, err = o.aead.Open(nil, chunkNonce(o.n, true), ct, o.hdr); err != nil {
				return 0, errBadBackup
			}
			o.done = true
			if _, err := o.r.ReadByte(); err != io.EOF {
				return 0, errBadBackup // bytes after the last chunk
			}
		}
		o.n++
		o.buf = pt
	}
	k := copy(p, o.buf)
	o.buf = o.buf[k:]
	return k, nil
}

// ---- backup ----

// backupServer writes the encrypted archive of the solo team: the database at
// dsn (a SQLite file, or postgres://), its files from bucket or from dataDir.
func backupServer(w io.Writer, pass, dataDir, pg string, bucket *s3Files) error {
	tmp, err := os.MkdirTemp("", "gliph-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, "gliph.db")
	if pg == "" {
		// VACUUM INTO is a consistent snapshot of a live database, taken
		// beside a running server without stopping it.
		db, err := sql.Open("sqlite", filepath.Join(dataDir, "gliph.db")+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return err
		}
		_, err = db.Exec(`VACUUM INTO ?`, snap)
		db.Close()
		if err != nil {
			return fmt.Errorf("backup: snapshot: %w", err)
		}
	} else if err := exportPG(pg, snap); err != nil {
		return err
	}

	// An upload row from before relative keys holds an absolute path. The
	// archive carries it as uploads/<name> and the row is rewritten to match,
	// so it restores into any data dir.
	abs := map[string]string{} // archive name -> disk path
	if lite, err := OpenStore(snap); err == nil {
		rows, err := lite.db.Query(`SELECT id,path FROM uploads WHERE path LIKE '/%'`)
		if err == nil {
			var fix [][2]string
			for rows.Next() {
				var id, p string
				rows.Scan(&id, &p)
				fix = append(fix, [2]string{id, p})
			}
			rows.Close()
			for _, f := range fix {
				key := "uploads/" + filepath.Base(f[1])
				abs[key] = f[1]
				lite.db.Exec(`UPDATE uploads SET path=? WHERE id=?`, key, f[0])
			}
		}
		lite.Close()
	}

	s, err := newSealer(w, pass)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(s)
	tw := tar.NewWriter(gz)
	add := func(name, disk string) error {
		f, err := os.Open(disk)
		if os.IsNotExist(err) {
			return nil // its row 404s on the restored server, as it does here
		}
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	}
	if err := add("gliph.db", snap); err != nil {
		return err
	}
	for name, disk := range abs {
		if err := add(name, disk); err != nil {
			return err
		}
	}
	for _, f := range []string{confName} {
		if err := add(f, filepath.Join(dataDir, f)); err != nil {
			return err
		}
	}
	dirs := []string{"acme"}
	if bucket == nil {
		dirs = append(dirs, "uploads", "roomnotes")
	} else if err := backupBucket(tw, bucket); err != nil {
		return err
	}
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(dataDir, d), func(p string, e fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dataDir, p)
			return add(filepath.ToSlash(rel), p)
		})
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return s.Close()
}

// exportPG copies a PostgreSQL server's tables into a fresh SQLite file.
func exportPG(pg, to string) error {
	from, err := OpenStore(pg)
	if err != nil {
		return err
	}
	defer from.Close()
	lite, err := OpenStore(to)
	if err != nil {
		return err
	}
	defer lite.Close()
	return copyTables(from, lite)
}

// backupBucket writes every upload and voice note in the bucket as one file,
// its parts concatenated in order (see s3Files).
func backupBucket(tw *tar.Writer, b *s3Files) error {
	for _, prefix := range []string{"uploads/", "roomnotes/"} {
		objs, err := b.list(prefix)
		if err != nil {
			return err
		}
		var keys []string
		size := map[string]int64{}
		parts := map[string][]string{}
		for _, o := range objs {
			key := path.Dir(o.Key) // <key>/<offset>
			if _, seen := size[key]; !seen {
				keys = append(keys, key)
			}
			size[key] += o.Size
			parts[key] = append(parts[key], o.Key)
		}
		for _, k := range keys {
			if err := tw.WriteHeader(&tar.Header{Name: k, Mode: 0o600, Size: size[k]}); err != nil {
				return err
			}
			for _, p := range parts[k] {
				resp, err := b.do("GET", p, nil, nil, 0, nil)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, resp.Body)
				resp.Body.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- restore ----

// restoreServer unpacks an archive into dataDir, which must hold no server
// yet. On PostgreSQL the database then goes in through importIntoPG, which
// refuses a database that already has accounts, and the files into the bucket.
func restoreServer(r io.Reader, pass, dataDir, pg string, bucket *s3Files) error {
	if _, err := os.Stat(filepath.Join(dataDir, "gliph.db")); err == nil {
		return fmt.Errorf("restore: %s already holds a server; restore onto an empty data dir", dataDir)
	}
	if pg != "" {
		st, err := OpenStore(pg)
		if err != nil {
			return err
		}
		n := st.CountUsers()
		st.Close()
		if n > 0 {
			return errors.New("restore: this PostgreSQL database already has accounts")
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	// Unpacked beside the data first and moved in only once the last chunk
	// has authenticated: a damaged archive leaves nothing half-restored.
	tmp, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	o, err := newOpener(r, pass)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(o)
	if err != nil {
		return errBadBackup
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Names come from an archive that passed authentication, but a path
		// out of the data dir is refused all the same.
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || !fs.ValidPath(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("restore: refusing %q", h.Name)
		}
		dst := filepath.Join(tmp, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(dst), 0o700)
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	// gzip stops at its own end; make the opener prove the last chunk.
	if _, err := io.Copy(io.Discard, o); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		dst := filepath.Join(dataDir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			// An empty uploads/ the server made, say: merge into it.
			sub, _ := os.ReadDir(filepath.Join(tmp, e.Name()))
			for _, f := range sub {
				if err := os.Rename(filepath.Join(tmp, e.Name(), f.Name()), filepath.Join(dst, f.Name())); err != nil {
					return err
				}
			}
			continue
		}
		if err := os.Rename(filepath.Join(tmp, e.Name()), dst); err != nil {
			return err
		}
	}
	if pg == "" {
		return nil
	}
	src := filepath.Join(dataDir, "gliph.db")
	if err := importIntoPG(pg, bucket, src); err != nil {
		return err
	}
	// Imported: a gliph.db left here would make the next restore refuse.
	return os.Remove(src)
}

// backupOrRestore is the -backup / -restore command.
func backupOrRestore(to, from, dataDir, pg string, bucket *s3Files) error {
	pass, err := backupPassphrase(to != "")
	if err != nil {
		return err
	}
	if from != "" {
		f, err := os.Open(from)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := restoreServer(f, pass, dataDir, pg, bucket); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "restored %s; start the server as before and clients reconnect on their own\n", from)
		return nil
	}
	f, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := backupServer(f, pass, dataDir, pg, bucket); err != nil {
		f.Close()
		os.Remove(to)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backup written to %s\n", to)
	return nil
}
