package decypharr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs"
	"github.com/sirrobot01/decypharr/pkg/mount/external"
	"github.com/sirrobot01/decypharr/pkg/mount/rclone"
	"github.com/sirrobot01/decypharr/pkg/server"
	"github.com/sirrobot01/decypharr/pkg/version"
	"golang.org/x/sync/errgroup"
)

var errServicesStopped = errors.New("services stopped unexpectedly")

type serviceStarter interface {
	Start(context.Context) error
}

func Start(ctx context.Context) error {
	// Start the global cached time updater to reduce time.Now() syscall overhead
	utils.StartGlobalCachedTime()
	defer utils.StopGlobalCachedTime()

	if umaskStr := os.Getenv("UMASK"); umaskStr != "" {
		umask, err := strconv.ParseInt(umaskStr, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid UMASK value: %s", umaskStr)
		}
		SetUmask(int(umask))
	}

	restartCh := make(chan struct{}, 1)
	restartFunc := func() {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	}

	mgr := manager.New()

	svcCtx, cancelSvc := context.WithCancel(ctx)
	defer func() {
		cancelSvc()
	}()

	// Create the logger path if it doesn't exist
	for {
		cfg := config.Get()
		_log := logger.Default()

		// ascii banner
		fmt.Printf(`
+-------------------------------------------------------+
|                                                       |
|  ╔╦╗╔═╗╔═╗╦ ╦╔═╗╦ ╦╔═╗╦═╗╦═╗                          |
|   ║║║╣ ║  └┬┘╠═╝╠═╣╠═╣╠╦╝╠╦╝ (%s)        |
|  ═╩╝╚═╝╚═╝ ┴ ╩  ╩ ╩╩ ╩╩╚═╩╚═                          |
|                                                       |
+-------------------------------------------------------+
|  Log Level: %s                                        |
+-------------------------------------------------------+
`, version.GetInfo(), cfg.LogLevel)

		// Initialize services
		mountMgr := createMountManager(mgr, cfg)
		mgr.SetMountManager(mountMgr)
		srv := server.New(mgr)

		srv.SetRestartFunc(restartFunc)

		resetFunc := func() {

			config.Reset()
			// Stop manager to reset ready channel and cleanup resources
			if err := mgr.Reset(); err != nil {
				_log.Warn().Err(err).Msg("Failed to reset manager")
			}
			// refresh GC
			runtime.GC()
		}

		shutdownFunc := func() {
			config.Reset()
			// Stop manager to cleanup all resources including mounts
			if err := mgr.Stop(); err != nil {
				_log.Warn().Err(err).Msg("Failed to stop manager during shutdown")
			}
			// refresh GC
			runtime.GC()
		}

		serviceDone := make(chan error, 1)
		go func(ctx context.Context) {
			serviceDone <- startServices(ctx, mgr, srv)
		}(svcCtx)

		select {
		case <-ctx.Done():
			// graceful shutdown
			cancelSvc() // propagate to services
			if err := <-serviceDone; err != nil {
				_log.Warn().Err(err).Msg("Service reported an error while shutting down")
			}
			_log.Info().Msg("Decypharr has been stopped gracefully.")
			shutdownFunc() // cleanup all resources including mounts
			return nil

		case <-restartCh:
			cancelSvc() // tell existing services to shut down
			_log.Info().Msg("Restarting Decypharr...")
			if err := <-serviceDone; err != nil {
				_log.Warn().Err(err).Msg("Service reported an error while restarting")
			}

			if ctx.Err() != nil {
				shutdownFunc()
				return nil
			}

			_log.Info().Msg("Decypharr has been restarted.")
			resetFunc() // reset store and services for restart
			// rebuild svcCtx off the original parent
			svcCtx, cancelSvc = context.WithCancel(ctx)

		case err := <-serviceDone:
			cancelSvc()
			shutdownFunc()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = errServicesStopped
			}
			return err
		}
	}
}

func createMountManager(mgr *manager.Manager, cfg *config.Config) manager.MountManager {
	switch cfg.Mount.Type {
	case config.MountTypeRclone:
		return rclone.NewManager(mgr)
	case config.MountTypeDFS:
		return dfs.NewManager(mgr)
	case config.MountTypeExternalRclone:
		return external.NewManager(mgr)
	default:
		return manager.NewStubMountManager()
	}
}

func startServices(ctx context.Context, managerService, httpService serviceStarter) error {
	group, serviceCtx := errgroup.WithContext(ctx)

	start := func(name string, service serviceStarter) {
		group.Go(func() error {
			return startService(serviceCtx, name, service)
		})
	}

	// The HTTP service remains active until cancellation. Manager.Start performs
	// initialization and then returns while its workers continue on serviceCtx.
	start("HTTP service", httpService)
	start("manager", managerService)

	err := group.Wait()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	return errServicesStopped
}

func startService(ctx context.Context, name string, service serviceStarter) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panicked: %v\n%s", name, recovered, debug.Stack())
		}
	}()

	if err := service.Start(ctx); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
