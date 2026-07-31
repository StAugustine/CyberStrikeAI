package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

type Option func(*appOptions) error

type appOptions struct {
	webFS fs.FS
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
