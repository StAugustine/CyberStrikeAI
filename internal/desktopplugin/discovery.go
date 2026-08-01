package desktopplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DiscoverySchemaVersion = 1
	DiscoveryFileName      = "plugin-discovery.json"
	ProductConfigDirectory = "com.cyberstrikeai.desktop"
	maximumDiscoveryBytes  = 16 * 1024
	maximumDiscoveryTTL    = 2 * time.Minute
)

// Discovery contains only short-lived routing metadata. Authentication
// remains on the existing login/session path; passwords and tokens are never
// written to this document or returned by the native messaging host.
type Discovery struct {
	SchemaVersion int    `json:"schema_version"`
	InstanceID    string `json:"instance_id"`
	BaseURL       string `json:"base_url"`
	AppVersion    string `json:"app_version"`
	IssuedAtUnix  int64  `json:"issued_at_unix"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
}

func DefaultDiscoveryPath() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve desktop plugin configuration: %w", err)
	}
	if !filepath.IsAbs(configRoot) {
		return "", errors.New("desktop plugin configuration root is not absolute")
	}
	return filepath.Join(configRoot, ProductConfigDirectory, DiscoveryFileName), nil
}

func LoadDiscovery(path string, now time.Time) (Discovery, error) {
	if !filepath.IsAbs(strings.TrimSpace(path)) {
		return Discovery{}, errors.New("desktop discovery path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Discovery{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Discovery{}, errors.New("desktop discovery is not a regular file")
	}
	if err := validateDiscoveryFilePermissions(info); err != nil {
		return Discovery{}, err
	}
	if info.Size() <= 0 || info.Size() > maximumDiscoveryBytes {
		return Discovery{}, errors.New("desktop discovery has an invalid size")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Discovery{}, err
	}
	if len(data) == 0 || len(data) > maximumDiscoveryBytes {
		return Discovery{}, errors.New("desktop discovery has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var discovery Discovery
	if err := decoder.Decode(&discovery); err != nil {
		return Discovery{}, errors.New("desktop discovery is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Discovery{}, errors.New("desktop discovery contains trailing data")
	}
	if err := validateDiscovery(discovery, now); err != nil {
		return Discovery{}, err
	}
	return discovery, nil
}

func validateDiscovery(discovery Discovery, now time.Time) error {
	if discovery.SchemaVersion != DiscoverySchemaVersion {
		return errors.New("desktop discovery schema is unsupported")
	}
	if !validInstanceID(discovery.InstanceID) {
		return errors.New("desktop discovery instance is invalid")
	}
	if strings.TrimSpace(discovery.AppVersion) == "" || len(discovery.AppVersion) > 64 {
		return errors.New("desktop discovery version is invalid")
	}
	if err := validateLoopbackBaseURL(discovery.BaseURL); err != nil {
		return err
	}
	issuedAt := time.Unix(discovery.IssuedAtUnix, 0)
	expiresAt := time.Unix(discovery.ExpiresAtUnix, 0)
	if discovery.IssuedAtUnix <= 0 || !expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maximumDiscoveryTTL {
		return errors.New("desktop discovery lifetime is invalid")
	}
	if issuedAt.After(now.Add(30 * time.Second)) {
		return errors.New("desktop discovery was issued in the future")
	}
	if !expiresAt.After(now) {
		return errors.New("desktop discovery has expired")
	}
	return nil
}

func validInstanceID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validateLoopbackBaseURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("desktop discovery endpoint is invalid")
	}
	port := parsed.Port()
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || net.ParseIP(parsed.Hostname()) == nil {
		return errors.New("desktop discovery endpoint is invalid")
	}
	return nil
}
