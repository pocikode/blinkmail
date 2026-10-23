package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// testAssets returns a minimal asset set shaped like the embedded one.
func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(`<!doctype html>
<html>
  <head><link rel="stylesheet" href="/styles.css" /></head>
  <body><script src="/app.js"></script></body>
</html>
`)},
		"app.js":     &fstest.MapFile{Data: []byte("console.log('v1')\n")},
		"styles.css": &fstest.MapFile{Data: []byte("body { color: red }\n")},
	}
}

var versionPattern = regexp.MustCompile(`^[0-9a-f]{10}$`)

func TestAssetVersionChangesWithContent(t *testing.T) {
	first, err := assetVersion(testAssets())
	if err != nil {
		t.Fatalf("assetVersion: %v", err)
	}
	if !versionPattern.MatchString(first) {
		t.Fatalf("version %q is not 10 lowercase hex characters", first)
	}

	same, err := assetVersion(testAssets())
	if err != nil {
		t.Fatalf("assetVersion: %v", err)
	}
	if same != first {
		t.Fatalf("version not deterministic: %q != %q", same, first)
	}

	changed := testAssets()
	changed["app.js"] = &fstest.MapFile{Data: []byte("console.log('v2')\n")}
	other, err := assetVersion(changed)
	if err != nil {
		t.Fatalf("assetVersion: %v", err)
	}
	if other == first {
		t.Fatalf("version did not change after editing app.js: %q", other)
	}
	if !versionPattern.MatchString(other) {
		t.Fatalf("version %q is not 10 lowercase hex characters", other)
	}
}

func TestIndexReferencesVersionedAssets(t *testing.T) {
	version, err := assetVersion(testAssets())
	if err != nil {
		t.Fatalf("assetVersion: %v", err)
	}

	body := get(t, testAssets(), "/")

	if ct := getHeader(t, testAssets(), "/", "Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "app.js?v="+version) {
		t.Fatalf("shell does not reference versioned app.js:\n%s", body)
	}
	if !strings.Contains(body, "styles.css?v="+version) {
		t.Fatalf("shell does not reference versioned styles.css:\n%s", body)
	}
	if strings.Contains(body, `src="/app.js"`) {
		t.Fatalf("shell still references bare app.js:\n%s", body)
	}
	if strings.Contains(body, `href="/styles.css"`) {
		t.Fatalf("shell still references bare styles.css:\n%s", body)
	}
}

func TestAssetsAreImmutable(t *testing.T) {
	for _, path := range []string{"/app.js", "/styles.css"} {
		t.Run(path, func(t *testing.T) {
			sub := testAssets()
			rec := request(t, sub, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
			cc := rec.Header().Get("Cache-Control")
			if !strings.Contains(cc, "immutable") {
				t.Fatalf("Cache-Control = %q, want immutable", cc)
			}
			if !strings.Contains(cc, "max-age=31536000") {
				t.Fatalf("Cache-Control = %q, want max-age=31536000", cc)
			}
		})
	}
}

func TestUnknownPathServesShell(t *testing.T) {
	version, err := assetVersion(testAssets())
	if err != nil {
		t.Fatalf("assetVersion: %v", err)
	}

	rec := request(t, testAssets(), "/some/route")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /some/route = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "app.js?v="+version) {
		t.Fatalf("fallback shell is not versioned:\n%s", body)
	}
}

func TestIndexIsNotCached(t *testing.T) {
	if cc := getHeader(t, testAssets(), "/", "Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

// request drives a handler built over sub and returns the recorder.
func request(t *testing.T, sub fstest.MapFS, path string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := handlerFor(sub)
	if err != nil {
		t.Fatalf("handlerFor: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// get returns the response body for path.
func get(t *testing.T, sub fstest.MapFS, path string) string {
	t.Helper()
	rec := request(t, sub, path)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	return string(body)
}

// getHeader returns one response header for path.
func getHeader(t *testing.T, sub fstest.MapFS, path, name string) string {
	t.Helper()
	return request(t, sub, path).Header().Get(name)
}
