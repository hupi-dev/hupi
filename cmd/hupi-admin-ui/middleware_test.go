package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireCSRFSafe has no database dependency, so unlike handlers_test.go
// (which now also covers requireOperatorAuth — it needs a real
// auth.Store, so it moved there) this runs unconditionally, no
// HUPI_TEST_DATABASE_URL skip needed.

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

// TestRequireCSRFSafe covers the Basic-Auth CSRF mitigation described in
// docs/ADMIN_UI.md: state-changing requests are rejected unless the
// browser-supplied Sec-Fetch-Site header is either absent (older
// clients/curl — allowed through since this is defense-in-depth, not the
// only layer) or exactly "same-origin".
func TestRequireCSRFSafe(t *testing.T) {
	handler := requireCSRFSafe(okHandler())

	tests := []struct {
		name         string
		method       string
		secFetchSite string
		wantStatus   int
	}{
		{"GET allowed regardless of Sec-Fetch-Site", http.MethodGet, "cross-site", http.StatusOK},
		{"GET allowed with no header", http.MethodGet, "", http.StatusOK},
		{"POST allowed when same-origin", http.MethodPost, "same-origin", http.StatusOK},
		{"POST allowed when header absent", http.MethodPost, "", http.StatusOK},
		{"POST rejected when cross-site", http.MethodPost, "cross-site", http.StatusForbidden},
		{"POST rejected when same-site (not same-origin)", http.MethodPost, "same-site", http.StatusForbidden},
		{"POST rejected when none (direct navigation)", http.MethodPost, "none", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", nil)
			if tc.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}
