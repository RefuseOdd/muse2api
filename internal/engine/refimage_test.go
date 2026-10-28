package engine

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muse2api/muse2api/internal/config"
)

func newTestEngine(localFiles, privateFetch bool) *Engine {
	return New(&config.Config{
		RefImageLocalFiles:   localFiles,
		RefImagePrivateFetch: privateFetch,
		MaxConcurrency:       1,
		LoginWait:            0,
		ImageTimeout:         1,
		VideoTimeout:         1,
		ChatTimeout:          1,
	})
}

func parseIPForTest(s string) net.IP { return net.ParseIP(s) }

func TestNormalizeImageDataURLAndBase64(t *testing.T) {
	eng := newTestEngine(false, false)
	payload := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	b64, mimeType, err := eng.NormalizeImage("data:image/png;base64," + payload)
	if err != nil || b64 != payload || mimeType != "image/png" {
		t.Fatalf("data URL normalization failed: %q %q %v", b64, mimeType, err)
	}
	if _, _, err := eng.NormalizeImage("data:text/plain;base64," + payload); err == nil {
		t.Fatalf("non-image MIME should be rejected")
	}
	if _, _, err := eng.NormalizeImage("data:image/png;base64,%%bad%%"); err == nil {
		t.Fatalf("invalid base64 should be rejected")
	}
	b64, mimeType, err = eng.NormalizeImage(payload)
	if err != nil || b64 != payload || mimeType != "image/png" {
		t.Fatalf("raw base64 normalization failed: %v", err)
	}
}

func TestNormalizeImageSizeLimit(t *testing.T) {
	eng := newTestEngine(false, false)
	big := make([]byte, MaxReferenceImageBytes+1)
	encoded := base64.StdEncoding.EncodeToString(big)
	_, _, err := eng.NormalizeImage("data:image/png;base64," + encoded)
	if err == nil || !strings.Contains(err.Error(), "10 MiB") {
		t.Fatalf("expected 10 MiB error, got %v", err)
	}
}

func TestNormalizeImageLocalPolicy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ref.png")
	if err := os.WriteFile(p, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(false, false)
	if _, _, err := eng.NormalizeImage(p); err == nil || !IsSafetyError(err) || !strings.Contains(err.Error(), "MUSE2API_REF_IMAGE_LOCAL_FILES=1") {
		t.Fatalf("expected local-file policy error, got %v", err)
	}
	eng = newTestEngine(true, false)
	b64, mimeType, err := eng.NormalizeImage(p)
	if err != nil || b64 == "" || mimeType != "image/png" {
		t.Fatalf("local file should be allowed with opt-in: %v", err)
	}
}

func TestNormalizeImagePrivateNetworkDenied(t *testing.T) {
	oldLookup, oldDial := refLookupIP, refDialContext
	defer func() { refLookupIP, refDialContext = oldLookup, oldDial }()
	dialed := false
	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	refDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}
	eng := newTestEngine(false, false)
	_, _, err := eng.NormalizeImage("http://private.example/ref")
	if err == nil || !IsSafetyError(err) || !strings.Contains(err.Error(), "MUSE2API_REF_IMAGE_PRIVATE_FETCH=1") {
		t.Fatalf("expected private-fetch policy error, got %v", err)
	}
	if dialed {
		t.Fatalf("policy rejection must happen before any dial")
	}
}

func TestNormalizeImagePublicFetchAndRedirect(t *testing.T) {
	oldLookup, oldDial := refLookupIP, refDialContext
	defer func() { refLookupIP, refDialContext = oldLookup, oldDial }()

	var receiver string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/image", http.StatusFound)
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("public-image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lookupCalls := 0
	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		lookupCalls++
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	refDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		receiver = address
		return net.Dial("tcp", server.Listener.Addr().String())
	}
	eng := newTestEngine(false, false)

	b64, mimeType, err := eng.NormalizeImage(server.URL + "/image")
	if err != nil {
		t.Fatalf("public fetch failed: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(b64)
	if string(decoded) != "public-image" || mimeType != "image/png" {
		t.Fatalf("unexpected fetched image: %q %q", decoded, mimeType)
	}
	if !strings.HasPrefix(receiver, "8.8.8.8:") {
		t.Fatalf("dial must be pinned to the checked IP, got %q", receiver)
	}

	lookupCalls = 0
	b64, _, err = eng.NormalizeImage(server.URL + "/redirect")
	if err != nil {
		t.Fatalf("redirected public fetch failed: %v", err)
	}
	decoded, _ = base64.StdEncoding.DecodeString(b64)
	if string(decoded) != "public-image" {
		t.Fatalf("unexpected redirect payload: %q", decoded)
	}
	if lookupCalls != 2 {
		t.Fatalf("expected DNS to be checked for both hops, got %d", lookupCalls)
	}
}

func TestNormalizeImageRedirectToPrivateDenied(t *testing.T) {
	oldLookup, oldDial := refLookupIP, refDialContext
	defer func() { refLookupIP, refDialContext = oldLookup, oldDial }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost/private", http.StatusFound)
	}))
	defer server.Close()

	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "localhost" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	refDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", server.Listener.Addr().String())
	}
	eng := newTestEngine(false, false)
	_, _, err := eng.NormalizeImage(server.URL + "/redirect")
	if err == nil || !IsSafetyError(err) {
		t.Fatalf("redirect to a private address must be denied, got %v", err)
	}
}

func TestNormalizeImageFetchFailureIsGenerationError(t *testing.T) {
	oldLookup, oldDial := refLookupIP, refDialContext
	defer func() { refLookupIP, refDialContext = oldLookup, oldDial }()
	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	refDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	}
	eng := newTestEngine(false, false)
	_, _, err := eng.NormalizeImage("http://unreachable.example/ref")
	if err == nil || IsSafetyError(err) || !strings.Contains(err.Error(), "download remote reference image failed") {
		t.Fatalf("expected a generation download error, got %v", err)
	}
}
