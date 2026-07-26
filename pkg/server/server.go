package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/server/qbit"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
	"github.com/sirrobot01/decypharr/pkg/server/webdav"
	"github.com/sirrobot01/decypharr/pkg/stats"
)

//go:embed templates/*
var content embed.FS

//go:embed assets/build/*
var assetsEmbed embed.FS

//go:embed assets/images/*
var imagesEmbed embed.FS

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpIdleTimeout       = 2 * time.Minute
	httpShutdownTimeout   = 15 * time.Second
)

type AddRequest struct {
	Url        string   `json:"url"`
	Arr        string   `json:"arr"`
	File       string   `json:"file"`
	NotSymlink bool     `json:"notSymlink"`
	Content    string   `json:"content"`
	Seasons    []string `json:"seasons"`
	Episodes   []string `json:"episodes"`
}

type ArrResponse struct {
	Name string `json:"name"`
	Url  string `json:"url"`
}

type ContentResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
	ArrID string `json:"arr"`
}

type Server struct {
	router         *chi.Mux
	logger         zerolog.Logger
	manager        *manager.Manager
	stats          *stats.Collector
	cookie         *sessions.CookieStore
	templates      *template.Template
	nzbUserAgent   string
	urlBase        string
	restartFunc    func()
	configMu       sync.Mutex
	restartPending bool
}

func New(mgr *manager.Manager) *Server {
	l := logger.New("http")
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.StripSlashes)
	r.Use(middleware.RedirectSlashes)

	cfg := config.Get()

	templates := template.Must(template.ParseFS(
		content,
		"templates/layout.html",
		"templates/setup_layout.html",
		"templates/index.html",
		"templates/download.html",
		"templates/repair.html",
		"templates/stats.html",
		"templates/config.html",
		"templates/browse.html",
		"templates/login.html",
		"templates/register.html",
		"templates/setup.html",
	))
	cookieStore := newSessionCookieStore(cfg.SecretKey())

	statsCollector := stats.New(mgr)

	s := &Server{
		logger:    l,
		manager:   mgr,
		stats:     statsCollector,
		cookie:    cookieStore,
		templates: templates,
		urlBase:   cfg.URLBase,
	}

	qb := qbit.New(mgr)
	sb := sabnzbd.New(mgr)
	wd := webdav.NewHandler(mgr)

	routes := make(map[string]http.Handler)
	routes["/api/v2"] = qb.Routes()

	if !wd.IsDisabled() {
		routes["/webdav"] = wd.Routes()
	}
	routes["/sabnzbd"] = sb.Routes()

	// Trim trailing slash so chi registers the URLBase root path itself
	routePath := cfg.URLBase
	if routePath != "/" {
		routePath = strings.TrimSuffix(routePath, "/")
	}
	r.Route(routePath, func(r chi.Router) {
		// Mount web routes
		r.Mount("/", s.WebRoutes())

		for path, handler := range routes {
			r.Mount(path, handler)
		}

		r.Group(func(r chi.Router) {
			r.Use(s.authMiddleware)

			//logs
			r.Get("/logs", s.getLogs) // deprecated, use /debug/logs

			r.Route("/debug", func(r chi.Router) {
				r.Get("/stats", s.stats.Handler())
				r.Post("/speedtest", s.handleSpeedTest)
				r.Get("/logs", s.getLogs)
				r.Get("/logs/rclone", s.getRcloneLogs)
				r.Get("/ingests", s.handleIngests)
				r.Get("/ingests/{debrid}", s.handleIngestsByDebrid)
			})
		})

		//webhooks
		r.Post("/webhooks/tautulli", s.handleTautulli)
	})
	s.router = r
	return s
}

func newSessionCookieStore(secret string) *sessions.CookieStore {
	store := sessions.NewCookieStore([]byte(secret))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	return store
}

func (s *Server) SetRestartFunc(restartFunc func()) {
	s.restartFunc = restartFunc
}

func (s *Server) Restart() {
	if s.restartFunc != nil {
		time.Sleep(200 * time.Millisecond)
		s.restartFunc()
	} else {
		s.logger.Warn().Msg("Restart function not set")
	}
}

func (s *Server) Start(ctx context.Context) error {
	cfg := config.Get()

	if insecureRemoteAccess(cfg.BindAddress, cfg.UseAuth) {
		s.logger.Warn().
			Str("bind_address", cfg.BindAddress).
			Bool("authentication", cfg.UseAuth).
			Msg("Authentication is disabled on a non-loopback HTTP listener; the UI, APIs, and provider credentials may be exposed. Bind to 127.0.0.1 or enable authentication before allowing remote access")
	}

	addr := fmt.Sprintf("%s:%s", cfg.BindAddress, cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	// Start background stats only after the HTTP listener is established.
	s.stats.Start(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	s.logger.Info().Msgf("Starting server on %s%s", addr, cfg.URLBase)
	err = serveHTTP(ctx, srv, listener, httpShutdownTimeout)
	if err != nil {
		return fmt.Errorf("HTTP server: %w", err)
	}
	return nil
}

func insecureRemoteAccess(bindAddress string, useAuth bool) bool {
	return !useAuth && !isLoopbackBindAddress(bindAddress)
}

func isLoopbackBindAddress(bindAddress string) bool {
	host := strings.TrimSpace(bindAddress)
	if strings.EqualFold(host, "localhost") {
		return true
	}

	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if zoneIndex := strings.LastIndexByte(host, '%'); zoneIndex >= 0 {
		host = host[:zoneIndex]
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func serveHTTP(ctx context.Context, srv *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancel()

	if shutdownErr != nil {
		closeErr := srv.Close()
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return errors.Join(
			fmt.Errorf("graceful shutdown: %w", shutdownErr),
			closeErr,
			err,
		)
	}

	err := <-serveErr
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	logFile := filepath.Join(logger.GetLogPath(), "decypharr.log")

	// Open and read the file
	file, err := os.Open(logFile)
	if err != nil {
		http.Error(w, "Error reading log file", http.StatusInternalServerError)
		return
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			s.logger.Error().Err(err).Msg("Error closing log file")
		}
	}(file)

	// Set headers
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=application.log")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Stream the file
	if _, err := io.Copy(w, file); err != nil {
		http.Error(w, "Error streaming log file", http.StatusInternalServerError)
		return
	}
}

func (s *Server) getRcloneLogs(w http.ResponseWriter, r *http.Request) {
	// Rclone logs resides in the same directory as the application logs
	logFile := filepath.Join(logger.GetLogPath(), "rclone.log")
	// Open and read the file
	file, err := os.Open(logFile)
	if err != nil {
		http.Error(w, "Error reading log file", http.StatusInternalServerError)
		return
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			return
		}
	}(file)

	// Set headers
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=application.log")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Stream the file
	if _, err := io.Copy(w, file); err != nil {
		http.Error(w, fmt.Sprintf("error stremaing file %s", err), http.StatusInternalServerError)
		return
	}
}
