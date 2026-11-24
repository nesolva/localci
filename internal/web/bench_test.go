package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

// A run of 500 tests on 8 shards renders server-side in under 200 ms.
func BenchmarkTimeline(b *testing.B) {
	f := newFixture(b)
	seedWide(b, f, "wide", 500, 8)
	h := Handler(f.st, f.bus)
	b.ResetTimer()
	var worst time.Duration
	for i := 0; i < b.N; i++ {
		start := time.Now()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/wide", nil))
		if rec.Code != 200 {
			b.Fatalf("status %d", rec.Code)
		}
		worst = max(worst, time.Since(start))
	}
	b.ReportMetric(float64(worst.Milliseconds()), "worst-ms")
	if worst > 200*time.Millisecond {
		b.Fatalf("slowest render %v, want under 200ms", worst)
	}
}
