package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
	strmurl "github.com/sirrobot01/decypharr/pkg/strm"
)

// StreamRoutes serves the stable identities embedded in generated .strm
// files. A valid HMAC is mandatory even when WebDAV or UI auth is disabled.
func (h *Handler) StreamRoutes() chi.Router {
	router := chi.NewRouter()
	router.Use(h.readinessMiddleware)
	router.Get("/v1/{infohash}/{fileID}/{name}", h.handleIdentityStream)
	router.Head("/v1/{infohash}/{fileID}/{name}", h.handleIdentityStream)
	return router
}

func (h *Handler) handleIdentityStream(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	infohash := chi.URLParam(r, "infohash")
	fileID := chi.URLParam(r, "fileID")
	if !cfg.Strm.Active() || !strmurl.Verify(cfg.Strm.Secret, "stream", r.URL.Query().Get("s"), infohash, fileID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	entry, err := h.manager.GetEntry(infohash)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	file, err := entry.GetFileByID(fileID)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Hash storage identities before placing them in a response header. The
	// source fields can originate with a remote provider and are not suitable
	// for direct inclusion in header syntax.
	etagIdentity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", infohash, fileID, file.Size)))
	etag := "\"strm-v1-" + hex.EncodeToString(etagIdentity[:16]) + "\""
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", utils.GetContentType(file.Name))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !file.AddedOn.IsZero() {
		w.Header().Set("Last-Modified", file.AddedOn.UTC().Format(http.TimeFormat))
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Metadata probes must not consume provider calls or NNTP connections.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	if cfg.Strm.DeliveryMode == config.StrmDeliveryRedirect && entry.IsTorrent() {
		link, linkErr := h.manager.GetDownloadLink(r.Context(), entry, file.Name)
		if linkErr == nil && link.DownloadLink != "" {
			http.Redirect(w, r, link.DownloadLink, http.StatusTemporaryRedirect)
			return
		}
		if linkErr != nil {
			h.logger.Rate(infohash+"/"+fileID).Warn().Err(linkErr).
				Msgf("STRM redirect failed; proxying %s", file.Name)
		}
	}

	if err := h.streamIdentityFile(entry, file, w, r); err != nil {
		h.writeStreamError(infohash+"/"+file.Name, err, w)
	}
}

func (h *Handler) streamIdentityFile(entry *storage.Entry, file *storage.File, w http.ResponseWriter, r *http.Request) error {
	start, end := int64(0), int64(-1)
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		ranges, err := parseRange(rangeHeader, file.Size)
		if err != nil || len(ranges) != 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return customerror.NewError(
				fmt.Errorf("invalid or unsupported byte range"),
				http.StatusRequestedRangeNotSatisfiable,
				"stream.invalid_range",
				true,
				true,
			)
		}
		start, end = ranges[0].start, ranges[0].end
	}

	client := r.UserAgent()
	if client == "" {
		client = "STRM"
	}
	streamID := h.manager.TrackStream(entry, file.Name, client)
	if streamID != "" {
		defer h.manager.UntrackStream(streamID)
	}

	headersWritten := false
	err := h.manager.Stream(r.Context(), entry, file.Name, start, end, w, func(meta *manager.StreamMetadata) error {
		if err := h.handleSuccessfulResponse(w, meta, start, end); err != nil {
			return err
		}
		headersWritten = true
		return nil
	}, client)
	if err == nil {
		return nil
	}
	return normalizeStreamError(err, headersWritten)
}

func (h *Handler) writeStreamError(logKey string, err error, w http.ResponseWriter) {
	var streamErr *customerror.Error
	if errors.As(err, &streamErr) {
		if streamErr.IsSilent() {
			return
		}
		if !streamErr.HeadersWritten {
			status := streamErr.HTTPStatus()
			if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
				w.Header().Set("Retry-After", "5")
			}
			http.Error(w, http.StatusText(status), status)
		}
		if h.logger != nil {
			h.logger.Rate(logKey).Error().Err(err).Msgf("Error streaming file: %s", logKey)
		}
		return
	}
	if customerror.IsSilentError(err) {
		return
	}
	if h.logger != nil {
		h.logger.Rate(logKey).Error().Err(err).Msgf("Error streaming file: %s", logKey)
	}
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}
