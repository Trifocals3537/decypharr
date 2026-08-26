package parser

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

func TestArticleProbeFailuresClassifiesOnlyUniformMissingArticles(t *testing.T) {
	t.Parallel()

	missingFirst := &nntp.Error{
		Type:    nntp.ErrorTypeArticleNotFound,
		Code:    430,
		Message: "first article missing",
	}
	missingSecond := &nntp.Error{
		Type:    nntp.ErrorTypeArticleNotFound,
		Code:    430,
		Message: "second article missing",
	}

	failures := articleProbeFailures{}
	failures.add(errors.Join(errors.New("wrapped"), missingFirst))
	failures.add(missingSecond)
	err := failures.unavailableError()
	if !errors.Is(err, ErrNZBArticlesUnavailable) {
		t.Fatalf("expected ErrNZBArticlesUnavailable, got %v", err)
	}
	if !nntp.IsArticleNotFoundError(err) {
		t.Fatalf("expected the first NNTP cause to remain discoverable, got %v", err)
	}
}

func TestArticleProbeFailuresPreservesTransientAndMixedFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		errs []error
	}{
		{
			name: "timeout",
			errs: []error{nntp.NewTimeoutError(errors.New("deadline"))},
		},
		{
			name: "missing and timeout",
			errs: []error{
				&nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430},
				nntp.NewTimeoutError(errors.New("deadline")),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failures := articleProbeFailures{}
			for _, err := range test.errs {
				failures.add(err)
			}
			if err := failures.unavailableError(); err != nil {
				t.Fatalf("transient or mixed failures were classified as unavailable: %v", err)
			}
		})
	}
}

func TestArticleProbeFailuresMergePreservesMixedClassification(t *testing.T) {
	t.Parallel()

	missing := articleProbeFailures{}
	missing.add(&nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430})
	other := articleProbeFailures{}
	other.add(errFileTypeUndetected)
	missing.merge(other)

	if err := missing.unavailableError(); err != nil {
		t.Fatalf("merged mixed failures were classified as unavailable: %v", err)
	}
}
