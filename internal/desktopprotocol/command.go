package desktopprotocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type CommandType string

const (
	CommandBootstrap          CommandType = "BOOTSTRAP"
	CommandMigrateCredentials CommandType = "MIGRATE_CREDENTIALS"
	CommandShutdown           CommandType = "SHUTDOWN"
)

// Command is a versioned JSON line sent to the core over its inherited stdin.
// Bootstrap passwords never appear in process arguments, files, or stdout.
type Command struct {
	Type            CommandType `json:"type"`
	ProtocolVersion int         `json:"protocol_version"`
	Password        string      `json:"password,omitempty"`
}

func ParseCommand(data []byte) (Command, error) {
	var command Command
	if err := json.Unmarshal(data, &command); err != nil {
		return Command{}, fmt.Errorf("decode desktop command: %w", err)
	}
	if err := command.Validate(); err != nil {
		return Command{}, err
	}
	return command, nil
}

func (c Command) Validate() error {
	if c.ProtocolVersion != Version {
		return fmt.Errorf("unsupported desktop command protocol version: %d", c.ProtocolVersion)
	}
	switch c.Type {
	case CommandBootstrap:
		if len(strings.TrimSpace(c.Password)) < 8 {
			return errors.New("desktop bootstrap password must be at least 8 characters")
		}
	case CommandMigrateCredentials, CommandShutdown:
		if c.Password != "" {
			return fmt.Errorf("desktop %s command must not include a password", c.Type)
		}
	default:
		return fmt.Errorf("unsupported desktop command type: %q", c.Type)
	}
	return nil
}
