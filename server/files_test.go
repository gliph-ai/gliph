package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The S3 half needs a real bucket: set S3_BUCKET and friends (the names
// `pl storage bucket create --set-secret` stores) to run it. The disk half
// is covered by every upload and voice-note test.
func TestS3FilesRoundTrip(t *testing.T) {
	if os.Getenv("S3_BUCKET") == "" {
		t.Skip("no S3_BUCKET")
	}
	f := filesFromEnv("").(*s3Files)
	key := "test/" + id("blob")
	t.Cleanup(func() { f.remove(key) })

	// two chunks, the way an upload arrives
	if err := f.write(key, 0, strings.NewReader("hello "), 6); err != nil {
		t.Fatal(err)
	}
	if err := f.write(key, 6, strings.NewReader("world, and spaces"), 17); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.serve(rec, httptest.NewRequest("GET", "/", nil), key)
	if got := rec.Body.String(); got != "hello world, and spaces" || rec.Header().Get("Content-Length") != "23" {
		t.Fatalf("multi-part read: %q %v", got, rec.Header())
	}

	// one part: Range passes through
	one := key + "-one"
	t.Cleanup(func() { f.remove(one) })
	f.write(one, 0, bytes.NewReader([]byte("0123456789")), 10)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec = httptest.NewRecorder()
	f.serve(rec, req, one)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}

	f.remove(key)
	rec = httptest.NewRecorder()
	f.serve(rec, httptest.NewRequest("GET", "/", nil), key)
	if rec.Code != 404 {
		b, _ := io.ReadAll(rec.Body)
		t.Fatalf("after remove: %d %s", rec.Code, b)
	}
}

// The SQLite era's files move into the bucket and their rows follow.
func TestImportFilesMovesDiskUploads(t *testing.T) {
	if os.Getenv("S3_BUCKET") == "" {
		t.Skip("no S3_BUCKET")
	}
	st, err := OpenStore(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	disk := dir + "/uploads/" + id("blob")
	os.MkdirAll(dir+"/uploads", 0o700)
	os.WriteFile(disk, []byte("old bytes"), 0o600)
	up, err := st.CreateUpload("c", "u", "n", "f.txt", 9, disk, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f := filesFromEnv("")
	if err := importFiles(st, f, dir); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Upload(up.ID)
	t.Cleanup(func() { f.remove(got.Path) })
	if !strings.HasPrefix(got.Path, "uploads/") {
		t.Fatalf("row not repointed: %q", got.Path)
	}
	rec := httptest.NewRecorder()
	f.serve(rec, httptest.NewRequest("GET", "/", nil), got.Path)
	if rec.Body.String() != "old bytes" {
		t.Fatalf("bucket has %q", rec.Body.String())
	}
}
