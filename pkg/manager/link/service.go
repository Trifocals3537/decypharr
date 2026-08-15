package link

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/cdntraffic"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"golang.org/x/sync/singleflight"
)

const (
	MaxReinsertionAttempt = 3
	maxLinkRefreshes      = 1
	maxValidatedEntries   = 8192
	maxLinkRetryWait      = 30 * time.Second
	refreshBackoffBase    = 30 * time.Second
	refreshBackoffMax     = 5 * time.Minute
	maxRefreshBackoffs    = 4096
)

var (
	emptyDownloadLink = types.DownloadLink{}
)

// EntryRefresher is a function that refreshes an entry by infohash
type EntryRefresher func(infohash string) (*storage.Entry, error)
type EntryRepairer func(ctx context.Context, entry *storage.Entry) error
type EntrySaver func(entry *storage.Entry) error
type ProviderTypeResolver func(provider string) string
type cachedLinkInvalidator interface {
	InvalidateCachedLink(types.DownloadLink) error
}

type refreshBackoffState struct {
	failures    int
	nextAttempt time.Time
}

// Service handles download link fetching and validation.
// It uses the account-level cache for storing links and only tracks validation state.
type Service struct {
	validated       *xsync.Map[string, struct{}]
	singleflight    singleflight.Group
	refreshflight   singleflight.Group
	refreshMu       sync.Mutex
	refreshBackoffs map[string]refreshBackoffState
	clients         *xsync.Map[string, debrid.Client]
	entryRefresher  EntryRefresher
	repairer        EntryRepairer
	entrySaver      EntrySaver
	providerType    ProviderTypeResolver
	httpClient      *http.Client
	retries         int
	wait            func(context.Context, time.Duration) error
	now             func() time.Time
	logger          zerolog.Logger
}

// New creates a new LinkService
func New(
	clients *xsync.Map[string, debrid.Client],
	entryRefresher EntryRefresher,
	entryReinsert EntryRepairer,
	entrySaver EntrySaver,
	httpClient *http.Client,
	retries int,
	logger zerolog.Logger,
	providerTypes ...ProviderTypeResolver,
) *Service {
	var providerType ProviderTypeResolver
	if len(providerTypes) > 0 {
		providerType = providerTypes[0]
	}
	return &Service{
		validated:       xsync.NewMap[string, struct{}](),
		refreshBackoffs: make(map[string]refreshBackoffState),
		clients:         clients,
		entryRefresher:  entryRefresher,
		repairer:        entryReinsert,
		entrySaver:      entrySaver,
		providerType:    providerType,
		httpClient:      httpClient,
		retries:         retries,
		wait:            waitWithContext,
		now:             time.Now,
		logger:          logger,
	}
}

// GetLink fetches and validates a download link for a file in an entry.
// Links are cached at the account level; this service only tracks validation state.
func (s *Service) GetLink(ctx context.Context, entry *storage.Entry, filename string) (types.DownloadLink, error) {
	// Use singleflight to deduplicate concurrent requests for the same file
	key := linkLifecycleKey(entry, filename)
	v, err, _ := s.singleflight.Do(key, func() (any, error) {
		return s.fetchAndValidate(ctx, entry, filename, 0, 0)
	})

	if err != nil {
		return emptyDownloadLink, err
	}

	return v.(types.DownloadLink), nil
}

// Refresh evicts a rejected URL from local caches and returns a validated
// replacement. Concurrent refreshes for the same file are coalesced, and a
// refresh never calls a provider-side deletion endpoint.
func (s *Service) Refresh(ctx context.Context, entry *storage.Entry, bad types.DownloadLink) (types.DownloadLink, error) {
	if bad.Filename == "" {
		return emptyDownloadLink, NewPermanentError(ErrEmptyLink, "empty_link")
	}

	return s.refreshRejectedLink(ctx, entry, bad, 0, 0)
}

func (s *Service) getClient(provider string) (debrid.Client, error) {
	c, ok := s.clients.Load(provider)
	if !ok {
		return nil, fmt.Errorf("client for provider %s not found", provider)
	}
	return c, nil
}

func (s *Service) withCDNIdentity(ctx context.Context, link *types.DownloadLink) context.Context {
	identity := cdntraffic.Identity{}
	if link != nil {
		identity.Provider = link.Debrid
		identity.AccountToken = link.Token
	}
	if identity.Provider != "" && s.providerType != nil {
		identity.ProviderType = s.providerType(identity.Provider)
	}
	return cdntraffic.WithIdentity(ctx, identity)
}

// fetchAndValidate fetches a download link and validates it.
// repairAttempt tracks how many re-insertion cycles we've already paid for during
// this GetLink call so we can bail out instead of looping forever when the
// underlying file never resolves (see fetchLink/handleBadLink).
func (s *Service) fetchAndValidate(ctx context.Context, entry *storage.Entry, filename string, repairAttempt, linkRefreshes int) (types.DownloadLink, error) {
	if err := ctx.Err(); err != nil {
		return emptyDownloadLink, err
	}
	link, err := s.fetchLink(ctx, entry, filename, repairAttempt, linkRefreshes)
	if err != nil {
		return s.handleBadLink(ctx, err, entry, link, repairAttempt, linkRefreshes)
	}

	// Only successful validations are memoized. Transient failures must be
	// allowed to recover on the next request.
	if _, exists := s.validated.Load(validationKey(link)); exists {
		s.clearRefreshBackoff(linkLifecycleKey(entry, filename))
		return link, nil
	}

	validationErr := s.validateWithRetry(ctx, &link)

	if validationErr != nil {
		// Handle link error categories
		if linkErr := GetLinkError(validationErr); linkErr != nil {
			if linkErr.ShouldDisableAccount() {
				hasAlternate, err := s.disableLinkAccount(link, linkErr)
				if err != nil {
					s.logger.Error().
						Err(err).
						Str("debrid", link.Debrid).
						Str("token", utils.Mask(link.Token)).
						Str("reason", linkErr.Code).
						Msg("Failed to disable account after link error")
					return emptyDownloadLink, err
				}
				if !hasAlternate {
					return emptyDownloadLink, NewPermanentError(ErrNoActiveAccount, "no_active_account")
				}
				// This will use the next available account and fetch a new link, so we need to refetch and revalidate.
				// Account swap doesn't consume a re-insertion attempt.
				return s.fetchAndValidate(ctx, entry, filename, repairAttempt, linkRefreshes)
			} else if linkErr.ShouldRefetch() {
				if linkRefreshes >= maxLinkRefreshes {
					// Preserve the refetchable error. The outer refresh governor records
					// the failed replacement and suppresses another provider regeneration
					// until its bounded cooldown expires.
					return emptyDownloadLink, validationErr
				}
				return s.refreshRejectedLink(ctx, entry, link, repairAttempt, linkRefreshes)
			}
		}
		return emptyDownloadLink, validationErr
	}

	if s.validated.Size() >= maxValidatedEntries {
		s.validated.Clear()
	}
	s.validated.Store(validationKey(link), struct{}{})
	s.clearRefreshBackoff(linkLifecycleKey(entry, filename))
	return link, nil
}

// refreshRejectedLink coalesces provider refreshes across validation and stream
// recovery paths. A rejected replacement starts an adaptive per-file cooldown:
// callers continue probing the cached CDN URL for recovery, but do not repeatedly
// regenerate provider links while that URL remains rejected.
func (s *Service) refreshRejectedLink(ctx context.Context, entry *storage.Entry, rejected types.DownloadLink, repairAttempt, linkRefreshes int) (types.DownloadLink, error) {
	key := linkLifecycleKey(entry, rejected.Filename)
	v, err, _ := s.refreshflight.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return emptyDownloadLink, err
		}
		if delay, blocked := s.refreshDelay(key); blocked {
			linkErr := NewLinkError(
				fmt.Errorf("download link refresh deferred for %s", delay.Round(time.Millisecond)),
				CategoryThrottled,
				"link_refresh_cooldown",
			)
			linkErr.RetryAfter = delay
			return emptyDownloadLink, linkErr
		}
		if err := s.invalidateCachedLink(rejected); err != nil {
			return emptyDownloadLink, err
		}

		replacement, err := s.fetchAndValidate(ctx, entry, rejected.Filename, repairAttempt, linkRefreshes+1)
		if err != nil {
			if linkErr := GetLinkError(err); linkErr != nil && linkErr.ShouldRefetch() &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				delay, failures := s.recordRefreshFailure(key)
				s.logger.Warn().
					Str("debrid", entry.ActiveProvider).
					Str("infohash", entry.InfoHash).
					Str("filename", rejected.Filename).
					Int("failures", failures).
					Dur("retry_after", delay).
					Msg("Download link replacement was rejected; deferring provider refresh")
			}
			return emptyDownloadLink, err
		}

		s.clearRefreshBackoff(key)
		return replacement, nil
	})
	if err != nil {
		return emptyDownloadLink, err
	}
	return v.(types.DownloadLink), nil
}

func linkLifecycleKey(entry *storage.Entry, filename string) string {
	placementID := ""
	if placement := entry.Providers[entry.ActiveProvider]; placement != nil {
		placementID = placement.ID
	}
	return entry.ActiveProvider + "\x00" + entry.InfoHash + "\x00" + placementID + "\x00" + filename
}

func (s *Service) refreshDelay(key string) (time.Duration, bool) {
	now := s.now()
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	state, exists := s.refreshBackoffs[key]
	if !exists || !now.Before(state.nextAttempt) {
		return 0, false
	}
	return state.nextAttempt.Sub(now), true
}

func (s *Service) recordRefreshFailure(key string) (time.Duration, int) {
	now := s.now()
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	state, exists := s.refreshBackoffs[key]
	if !exists && len(s.refreshBackoffs) >= maxRefreshBackoffs {
		s.evictOldestRefreshBackoffLocked()
	}
	state.failures++
	delay := refreshBackoffDelay(state.failures)
	state.nextAttempt = now.Add(delay)
	s.refreshBackoffs[key] = state
	return delay, state.failures
}

func (s *Service) clearRefreshBackoff(key string) {
	s.refreshMu.Lock()
	delete(s.refreshBackoffs, key)
	s.refreshMu.Unlock()
}

func (s *Service) evictOldestRefreshBackoffLocked() {
	var oldestKey string
	var oldestTime time.Time
	found := false
	for key, state := range s.refreshBackoffs {
		if !found || state.nextAttempt.Before(oldestTime) {
			oldestKey = key
			oldestTime = state.nextAttempt
			found = true
		}
	}
	if found {
		delete(s.refreshBackoffs, oldestKey)
	}
}

func refreshBackoffDelay(failures int) time.Duration {
	delay := refreshBackoffBase
	for attempt := 1; attempt < failures && delay < refreshBackoffMax; attempt++ {
		if delay >= refreshBackoffMax/2 {
			return refreshBackoffMax
		}
		delay *= 2
	}
	if delay > refreshBackoffMax {
		return refreshBackoffMax
	}
	return delay
}

func (s *Service) handleBadLink(ctx context.Context, err error, entry *storage.Entry, dl types.DownloadLink, repairAttempt, linkRefreshes int) (types.DownloadLink, error) {
	if errors.Is(err, customerror.HosterUnavailableError) {
		if entry.Bad {
			return emptyDownloadLink, fmt.Errorf("can't repair %s since it's been marked as bad", entry.GetFolder())
		}
		if repairAttempt >= MaxReinsertionAttempt {
			s.markEntryBad(entry, dl.Filename, repairAttempt, "hoster_unavailable")
			return emptyDownloadLink, fmt.Errorf("entry %s file %s still unresolvable after %d re-insertion attempts", entry.GetFolder(), dl.Filename, repairAttempt)
		}
		if err := s.repairer(ctx, entry); err != nil {
			return emptyDownloadLink, err
		}

		if entry.Bad {
			// Entry is still bad
			return emptyDownloadLink, fmt.Errorf("entry %s(%s) still bad after repair, un-repairable", entry.GetFolder(), dl.Link)
		}
		// Bypass singleflight re-entry to avoid deadlock
		return s.fetchAndValidate(ctx, entry, dl.Filename, repairAttempt+1, linkRefreshes)
	}
	// Just return the error
	return dl, err
}

// markEntryBad sets entry.Bad and persists it so subsequent GetLink calls
// for the same entry short-circuit instead of triggering another re-insertion
// cycle. Logged once per call.
func (s *Service) markEntryBad(entry *storage.Entry, filename string, attempt int, reason string) {
	entry.Bad = true
	if s.entrySaver != nil {
		if err := s.entrySaver(entry); err != nil {
			s.logger.Warn().
				Err(err).
				Str("infohash", entry.InfoHash).
				Msg("Failed to persist Bad flag after exhausting re-insertion attempts")
		}
	}
	s.logger.Warn().
		Str("infohash", entry.InfoHash).
		Str("name", entry.Name).
		Str("filename", filename).
		Int("attempts", attempt).
		Str("reason", reason).
		Msg("Giving up on entry after repeated failed re-insertions")
}

// fetchLink fetches a download link from the debrid provider (via account cache)
func (s *Service) fetchLink(ctx context.Context, entry *storage.Entry, filename string, attempt, linkRefreshes int) (types.DownloadLink, error) {
	file, err := entry.GetFile(filename)
	if err != nil {
		return emptyDownloadLink, NewPermanentError(
			fmt.Errorf("file %s not found in entry %s: %w", filename, entry.Name, err),
			"file_not_found",
		)
	}

	placementFile, err := s.getPlacementFile(entry, filename)
	if err != nil {
		return emptyDownloadLink, err
	}

	if placementFile.Link == "" && placementFile.Id == "" {
		return emptyDownloadLink, NewPermanentError(
			fmt.Errorf("file link is missing for %s in entry %s", filename, entry.Name),
			"link_missing",
		)
	}

	client, err := s.getClient(entry.ActiveProvider)
	if err != nil {
		return emptyDownloadLink, NewPermanentError(
			fmt.Errorf("debrid client not found: %s", entry.ActiveProvider),
			"client_not_found",
		)
	}

	placement := entry.Providers[entry.ActiveProvider]
	if placement == nil {
		return emptyDownloadLink, NewPermanentError(
			fmt.Errorf("no placement found for debrid %s with infohash %s", entry.ActiveProvider, entry.InfoHash),
			"placement_not_found",
		)
	}

	debridFile := &types.File{
		Id:        placementFile.Id,
		Link:      placementFile.Link,
		Path:      placementFile.Path,
		Name:      file.Name,
		Size:      file.Size,
		ByteRange: file.ByteRange,
		Deleted:   file.Deleted,
	}

	// This uses account-level caching internally
	downloadLink, err := client.GetDownloadLink(placement.ID, debridFile)
	if err != nil {
		return downloadLink, err
	}

	if downloadLink.Empty() {
		// Let's try to reinsert the entry
		if entry.Bad {
			return emptyDownloadLink, fmt.Errorf("can't repair %s since it's been marked as bad", entry.GetFolder())
		}
		if attempt >= MaxReinsertionAttempt {
			s.markEntryBad(entry, filename, attempt, "empty_link")
			return emptyDownloadLink, fmt.Errorf("entry %s file %s still resolves to an empty link after %d re-insertion attempts", entry.GetFolder(), filename, attempt)
		}
		if err := s.repairer(ctx, entry); err != nil {
			return emptyDownloadLink, err
		}

		if entry.Bad {
			// Entry is still bad
			return emptyDownloadLink, fmt.Errorf("entry %s(%s) still bad after repair, un-repairable", entry.GetFolder(), downloadLink.Link)
		}
		// Bypass singleflight re-entry to avoid deadlock
		return s.fetchAndValidate(ctx, entry, filename, attempt+1, linkRefreshes)
	}

	return downloadLink, nil
}

// getPlacementFile retrieves the placement file with refresh fallback
func (s *Service) getPlacementFile(entry *storage.Entry, filename string) (*storage.ProviderFile, error) {
	_, ok := entry.Files[filename]
	if !ok {
		return nil, NewPermanentError(
			fmt.Errorf("file %s not found in entry", filename),
			"file_not_found",
		)
	}

	placement := entry.Providers[entry.ActiveProvider]
	if placement == nil {
		return nil, NewPermanentError(
			fmt.Errorf("no placement found for debrid %s with infohash %s", entry.ActiveProvider, entry.InfoHash),
			"placement_not_found",
		)
	}

	placementFile := placement.Files[filename]
	if placementFile == nil || (placementFile.Link == "" && placementFile.Id == "") {
		if s.entryRefresher == nil {
			return nil, NewPermanentError(
				fmt.Errorf("file %s not available and no refresher configured", filename),
				"no_refresher",
			)
		}

		refreshed, err := s.entryRefresher(entry.InfoHash)
		if err != nil {
			return nil, NewRefetchableError(
				fmt.Errorf("failed to refresh entry: %w", err),
				"refresh_failed",
			)
		}

		file := refreshed.Files[filename]
		if file == nil {
			return nil, NewPermanentError(
				fmt.Errorf("file disappeared after refresh"),
				"file_disappeared",
			)
		}

		placement = refreshed.Providers[entry.ActiveProvider]
		if placement == nil {
			return nil, NewPermanentError(
				fmt.Errorf("placement disappeared after refresh for debrid %s", entry.ActiveProvider),
				"placement_disappeared",
			)
		}

		placementFile = placement.Files[filename]
		if placementFile == nil || (placementFile.Link == "" && placementFile.Id == "") {
			return nil, NewPermanentError(
				fmt.Errorf("file %s not available after refresh", filename),
				"file_not_available",
			)
		}

		*entry = *refreshed
	}

	return placementFile, nil
}

// validateLink validates a download link by making a HEAD request
func (s *Service) validateLink(ctx context.Context, link *types.DownloadLink) error {
	if link == nil {
		return NewPermanentError(ErrEmptyLink, "empty_link")
	}
	if link.Empty() {
		return NewPermanentError(fmt.Errorf("download url is empty for %s||%s", link.Filename, link.Link), "empty_link")
	}
	ctx = s.withCDNIdentity(ctx, link)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link.DownloadLink, nil)
	if err != nil {
		return NewPermanentError(
			fmt.Errorf("failed to create HEAD request: %w", err),
			"request_creation_failed",
		)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return NewRetryableError(
			fmt.Errorf("HEAD request failed: %w", err),
			"network_error",
		)
	}
	resp.Body.Close()

	// Some CDNs reject HEAD even though byte-range GETs are supported. Probe a
	// single byte in that case so validation does not discard a healthy link.
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, link.DownloadLink, nil)
		if err != nil {
			return NewPermanentError(fmt.Errorf("failed to create range probe: %w", err), "request_creation_failed")
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err = s.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return NewRetryableError(fmt.Errorf("range probe failed: %w", err), "network_error")
		}
		resp.Body.Close()
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	errorCode := resp.Header.Get("X-Error")
	if errorCode != "" {
		return ErrorCodeToLinkError(errorCode)
	}
	return ClassifyHTTPStatus(resp.StatusCode, resp.Header)
}

func (s *Service) validateWithRetry(ctx context.Context, link *types.DownloadLink) error {
	attempts := max(1, s.retries+1)
	remainingWait := maxLinkRetryWait
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		lastErr = s.validateLink(ctx, link)
		if lastErr == nil {
			return nil
		}
		linkErr := GetLinkError(lastErr)
		if linkErr == nil || (!linkErr.ShouldRetry() && !linkErr.ShouldBackoff()) || attempt+1 >= attempts {
			return lastErr
		}

		delay := linkRetryDelay(linkErr, attempt, remainingWait)
		if delay <= 0 {
			return lastErr
		}
		if err := s.wait(ctx, delay); err != nil {
			return err
		}
		remainingWait -= delay
	}
	return lastErr
}

func linkRetryDelay(linkErr *Error, attempt int, remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	delay := 500 * time.Millisecond
	if linkErr != nil && linkErr.ShouldBackoff() && linkErr.RetryAfter > 0 {
		delay = linkErr.RetryAfter
	} else {
		for range attempt {
			if delay >= maxLinkRetryWait/2 {
				delay = maxLinkRetryWait
				break
			}
			delay *= 2
		}
	}
	if delay > maxLinkRetryWait {
		delay = maxLinkRetryWait
	}
	if delay > remaining {
		delay = remaining
	}
	return delay
}

func waitWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validationKey(link types.DownloadLink) string {
	return link.DownloadLink + "\x00" +
		link.Generated.UTC().Format(time.RFC3339Nano) + "\x00" +
		link.ExpiresAt.UTC().Format(time.RFC3339Nano)
}

// disableLinkAccount handles errors that require disabling an account
func (s *Service) disableLinkAccount(link types.DownloadLink, linkErr *Error) (bool, error) {
	client, err := s.getClient(link.Debrid)
	if err != nil {
		return false, fmt.Errorf("failed to get client for debrid %s: %w", link.Debrid, err)
	}

	accountManager := client.AccountManager()
	if accountManager == nil {
		return false, fmt.Errorf("account manager not available for debrid %s", link.Debrid)
	}
	account, err := accountManager.GetAccount(link.Token)
	if err != nil {
		return false, fmt.Errorf("failed to get account for token %s: %w", utils.Mask(link.Token), err)
	}

	if account == nil {
		return false, fmt.Errorf("account not found for token %s", utils.Mask(link.Token))
	}

	accountManager.Disable(account)

	// Remove all validations for all the links
	s.validated.Clear()
	s.logger.Warn().
		Str("debrid", link.Debrid).
		Str("token", utils.Mask(account.Token)).
		Str("account", utils.Mask(account.Username)).
		Str("reason", linkErr.Code).
		Msg("Disabled account due to error")
	return len(accountManager.Active()) > 0, nil
}

// invalidateCachedLink removes only local validation and account-cache state.
func (s *Service) invalidateCachedLink(link types.DownloadLink) error {
	s.validated.Delete(validationKey(link))

	if link.Debrid == "" {
		return fmt.Errorf("invalid link")
	}

	client, err := s.getClient(link.Debrid)
	if err != nil {
		return err
	}
	if invalidator, ok := client.(cachedLinkInvalidator); ok {
		return invalidator.InvalidateCachedLink(link)
	}
	accounts := client.AccountManager()
	if accounts == nil {
		return fmt.Errorf("account manager not available for debrid %s", link.Debrid)
	}
	return accounts.InvalidateDownloadLink(link)
}

// Clear removes all validation and refresh-backoff tracking entries.
func (s *Service) Clear() {
	s.validated.Clear()
	s.refreshMu.Lock()
	clear(s.refreshBackoffs)
	s.refreshMu.Unlock()
}
