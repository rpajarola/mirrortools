package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSanitizeForPath(t *testing.T) {
	cases := map[string]string{
		"someone@example.com": "someone_example.com",
		"a.b-c_d123":          "a.b-c_d123",
		"":                    "_",
		"../../etc/passwd":    ".._.._etc_passwd",
	}
	for in, want := range cases {
		if got := sanitizeForPath(in); got != want {
			t.Errorf("sanitizeForPath(%q) = %q, want %q", in, got, want)
		}
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
	for _, bad := range []string{"", "30", "d", "30x", "-5d"} {
		if _, err := parseLast(bad, time.Now()); err == nil {
			t.Errorf("parseLast(%q): expected error", bad)
		}
	}
}

func TestCleanFilename(t *testing.T) {
	cases := map[string]string{
		"IMG_0001.JPG":     "IMG_0001.JPG",
		"a photo (1).heic": "a photo (1).heic", // spaces/punctuation kept, unlike icloudgo's aggressive cleaning
		"a/b.jpg":          "a_b.jpg",
		"bad\x00name.jpg":  "bad_name.jpg",
	}
	for in, want := range cases {
		if got := cleanFilename(in); got != want {
			t.Errorf("cleanFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeFilename(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("IMG_1234.HEIC"))
	if got := decodeFilename(enc, "fallback-id"); got != "IMG_1234.HEIC" {
		t.Errorf("got %q", got)
	}
	if got := decodeFilename("", "fallback-id"); got != "fallback-id" {
		t.Errorf("empty field: got %q", got)
	}
	if got := decodeFilename("not-valid-base64!!!", "fallback-id"); got != "fallback-id" {
		t.Errorf("bad base64: got %q", got)
	}
}

func TestLivePhotoVideoName(t *testing.T) {
	cases := map[string]string{
		"IMG_0001.HEIC": "IMG_0001.MOV",
		"IMG_0001":      "IMG_0001.MOV",
		"a.b.c.jpg":     "a.MOV",
	}
	for in, want := range cases {
		if got := livePhotoVideoName(in); got != want {
			t.Errorf("livePhotoVideoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDateSubdir(t *testing.T) {
	d := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	want := filepath.Join("2026", "03")
	if got := dateSubdir(d); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveFilenameFreeName(t *testing.T) {
	dir := t.TempDir()
	name, adopted := resolveFilename(dir, "2026/03", "IMG_0001.jpg", map[string]bool{})
	want := filepath.Join("2026/03", "IMG_0001.jpg")
	if name != want || adopted {
		t.Fatalf("got %q, adopted=%v", name, adopted)
	}
}

func TestResolveFilenameAdoptsUnknownExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "2026", "03"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "2026", "03", "IMG_0001.jpg")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, adopted := resolveFilename(dir, "2026/03", "IMG_0001.jpg", map[string]bool{})
	want := filepath.Join("2026/03", "IMG_0001.jpg")
	if name != want || !adopted {
		t.Fatalf("got %q, adopted=%v, want adopted", name, adopted)
	}
}

func TestResolveFilenameDisambiguatesKnownCollision(t *testing.T) {
	dir := t.TempDir()
	used := map[string]bool{filepath.Join("2026/03", "IMG_0001.jpg"): true}
	name, adopted := resolveFilename(dir, "2026/03", "IMG_0001.jpg", used)
	want := filepath.Join("2026/03", "IMG_0001-2.jpg")
	if name != want || adopted {
		t.Fatalf("got %q, adopted=%v, want %q", name, adopted, want)
	}
}

func TestDownloadIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	idx, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if idx.has("id1") {
		t.Fatal("fresh index should not have id1")
	}
	idx.record("id1", "IMG_0001.jpg", "IMG_0001.MOV", 12345)
	idx.NextOffset = 200
	if err := idx.save(); err != nil {
		t.Fatal(err)
	}

	idx2, err := loadDownloadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.has("id1") {
		t.Fatal("reloaded index should have id1")
	}
	if idx2.NextOffset != 200 {
		t.Fatalf("NextOffset: got %d", idx2.NextOffset)
	}
	e := idx2.Entries["id1"]
	if e.Filename != "IMG_0001.jpg" || e.VideoFilename != "IMG_0001.MOV" || e.AssetDateMS != 12345 {
		t.Fatalf("got %#v", e)
	}
	got := e.filenames()
	if len(got) != 2 || got[0] != "IMG_0001.jpg" || got[1] != "IMG_0001.MOV" {
		t.Fatalf("filenames() = %v", got)
	}
}

func TestParseAPIError(t *testing.T) {
	body := []byte(`{"service_errors":[{"code":"-21669","title":"Incorrect verification code.","message":"Please try again."}],"hasError":true}`)
	err := parseAPIError(body)
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*apiError)
	if !ok || apiErr.Code != errCodeWrongVerificationCode {
		t.Fatalf("got %#v", err)
	}
}

func TestParseAPIErrorSimpleShape(t *testing.T) {
	body := []byte(`{"reason":"Account temporarily locked","error":"locked"}`)
	err := parseAPIError(body)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseAPIErrorNoError(t *testing.T) {
	if err := parseAPIError([]byte(`{"dsInfo":{"hsaVersion":2}}`)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestExportableJarRoundTrip(t *testing.T) {
	jar := newExportableJar()
	u := &url.URL{Scheme: "https", Host: "idmsa.apple.com"}
	jar.SetCookies(u, []*http.Cookie{
		{Name: "aasp", Value: "v1", Path: "/"},
	})
	// A second SetCookies for the same name should overwrite, not
	// accumulate duplicates.
	jar.SetCookies(u, []*http.Cookie{
		{Name: "aasp", Value: "v2", Path: "/"},
	})

	exported := jar.export()
	if len(exported["idmsa.apple.com"]) != 1 {
		t.Fatalf("expected exactly one cookie recorded, got %v", exported)
	}
	if exported["idmsa.apple.com"]["aasp"].Value != "v2" {
		t.Fatalf("expected latest value to win, got %#v", exported["idmsa.apple.com"]["aasp"])
	}

	jar2 := newExportableJar()
	jar2.restore(exported)
	cookies := jar2.Cookies(u)
	if len(cookies) != 1 || cookies[0].Value != "v2" {
		t.Fatalf("restored jar cookies: got %v", cookies)
	}
}

func TestParsePhotosResponse(t *testing.T) {
	filenameEnc := base64.StdEncoding.EncodeToString([]byte("IMG_0001.HEIC"))
	body := []byte(`{"records":[
		{"recordName":"master1","recordType":"CPLMaster","fields":{
			"filenameEnc":{"value":"` + filenameEnc + `"},
			"resOriginalRes":{"value":{"downloadURL":"https://example.com/orig1","size":1000}}
		}},
		{"recordName":"asset1","recordType":"CPLAsset","fields":{
			"masterRef":{"value":{"recordName":"master1"}},
			"assetDate":{"value":1700000000000},
			"isHidden":{"value":0}
		}},
		{"recordName":"master2","recordType":"CPLMaster","fields":{
			"filenameEnc":{"value":""},
			"resOriginalRes":{"value":{"downloadURL":"https://example.com/orig2","size":2000}},
			"resOriginalVidComplRes":{"value":{"downloadURL":"https://example.com/vid2","size":500}}
		}},
		{"recordName":"asset2","recordType":"CPLAsset","fields":{
			"masterRef":{"value":{"recordName":"master2"}},
			"assetDate":{"value":1700000001000},
			"isHidden":{"value":1}
		}},
		{"recordName":"master3-no-asset","recordType":"CPLMaster","fields":{
			"filenameEnc":{"value":""},
			"resOriginalRes":{"value":{"downloadURL":"https://example.com/orig3","size":3000}}
		}}
	]}`)

	items, err := parsePhotosResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	// master3 has no matching CPLAsset and must be skipped, not guessed at.
	if len(items) != 2 {
		t.Fatalf("got %d items: %#v", len(items), items)
	}

	if items[0].ID != "master1" || items[0].Filename != "IMG_0001.HEIC" || items[0].OriginalURL != "https://example.com/orig1" ||
		items[0].AssetDateMS != 1700000000000 || items[0].Hidden || items[0].IsLivePhoto() {
		t.Fatalf("item 0: got %#v", items[0])
	}

	if items[1].ID != "master2" || items[1].Filename != "master2" /* no filenameEnc -> falls back to ID */ ||
		!items[1].Hidden || !items[1].IsLivePhoto() || items[1].LivePhotoVideoURL != "https://example.com/vid2" {
		t.Fatalf("item 1: got %#v", items[1])
	}
}

func TestParsePhotosResponseEmpty(t *testing.T) {
	items, err := parsePhotosResponse([]byte(`{"records":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("got %v", items)
	}
}

func TestPhotoServiceEndpoint(t *testing.T) {
	c := &Client{}
	if _, err := c.photoServiceEndpoint(); err == nil {
		t.Fatal("expected error when not authenticated")
	}

	c.validated = &validateData{Webservices: map[string]webService{}}
	if _, err := c.photoServiceEndpoint(); err == nil {
		t.Fatal("expected error when ckdatabasews is missing")
	}

	c.validated = &validateData{Webservices: map[string]webService{
		"ckdatabasews": {URL: "https://p12-ckdatabasews.icloud.com"},
	}}
	got, err := c.photoServiceEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	want := "https://p12-ckdatabasews.icloud.com/database/1/com.apple.photos.cloud/production/private"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ---- HTTP-touching pieces, via httptest ----

func newTestClient(t *testing.T, authEndpoint, setupEndpoint string) *Client {
	t.Helper()
	c, err := newClient("someone@example.com", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if authEndpoint != "" {
		c.authEndpoint = authEndpoint
	}
	if setupEndpoint != "" {
		c.setupEndpoint = setupEndpoint
	}
	return c
}

func TestSignInAndAuthWithToken(t *testing.T) {
	var signinBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/signin", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&signinBody)
		w.Header().Set("X-Apple-Session-Token", "tok-123")
		w.Header().Set("scnt", "scnt-abc")
		w.Header().Set("X-Apple-ID-Session-Id", "sess-xyz")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/accountLogin", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["dsWebAuthToken"] != "tok-123" {
			t.Errorf("accountLogin should reuse the session token from signin, got %v", body["dsWebAuthToken"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"dsInfo":               map[string]any{"hsaVersion": 2},
			"hsaChallengeRequired": true,
			"hsaTrustedBrowser":    false,
			"webservices":          map[string]any{"ckdatabasews": map[string]any{"url": "https://example.com/ck"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, srv.URL)
	if err := c.signIn(context.Background(), "hunter2"); err != nil {
		t.Fatal(err)
	}
	if signinBody["accountName"] != "someone@example.com" {
		t.Errorf("signin body: got %v", signinBody)
	}
	if c.session.SessionToken != "tok-123" || c.session.Scnt != "scnt-abc" || c.session.SessionID != "sess-xyz" {
		t.Fatalf("session not captured from response headers: %#v", c.session)
	}
	if !c.requires2FA() {
		t.Fatal("expected requires2FA to be true")
	}
}

func TestValidate2FACodeWrongCode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/verify/trusteddevice/securitycode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"service_errors":[{"code":"-21669","title":"Incorrect verification code.","message":"Please try again."}],"hasError":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, srv.URL)
	err := c.validate2FACode(context.Background(), "000000")
	if err == nil || err.Error() != "incorrect verification code" {
		t.Fatalf("got %v", err)
	}
}

func TestTrustSessionRefreshesValidatedData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/2sv/trust", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Apple-TwoSV-Trust-Token", "trust-abc")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/accountLogin", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dsInfo":               map[string]any{"hsaVersion": 2},
			"hsaChallengeRequired": false,
			"hsaTrustedBrowser":    true,
			"webservices":          map[string]any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, srv.URL)
	if err := c.trustSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.session.TrustToken != "trust-abc" {
		t.Fatalf("trust token not captured: %#v", c.session)
	}
	if c.requires2FA() {
		t.Fatal("should no longer require 2FA after trustSession")
	}
}

func TestValidateTokenSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/validate", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dsInfo":      map[string]any{"hsaVersion": 2},
			"webservices": map[string]any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, srv.URL)
	c.session.SessionToken = "already-have-one"
	if err := c.authenticate(context.Background(), "", nil); err != nil {
		t.Fatalf("authenticate should succeed via validateToken alone: %v", err)
	}
}

func TestAuthenticateNoSessionNoPassword(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:0", "http://127.0.0.1:0")
	err := c.authenticate(context.Background(), "", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDownloadURLWritesFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := downloadURL(context.Background(), srv.Client(), srv.URL, "photo.jpg", dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "photo.jpg"))
	if err != nil || string(got) != "file bytes" {
		t.Fatalf("got %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+tmpSuffix)); len(matches) != 0 {
		t.Fatalf("leftover temp file(s): %v", matches)
	}
}

func TestDownloadURLFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := downloadURL(context.Background(), srv.Client(), srv.URL, "photo.jpg", dir); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "photo.jpg")); !os.IsNotExist(err) {
		t.Fatalf("file should not exist, stat err = %v", err)
	}
}

func TestListPhotosPage(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/records/query", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"records":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL, srv.URL)
	items, err := c.listPhotosPage(context.Background(), srv.URL, smartAlbumQueries[AlbumAll], 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("got %v", items)
	}
	q, ok := gotBody["query"].(map[string]any)
	if !ok {
		t.Fatalf("body: got %v", gotBody)
	}
	if q["recordType"] != "CPLAssetAndMasterByAddedDate" {
		t.Fatalf("recordType: got %v", q["recordType"])
	}
	filterBy, ok := q["filterBy"].([]any)
	if !ok || len(filterBy) == 0 {
		t.Fatalf("filterBy: got %v", q["filterBy"])
	}
	first := filterBy[0].(map[string]any)
	if first["fieldName"] != "startRank" {
		t.Fatalf("first filter: got %v", first)
	}
}
