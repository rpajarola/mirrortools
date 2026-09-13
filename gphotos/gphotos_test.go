package gphotos

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFingerprint(t *testing.T) {
	if got := fingerprint(""); got != "<absent>" {
		t.Errorf("empty: got %q", got)
	}
	if got := fingerprint("ab"); got != "<len=2 ...ab>" {
		t.Errorf("short: got %q", got)
	}
	long := "abcdefghij1234567890"
	if got, want := fingerprint(long), "<len=20 ...567890>"; got != want {
		t.Errorf("long: got %q, want %q", got, want)
	}
	// Never leaks enough to reconstruct the cookie.
	if strings.Contains(fingerprint(long), long) {
		t.Errorf("fingerprint leaked the full value: %q", fingerprint(long))
	}
}

func TestCookieValue(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(&url.URL{Scheme: "https", Host: "google.com"}, []*http.Cookie{
		{Name: "SID", Value: "secret-value", Domain: "google.com"},
	})
	c := &http.Client{Jar: jar}

	if got := cookieValue(c, "google.com", "SID"); got != "secret-value" {
		t.Errorf("got %q", got)
	}
	if got := cookieValue(c, "google.com", "NOPE"); got != "" {
		t.Errorf("absent cookie: got %q", got)
	}
}

// ---- binarycookies test fixtures ----
//
// These build a .binarycookies file byte-by-byte from the format spec,
// independently of parseBinaryCookieRecord/parseBinaryCookiesPage's own
// logic, so the tests actually catch a parsing bug rather than just
// mirroring whatever the parser happens to do.

type testBinCookie struct {
	domain, name, value, path string
	expiresIn                 time.Duration // relative to time.Now(); negative = already expired
	// valueTrailer simulates the fixed-size bplist blob current Safari
	// appends after the value's own null terminator, still within the
	// value field's declared span — see parseBinaryCookieRecord's
	// readField comment for why this matters.
	valueTrailer []byte
}

func buildCookieRecord(t *testing.T, c testBinCookie) []byte {
	t.Helper()
	const headerSize = 56 // 9 uint32 fields + 4-byte end marker + 2 uint64 fields = 9*4+4+2*8 = 56
	domain := append([]byte(c.domain), 0)
	name := append([]byte(c.name), 0)
	path := append([]byte(c.path), 0)
	value := append(append([]byte(c.value), 0), c.valueTrailer...)

	domainOff := uint32(headerSize)
	nameOff := domainOff + uint32(len(domain))
	pathOff := nameOff + uint32(len(name))
	valueOff := pathOff + uint32(len(path))
	size := valueOff + uint32(len(value))

	expiresRaw := math.Float64bits(float64(time.Now().Add(c.expiresIn).Unix() - macEpochOffset))
	creationRaw := math.Float64bits(float64(time.Now().Unix() - macEpochOffset))

	var buf bytes.Buffer
	put32 := func(v uint32) { binary.Write(&buf, binary.LittleEndian, v) }
	put64 := func(v uint64) { binary.Write(&buf, binary.LittleEndian, v) }

	put32(size)
	put32(0) // unknown1
	put32(0) // flags
	put32(0) // unknown2
	put32(domainOff)
	put32(nameOff)
	put32(pathOff)
	put32(valueOff)
	put32(0) // commentOff = 0 (no comment)
	buf.Write([]byte{0, 0, 0, 0})
	put64(expiresRaw)
	put64(creationRaw)
	buf.Write(domain)
	buf.Write(name)
	buf.Write(path)
	buf.Write(value)

	if uint32(buf.Len()) != size {
		t.Fatalf("test fixture bug: built %d bytes but computed size %d", buf.Len(), size)
	}
	return buf.Bytes()
}

func buildBinaryCookiesFile(t *testing.T, cookies []testBinCookie) []byte {
	t.Helper()
	var blobs [][]byte
	for _, c := range cookies {
		blobs = append(blobs, buildCookieRecord(t, c))
	}

	var page bytes.Buffer
	page.Write([]byte{0, 0, 1, 0}) // page tag
	binary.Write(&page, binary.LittleEndian, uint32(len(cookies)))

	offset := uint32(4 + 4 + 4*len(cookies) + 4) // tag + count + offsets + page-end
	for _, blob := range blobs {
		binary.Write(&page, binary.LittleEndian, offset)
		offset += uint32(len(blob))
	}
	page.Write([]byte{0, 0, 0, 0}) // page end
	for _, blob := range blobs {
		page.Write(blob)
	}

	var file bytes.Buffer
	file.Write([]byte("cook"))
	binary.Write(&file, binary.BigEndian, uint32(1)) // page count
	binary.Write(&file, binary.BigEndian, uint32(page.Len()))
	file.Write(page.Bytes())
	file.Write(make([]byte, 8)) // checksum, ignored by our parser
	return file.Bytes()
}

func TestLoadCookiesFromBinaryCookiesStripsValueTrailer(t *testing.T) {
	// Regression test for a real bug: a naive "trim one trailing NUL" read
	// swallowed this trailer straight into the cookie value, corrupting it
	// with ~66 bytes of binary plist data and breaking authentication.
	data := buildBinaryCookiesFile(t, []testBinCookie{
		{
			domain: ".google.com", name: "SID", value: "clean-sid-value", expiresIn: time.Hour,
			valueTrailer: append([]byte("bplist00"), 0xd1, 0x01, 0x02, 0x5a, 'A', 'c', 'c', 'e', 's', 's', 'T', 'i', 'm', 'e', 0x23),
		},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "Cookies.binarycookies")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadCookiesFromBinaryCookies(jar, path); err != nil {
		t.Fatal(err)
	}
	for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: "photos.google.com"}) {
		if c.Name == "SID" && c.Value != "clean-sid-value" {
			t.Fatalf("SID value contaminated by trailer: %q", c.Value)
		}
	}
}

func TestLoadCookiesFromBinaryCookies(t *testing.T) {
	data := buildBinaryCookiesFile(t, []testBinCookie{
		{domain: ".google.com", name: "SID", value: "sid-value", expiresIn: 24 * time.Hour},
		{domain: ".google.com", name: "__Secure-1PSIDTS", value: "sidts-value", expiresIn: time.Hour},
		{domain: ".example.com", name: "unrelated", value: "should-be-filtered-out", expiresIn: 24 * time.Hour},
		{domain: ".google.com", name: "expired-cookie", value: "stale", expiresIn: -time.Hour},
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "Cookies.binarycookies")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadCookiesFromBinaryCookies(jar, path); err != nil {
		t.Fatal(err)
	}

	// A domain cookie (leading dot) must reach photos.google.com, not just
	// the bare google.com it was recorded under — this is the same
	// domain-vs-host-only distinction that bit the Netscape-file loader.
	got := map[string]string{}
	for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: "photos.google.com"}) {
		got[c.Name] = c.Value
	}
	if got["SID"] != "sid-value" {
		t.Errorf("SID: got %q", got["SID"])
	}
	if got["__Secure-1PSIDTS"] != "sidts-value" {
		t.Errorf("__Secure-1PSIDTS: got %q", got["__Secure-1PSIDTS"])
	}
	if _, ok := got["unrelated"]; ok {
		t.Errorf("non-google.com cookie leaked into the jar: %v", got)
	}
	if _, ok := got["expired-cookie"]; ok {
		t.Errorf("expired cookie should have been filtered out: %v", got)
	}
}

func TestLoadCookiesFromBinaryCookiesNoSID(t *testing.T) {
	data := buildBinaryCookiesFile(t, []testBinCookie{
		{domain: ".google.com", name: "NID", value: "v", expiresIn: time.Hour},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "Cookies.binarycookies")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	if err := loadCookiesFromBinaryCookies(jar, path); err == nil {
		t.Fatal("expected an error when no SID cookie is present")
	}
}

func TestLoadCookiesFromBinaryCookiesBadMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-binarycookies")
	if err := os.WriteFile(path, []byte("not a binarycookies file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	if err := loadCookiesFromBinaryCookies(jar, path); err == nil {
		t.Fatal("expected an error for bad magic")
	}
}

func TestLoadCookiesAutoDetectsFormat(t *testing.T) {
	dir := t.TempDir()

	binPath := filepath.Join(dir, "bin")
	binData := buildBinaryCookiesFile(t, []testBinCookie{
		{domain: ".google.com", name: "SID", value: "from-binarycookies", expiresIn: time.Hour},
	})
	if err := os.WriteFile(binPath, binData, 0o644); err != nil {
		t.Fatal(err)
	}

	netscapePath := filepath.Join(dir, "netscape.txt")
	netscapeData := "google.com\tTRUE\t/\tTRUE\t0\tSID\tfrom-netscape\n"
	if err := os.WriteFile(netscapePath, []byte(netscapeData), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ path, wantValue string }{
		{binPath, "from-binarycookies"},
		{netscapePath, "from-netscape"},
	} {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := loadCookies(jar, tc.path); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		got := ""
		for _, c := range jar.Cookies(&url.URL{Scheme: "https", Host: "google.com"}) {
			if c.Name == "SID" {
				got = c.Value
			}
		}
		if got != tc.wantValue {
			t.Errorf("%s: got %q, want %q", tc.path, got, tc.wantValue)
		}
	}
}

func TestSnippet(t *testing.T) {
	if got := snippet([]byte("line1\nline2"), 100); got != "line1\\nline2" {
		t.Errorf("got %q", got)
	}
	if got := snippet([]byte("0123456789"), 5); got != "01234...(truncated)" {
		t.Errorf("got %q", got)
	}
}

func TestParseBatchFilenames(t *testing.T) {
	// Shape: raw[0] = [ <ignored>, [items...] ], matching safe_get(data, 0,
	// 1) from the reference client — data[0][1] is the item list, each item
	// = [media_key, [desc_at_2, file_name_at_3, ...]] (0-indexed).
	raw := json.RawMessage(`[["ignored",[["mk1",[null,null,null,"IMG_0001.jpg"]],["mk2",[null,null,null,"video.mp4"]]]]]`)

	names, err := parseBatchFilenames(raw)
	if err != nil {
		t.Fatal(err)
	}
	if names["mk1"] != "IMG_0001.jpg" || names["mk2"] != "video.mp4" {
		t.Fatalf("got %#v", names)
	}
}

func TestParseBatchFilenamesBadShape(t *testing.T) {
	if _, err := parseBatchFilenames(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unexpected response shape")
	}
}

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		body []byte
		want bool
	}{
		{[]byte(`<!doctype html><html>...`), true},
		{[]byte("  \n<HTML><head>"), true},
		{[]byte("<!DOCTYPE HTML>"), true},
		{[]byte("\xff\xd8\xff\xe0JFIF"), false},         // real jpeg magic
		{[]byte("PK\x03\x04"), false},                   // real zip magic
		{[]byte("<?xml version=\"1.0\"?><rss>"), false}, // not html specifically
		{[]byte(""), false},
	}
	for _, c := range cases {
		if got := looksLikeHTML(c.body); got != c.want {
			t.Errorf("looksLikeHTML(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestResolveFilenameFreeName(t *testing.T) {
	dir := t.TempDir()
	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk1", map[string]bool{})
	if name != "IMG_0001.jpg" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}
}

func TestResolveFilenameEmptyNameFallsBackToMediaKey(t *testing.T) {
	dir := t.TempDir()
	name, adopted := resolveFilename(dir, "", "mk1", map[string]bool{})
	if name != "mk1" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}
}

func TestResolveFilenameAdoptsUnknownExistingFile(t *testing.T) {
	// A file already on disk that nothing (this run or the index) has
	// claimed yet is assumed to be this same item from before the index
	// existed — e.g. exactly what happens the first run after adding the
	// index to a directory a pre-index run already populated.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "IMG_0001.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk1", map[string]bool{})
	if name != "IMG_0001.jpg" || !adopted {
		t.Fatalf("got %q, adopted=%v, want adopted", name, adopted)
	}
}

func TestResolveFilenameDisambiguatesKnownCollision(t *testing.T) {
	// A name already in `used` (claimed by a different, already-known item
	// — from the index or earlier this run) is a genuine collision between
	// two distinct items and must never be silently adopted or overwritten.
	dir := t.TempDir()
	used := map[string]bool{"IMG_0001.jpg": true} // claimed by some other media key

	name, adopted := resolveFilename(dir, "IMG_0001.jpg", "mk2", used)
	if name != "IMG_0001-2.jpg" || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}

	// A second distinct item wanting the same name gets the next suffix.
	name2, adopted2 := resolveFilename(dir, "IMG_0001.jpg", "mk3", used)
	if name2 != "IMG_0001-3.jpg" || adopted2 {
		t.Fatalf("got %q, adopted=%v", name2, adopted2)
	}
}

func TestDateSubdir(t *testing.T) {
	// 2026-01-01 00:30 UTC is still 2025-12-31 in UTC-9 local time — the
	// whole point of using TimezoneOffsetMS instead of raw UTC.
	ts := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC).UnixMilli()
	it := libraryItem{TimestampMS: ts, TimezoneOffsetMS: -9 * 3600 * 1000}

	want := filepath.Join("2025", "12")
	if got := dateSubdir(it); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDateSubdirUTCFallback(t *testing.T) {
	// A zero TimezoneOffsetMS (e.g. an older response missing the field)
	// falls back to plain UTC rather than erroring.
	ts := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	it := libraryItem{TimestampMS: ts}

	want := filepath.Join("2026", "06")
	if got := dateSubdir(it); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTargetName(t *testing.T) {
	if got, want := targetName("2026/09", "IMG_0001.jpg", "mk1"), filepath.Join("2026/09", "IMG_0001.jpg"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Empty resolvedName falls back to the media key, same as
	// resolveFilename's own internal fallback.
	if got, want := targetName("2026/09", "", "mk1"), filepath.Join("2026/09", "mk1"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRawCoverRegex(t *testing.T) {
	cases := []struct {
		name      string
		wantGroup string
		wantMatch bool
	}{
		{"PXL_20260911_103945315.RAW-01.COVER.jpg", "20260911_103945315", true},
		{"PXL_20260911_030743453.RAW-01.MP.COVER.jpg", "20260911_030743453", true},
		{"pxl_20260911_103945315.raw-01.cover.jpg", "20260911_103945315", true}, // case-insensitive
		{"PXL_20260912_113018488.RAW-02.ORIGINAL.dng", "", false},               // the companion itself, not a cover
		{"IMG_0001.jpg", "", false},
		{"PXL_20260911_103945315.jpg", "", false}, // plain Pixel photo, no RAW pair
	}
	for _, c := range cases {
		m := rawCoverRe.FindStringSubmatch(c.name)
		if c.wantMatch != (m != nil) {
			t.Errorf("rawCoverRe.MatchString(%q) = %v, want %v", c.name, m != nil, c.wantMatch)
			continue
		}
		if c.wantMatch && m[1] != c.wantGroup {
			t.Errorf("rawCoverRe group for %q = %q, want %q", c.name, m[1], c.wantGroup)
		}
	}
}

func TestParseRawGroup(t *testing.T) {
	// Shape confirmed against real captured traffic for wgZjtc: raw[2] is
	// the item list, each item in the same shape as an lcxiM library item.
	raw := json.RawMessage(`[null,null,[["mk-jpeg",["https://example.com/pw/AAA",4000,3000],1000,"dedup-jpeg"],["mk-dng",["https://example.com/pw/BBB",4140,3100],1000,"dedup-dng"]],"20260912_113018488"]`)

	items, err := parseRawGroup(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items: %#v", len(items), items)
	}
	if items[0].MediaKey != "mk-jpeg" || items[0].BaseURL != "https://example.com/pw/AAA" {
		t.Fatalf("item 0: got %#v", items[0])
	}
	if items[1].MediaKey != "mk-dng" || items[1].BaseURL != "https://example.com/pw/BBB" {
		t.Fatalf("item 1: got %#v", items[1])
	}
}

func TestParseRawGroupBadShape(t *testing.T) {
	if _, err := parseRawGroup(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unexpected response shape")
	}
}

func TestFetchRawCompanionOriginal(t *testing.T) {
	var gotPath, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("raw bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk-dng", BaseURL: srv.URL + "/dl"}
	if err := fetchRawCompanionOriginal(context.Background(), srv.Client(), item, "photo.dng", dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "photo.dng"))
	if err != nil || !bytes.Equal(got, []byte("raw bytes")) {
		t.Fatalf("final file: got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s): %v", matches)
	}
	if !strings.HasSuffix(gotPath, "=s0-d-I") {
		t.Fatalf("expected the =s0-d-I suffix, got path %q", gotPath)
	}
	if gotReferer != "https://photos.google.com/" {
		t.Fatalf("Referer header: got %q", gotReferer)
	}
}

func TestFetchRawCompanionOriginalRejectsSignInRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><html><body>sign in</body></html>"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk-dng", BaseURL: srv.URL + "/dl"}
	if err := fetchRawCompanionOriginal(context.Background(), srv.Client(), item, "photo.dng", dir); err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	if _, err := os.Stat(filepath.Join(dir, "photo.dng")); !os.IsNotExist(err) {
		t.Fatalf("HTML should never be written to the final name, stat err = %v", err)
	}
}

func TestDownloadCompanionsNoMatchReturnsNil(t *testing.T) {
	// A cover filename that doesn't match a Pixel RAW+JPEG pair must bail
	// out before making any network call (nil client would panic if it
	// tried).
	item := libraryItem{MediaKey: "mk1"}
	extra, err := downloadCompanions(context.Background(), nil, nil, item, "IMG_0001.jpg", "2026/09", t.TempDir(), map[string]bool{})
	if err != nil || extra != nil {
		t.Fatalf("got %v, %v; want nil, nil", extra, err)
	}
}

func TestRotateCookiesSendsExpectedRequest(t *testing.T) {
	var gotMethod, gotContentType, gotOrigin, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotOrigin = r.Header.Get("Origin")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := rotateCookies(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q", gotContentType)
	}
	if gotOrigin != "https://accounts.google.com" {
		t.Errorf("Origin: got %q", gotOrigin)
	}
	if gotBody != `[000,"-0000000000000000000"]` {
		t.Errorf("body: got %q", gotBody)
	}
}

func TestRotateCookiesUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := rotateCookies(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

func TestRotateCookiesUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := rotateCookies(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestIndexEntryFilenames(t *testing.T) {
	if got := (indexEntry{}).filenames(); got != nil {
		t.Fatalf("empty entry: got %v", got)
	}
	e := indexEntry{Filename: "IMG_0001.jpg", ExtraFilenames: []string{"IMG_0001.dng"}}
	got := e.filenames()
	if len(got) != 2 || got[0] != "IMG_0001.jpg" || got[1] != "IMG_0001.dng" {
		t.Fatalf("got %v", got)
	}
}

func TestDownloadOriginalWritesViaTempThenRename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("photo bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	item := libraryItem{MediaKey: "mk1", BaseURL: srv.URL + "/photo"}
	if err := downloadOriginal(context.Background(), srv.Client(), item, "photo.jpg", dir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "photo.jpg"))
	if err != nil || !bytes.Equal(got, []byte("photo bytes")) {
		t.Fatalf("final file: got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s) after successful download: %v", matches)
	}
}

func TestWriteAtomicallyCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join("2026", "09", "photo.jpg")
	if err := writeAtomically(dir, name, bytes.NewReader([]byte("photo bytes"))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || !bytes.Equal(got, []byte("photo bytes")) {
		t.Fatalf("final file: got %q, %v", got, err)
	}
}

func TestDownloadOriginalCanceledLeavesNoFinalFile(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-block // hang until the client gives up, simulating a cut-short download
	}))
	defer srv.Close()
	defer close(block)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	item := libraryItem{MediaKey: "mk1", BaseURL: srv.URL + "/photo"}
	if err := downloadOriginal(ctx, srv.Client(), item, "photo.jpg", dir); err == nil {
		t.Fatal("expected an error from a canceled download")
	}

	if _, err := os.Stat(filepath.Join(dir, "photo.jpg")); !os.IsNotExist(err) {
		t.Fatalf("final file should not exist after a canceled download, stat err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("temp file should have been cleaned up, found: %v", matches)
	}
}

func TestRemoveStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	// Files now live under per-item date subdirectories, so a stale temp
	// file can be nested arbitrarily deep — this must walk, not just glob
	// the top level.
	nested := filepath.Join(dir, "2026", "09")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(nested, "IMG_0001.jpg"+tmpSuffix)
	keep := filepath.Join(dir, "IMG_0002.jpg")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeStaleTempFiles(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file should be untouched: %v", err)
	}
}

func TestParseLast(t *testing.T) {
	ref := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		last string
		want time.Time
	}{
		{"30d", ref.AddDate(0, 0, -30)},
		{"2w", ref.AddDate(0, 0, -14)},
		// AddDate(0,-1,0) on Mar 31 lands on "Feb 31", which Go normalizes by
		// overflowing into March (Feb 2026 has 28 days) rather than clamping.
		{"1m", time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)},
		{"1y", time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseLast(c.last, ref)
		if err != nil {
			t.Errorf("parseLast(%q): %v", c.last, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseLast(%q) = %v, want %v", c.last, got, c.want)
		}
	}
}

func TestParseLastInvalid(t *testing.T) {
	for _, bad := range []string{"", "30", "d", "30x", "-5d", "1.5m"} {
		if _, err := parseLast(bad, time.Now()); err == nil {
			t.Errorf("parseLast(%q): expected error", bad)
		}
	}
}

func TestDownloadIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()

	idx, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if idx.has("mk1") {
		t.Fatal("fresh index should not have mk1")
	}
	idx.record("mk1", []string{"IMG_0001.jpg"}, 12345)
	if !idx.has("mk1") {
		t.Fatal("record then has should be true")
	}
	if err := idx.save(); err != nil {
		t.Fatal(err)
	}

	idx2, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.has("mk1") {
		t.Fatal("reloaded index should still have mk1")
	}
	if idx2.entries["mk1"].Filename != "IMG_0001.jpg" || idx2.entries["mk1"].TimestampMS != 12345 {
		t.Fatalf("got %#v", idx2.entries["mk1"])
	}
	if idx2.has("mk2") {
		t.Fatal("reloaded index should not have unrelated mk2")
	}
}
