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
