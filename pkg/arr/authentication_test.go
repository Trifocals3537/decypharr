package arr

import (
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
)

func TestMatchCredentialsOnlyReturnsConfiguredArrs(t *testing.T) {
	storage := &Storage{arrs: xsync.NewMap[string, *Arr]()}
	sonarr := New("sonarr", "http://sonarr:8989", "sonarr-token", false, nil, "", "manual")
	radarr := New("radarr", "http://radarr:7878", "radarr-token", false, nil, "", "manual")
	storage.arrs.Store(sonarr.Name, sonarr)
	storage.arrs.Store(radarr.Name, radarr)

	if got := storage.MatchCredentials("sonarr", sonarr.Host, sonarr.Token); got != sonarr {
		t.Fatalf("category match = %#v, want sonarr", got)
	}
	if got := storage.MatchCredentials("", radarr.Host, radarr.Token); got != radarr {
		t.Fatalf("credential search = %#v, want radarr", got)
	}
	if got := storage.MatchCredentials("sonarr", "http://attacker.invalid", "secret"); got != nil {
		t.Fatalf("unconfigured host matched %#v", got)
	}
	if got := storage.MatchCredentials("sonarr", sonarr.Host, "wrong"); got != nil {
		t.Fatalf("wrong token matched %#v", got)
	}
}
