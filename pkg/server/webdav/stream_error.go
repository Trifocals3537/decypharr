package webdav

import (
	"errors"
	"net/http"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

func normalizeStreamError(err error, headersWritten bool) *customerror.Error {
	var existing *customerror.Error
	if errors.As(err, &existing) {
		existing.HeadersWritten = headersWritten
		return existing
	}

	status, retryable := manager.StreamErrorHTTPStatus(err)
	code := "server.internal_error"
	if status == http.StatusServiceUnavailable {
		code = "stream.provider_unavailable"
	} else if status == http.StatusRequestedRangeNotSatisfiable {
		code = "stream.invalid_range"
	}
	streamErr := customerror.NewError(err, status, code, customerror.IsSilentError(err), headersWritten)
	if retryable {
		streamErr.Retryable()
	}
	if status == http.StatusRequestedRangeNotSatisfiable {
		streamErr.Permanent()
	}
	return streamErr
}
