package desktopprotocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const Version = 1

type MessageType string

const (
	MessageReady             MessageType = "READY"
	MessageBootstrapRequired MessageType = "BOOTSTRAP_REQUIRED"
)

// Handshake is the versioned, non-secret startup message exchanged between
// the desktop core and shell. Unknown JSON fields are intentionally ignored.
type Handshake struct {
	Type            MessageType `json:"type"`
	ProtocolVersion int         `json:"protocol_version"`
	URL             string      `json:"url,omitempty"`
	AppVersion      string      `json:"app_version"`
}

func NewReady(appVersion, baseURL string) Handshake {
	return Handshake{
		Type:            MessageReady,
		ProtocolVersion: Version,
		URL:             baseURL,
		AppVersion:      appVersion,
	}
}

func NewBootstrapRequired(appVersion string) Handshake {
	return Handshake{
		Type:            MessageBootstrapRequired,
		ProtocolVersion: Version,
		AppVersion:      appVersion,
	}
}

func Parse(data []byte) (Handshake, error) {
	var message Handshake
	if err := json.Unmarshal(data, &message); err != nil {
		return Handshake{}, fmt.Errorf("decode desktop handshake: %w", err)
	}
	if err := message.Validate(); err != nil {
		return Handshake{}, err
	}
	return message, nil
}

func (m Handshake) Validate() error {
	if m.ProtocolVersion != Version {
		return fmt.Errorf("unsupported desktop protocol version: %d", m.ProtocolVersion)
	}
	if strings.TrimSpace(m.AppVersion) == "" {
		return errors.New("desktop handshake app_version is required")
	}
	switch m.Type {
	case MessageReady:
		if err := validateLoopbackURL(m.URL); err != nil {
			return fmt.Errorf("invalid READY URL: %w", err)
		}
	case MessageBootstrapRequired:
		if m.URL != "" {
			return errors.New("BOOTSTRAP_REQUIRED must not include a URL")
		}
	default:
		return fmt.Errorf("unsupported desktop handshake type: %q", m.Type)
	}
	return nil
}

func validateLoopbackURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return errors.New("must use an explicit IPv4 loopback HTTP origin")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("must include a valid TCP port")
	}
	if parsed.User != nil || parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must not include credentials, a non-root path, query, or fragment")
	}
	return nil
}
