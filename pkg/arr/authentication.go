package arr

import (
	"crypto/sha256"
	"crypto/subtle"
	"sort"
	"strings"
)

// MatchCredentials returns a configured Arr whose host and token match the
// supplied credentials. Authentication never probes a caller-supplied URL;
// only hosts already admitted into the runtime configuration can match.
func (s *Storage) MatchCredentials(category, host, token string) *Arr {
	if s == nil || strings.TrimSpace(host) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	host = strings.TrimSpace(host)
	token = strings.TrimSpace(token)

	if category != "" {
		if candidate := s.Get(category); arrCredentialsEqual(candidate, host, token) {
			return candidate
		}
	}

	candidates := s.GetAll()
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})
	for _, candidate := range candidates {
		if arrCredentialsEqual(candidate, host, token) {
			return candidate
		}
	}
	return nil
}

func arrCredentialsEqual(candidate *Arr, host, token string) bool {
	if candidate == nil {
		return false
	}
	return constantTimeStringEqual(strings.TrimSpace(candidate.Host), host) &&
		constantTimeStringEqual(strings.TrimSpace(candidate.Token), token)
}

func constantTimeStringEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}
