package qbit

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestTorrentAddErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		status     int
		idempotent bool
	}{
		{
			name:       "duplicate",
			err:        fmtWrap(manager.ErrJobQueueDuplicate),
			status:     http.StatusOK,
			idempotent: true,
		},
		{
			name:   "full",
			err:    fmtWrap(manager.ErrJobQueueFull),
			status: http.StatusTooManyRequests,
		},
		{
			name:   "closed",
			err:    fmtWrap(manager.ErrJobQueueClosed),
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "deleting",
			err:    errors.Join(manager.ErrQueueEntryDeleting, storage.ErrQueuedEntryDeleting),
			status: http.StatusConflict,
		},
		{
			name:   "invalid",
			err:    errors.New("invalid torrent"),
			status: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, idempotent := torrentAddErrorStatus(test.err)
			if status != test.status || idempotent != test.idempotent {
				t.Fatalf(
					"torrentAddErrorStatus() = (%d, %t), want (%d, %t)",
					status,
					idempotent,
					test.status,
					test.idempotent,
				)
			}
		})
	}
}

func fmtWrap(err error) error {
	return errors.Join(errors.New("wrapped"), err)
}
