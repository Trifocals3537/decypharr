package cdntraffic

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

// Transport applies logical request admission around another RoundTripper.
// A permit remains active until the response body is closed or reaches EOF.
type Transport struct {
	base     http.RoundTripper
	governor *Governor
}

// NewTransport wraps base with the shared governor.
func NewTransport(base http.RoundTripper, governor *Governor) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	if governor == nil {
		governor = New(Options{})
	}
	return &Transport{base: base, governor: governor}
}

// BaseTransport exposes the wrapped transport for TLS test setup and shutdown.
func (t *Transport) BaseTransport() http.RoundTripper {
	if t == nil {
		return nil
	}
	return t.base
}

// RoundTrip acquires a request slot and transfers ownership to the response
// body. Redirect hops naturally release before the next hop is admitted.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	metadata, _ := metadataFromContext(req.Context())
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	permit, err := t.governor.Acquire(req.Context(), metadata.identity, host, metadata.priority)
	if err != nil {
		return nil, err
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		permit.Release()
		return nil, err
	}
	if resp == nil {
		permit.Release()
		return nil, errors.New("cdn base transport returned a nil response")
	}
	t.governor.Observe(metadata.identity, host, resp.StatusCode, resp.Header)
	body := resp.Body
	if body == nil {
		body = http.NoBody
	}
	resp.Body = &permitBody{ReadCloser: body, permit: permit}
	return resp, nil
}

type permitBody struct {
	io.ReadCloser
	permit *Permit
	once   sync.Once
}

func (b *permitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *permitBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func (b *permitBody) release() {
	b.once.Do(b.permit.Release)
}
