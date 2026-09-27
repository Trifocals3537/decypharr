package manager

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager/link"
)

func TestDiagnoseStreamFailureCarriesCorrelationWithoutRawError(t *testing.T) {
	mgr := &Manager{}
	ctx, requestID := mgr.beginStreamRequest(context.Background())
	if requestID == "" || streamRequestID(ctx) != requestID {
		t.Fatalf("request correlation = %q / %q", requestID, streamRequestID(ctx))
	}

	secret := "https://cdn.example/media?token=do-not-log"
	err := wrapStreamRequestError(requestID, StreamError{
		Err:       link.NewRetryableError(errors.New(secret), "503"),
		Retryable: true,
	})
	diagnostic := DiagnoseStreamFailure(err)
	if diagnostic.RequestID != requestID {
		t.Fatalf("request ID = %q, want %q", diagnostic.RequestID, requestID)
	}
	if diagnostic.Class != "upstream_status" || diagnostic.Status != http.StatusServiceUnavailable || !diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
}

func TestWrapStreamRequestErrorPreservesExistingCorrelation(t *testing.T) {
	inner := wrapStreamRequestError("r0001", errors.New("failure"))
	outer := wrapStreamRequestError("r0002", inner)
	if outer != inner {
		t.Fatal("existing correlated error was wrapped again")
	}
	if got := DiagnoseStreamFailure(outer).RequestID; got != "r0001" {
		t.Fatalf("request ID = %q, want original", got)
	}
}
