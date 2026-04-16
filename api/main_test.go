package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSpaFileServer_ServesExistingAsset(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>index</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := spaFileServer(tmp)
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "console.log('ok')" {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestSpaFileServer_FallbackForRoutePath(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>index</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := spaFileServer(tmp)
	req := httptest.NewRequest(http.MethodGet, "/workers/terminal_1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "<html>index</html>" {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestSpaFileServer_404ForMissingAssetWithExtension(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>index</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := spaFileServer(tmp)
	req := httptest.NewRequest(http.MethodGet, "/static/novnc/vendor/pako/lib/zlib/inflate.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
