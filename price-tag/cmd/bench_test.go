package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Micro-benchmarks for the request metrics middleware. The metrics update path is
// lock-free atomic counter/histogram increments plus a vec label lookup — see
// internal/metrics/metrics.go. These quantify the added per-request cost of
// instrument() versus serving the handler bare, so the "overhead" tradeoff of the
// in-process listener is measured, not asserted.
func benchmarkMiddleware(b *testing.B, handler http.Handler) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/overview", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkMiddlewareBare(b *testing.B) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	benchmarkMiddleware(b, inner)
}

func BenchmarkMiddlewareInstrumented(b *testing.B) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	benchmarkMiddleware(b, instrument(inner))
}
