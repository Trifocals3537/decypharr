package debridlink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	json "github.com/bytedance/sonic"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"go.uber.org/ratelimit"
)

const (
	debridLinkProfileCacheDuration = time.Hour
	debridLinkMaxPremiumSeconds    = int64((1<<63 - 1) / int64(time.Second))
)

type DebridLink struct {
	Host             string `json:"host"`
	APIKey           string
	accountsManager  *account.Manager
	DownloadUncached bool
	client           *request.Client
	repairClient     *request.Client

	autoExpiresLinksAfter time.Duration
	logger                zerolog.Logger
	config                config.Debrid

	Profile            *types.Profile `json:"profile,omitempty"`
	profileLastFetched time.Time
}

func New(dc config.Debrid, ratelimits map[string]ratelimit.Limiter) (*DebridLink, error) {
	cfg := config.Get()
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	}
	log := logger.New(dc.Name)

	opts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithRateLimiter(ratelimits["main"]),
		request.WithMaxRetries(cfg.Retries),
		request.WithRetryableStatus(http.StatusTooManyRequests, http.StatusBadGateway),
	}
	if dc.Proxy != "" {
		opts = append(opts, request.WithProxy(dc.Proxy))
	}
	repairOpts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithRateLimiter(ratelimits["repair"]),
		request.WithMaxRetries(4),
		request.WithRetryableStatus(http.StatusTooManyRequests),
	}
	if dc.Proxy != "" {
		repairOpts = append(repairOpts, request.WithProxy(dc.Proxy))
	}

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = 48 * time.Hour
	}
	dbl := &DebridLink{
		Host:                  "https://debrid-link.com/api/v2",
		APIKey:                dc.APIKey,
		accountsManager:       account.NewManager(dc, ratelimits["download"], log),
		DownloadUncached:      dc.DownloadUncached,
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		client:                request.New(opts...),
		repairClient:          request.New(repairOpts...),
		logger:                log,
		config:                dc,
	}
	return dbl, nil
}

func (dl *DebridLink) Config() config.Debrid {
	return dl.config
}

func (dl *DebridLink) Logger() zerolog.Logger {
	return dl.logger
}

// doGet performs a GET request and unmarshals the response
func (dl *DebridLink) doGet(endpoint string, queryParams map[string]string, result any) (*http.Response, error) {
	u, err := url.Parse(dl.Host + endpoint)
	if err != nil {
		return nil, err
	}

	if queryParams != nil {
		q := u.Query()
		for k, v := range queryParams {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := dl.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if result != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.ContentLength != 0 {
		if err := utils.DecodeJSONResponse(resp.Body, result); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

func (dl *DebridLink) filesByLogicalName(
	torrentID string,
	providerFiles []torrentFile,
) (map[string]types.File, error) {
	cfg := config.Get()
	files := make([]types.File, 0, len(providerFiles))
	for _, providerFile := range providerFiles {
		if err := cfg.IsFileAllowed(providerFile.Name, providerFile.Size); err != nil {
			continue
		}
		files = append(files, types.File{
			TorrentId: torrentID,
			Id:        providerFile.ID,
			Name:      providerFile.Name,
			Size:      providerFile.Size,
			Path:      providerFile.Name,
			Link:      providerFile.DownloadURL,
		})
	}
	return types.FilesByLogicalName(files)
}

func (dl *DebridLink) attachDownloadLinks(files map[string]types.File) {
	now := time.Now()
	for name, file := range files {
		link := types.DownloadLink{
			Debrid:       dl.config.Name,
			Token:        dl.APIKey,
			Filename:     name,
			Size:         file.Size,
			Link:         file.Link,
			DownloadLink: file.Link,
			Generated:    now,
			ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
		}
		file.DownloadLink = link
		files[name] = file
		dl.accountsManager.StoreDownloadLink(link)
	}
}

func (dl *DebridLink) IsAvailable(hashes []string) map[string]bool {
	result := make(map[string]bool)

	for i := 0; i < len(hashes); i += 100 {
		end := min(i+100, len(hashes))

		validHashes := make([]string, 0, end-i)
		for _, hash := range hashes[i:end] {
			if hash != "" {
				validHashes = append(validHashes, hash)
			}
		}

		if len(validHashes) == 0 {
			continue
		}

		hashStr := strings.Join(validHashes, ",")
		endpoint := fmt.Sprintf("/seedbox/cached/%s", hashStr)
		var data AvailableResponse

		resp, err := dl.doGet(endpoint, nil, &data)
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		if data.Value == nil {
			return result
		}
		value := *data.Value
		for _, h := range hashes[i:end] {
			_, exists := value[h]
			if exists {
				result[h] = true
			}
		}
	}
	return result
}

func (dl *DebridLink) GetTorrent(torrentId string) (*types.Torrent, error) {
	if strings.TrimSpace(torrentId) == "" {
		return nil, fmt.Errorf("torrent ID is empty")
	}
	var res torrentInfo

	resp, err := dl.doGet("/seedbox/list", map[string]string{"ids": torrentId}, &res)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error getting torrent")
	}
	data, found := debridLinkTorrentByID(*res.Value, torrentId)
	if !found {
		return nil, fmt.Errorf("torrent not found")
	}
	name := utils.RemoveInvalidChars(data.Name)
	torrent := &types.Torrent{
		Id:               data.ID,
		InfoHash:         data.HashString,
		Name:             name,
		Bytes:            data.TotalSize,
		Progress:         data.DownloadPercent,
		Status:           debridLinkTorrentStatus(data.Status),
		Speed:            data.DownloadSpeed,
		Seeders:          data.PeersConnected,
		Filename:         name,
		OriginalFilename: name,
		Debrid:           dl.config.Name,
	}
	if data.Created > 0 {
		torrent.Added = time.Unix(data.Created, 0)
	}
	torrent.Files, err = dl.filesByLogicalName(data.ID, data.Files)
	if err != nil {
		return nil, err
	}
	dl.attachDownloadLinks(torrent.Files)

	return torrent, nil
}

func (dl *DebridLink) UpdateTorrent(t *types.Torrent) error {
	if t == nil || strings.TrimSpace(t.Id) == "" {
		return fmt.Errorf("torrent ID is missing")
	}
	var res torrentInfo

	resp, err := dl.doGet("/seedbox/list", map[string]string{"ids": t.Id}, &res)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	if !res.Success {
		return fmt.Errorf("error getting torrent")
	}
	if res.Value == nil {
		return fmt.Errorf("torrent not found")
	}
	data, found := debridLinkTorrentByID(*res.Value, t.Id)
	if !found {
		return fmt.Errorf("torrent not found")
	}
	name := utils.RemoveInvalidChars(data.Name)
	t.Id = data.ID
	t.Name = name
	t.Bytes = data.TotalSize
	t.Progress = data.DownloadPercent
	t.Status = debridLinkTorrentStatus(data.Status)
	t.Speed = data.DownloadSpeed
	t.Seeders = data.PeersConnected
	t.Filename = name
	t.OriginalFilename = name
	if data.HashString != "" {
		t.InfoHash = data.HashString
	}
	if data.Created > 0 {
		t.Added = time.Unix(data.Created, 0)
	}
	files, err := dl.filesByLogicalName(t.Id, data.Files)
	if err != nil {
		return err
	}
	dl.attachDownloadLinks(files)
	t.Files = files

	return nil
}

func debridLinkTorrentByID(torrents []_torrentInfo, torrentID string) (_torrentInfo, bool) {
	for _, torrent := range torrents {
		if torrent.ID == torrentID {
			return torrent, true
		}
	}
	return _torrentInfo{}, false
}

func debridLinkTorrentStatus(status int) types.TorrentStatus {
	if status == 100 {
		return types.TorrentStatusDownloaded
	}
	return types.TorrentStatusDownloading
}

func (dl *DebridLink) SubmitMagnet(t *types.Torrent) (*types.Torrent, error) {
	req, err := dl.newSubmitRequest(t)
	if err != nil {
		return nil, err
	}

	resp, err := dl.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeDebridLinkResponse(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("error adding torrent: Status: %d", resp.StatusCode)
	}
	if resp.ContentLength == 0 {
		return nil, fmt.Errorf("empty response from debridlink API")
	}

	var res SubmitTorrentInfo
	if err := utils.DecodeJSONResponseBounded(resp.Body, &res, utils.MaxJSONResponseBytes); err != nil {
		return nil, err
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error adding torrent")
	}
	data := *res.Value
	if data.ID == "" {
		return nil, fmt.Errorf("debridlink API returned an empty torrent ID")
	}

	return dl.applySubmittedTorrent(t, data)
}

func (dl *DebridLink) newSubmitRequest(t *types.Torrent) (*http.Request, error) {
	if t == nil || t.Magnet == nil {
		return nil, fmt.Errorf("torrent source is missing")
	}

	if t.Magnet.IsTorrent() {
		if len(t.Magnet.File) == 0 {
			return nil, fmt.Errorf("torrent file is empty")
		}
		if int64(len(t.Magnet.File)) > utils.MaxMetadataFileBytes {
			return nil, fmt.Errorf("torrent file exceeds %d bytes", utils.MaxMetadataFileBytes)
		}

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", "upload.torrent")
		if err != nil {
			return nil, fmt.Errorf("create torrent upload: %w", err)
		}
		if _, err := part.Write(t.Magnet.File); err != nil {
			return nil, fmt.Errorf("write torrent upload: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("finish torrent upload: %w", err)
		}

		req, err := http.NewRequest(http.MethodPost, dl.Host+"/seedbox/add", &body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return req, nil
	}

	if strings.TrimSpace(t.Magnet.Link) == "" {
		return nil, fmt.Errorf("magnet link is empty")
	}
	payload, err := json.Marshal(map[string]string{"url": t.Magnet.Link})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, dl.Host+"/seedbox/add", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (dl *DebridLink) applySubmittedTorrent(t *types.Torrent, data _torrentInfo) (*types.Torrent, error) {
	name := utils.RemoveInvalidChars(data.Name)
	t.Id = data.ID
	t.Name = name
	t.Bytes = data.TotalSize
	t.Progress = data.DownloadPercent
	t.Status = types.TorrentStatusDownloading
	t.Speed = data.DownloadSpeed
	t.Seeders = data.PeersConnected
	t.Filename = name
	t.OriginalFilename = name
	t.Debrid = dl.config.Name
	t.Added = time.Now()
	if data.Created > 0 {
		t.Added = time.Unix(data.Created, 0)
	}
	if data.HashString != "" {
		t.InfoHash = data.HashString
	}
	files, err := dl.filesByLogicalName(t.Id, data.Files)
	if err != nil {
		return nil, err
	}
	dl.attachDownloadLinks(files)
	t.Files = files

	return t, nil
}

func closeDebridLinkResponse(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, (64<<10)+1))
	_ = body.Close()
}

func (dl *DebridLink) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	for {
		err := dl.UpdateTorrent(torrent)
		if err != nil || torrent == nil {
			return torrent, err
		}
		switch torrent.Status {
		case types.TorrentStatusDownloading:
			if !torrent.DownloadUncached {
				return torrent, customerror.NewTorrentNotCachedError(torrent.Name)
			}
			return torrent, nil
		case types.TorrentStatusDownloaded:
			dl.logger.Info().Msgf("Torrent: %s downloaded", torrent.Name)
			return torrent, nil
		default:
			return torrent, fmt.Errorf("torrent: %s has error", torrent.Name)
		}
	}
}

func (dl *DebridLink) DeleteTorrent(torrentId string) error {
	endpoint := fmt.Sprintf("/seedbox/%s/remove", torrentId)

	req, err := http.NewRequest(http.MethodDelete, dl.Host+endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := dl.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, (64<<10)+1))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return customerror.TorrentNotFoundError
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}

	dl.logger.Info().Msgf("Torrent: %s deleted from DebridLink", torrentId)
	return nil
}

func (dl *DebridLink) fetchDownloadLink(account *account.Account, id string, file *types.File) (types.DownloadLink, error) {
	now := time.Now()
	link := types.DownloadLink{
		Debrid:       dl.config.Name,
		Token:        account.Token,
		Filename:     file.Name,
		Size:         file.Size,
		Link:         file.Link,
		DownloadLink: file.Link,
		Generated:    now,
		ExpiresAt:    now.Add(dl.autoExpiresLinksAfter),
	}
	return link, nil
}

func (dl *DebridLink) GetDownloadLink(id string, file *types.File) (types.DownloadLink, error) {
	return dl.accountsManager.GetDownloadLink(id, file, dl.fetchDownloadLink)
}

func (dl *DebridLink) GetDownloadUncached() bool {
	return dl.DownloadUncached
}

func (dl *DebridLink) GetTorrents() ([]*types.Torrent, error) {
	page := 0
	perPage := 100
	torrents := make([]*types.Torrent, 0)
	var fetchErr error
	for {
		t, err := dl.getTorrents(page, perPage)
		if err != nil {
			fetchErr = err
			break
		}
		if len(t) == 0 {
			break
		}
		torrents = append(torrents, t...)
		page++
	}
	if fetchErr != nil {
		return torrents, fetchErr
	}
	return torrents, nil
}

func (dl *DebridLink) fetchDownloadLinks(account *account.Account) ([]types.DownloadLink, error) {
	links := make([]types.DownloadLink, 0)
	limit := 100
	page := 0
	for {
		data, err := dl._fetchDownloadLinks(account, page, limit)
		if err != nil {
			return links, err
		}
		links = append(links, data...)
		if len(data) < limit {
			break
		}
		page++
	}
	return links, nil
}

func (dl *DebridLink) _fetchDownloadLinks(account *account.Account, page, limit int) ([]types.DownloadLink, error) {
	links := make([]types.DownloadLink, 0)

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/downloader/list?page=%d&perPage=%d", dl.Host, page, limit), nil)
	if err != nil {
		return links, err
	}

	resp, err := account.Client().Do(req)
	if err != nil {
		return links, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return links, fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	var res DownloadLinksResponse

	if resp.ContentLength == 0 {
		return links, fmt.Errorf("empty response from debridlink API")
	}
	if err := utils.DecodeJSONResponse(resp.Body, &res); err != nil {
		return links, err
	}
	if !res.Success || res.Value == nil {
		return links, fmt.Errorf("error getting download links")
	}
	data := *res.Value
	if len(data) == 0 {
		return links, nil
	}
	for _, l := range data {
		created := time.Unix(l.Created, 0)
		if created.IsZero() {
			continue
		}
		// Then check if created has expired
		if time.Since(created) > dl.autoExpiresLinksAfter {
			continue
		}
		link := types.DownloadLink{
			Debrid:       dl.config.Name,
			Id:           l.Id,
			Token:        dl.APIKey,
			Filename:     l.Name,
			Size:         int64(l.Size),
			Link:         l.Url,
			DownloadLink: l.DownloadUrl,
			Generated:    created,
			ExpiresAt:    created.Add(dl.autoExpiresLinksAfter),
		}
		links = append(links, link)
	}
	return links, nil
}

func (dl *DebridLink) RefreshDownloadLinks() error {
	return dl.accountsManager.RefreshLinks(dl.fetchDownloadLinks)
}

func (dl *DebridLink) getTorrents(page, perPage int) ([]*types.Torrent, error) {
	torrents := make([]*types.Torrent, 0)
	var res torrentInfo

	params := map[string]string{
		"page":    fmt.Sprintf("%d", page),
		"perPage": fmt.Sprintf("%d", perPage),
	}

	resp, err := dl.doGet("/seedbox/list", params, &res)
	if err != nil {
		return torrents, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return torrents, fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	if !res.Success || res.Value == nil {
		return torrents, fmt.Errorf("error getting torrents")
	}

	data := *res.Value

	if len(data) == 0 {
		return torrents, nil
	}
	for _, t := range data {
		if t.Status != 100 {
			continue
		}
		torrent := &types.Torrent{
			Id:               t.ID,
			Name:             t.Name,
			Bytes:            t.TotalSize,
			Status:           "downloaded",
			Filename:         t.Name,
			OriginalFilename: t.Name,
			InfoHash:         t.HashString,
			Files:            make(map[string]types.File),
			Debrid:           dl.config.Name,
			Added:            time.Unix(t.Created, 0),
		}
		torrent.Files, err = dl.filesByLogicalName(torrent.Id, t.Files)
		if err != nil {
			return nil, err
		}
		dl.attachDownloadLinks(torrent.Files)
		torrents = append(torrents, torrent)
	}

	return torrents, nil
}

func (dl *DebridLink) CheckFile(ctx context.Context, _, link string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := dl.repairClient.Do(req)
	if err != nil {
		// net/http errors contain the complete signed download URL.
		return fmt.Errorf("debridlink file check request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return customerror.HosterUnavailableError
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("debridlink file check error: Status: %d", resp.StatusCode)
	}
	return nil
}

func (dl *DebridLink) GetAvailableSlots() (int, error) {
	// AllDebrid does not provide available slots info
	return config.DefaultAvailableSlots, nil
}

func (dl *DebridLink) GetProfile() (*types.Profile, error) {
	if dl.Profile != nil && time.Since(dl.profileLastFetched) < debridLinkProfileCacheDuration {
		return dl.Profile, nil
	}
	var res UserInfo

	resp, err := dl.doGet("/account/infos", nil, &res)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}
	if !res.Success || res.Value == nil {
		return nil, fmt.Errorf("error getting user info")
	}
	data := *res.Value
	now := time.Now()
	expiration, err := debridLinkPremiumExpiration(now, data.PremiumLeft)
	if err != nil {
		return nil, err
	}
	premiumUntil := int64(0)
	if !expiration.IsZero() {
		premiumUntil = expiration.Unix()
	}
	profile := &types.Profile{
		Id:         1,
		Username:   data.Username,
		Name:       dl.config.Name,
		Email:      data.Email,
		Points:     data.Points,
		Premium:    premiumUntil,
		Expiration: expiration,
	}
	if data.PremiumLeft > 0 {
		profile.Type = "premium"
	} else {
		profile.Type = "free"
	}
	dl.Profile = profile
	dl.profileLastFetched = now
	return profile, nil
}

func debridLinkPremiumExpiration(now time.Time, seconds int64) (time.Time, error) {
	if seconds <= 0 {
		return time.Time{}, nil
	}
	if seconds > debridLinkMaxPremiumSeconds {
		return time.Time{}, fmt.Errorf("debridlink API returned an invalid premium duration")
	}
	return now.Add(time.Duration(seconds) * time.Second), nil
}

func (dl *DebridLink) AccountManager() *account.Manager {
	return dl.accountsManager
}

func (dl *DebridLink) syncAccount(account *account.Account) error {
	// Currently no account-specific data to sync
	return nil
}

func (dl *DebridLink) SyncAccounts() {
	dl.accountsManager.Sync(dl.syncAccount)
}

func (dl *DebridLink) deleteDownloadLink(account *account.Account, downloadLink types.DownloadLink) error {
	deleteURL := fmt.Sprintf("%s/downloader/%s/remove", dl.Host, downloadLink.Id)
	req, err := http.NewRequest(http.MethodDelete, deleteURL, nil)
	if err != nil {
		return err
	}

	resp, err := account.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, (64<<10)+1))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("debridlink API error: Status: %d", resp.StatusCode)
	}

	dl.logger.Info().Msgf("Download link: %s deleted from DebridLink", downloadLink.Filename)
	return nil
}

func (dl *DebridLink) DeleteLink(downloadLink types.DownloadLink) error {
	return dl.accountsManager.DeleteDownloadLink(downloadLink, dl.deleteDownloadLink)
}

// SpeedTest measures API latency and download speed using cached links
func (dl *DebridLink) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: dl.config.Name,
		TestedAt: time.Now(),
	}

	start := time.Now()
	resp, err := dl.doGet("/account/infos", nil, nil)
	latency := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", resp.StatusCode)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	// Try to measure download speed using a cached link
	current := dl.accountsManager.Current()
	if current == nil {
		return result
	}

	link, found := current.GetRandomLink()
	if !found || link.DownloadLink == "" {
		return result
	}

	// Download first 1MB to measure speed
	const downloadSize = 1 * 1024 * 1024 // 1MB
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.DownloadLink, nil)
	if err != nil {
		return result
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", downloadSize-1))

	downloadStart := time.Now()
	dlResp, err := current.Client().Do(req)
	if err != nil {
		return result
	}
	defer dlResp.Body.Close()

	bytesRead, err := io.Copy(io.Discard, io.LimitReader(dlResp.Body, downloadSize))
	downloadDuration := time.Since(downloadStart)

	if err != nil || bytesRead == 0 {
		return result
	}

	result.BytesRead = bytesRead
	if downloadDuration.Seconds() > 0 {
		result.SpeedMBps = float64(result.BytesRead) / downloadDuration.Seconds() / (1024 * 1024)
	}

	return result
}

func (dl *DebridLink) SupportsCheck() bool {
	return true
}
