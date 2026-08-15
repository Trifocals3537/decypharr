package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
)

func (s *Server) sessionAuthenticated(r *http.Request) bool {
	if s == nil || s.cookie == nil || r == nil {
		return false
	}
	session, err := s.cookie.Get(r, "auth-session")
	if err != nil {
		return false
	}
	authenticated, ok := session.Values["authenticated"].(bool)
	if !ok || !authenticated {
		return false
	}
	auth := config.Get().GetAuth()
	if auth == nil {
		return false
	}
	version, ok := sessionVersion(session.Values["session_version"])
	return ok && version == auth.SessionVersion
}

func sessionVersion(value any) (uint64, bool) {
	switch version := value.(type) {
	case uint64:
		return version, true
	case uint:
		return uint64(version), true
	case int:
		return uint64(version), version >= 0
	case int64:
		return uint64(version), version >= 0
	default:
		return 0, false
	}
}

func setAuthenticatedSession(sessionValues map[any]any, username string) {
	sessionValues["authenticated"] = true
	sessionValues["username"] = username
	if auth := config.Get().GetAuth(); auth != nil {
		sessionValues["session_version"] = auth.SessionVersion
	}
}

func (s *Server) browserMutationAllowed(r *http.Request) bool {
	if r == nil || isSafeHTTPMethod(r.Method) || s.isValidAPIToken(r) {
		return true
	}
	return requestOriginAllowed(r, config.Get().AppURL)
}

func browserOriginPresent(r *http.Request) bool {
	return r != nil &&
		(strings.TrimSpace(r.Header.Get("Origin")) != "" ||
			strings.TrimSpace(r.Header.Get("Referer")) != "")
}

func isSafeHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func requestOriginAllowed(r *http.Request, appURL string) bool {
	if r == nil {
		return false
	}
	rawOrigin := strings.TrimSpace(r.Header.Get("Origin"))
	if rawOrigin == "" {
		rawOrigin = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if rawOrigin == "" || strings.EqualFold(rawOrigin, "null") {
		return false
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Host == "" ||
		(!strings.EqualFold(origin.Scheme, "http") && !strings.EqualFold(origin.Scheme, "https")) {
		return false
	}
	if sameOriginHost(origin.Host, r.Host, origin.Scheme) {
		return true
	}
	configured, err := url.Parse(strings.TrimSpace(appURL))
	return err == nil && configured.Host != "" &&
		strings.EqualFold(origin.Scheme, configured.Scheme) &&
		sameOriginHost(origin.Host, configured.Host, origin.Scheme)
}

func sameOriginHost(left, right, scheme string) bool {
	leftURL, leftErr := url.Parse("//" + strings.TrimSpace(left))
	rightURL, rightErr := url.Parse("//" + strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil || leftURL.Hostname() == "" || rightURL.Hostname() == "" {
		return false
	}
	defaultPort := ""
	switch strings.ToLower(scheme) {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	}
	leftPort, rightPort := leftURL.Port(), rightURL.Port()
	if leftPort == "" {
		leftPort = defaultPort
	}
	if rightPort == "" {
		rightPort = defaultPort
	}
	return strings.EqualFold(leftURL.Hostname(), rightURL.Hostname()) && leftPort == rightPort
}

func (s *Server) requireBrowserMutation(w http.ResponseWriter, r *http.Request) bool {
	if s.browserMutationAllowed(r) {
		return true
	}
	http.Error(w, "Cross-site request rejected", http.StatusForbidden)
	return false
}

func (s *Server) setupAccessAllowed(r *http.Request) bool {
	cfg := config.Get()
	if isLoopbackBindAddress(cfg.BindAddress) {
		return true
	}
	return s.isValidAPIToken(r) || s.sessionAuthenticated(r)
}

func (s *Server) requireSetupAccess(w http.ResponseWriter, r *http.Request) bool {
	if s.setupAccessAllowed(r) {
		return true
	}
	cfg := config.Get()
	if cfg.NeedsAuth() {
		http.Error(
			w,
			"Remote setup is locked. Set credentials on the host with --set-auth, then sign in.",
			http.StatusForbidden,
		)
		return false
	}
	if r.Method == http.MethodGet {
		http.Redirect(w, r, setupLoginPath(cfg.URLBase), http.StatusSeeOther)
		return false
	}
	http.Error(w, "Authentication required for remote setup", http.StatusUnauthorized)
	return false
}

func setupLoginPath(urlBase string) string {
	return urlBasePath(urlBase, "login")
}

func urlBasePath(urlBase, target string) string {
	base := "/" + strings.Trim(strings.TrimSpace(urlBase), "/")
	if base == "/" {
		return "/" + strings.TrimLeft(target, "/")
	}
	if strings.Trim(target, "/") == "" {
		return base + "/"
	}
	return base + "/" + strings.TrimLeft(target, "/")
}

func pathWithoutURLBase(path, urlBase string) string {
	base := strings.TrimSuffix(urlBasePath(urlBase, ""), "/")
	if base == "" || base == "/" {
		return path
	}
	if path == base {
		return "/"
	}
	if relative, ok := strings.CutPrefix(path, base+"/"); ok {
		return "/" + relative
	}
	return path
}

func setRetryAfter(w http.ResponseWriter, seconds int) {
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}
