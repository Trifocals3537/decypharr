package cdntraffic

import (
	"context"
	"strings"
)

// Priority describes how latency-sensitive a CDN request is.
type Priority uint8

const (
	// PriorityBackground is used for bulk downloads and maintenance probes.
	PriorityBackground Priority = iota
	// PriorityInteractive is used for playback, seeks, and their link probes.
	PriorityInteractive
)

// Identity groups requests that consume the same provider-side CDN budget.
// AccountToken is hashed before it is used as an internal key and is never
// included in snapshots.
type Identity struct {
	Provider     string
	ProviderType string
	AccountToken string
}

type requestMetadata struct {
	identity Identity
	priority Priority
}

type metadataContextKey struct{}

// WithIdentity attaches provider/account ownership to future HTTP requests.
// Any priority already present on the context is preserved.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, _ := metadataFromContext(ctx)
	metadata.identity = Identity{
		Provider:     strings.TrimSpace(identity.Provider),
		ProviderType: strings.ToLower(strings.TrimSpace(identity.ProviderType)),
		AccountToken: identity.AccountToken,
	}
	return context.WithValue(ctx, metadataContextKey{}, metadata)
}

// WithPriority marks future CDN requests as playback-sensitive or background.
// Any provider identity already present on the context is preserved.
func WithPriority(ctx context.Context, priority Priority) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, _ := metadataFromContext(ctx)
	metadata.priority = priority
	return context.WithValue(ctx, metadataContextKey{}, metadata)
}

func metadataFromContext(ctx context.Context) (requestMetadata, bool) {
	if ctx == nil {
		return requestMetadata{}, false
	}
	metadata, ok := ctx.Value(metadataContextKey{}).(requestMetadata)
	return metadata, ok
}
