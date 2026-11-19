package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedServer makes a server worth restoring: an account with a live session,
// an encrypted line in a channel, an upload under a relative key and one under
// an old absolute path, and a voice note.
func seedServer(t *testing.T, dsn, dataDir string) (tok, chID string, ups []string) {
	t.Helper()
	st, err := OpenStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Register("boss", "b@x.io", "hunter2hunter2", ""); err != nil {
		t.Fatal(err)
	}
	tok, _, err = st.Login("boss", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := st.CreateChannel("secret", "", false)
	if _, err := st.Append(&Event{Channel: ch.ID, Kind: "message", Actor: "boss", Body: "Y2lwaGVydGV4dA==", Enc: true, Epoch: 3}, ""); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dataDir, "uploads"), 0o700)
	os.MkdirAll(filepath.Join(dataDir, "roomnotes"), 0o700)
	os.WriteFile(filepath.Join(dataDir, "uploads", "blob_a"), []byte("sealed file a"), 0o600)
	old := filepath.Join(t.TempDir(), "blob_old")
	os.WriteFile(old, []byte("sealed file old"), 0o600)
	os.WriteFile(filepath.Join(dataDir, "roomnotes", "vn_1"), []byte("voice"), 0o600)
	a, _ := st.CreateUpload(ch.ID, "", "boss", "a.txt", 13, "uploads/blob_a", time.Hour)
	b, _ := st.CreateUpload(ch.ID, "", "boss", "old.txt", 15, old, time.Hour)
	return tok, ch.ID, []string{a.ID, b.ID}
}

func checkRestored(t *testing.T, dsn, dataDir, tok, chID string, ups []string) {
	t.Helper()
	st, err := OpenStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The client's own token still works: no new login, no re-onboarding.
	if u, err := st.UserByToken(tok); err != nil || u.Nick != "boss" || u.Role != "admin" {
		t.Fatalf("session did not survive: %v %+v", err, u)
	}
	evs, err := st.Since(chID, 0, 10)
	if err != nil || len(evs) != 1 || evs[0].Body != "Y2lwaGVydGV4dA==" || !evs[0].Enc || evs[0].Epoch != 3 {
		t.Fatalf("history: %v %+v", err, evs)
	}
	for i, want := range []string{"sealed file a", "sealed file old"} {
		u, err := st.Upload(ups[i])
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(diskFiles(dataDir).path(u.Path))
		if err != nil || string(got) != want {
			t.Fatalf("upload %s at %s: %q %v", ups[i], u.Path, got, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dataDir, "roomnotes", "vn_1")); string(got) != "voice" {
		t.Fatalf("voice note: %q", got)
	}
}

func TestBackupRestoresOntoAFreshHost(t *testing.T) {
	if os.Getenv("GLIPH_TEST_PG") != "" {
		t.Skip("the SQLite path; TestBackupFromPostgres covers PostgreSQL")
	}
	src := t.TempDir()
	tok, chID, ups := seedServer(t, filepath.Join(src, "gliph.db"), src)
	os.WriteFile(filepath.Join(src, confName), []byte("-name=acme\n"), 0o600)

	var arch bytes.Buffer
	if err := backupServer(&arch, "correct horse", src, "", nil); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(arch.Bytes(), []byte("Y2lwaGVydGV4dA")) || bytes.Contains(arch.Bytes(), []byte("boss")) {
		t.Fatal("the archive is readable without the passphrase")
	}

	dst := filepath.Join(t.TempDir(), "data")
	if err := restoreServer(bytes.NewReader(arch.Bytes()), "wrong horse", dst, "", nil); !errors.Is(err, errBadBackup) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if err := restoreServer(bytes.NewReader(arch.Bytes()), "correct horse", dst, "", nil); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, filepath.Join(dst, "gliph.db"), dst, tok, chID, ups)
	if c, _ := os.ReadFile(filepath.Join(dst, confName)); string(c) != "-name=acme\n" {
		t.Fatalf("conf: %q", c)
	}
	// Never over a live server.
	if err := restoreServer(bytes.NewReader(arch.Bytes()), "correct horse", dst, "", nil); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("restore over a server: %v", err)
	}
}

// A cut, a flipped bit and a dropped chunk all fail, and leave nothing behind.
func TestDamagedBackupRestoresNothing(t *testing.T) {
	src := t.TempDir()
	seedServer(t, filepath.Join(src, "gliph.db"), src)
	// Big enough for several chunks.
	os.WriteFile(filepath.Join(src, "uploads", "big"), bytes.Repeat([]byte{7}, 3*backupChunk), 0o600)
	var arch bytes.Buffer
	if err := backupServer(&arch, "correct horse", src, "", nil); err != nil {
		t.Fatal(err)
	}
	b := arch.Bytes()
	flipped := append([]byte(nil), b...)
	flipped[len(flipped)/2] ^= 1
	for name, bad := range map[string][]byte{
		"cut":     b[:len(b)-100],
		"flipped": flipped,
		"extra":   append(append([]byte(nil), b...), 0),
	} {
		dst := t.TempDir()
		if err := restoreServer(bytes.NewReader(bad), "correct horse", dst, "", nil); err == nil {
			t.Fatalf("%s: restored", name)
		}
		if es, _ := os.ReadDir(dst); len(es) != 0 {
			t.Fatalf("%s: left %v behind", name, es)
		}
	}
}

// A server on PostgreSQL backs up to the same archive, and it restores into
// another PostgreSQL database (files on disk here; the bucket is the same
// walk over its objects).
func TestBackupFromPostgres(t *testing.T) {
	if os.Getenv("GLIPH_TEST_PG") == "" {
		t.Skip("GLIPH_TEST_PG")
	}
	src, from := t.TempDir(), testDSN(t)
	tok, chID, ups := seedServer(t, from, src)
	var arch bytes.Buffer
	if err := backupServer(&arch, "correct horse", src, from, nil); err != nil {
		t.Fatal(err)
	}
	dst, to := t.TempDir(), testDSN(t)
	if err := restoreServer(bytes.NewReader(arch.Bytes()), "correct horse", dst, to, nil); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, to, dst, tok, chID, ups)
	if err := restoreServer(bytes.NewReader(arch.Bytes()), "correct horse", t.TempDir(), to, nil); err == nil {
		t.Fatal("restored over a database with accounts")
	}
}
