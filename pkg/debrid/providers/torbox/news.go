package torbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	json "github.com/bytedance/sonic"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
)

const (
	newsServerAccountEndpoint = "/api/usenet/provider/account"
	newsServerResetEndpoint   = newsServerAccountEndpoint + "/resetpw"
	maxNewsServerResponseSize = 64 << 10

	// TorBox currently permits no more than ten simultaneous News Server
	// connections per account. Treat the remote value as an upper bound, not
	// permission to exceed the documented service limit.
	maxNewsServerConnections = 10
)

var (
	// ErrNewsServerPasswordUnavailable means the account already existed and
	// TorBox returned its password mask. The caller must supply the existing
	// password or explicitly request a rotation; provisioning never resets it.
	ErrNewsServerPasswordUnavailable = errors.New("TorBox News password is unavailable")
)

// NewsServerAccount is the one-time credential document returned when TorBox
// creates or explicitly rotates a News Server account.
type NewsServerAccount struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	SSL         bool   `json:"ssl"`
	Connections int    `json:"connections"`
	Username    string `json:"username"`
	Password    string `json:"password"`
}

type newsServerAccountResponse struct {
	Success bool               `json:"success"`
	Error   any                `json:"error"`
	Detail  string             `json:"detail"`
	Data    *NewsServerAccount `json:"data"`
}

// NewsServerProvision contains a validated generic NNTP provider plus whether
// this request received a newly-created, one-time password from TorBox.
type NewsServerProvision struct {
	Provider config.UsenetProvider
	Created  bool
}

// ProvisionNewsServer creates the TorBox News account on first use and returns
// a ready-to-save NNTP provider. If the account already exists, TorBox masks
// the password; in that case existingPassword must be supplied by the user.
//
// This method intentionally never calls the password-reset endpoint.
func (tb *Torbox) ProvisionNewsServer(
	ctx context.Context,
	existingPassword string,
) (*NewsServerProvision, error) {
	var result newsServerAccountResponse
	resp, err := tb.doGetContextBounded(
		ctx,
		newsServerAccountEndpoint,
		nil,
		&result,
		maxNewsServerResponseSize,
	)
	if err != nil {
		return nil, fmt.Errorf("get TorBox News account: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("TorBox News account request failed with status %d", resp.StatusCode)
	}
	if !result.Success || result.Data == nil {
		return nil, errors.New("TorBox News account request was unsuccessful")
	}

	account := *result.Data
	created := !isMaskedNewsServerPassword(account.Password)
	if !created {
		account.Password = existingPassword
	}
	if strings.TrimSpace(account.Password) == "" {
		return nil, ErrNewsServerPasswordUnavailable
	}

	provider, err := newsServerProvider(account)
	if err != nil {
		return nil, err
	}
	return &NewsServerProvision{
		Provider: provider,
		Created:  created,
	}, nil
}

// ResetNewsServerPassword explicitly rotates the News Server password and
// returns the replacement credential. Callers must present this as a distinct,
// confirmed action because TorBox reveals the generated password only once.
func (tb *Torbox) ResetNewsServerPassword(
	ctx context.Context,
) (*config.UsenetProvider, error) {
	var result newsServerAccountResponse
	resp, err := tb.doPostJSONResult(ctx, newsServerResetEndpoint, nil, &result)
	if err != nil {
		return nil, fmt.Errorf("reset TorBox News password: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("TorBox News password reset failed with status %d", resp.StatusCode)
	}
	if !result.Success || result.Data == nil {
		return nil, errors.New("TorBox News password reset was unsuccessful")
	}
	if strings.TrimSpace(result.Data.Password) == "" ||
		isMaskedNewsServerPassword(result.Data.Password) {
		return nil, errors.New("TorBox News password reset returned no usable password")
	}

	provider, err := newsServerProvider(*result.Data)
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

func (tb *Torbox) doPostJSONResult(
	ctx context.Context,
	endpoint string,
	payload any,
	result any,
) (*http.Response, error) {
	if ctx == nil {
		return nil, fmt.Errorf("torbox request context is required")
	}

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tb.Host+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tb.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, (64<<10)+1))
		_ = resp.Body.Close()
	}()

	if result != nil &&
		resp.StatusCode >= http.StatusOK &&
		resp.StatusCode < http.StatusMultipleChoices &&
		resp.ContentLength != 0 {
		if err := utils.DecodeJSONResponseBounded(
			resp.Body,
			result,
			maxNewsServerResponseSize,
		); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

func newsServerProvider(account NewsServerAccount) (config.UsenetProvider, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(account.Host), "."))
	if host != "torbox.app" && !strings.HasSuffix(host, ".torbox.app") {
		return config.UsenetProvider{}, errors.New("TorBox News returned an unexpected server host")
	}
	if account.Port < 1 || account.Port > 65535 {
		return config.UsenetProvider{}, errors.New("TorBox News returned an invalid server port")
	}
	if !account.SSL {
		return config.UsenetProvider{}, errors.New("TorBox News returned an insecure connection")
	}
	if account.Connections < 1 {
		return config.UsenetProvider{}, errors.New("TorBox News returned an invalid connection limit")
	}

	username := strings.TrimSpace(account.Username)
	if username == "" {
		return config.UsenetProvider{}, errors.New("TorBox News returned no username")
	}
	if strings.TrimSpace(account.Password) == "" ||
		isMaskedNewsServerPassword(account.Password) {
		return config.UsenetProvider{}, ErrNewsServerPasswordUnavailable
	}

	return config.UsenetProvider{
		Host:           host,
		Port:           account.Port,
		Username:       username,
		Password:       account.Password,
		MaxConnections: min(account.Connections, maxNewsServerConnections),
		SSL:            true,
	}, nil
}

func isMaskedNewsServerPassword(password string) bool {
	password = strings.TrimSpace(password)
	return password != "" && strings.Trim(password, "*") == ""
}
