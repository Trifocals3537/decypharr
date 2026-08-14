package link

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		status   int
		category ErrorCategory
	}{
		{status: http.StatusForbidden, category: CategoryRefetchable},
		{status: http.StatusNotFound, category: CategoryRefetchable},
		{status: http.StatusTooManyRequests, category: CategoryThrottled},
		{status: http.StatusBadGateway, category: CategoryRetryable},
		{status: http.StatusRequestedRangeNotSatisfiable, category: CategoryPermanent},
	}

	for _, tt := range tests {
		err := ClassifyHTTPStatus(tt.status, make(http.Header))
		if err.Category != tt.category {
			t.Fatalf("status %d category = %s, want %s", tt.status, err.Category, tt.category)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("7", now); got != 7*time.Second {
		t.Fatalf("delta Retry-After = %s, want 7s", got)
	}
	date := now.Add(11 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(date, now); got != 11*time.Second {
		t.Fatalf("date Retry-After = %s, want 11s", got)
	}
	if got := parseRetryAfter("invalid", now); got != 0 {
		t.Fatalf("invalid Retry-After = %s, want 0", got)
	}
}

func TestProviderErrorCodesKeepThrottleSeparateFromRefresh(t *testing.T) {
	if err := ErrorCodeToLinkError("429"); !err.ShouldBackoff() || err.ShouldRefetch() {
		t.Fatalf("429 classification = %s, want throttled only", err.Category)
	}
	if err := ErrorCodeToLinkError("read_pxy_timeout"); !err.ShouldRetry() || err.ShouldRefetch() {
		t.Fatalf("read_pxy_timeout classification = %s, want same-link retry", err.Category)
	}
	if err := ErrorCodeToLinkError("link_not_found"); !err.ShouldRefetch() {
		t.Fatalf("link_not_found classification = %s, want refetchable", err.Category)
	}
}
