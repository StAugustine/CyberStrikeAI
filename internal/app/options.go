package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"cyberstrike-ai/internal/desktopcredentials"
)

type Option func(*appOptions) error

type appOptions struct {
	webFS                        fs.FS
	initialAdminPasswordProvider func() (string, error)
	desktopCredentialManager     *desktopcredentials.Manager
	desktopMode                  bool
	desktopUploadsRoot           string
}

// WithDesktopMode limits routes, background services, and tools to the
// explicitly supported desktop-client feature set.
func WithDesktopMode() Option {
	return func(options *appOptions) error {
		options.desktopMode = true
		return nil
	}
}

// WithDesktopUploadsRoot stores chat attachments under the platform data
// directory supplied by the desktop shell instead of deriving it from CWD.
func WithDesktopUploadsRoot(root string) Option {
	return func(options *appOptions) error {
		root = strings.TrimSpace(root)
		if root == "" || !filepath.IsAbs(root) {
			return fmt.Errorf("desktop uploads root must be absolute: %q", root)
		}
		options.desktopUploadsRoot = filepath.Clean(root)
		return nil
	}
}

// WithDesktopCredentialManager enables keyring-backed secret persistence and
// redacted configuration responses for the desktop-hosted application.
func WithDesktopCredentialManager(manager *desktopcredentials.Manager) Option {
	return func(options *appOptions) error {
		if manager == nil {
			return errors.New("desktop credential manager is required")
		}
		options.desktopCredentialManager = manager
		return nil
	}
}

// WithWebFS replaces the default on-disk web directory with a caller-owned
// filesystem, such as the curated embedded filesystem used by desktop builds.
func WithWebFS(webFS fs.FS) Option {
	return func(options *appOptions) error {
		if webFS == nil {
			return errors.New("web filesystem is required")
		}
		options.webFS = webFS
		return nil
	}
}

// WithInitialAdminPasswordProvider supplies the first local administrator
// password only when a fresh database requires bootstrap.
func WithInitialAdminPasswordProvider(provider func() (string, error)) Option {
	return func(options *appOptions) error {
		if provider == nil {
			return errors.New("initial admin password provider is required")
		}
		options.initialAdminPasswordProvider = provider
		return nil
	}
}

func resolveOptions(options []Option) (appOptions, error) {
	resolved := appOptions{webFS: os.DirFS("web")}
	for index, option := range options {
		if option == nil {
			return appOptions{}, fmt.Errorf("application option %d is nil", index)
		}
		if err := option(&resolved); err != nil {
			return appOptions{}, fmt.Errorf("apply application option %d: %w", index, err)
		}
	}
	return resolved, nil
}
