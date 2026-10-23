package gce_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptestServer serves h and returns its host:port.
func httptestServer(t *testing.T, h http.Handler) string {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}
