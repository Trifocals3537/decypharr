package qbit

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (q *QBit) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := config.Get()
	username := r.FormValue("username")
	password := r.FormValue("password")
	a, err := q.authenticate(getCategory(ctx), username, password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if cfg.UseAuth {
		http.SetCookie(w, newSIDCookie(createSID(a.Host, a.Token)))
	}
	_, _ = w.Write([]byte("Ok."))
}

func newSIDCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     "SID",
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (q *QBit) handleVersion(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("v4.3.2"))
}

func (q *QBit) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("2.7"))
}

func (q *QBit) handlePreferences(w http.ResponseWriter, r *http.Request) {
	preferences := getAppPreferences()

	preferences.SavePath = q.downloadFolder
	preferences.TempPath = filepath.Join(q.downloadFolder, "temp")

	utils.JSONResponse(w, preferences, http.StatusOK)
}

func (q *QBit) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	res := BuildInfo{
		Bitness:    64,
		Boost:      "1.75.0",
		Libtorrent: "1.2.11.0",
		Openssl:    "1.1.1i",
		Qt:         "5.15.2",
		Zlib:       "1.2.11",
	}
	utils.JSONResponse(w, res, http.StatusOK)
}

func (q *QBit) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleTorrentsInfo(w http.ResponseWriter, r *http.Request) {
	//log all url params
	ctx := r.Context()
	category := getCategory(ctx)
	state := strings.Trim(r.URL.Query().Get("filter"), "")
	hashes := getHashes(ctx)

	// Convert hashes to filter function
	torrents := q.manager.Queue().ListFilter(category, config.ProtocolTorrent, storage.TorrentState(state), hashes, "added_on", false)
	qbitTorrents := make([]Torrent, len(torrents))
	for i, t := range torrents {
		qbitTorrents[i] = convertToQBitTorrentTorrent(t)
	}
	utils.JSONResponse(w, qbitTorrents, http.StatusOK)
}

func (q *QBit) handleTorrentsAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse form based on content type
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := utils.ParseMultipartFormBounded(
			w,
			r,
			utils.MaxImportRequestBytes,
			utils.MaxMultipartMemoryBytes,
		); err != nil {
			if utils.IsRequestTooLarge(err) {
				http.Error(w, "Request is too large", http.StatusRequestEntityTooLarge)
				return
			}
			q.logger.Error().Err(err).Msg("Error parsing multipart form")
			http.Error(w, "Invalid multipart form", http.StatusBadRequest)
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		if utils.MultipartFormPartCount(r.MultipartForm) > utils.MaxMultipartFormParts {
			http.Error(w, "Request has too many multipart fields", http.StatusRequestEntityTooLarge)
			return
		}
	} else if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := utils.ParseFormBounded(w, r, utils.MaxImportRequestBytes); err != nil {
			if utils.IsRequestTooLarge(err) {
				http.Error(w, "Request is too large", http.StatusRequestEntityTooLarge)
				return
			}
			q.logger.Error().Err(err).Msg("Error parsing form")
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}
	} else {
		http.Error(w, "Invalid content type", http.StatusBadRequest)
		return
	}

	itemCount := 0
	if urls := r.FormValue("urls"); urls != "" {
		for rawURL := range strings.SplitSeq(urls, "\n") {
			if strings.TrimSpace(rawURL) != "" {
				itemCount++
			}
		}
	}
	if r.MultipartForm != nil {
		itemCount += len(r.MultipartForm.File["torrents"])
	}
	if itemCount > utils.MaxImportItems {
		http.Error(
			w,
			fmt.Sprintf("Request exceeds the %d-item limit", utils.MaxImportItems),
			http.StatusRequestEntityTooLarge,
		)
		return
	}

	cfg := config.Get()
	action := cfg.DefaultDownloadAction
	if strings.ToLower(r.FormValue("sequentialDownload")) == "true" {
		action = config.DownloadActionDownload
	}

	rmTrackerUrls := strings.ToLower(r.FormValue("firstLastPiecePrio")) == "true"

	// Check config setting - if always remove tracker URLs is enabled, force it to true
	if q.alwaysRemoveTrackerURLS {
		rmTrackerUrls = true
	}

	debridName := r.FormValue("debrid")
	category := r.FormValue("category")
	_arr := getArrFromContext(ctx)
	if _arr == nil {
		// Arr is not in context
		_arr = arr.New(category, "", "", false, nil, "", "")
	}
	atleastOne := false
	var retainedBytes int64

	// Handle magnet URLs
	if urls := r.FormValue("urls"); urls != "" {
		for u := range strings.SplitSeq(urls, "\n") {
			rawURL := strings.TrimSpace(u)
			if rawURL == "" {
				continue
			}
			remainingBytes := utils.MaxImportRequestBytes - retainedBytes
			maxBytes := min(utils.MaxMetadataFileBytes, remainingBytes)
			if maxBytes <= 0 {
				http.Error(w, "Request metadata exceeds the byte limit", http.StatusRequestEntityTooLarge)
				return
			}
			itemBytes, err := q.addMagnet(
				ctx,
				rawURL,
				_arr,
				debridName,
				action,
				cfg.Notifications.CallbackURL,
				rmTrackerUrls,
				cfg.SkipMultiSeason,
				maxBytes,
			)
			retainedBytes += itemBytes
			if err != nil {
				q.logger.Debug().Err(err).Msg("Error adding magnet")
				status, idempotent := torrentAddErrorStatus(err)
				if idempotent {
					atleastOne = true
					continue
				}
				if errors.Is(err, utils.ErrContentTooLarge) {
					http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
					return
				}
				if status == http.StatusTooManyRequests ||
					status == http.StatusServiceUnavailable {
					w.Header().Set("Retry-After", "5")
				}
				http.Error(w, err.Error(), status)
				return
			}
			atleastOne = true
		}
	}

	// Handle torrent files
	if r.MultipartForm != nil && r.MultipartForm.File != nil {
		if files := r.MultipartForm.File["torrents"]; len(files) > 0 {
			for _, fileHeader := range files {
				remainingBytes := utils.MaxImportRequestBytes - retainedBytes
				maxBytes := min(utils.MaxMetadataFileBytes, remainingBytes)
				if maxBytes <= 0 || fileHeader.Size > maxBytes {
					http.Error(w, "Request metadata exceeds the byte limit", http.StatusRequestEntityTooLarge)
					return
				}
				itemBytes, err := q.addTorrent(
					ctx,
					fileHeader,
					_arr,
					debridName,
					action,
					cfg.Notifications.CallbackURL,
					rmTrackerUrls,
					cfg.SkipMultiSeason,
					maxBytes,
				)
				retainedBytes += itemBytes
				if err != nil {
					q.logger.Debug().Err(err).Str("torrent", fileHeader.Filename).Msgf("Error adding torrent")
					status, idempotent := torrentAddErrorStatus(err)
					if idempotent {
						atleastOne = true
						continue
					}
					if errors.Is(err, utils.ErrContentTooLarge) {
						http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
						return
					}
					if status == http.StatusTooManyRequests ||
						status == http.StatusServiceUnavailable {
						w.Header().Set("Retry-After", "5")
					}
					http.Error(w, err.Error(), status)
					return
				}
				atleastOne = true
			}
		}
	}

	if !atleastOne {
		http.Error(w, "No valid URLs or torrents provided", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func torrentAddErrorStatus(err error) (status int, idempotent bool) {
	switch {
	case errors.Is(err, manager.ErrJobQueueDuplicate):
		// qBittorrent treats re-adding an already admitted hash as a successful
		// idempotent operation.
		return http.StatusOK, true
	case errors.Is(err, manager.ErrJobQueueFull):
		return http.StatusTooManyRequests, false
	case errors.Is(err, manager.ErrJobQueueClosed):
		return http.StatusServiceUnavailable, false
	case errors.Is(err, manager.ErrQueueEntryDeleting),
		errors.Is(err, storage.ErrQueuedEntryDeleting):
		return http.StatusConflict, false
	default:
		return http.StatusBadRequest, false
	}
}

func (q *QBit) handleTorrentsDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hashes := getHashes(ctx)

	if len(hashes) == 0 {
		http.Error(w, "No hashes provided", http.StatusBadRequest)
		return
	}
	for _, hash := range hashes {
		err := q.manager.Queue().Delete(hash, nil)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleTorrentsPause(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hashes := getHashes(ctx)
	for _, hash := range hashes {
		torrent, err := q.manager.Queue().GetTorrent(hash)
		if err != nil {
			continue
		}
		go q.PauseTorrent(torrent)
	}

	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleTorrentsResume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hashes := getHashes(ctx)
	for _, hash := range hashes {
		torrent, err := q.manager.Queue().GetTorrent(hash)
		if err != nil {
			continue
		}
		go q.ResumeTorrent(torrent)
	}

	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleTorrentRecheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hashes := getHashes(ctx)
	for _, hash := range hashes {
		torrent, err := q.manager.Queue().GetTorrent(hash)
		if err != nil {
			continue
		}
		go q.RefreshTorrent(torrent)
	}

	w.WriteHeader(http.StatusOK)
}

func (q *QBit) handleCategories(w http.ResponseWriter, r *http.Request) {
	var categories = map[string]TorrentCategory{}
	for _, cat := range q.categories {
		path := filepath.Join(q.downloadFolder, cat)
		categories[cat] = TorrentCategory{
			Name:     cat,
			SavePath: path,
		}
	}
	utils.JSONResponse(w, categories, http.StatusOK)
}

func (q *QBit) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}

	name := r.Form.Get("category")
	if name == "" {
		http.Error(w, "No name provided", http.StatusBadRequest)
		return
	}

	q.categories = append(q.categories, name)

	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleTorrentProperties(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	torrent, err := q.manager.Queue().GetTorrent(hash)
	if err != nil {
		http.Error(w, "Entry not found", http.StatusNotFound)
		return
	}

	properties := q.GetTorrentProperties(torrent)
	utils.JSONResponse(w, properties, http.StatusOK)
}

func (q *QBit) handleTorrentFiles(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	torrent, err := q.manager.Queue().GetTorrent(hash)
	if err != nil {
		http.Error(w, "Entry not found", http.StatusNotFound)
		return
	}
	utils.JSONResponse(w, getTorrentFiles(torrent), http.StatusOK)
}

func (q *QBit) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	category := getCategory(ctx)
	hashes := getHashes(ctx)
	var filterFunc func(t *storage.Entry) bool

	hashSet := make(map[string]bool)
	if len(hashes) > 0 {
		for _, h := range hashes {
			hashSet[h] = true
		}

	}

	updateFunc := func(t *storage.Entry) bool {
		if t.Category != category {
			t.Category = category
			return true
		}
		return false
	}

	if err := q.manager.Queue().UpdateWhere(filterFunc, updateFunc); err != nil {
		q.logger.Warn().Err(err).Msgf("Error adding torrent")
		http.Error(w, "Failed to update torrents", http.StatusInternalServerError)
		return
	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleAddTorrentTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	hashes := getHashes(ctx)
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	torrents := q.manager.Queue().ListFilter("", config.ProtocolTorrent, "", hashes, "", false)
	for _, t := range torrents {
		q.setTorrentTags(t, tags)
	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleRemoveTorrentTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	hashes := getHashes(ctx)
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	torrents := q.manager.Queue().ListFilter("", config.ProtocolTorrent, "", hashes, "", false)
	for _, torrent := range torrents {
		q.removeTorrentTags(torrent, tags)

	}
	utils.JSONResponse(w, nil, http.StatusOK)
}

func (q *QBit) handleGetTags(w http.ResponseWriter, r *http.Request) {
	utils.JSONResponse(w, q.Tags, http.StatusOK)
}

func (q *QBit) handleCreateTags(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}
	tags := strings.Split(r.FormValue("tags"), ",")
	for i, tag := range tags {
		tags[i] = strings.TrimSpace(tag)
	}
	q.addTags(tags)
	utils.JSONResponse(w, nil, http.StatusOK)
}
