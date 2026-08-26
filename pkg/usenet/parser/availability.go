package parser

import (
	"errors"
	"fmt"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

// ErrNZBArticlesUnavailable identifies an NZB whose required probe articles
// are definitively absent from every configured provider. Callers may treat
// this as a release-specific rejection; connection, authentication, timeout,
// and parser failures deliberately remain distinguishable.
var ErrNZBArticlesUnavailable = errors.New("NZB articles unavailable")

var errFileTypeUndetected = errors.New("file type could not be detected")

type articleProbeFailures struct {
	count              int
	allArticlesMissing bool
	first              error
}

func (f *articleProbeFailures) add(err error) {
	if err == nil {
		return
	}
	if f.count == 0 {
		f.allArticlesMissing = true
		f.first = err
	}
	f.count++
	if !nntp.IsArticleNotFoundError(err) {
		f.allArticlesMissing = false
	}
}

func (f *articleProbeFailures) merge(other articleProbeFailures) {
	if other.count == 0 {
		return
	}
	if f.count == 0 {
		*f = other
		return
	}
	f.count += other.count
	f.allArticlesMissing = f.allArticlesMissing && other.allArticlesMissing
}

func (f articleProbeFailures) unavailableError() error {
	if f.count == 0 || !f.allArticlesMissing {
		return nil
	}
	return &articlesUnavailableError{count: f.count, cause: f.first}
}

type articlesUnavailableError struct {
	count int
	cause error
}

func (e *articlesUnavailableError) Error() string {
	return fmt.Sprintf(
		"%s: %d required article probe(s) failed; first failure: %v",
		ErrNZBArticlesUnavailable,
		e.count,
		e.cause,
	)
}

func (e *articlesUnavailableError) Unwrap() []error {
	return []error{ErrNZBArticlesUnavailable, e.cause}
}

func newArticlesUnavailableError(count int, cause error) error {
	failures := articleProbeFailures{}
	if count <= 0 {
		count = 1
	}
	failures.count = count
	failures.allArticlesMissing = true
	failures.first = cause
	return failures.unavailableError()
}
