package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCredentialsNeverFollowRedirects(t *testing.T) {
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 43) {
			t.Error("missing authorized credential")
		}
		http.Redirect(w, r, other.URL, 307)
	}))
	defer origin.Close()
	c := HTTPClient(origin.URL, strings.Repeat("x", 43))
	if _, err := c.Get(origin.URL); err == nil {
		t.Fatal("redirect followed")
	}
	if received.Load() != 0 {
		t.Fatal("credential reached another server")
	}
	if _, err := c.Get(other.URL); err == nil {
		t.Fatal("cross-origin credential request accepted")
	}
}

func TestTokenEnvironmentAndResponseLimit(t *testing.T) {
	for _, secret := range []string{strings.Repeat("a", 1025), strings.Repeat("a", 32) + " b", strings.Repeat("a", 32) + "\nother"} {
		t.Setenv("HOSTMCP_TOKEN", secret)
		if _, err := Token(""); err == nil {
			t.Fatal("malformed credential accepted")
		}
	}
	b := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader("123456")), remaining: 5}
	out, err := io.ReadAll(b)
	if err == nil || string(out) != "12345" {
		t.Fatalf("response limit failed: %q %v", out, err)
	}
}
