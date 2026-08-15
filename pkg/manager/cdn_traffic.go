package manager

import (
	"context"
	"strings"

	"github.com/sirrobot01/decypharr/internal/cdntraffic"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func (m *Manager) withCDNIdentity(ctx context.Context, link debridTypes.DownloadLink, fallbackProvider string) context.Context {
	provider := strings.TrimSpace(link.Debrid)
	if provider == "" {
		provider = strings.TrimSpace(fallbackProvider)
	}
	providerType := m.cdnProviderType(provider)
	return cdntraffic.WithIdentity(ctx, cdntraffic.Identity{
		Provider:     provider,
		ProviderType: providerType,
		AccountToken: link.Token,
	})
}

func (m *Manager) cdnProviderType(provider string) string {
	if m == nil || m.clients == nil || provider == "" {
		return ""
	}
	client, ok := m.clients.Load(provider)
	if !ok || client == nil {
		return ""
	}
	return client.Config().Provider
}

// CDNTrafficStats returns secret-free adaptive admission statistics.
func (m *Manager) CDNTrafficStats() cdntraffic.Stats {
	if m == nil || m.cdnTraffic == nil {
		return cdntraffic.Stats{}
	}
	return m.cdnTraffic.Snapshot()
}
