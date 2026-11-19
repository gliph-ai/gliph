package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Where upload and voice-note bytes live. A self-host keeps them on disk
// under -data; gliph.sh keeps them in an S3 bucket, because its instances have
// no disk that outlives a deploy.
//
// Keys are relative ("uploads/blob_…", "roomnotes/vn_…"). An upload row from
// before this existed holds an absolute disk path, which diskFiles still takes.
type files interface {
	// write stores the n bytes of r at offset off of key.
	write(key string, off int64, r io.Reader, n int64) error
	serve(w http.ResponseWriter, r *http.Request, key string)
	remove(key string)
}

// filesFromEnv picks S3 when the bucket's variables are set (Pushleft's
// `pl storage bucket create --set-secret` names), the data dir otherwise.
func filesFromEnv(dataDir string) files {
	if b := os.Getenv("S3_BUCKET"); b != "" {
		return &s3Files{
			endpoint: strings.TrimRight(os.Getenv("S3_ENDPOINT"), "/"),
			bucket:   b,
			region:   os.Getenv("AWS_REGION"),
			keyID:    os.Getenv("AWS_ACCESS_KEY_ID"),
			secret:   os.Getenv("AWS_SECRET_ACCESS_KEY"),
		}
	}
	return diskFiles(dataDir)
}

// ---- disk ----

type diskFiles string

func (d diskFiles) path(key string) string {
	if filepath.IsAbs(key) {
		return key
	}
	return filepath.Join(string(d), filepath.FromSlash(key))
}

func (d diskFiles) write(key string, off int64, r io.Reader, n int64) error {
	p := d.path(key)
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	m, err := io.Copy(f, io.LimitReader(r, n))
	if err == nil && m < n {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (d diskFiles) serve(w http.ResponseWriter, r *http.Request, key string) {
	http.ServeFile(w, r, d.path(key))
}

func (d diskFiles) remove(key string) { os.Remove(d.path(key)) }

// ---- S3 ----
//
// Each write is its own object, key/<offset>, zero-padded so the names sort in
// byte order. An upload arrives as 4 MiB PATCHes and S3 refuses multipart parts
// under 5 MiB, so the chunks stay separate objects and a read streams them back
// in order. A voice note is one part, and that case passes Range through, which
// <audio> needs to seek (and Safari needs to play at all).
//
// ponytail: plain PUT/GET/LIST/DELETE signed here, no SDK. A multi-part file
// is served whole, without Range; files are downloaded, not streamed.

type s3Files struct {
	endpoint, bucket, region, keyID, secret string
	// prefix scopes every key: "tenants/<slug>/" for a team on a shard, ""
	// for the solo team. Keys handed in and out never include it.
	prefix string
}

// under is the same bucket seen from inside one prefix.
func (s *s3Files) under(prefix string) *s3Files {
	c := *s
	c.prefix = prefix
	return &c
}

var s3Client = &http.Client{Timeout: 10 * time.Minute}

func partKey(key string, off int64) string { return fmt.Sprintf("%s/%020d", key, off) }

func (s *s3Files) do(method, key string, query url.Values, body io.Reader, n int64, hdr http.Header) (*http.Response, error) {
	if key != "" {
		key = s.prefix + key
	}
	u := s.endpoint + "/" + s.bucket + "/" + key
	if len(query) > 0 {
		u += "?" + strings.ReplaceAll(query.Encode(), "+", "%20")
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = n
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	s.sign(req, time.Now().UTC())
	resp, err := s3Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 %s %s: HTTP %d: %s", method, key, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return resp, nil
}

func (s *s3Files) write(key string, off int64, r io.Reader, n int64) error {
	resp, err := s.do("PUT", partKey(key, off), nil, io.LimitReader(r, n), n, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type s3Part struct {
	Key  string
	Size int64
}

func (s *s3Files) parts(key string) ([]s3Part, error) { return s.list(key + "/") }

// list returns every object under prefix, sorted by key.
func (s *s3Files) list(prefix string) ([]s3Part, error) {
	var out []s3Part
	q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + prefix}}
	for {
		resp, err := s.do("GET", "", q, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents              []s3Part
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			c.Key = strings.TrimPrefix(c.Key, s.prefix)
			out = append(out, c)
		}
		if !page.IsTruncated {
			break
		}
		q.Set("continuation-token", page.NextContinuationToken)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *s3Files) serve(w http.ResponseWriter, r *http.Request, key string) {
	parts, err := s.parts(key)
	if err != nil || len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		s.serveObject(w, r, parts[0].Key)
		return
	}
	var total int64
	for _, p := range parts {
		total += p.Size
	}
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	for _, p := range parts {
		resp, err := s.do("GET", p.Key, nil, nil, 0, nil)
		if err != nil {
			return // headers are gone; a short body is how the client learns
		}
		_, err = io.Copy(w, resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}
	}
}

// get reads one whole (small) object.
func (s *s3Files) get(key string) ([]byte, error) {
	resp, err := s.do("GET", key, nil, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// serveObject relays one object, passing Range through.
func (s *s3Files) serveObject(w http.ResponseWriter, r *http.Request, key string) {
	hdr := http.Header{}
	if rg := r.Header.Get("Range"); rg != "" {
		hdr.Set("Range", rg)
	}
	resp, err := s.do("GET", key, nil, nil, 0, hdr)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			http.NotFound(w, r)
		} else {
			http.Error(w, "file unavailable", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *s3Files) remove(key string) {
	parts, _ := s.parts(key)
	for _, p := range parts {
		if resp, err := s.do("DELETE", p.Key, nil, nil, 0, nil); err == nil {
			resp.Body.Close()
		}
	}
}

// sign is AWS Signature Version 4 for S3, with an unsigned payload: the body
// streams straight from the client and is never hashed or held in memory. Same
// procedure as the SES signer in email.go, plus the query string and S3's
// x-amz-content-sha256 header.
func (s *s3Files) sign(req *http.Request, now time.Time) {
	const service = "s3"
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + s.region + "/" + service + "/aws4_request"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	vals := map[string]string{"host": req.URL.Host, "x-amz-content-sha256": "UNSIGNED-PAYLOAD", "x-amz-date": amzDate}
	if rg := req.Header.Get("Range"); rg != "" {
		names = append(names, "range")
		vals["range"] = rg
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		canonicalQuery(req.URL.Query()),
		ch.String(),
		signed,
		"UNSIGNED-PAYLOAD",
	}, "\n")
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secret), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%x",
		s.keyID, scope, signed, hmacSHA256(key, toSign)))
}

// canonicalQuery: sorted, and encoded the RFC 3986 way (space is %20, not +).
func canonicalQuery(q url.Values) string {
	return strings.ReplaceAll(q.Encode(), "+", "%20")
}
