package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/sirrobot01/decypharr/cmd/decypharr"
	"github.com/sirrobot01/decypharr/internal/config"
	"golang.org/x/term"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("FATAL: Recovered from panic in main: %v\n", r)
			debug.PrintStack()
		}
	}()

	var configPath string
	var pprofAddr string
	var checkConfig bool
	var setAuthUsername string

	// Create a default config directory if it doesn't exist
	flag.StringVar(&configPath, "config", "", "path to the data folder")
	flag.StringVar(&pprofAddr, "pprof", ":6060", "pprof server address (set to empty to disable)")
	flag.BoolVar(&checkConfig, "check-config", false, "validate configuration without starting services")
	flag.StringVar(
		&setAuthUsername,
		"set-auth",
		"",
		"securely set the web username and password, then exit",
	)
	flag.Parse()

	// get enable pprof flag from environment variable if not set via flag
	enablePprof := os.Getenv("ENABLE_PPROF") != ""

	if configPath == "" {
		defaultDir, err := os.UserHomeDir()
		if err != nil {
			// If we can't get the user home directory, fallback to current directory
			defaultDir = "."
		}
		defaultConfigDir := filepath.Join(defaultDir, ".decypharr")
		configPath = defaultConfigDir
	}

	if checkConfig && setAuthUsername != "" {
		log.Fatal("--check-config and --set-auth cannot be used together")
	}

	if checkConfig {
		cfg, err := config.LoadForValidation(configPath)
		if err != nil {
			log.Fatalf("Decypharr configuration check failed: %v", err)
		}
		if err := cfg.Validate(); err != nil {
			log.Fatalf("Decypharr configuration check failed: %v", err)
		}
		if err := cfg.ValidateDeployment(); err != nil {
			log.Fatalf("Decypharr deployment safety check failed: %v", err)
		}
		fmt.Printf(
			"Decypharr configuration is valid: %s\n",
			filepath.Join(configPath, "config.json"),
		)
		return
	}

	config.SetConfigPath(configPath)
	cfg := config.Get()

	if setAuthUsername != "" {
		if err := configureAuthFromTerminal(cfg, setAuthUsername); err != nil {
			log.Fatalf("Decypharr authentication setup failed: %v", err)
		}
		fmt.Printf(
			"Authentication enabled for %q in %s\n",
			strings.TrimSpace(setAuthUsername),
			configPath,
		)
		return
	}

	// Buffer pools are owned by their subsystems: the DFS cache (vfs.NewCache)
	// and the usenet reader each create a buffer.Pool with their own configured
	// RAM budget and disk limit.

	// Start pprof server if enabled
	if pprofAddr != "" && enablePprof {
		go func() {
			log.Printf("Starting pprof server on %s", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof server error: %v", err)
			}
		}()
	}

	// Create a context canceled on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := decypharr.Start(ctx); err != nil {
		log.Fatal(err)
	}
}

func configureAuthFromTerminal(cfg *config.Config, username string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf(
			"standard input is not a terminal; run --set-auth interactively",
		)
	}

	fmt.Fprint(os.Stderr, "Password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}

	fmt.Fprint(os.Stderr, "Confirm password: ")
	confirmation, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return fmt.Errorf("read password confirmation: %w", err)
	}
	defer clear(password)
	defer clear(confirmation)
	if string(password) != string(confirmation) {
		return fmt.Errorf("passwords do not match")
	}

	if err := cfg.SetAuthCredentials(username, string(password)); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save authentication setting: %w", err)
	}
	return nil
}
