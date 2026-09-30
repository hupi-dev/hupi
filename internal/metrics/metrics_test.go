package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInstrumentHandlerRecordsExplicitStatus(t *testing.T) {
	route := "/test/explicit-status"
	h := InstrumentHandler(route, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/anything", nil))

	got := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(route, http.MethodPost, "502"))
	if got != 1 {
		t.Errorf("hupi_http_requests_total{route=%q,method=POST,status=502} = %v, want 1", route, got)
	}
}

func TestInstrumentHandlerDefaultsToOKWhenWriteHeaderNeverCalled(t *testing.T) {
	route := "/test/implicit-status"
	h := InstrumentHandler(route, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok, no explicit WriteHeader call"))
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/anything", nil))

	got := testutil.ToFloat64(HTTPRequestsTotal.WithLabelValues(route, http.MethodGet, "200"))
	if got != 1 {
		t.Errorf("hupi_http_requests_total{route=%q,method=GET,status=200} = %v, want 1", route, got)
	}
}

// TestInstrumentHandlerPreservesFlusher is a real regression test for a
// bug found by testing streamed chat completions through the actual
// registered route (metrics.InstrumentHandler wraps /v1/chat/completions
// in cmd/hupi/main.go), not a synthetic case: gateway.handleStream's own
// `w.(http.Flusher)` type assertion failed with "streaming not
// supported" on every real request through this wrapper, because
// embedding http.ResponseWriter as an interface field doesn't promote
// Flush (a separate interface, http.Flusher) even when the concrete
// value underneath has one. httptest.NewRecorder() already implements
// Flusher itself, so this needs its own explicit check — the two tests
// above never would have caught this, since they only assert on
// metrics recorded, never on what the handler's writer still supports.
func TestInstrumentHandlerPreservesFlusher(t *testing.T) {
	h := InstrumentHandler("/test/flusher", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("writer passed to the wrapped handler does not implement http.Flusher")
		}
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/anything", nil))
}
