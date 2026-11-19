package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizePartnerUsageRange(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		from, to time.Time
		want     time.Time
		wantErr  bool
	}{
		{name: "past range unchanged", from: now.Add(-time.Hour), to: now.Add(-time.Minute), want: now.Add(-time.Minute)},
		{name: "future end clamped to now", from: now.Add(-time.Hour), to: now.Add(24 * time.Hour), want: now},
		{name: "exactly 366 days allowed", from: now.Add(-partnerUsageMaxRange), to: now.Add(time.Hour), want: now},
		{name: "future start rejected", from: now.Add(time.Minute), to: now.Add(time.Hour), wantErr: true},
		{name: "unordered rejected", from: now.Add(-time.Hour), to: now.Add(-time.Hour), wantErr: true},
		{name: "effective range too long", from: now.Add(-partnerUsageMaxRange - time.Second), to: now.Add(time.Hour), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizePartnerUsageRange(tc.from, tc.to, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !got.Equal(tc.want) {
				t.Fatalf("effective to = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBatchUserUsageValidatesRequestBeforeDatabaseAccess(t *testing.T) {
	h := NewPartnerUserUsageHandler(nil)
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "missing IDs", body: `{"user_ids":[],"from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z"}`, want: http.StatusBadRequest},
		{name: "invalid UUID", body: `{"user_ids":["not-a-uuid"],"from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z"}`, want: http.StatusBadRequest},
		{name: "malformed timestamp", body: `{"user_ids":["123e4567-e89b-12d3-a456-426614174000"],"from":"yesterday","to":"2026-09-02T00:00:00Z"}`, want: http.StatusBadRequest},
		{name: "future start", body: `{"user_ids":["123e4567-e89b-12d3-a456-426614174000"],"from":"2999-09-01T00:00:00Z","to":"2999-09-02T00:00:00Z"}`, want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/usage/reports", strings.NewReader(tc.body))
			recorder := httptest.NewRecorder()
			h.HandleBatchUserUsage(recorder, req)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}
