package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cyberstrike-ai/internal/app"
	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/desktopcredentials"
	"cyberstrike-ai/internal/desktopmigration"
	"cyberstrike-ai/internal/desktopprotocol"
	"cyberstrike-ai/internal/desktopruntime"
	"cyberstrike-ai/internal/logger"
	"cyberstrike-ai/internal/pythonruntime"
	webassets "cyberstrike-ai/web"
	"github.com/gin-gonic/gin"
)

const (
	maximumCommandBytes = 4096
	readyTimeout        = 10 * time.Second
)

type runOptions struct {
	Roots            desktopruntime.Roots
	ResourceDir      string
	AppVersion       string
	PythonRuntimeDir string
	PythonExecutable string
	CredentialStore  desktopcredentials.Store
}

type commandEvent struct {
	command desktopprotocol.Command
	err     error
}

func main() {
	var options runOptions
	var maintenanceMode string
	var maintenanceSource string
	var maintenanceBackupID string
	flag.StringVar(&options.Roots.DataDir, "data-dir", "", "absolute desktop application data directory")
	flag.StringVar(&options.Roots.ConfigDir, "config-dir", "", "absolute desktop configuration directory")
	flag.StringVar(&options.Roots.CacheDir, "cache-dir", "", "absolute desktop cache directory")
	flag.StringVar(&options.Roots.LogDir, "log-dir", "", "absolute desktop log directory")
	flag.StringVar(&options.Roots.TempDir, "temp-dir", "", "absolute desktop temporary directory")
	flag.StringVar(&options.ResourceDir, "resource-dir", "", "absolute bundled defaults directory")
	flag.StringVar(&options.AppVersion, "app-version", "", "desktop application version")
	flag.StringVar(&options.PythonRuntimeDir, "python-runtime-dir", "", "absolute bundled Python runtime directory")
	flag.StringVar(&options.PythonExecutable, "python-executable", "", "absolute bundled Python executable")
	flag.StringVar(&maintenanceMode, "maintenance", "", "run one desktop maintenance operation")
	flag.StringVar(&maintenanceSource, "source-dir", "", "absolute legacy instance directory for import preparation")
	flag.StringVar(&maintenanceBackupID, "backup-id", "", "desktop recovery point identifier")
	flag.Parse()
	if err := pythonruntime.Configure(options.PythonRuntimeDir, options.PythonExecutable); err != nil {
		fmt.Fprintf(os.Stderr, "desktop process failed: configure bundled Python runtime: %v\n", err)
		os.Exit(1)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	var err error
	if strings.TrimSpace(maintenanceMode) != "" {
		err = runDesktopMaintenance(ctx, os.Stdout, options, maintenanceMode, maintenanceSource, maintenanceBackupID, time.Now())
	} else {
		err = runDesktopCore(ctx, os.Stdin, os.Stdout, options)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktop process failed: %v\n", err)
		os.Exit(1)
	}
}

func runDesktopCore(parent context.Context, stdin io.Reader, stdout io.Writer, options runOptions) error {
	if parent == nil {
		return errors.New("desktop core context is required")
	}
	if stdin == nil || stdout == nil {
		return errors.New("desktop core stdin and stdout are required")
	}
	// stdout is reserved for the versioned desktop protocol. Gin's request and
	// recovery logs must use stderr so they cannot corrupt sidecar messages.
	gin.DefaultWriter = os.Stderr
	gin.DefaultErrorWriter = os.Stderr
	options.ResourceDir = filepath.Clean(strings.TrimSpace(options.ResourceDir))
	if options.ResourceDir == "." || !filepath.IsAbs(options.ResourceDir) {
		return fmt.Errorf("desktop resource directory must be absolute: %q", options.ResourceDir)
	}
	options.AppVersion = strings.TrimSpace(options.AppVersion)
	if options.AppVersion == "" {
		return errors.New("desktop app version is required")
	}

	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		return err
	}
	if _, err := desktopmigration.RecoverInterruptedRestore(paths); err != nil {
		return fmt.Errorf("recover interrupted desktop restore: %w", err)
	}
	if err := paths.Prepare(); err != nil {
		return err
	}

	resourceSource := os.DirFS(options.ResourceDir)
	manifestData, err := fs.ReadFile(resourceSource, "manifest.json")
	if err != nil {
		return fmt.Errorf("read bundled resource manifest: %w", err)
	}
	manifest, err := desktopruntime.ParseResourceManifest(manifestData)
	if err != nil {
		return err
	}
	if manifest.AppVersion != options.AppVersion {
		return fmt.Errorf("desktop resource version %q does not match app version %q", manifest.AppVersion, options.AppVersion)
	}
	var upgradeSession *desktopmigration.UpgradeSession
	installedVersion, installed, err := desktopruntime.InstalledResourceVersion(paths.ResourceStateFile)
	if err != nil {
		return fmt.Errorf("inspect desktop installed version: %w", err)
	}
	if installed {
		upgradeSession, err = desktopmigration.PrepareUpgrade(parent, paths, installedVersion, options.AppVersion, time.Now())
		if err != nil {
			return fmt.Errorf("prepare desktop upgrade migration: %w", err)
		}
	}
	if _, err := desktopruntime.InstallResources(resourceSource, manifest, paths.ResourcesDir, paths.ResourceStateFile); err != nil {
		return fmt.Errorf("install desktop resources: %w", err)
	}
	if _, err := config.EnsureLocalConfigFromTemplate(paths.ConfigFile, filepath.Join(paths.ResourcesDir, "config.example.yaml")); err != nil {
		return fmt.Errorf("prepare desktop config: %w", err)
	}

	runContext, cancel := context.WithCancel(parent)
	defer cancel()
	commands := startCommandStream(runContext, stdin)
	encoder := json.NewEncoder(stdout)

	credentialStore := options.CredentialStore
	if credentialStore == nil {
		credentialStore = desktopcredentials.KeyringStore{}
	}
	credentialManager, err := desktopcredentials.NewManager(credentialStore)
	if err != nil {
		return fmt.Errorf("initialize desktop credential manager: %w", err)
	}
	cfg, err := config.LoadWithTransform(paths.ConfigFile, func(cfg *config.Config) error {
		if err := credentialManager.ResolveAndMigrate(cfg, func(paths []string) error {
			if err := encoder.Encode(desktopprotocol.NewCredentialMigrationRequired(options.AppVersion, paths)); err != nil {
				return fmt.Errorf("write CREDENTIAL_MIGRATION_REQUIRED handshake: %w", err)
			}
			for {
				select {
				case event, open := <-commands:
					if !open {
						return errors.New("desktop command stream closed before credential migration")
					}
					if event.err != nil {
						return event.err
					}
					switch event.command.Type {
					case desktopprotocol.CommandMigrateCredentials:
						return nil
					case desktopprotocol.CommandShutdown:
						cancel()
						return context.Canceled
					default:
						return fmt.Errorf("unexpected desktop command before credential migration: %s", event.command.Type)
					}
				case <-runContext.Done():
					return runContext.Err()
				}
			}
		}, func(persisted *config.Config) error {
			return desktopcredentials.WriteConfigAtomically(paths.ConfigFile, persisted)
		}); err != nil {
			return err
		}
		if err := paths.ApplyConfigPaths(cfg); err != nil {
			return err
		}
		desktopruntime.ApplyScope(cfg)
		config.ApplyPlainHTTPBootstrap(cfg)
		cfg.Server.Host = "127.0.0.1"
		cfg.Server.Port = 0
		return nil
	})
	if err != nil {
		if runContext.Err() != nil {
			return nil
		}
		return fmt.Errorf("load desktop config: %w", err)
	}

	passwordProvider := func() (string, error) {
		if err := encoder.Encode(desktopprotocol.NewBootstrapRequired(options.AppVersion)); err != nil {
			return "", fmt.Errorf("write BOOTSTRAP_REQUIRED handshake: %w", err)
		}
		for {
			select {
			case event, open := <-commands:
				if !open {
					return "", errors.New("desktop command stream closed before bootstrap")
				}
				if event.err != nil {
					return "", event.err
				}
				switch event.command.Type {
				case desktopprotocol.CommandBootstrap:
					return event.command.Password, nil
				case desktopprotocol.CommandShutdown:
					cancel()
					return "", context.Canceled
				}
			case <-runContext.Done():
				return "", runContext.Err()
			}
		}
	}

	log := logger.New(cfg.Log.Level, cfg.Log.Output)
	defer func() { _ = log.Close() }()
	application, err := app.New(
		cfg,
		log,
		paths.ConfigFile,
		app.WithWebFS(webassets.FS()),
		app.WithDesktopMode(),
		app.WithDesktopUploadsRoot(paths.UploadsDir),
		app.WithInitialAdminPasswordProvider(passwordProvider),
		app.WithDesktopCredentialManager(credentialManager),
	)
	if err != nil {
		if runContext.Err() != nil {
			return nil
		}
		return fmt.Errorf("initialize desktop application: %w", err)
	}
	defer application.Shutdown()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on desktop loopback: %w", err)
	}
	baseURL := "http://" + listener.Addr().String() + "/"
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- application.Serve(runContext, listener)
	}()
	if err := waitForReady(runContext, application, serveErrors); err != nil {
		cancel()
		return err
	}
	if err := upgradeSession.Complete(); err != nil {
		cancel()
		<-serveErrors
		return fmt.Errorf("complete desktop upgrade migration: %w", err)
	}
	if err := encoder.Encode(desktopprotocol.NewReady(options.AppVersion, baseURL)); err != nil {
		cancel()
		<-serveErrors
		return fmt.Errorf("write READY handshake: %w", err)
	}

	for {
		select {
		case event, open := <-commands:
			if !open {
				cancel()
				<-serveErrors
				return errors.New("desktop command stream closed")
			}
			if event.err != nil {
				cancel()
				<-serveErrors
				return event.err
			}
			if event.command.Type != desktopprotocol.CommandShutdown {
				cancel()
				<-serveErrors
				return fmt.Errorf("unexpected desktop command after startup: %s", event.command.Type)
			}
			cancel()
			return normalizeServeError(<-serveErrors)
		case err := <-serveErrors:
			return normalizeServeError(err)
		case <-runContext.Done():
			cancel()
			return normalizeServeError(<-serveErrors)
		}
	}
}

func startCommandStream(ctx context.Context, reader io.Reader) <-chan commandEvent {
	events := make(chan commandEvent)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024), maximumCommandBytes)
		for scanner.Scan() {
			command, err := desktopprotocol.ParseCommand(scanner.Bytes())
			event := commandEvent{command: command, err: err}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case events <- commandEvent{err: fmt.Errorf("read desktop command: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()
	return events
}

func waitForReady(ctx context.Context, application *app.App, serveErrors <-chan error) error {
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if application.Ready() {
			return nil
		}
		select {
		case err := <-serveErrors:
			if err == nil {
				return errors.New("desktop server stopped before becoming ready")
			}
			return fmt.Errorf("start desktop server: %w", err)
		case <-ticker.C:
		case <-timer.C:
			return errors.New("desktop server readiness timed out")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func normalizeServeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return fmt.Errorf("serve desktop application: %w", err)
}
