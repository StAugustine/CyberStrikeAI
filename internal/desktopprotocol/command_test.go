package desktopprotocol

import "testing"

func TestParseBootstrapCommandAcceptsUnknownFields(t *testing.T) {
	command, err := ParseCommand([]byte(`{
		"type":"BOOTSTRAP",
		"protocol_version":1,
		"password":"desktop-secret",
		"future_field":true
	}`))
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if command.Type != CommandBootstrap || command.Password != "desktop-secret" {
		t.Fatalf("unexpected command: %#v", command)
	}
}

func TestParseShutdownCommand(t *testing.T) {
	command, err := ParseCommand([]byte(`{"type":"SHUTDOWN","protocol_version":1}`))
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if command.Type != CommandShutdown || command.Password != "" {
		t.Fatalf("unexpected command: %#v", command)
	}
}

func TestParseCommandRejectsInvalidCommands(t *testing.T) {
	tests := []string{
		`{"type":"BOOTSTRAP","protocol_version":2,"password":"desktop-secret"}`,
		`{"type":"BOOTSTRAP","protocol_version":1}`,
		`{"type":"BOOTSTRAP","protocol_version":1,"password":"short"}`,
		`{"type":"SHUTDOWN","protocol_version":1,"password":"desktop-secret"}`,
		`{"type":"UNKNOWN","protocol_version":1}`,
	}
	for _, data := range tests {
		if _, err := ParseCommand([]byte(data)); err == nil {
			t.Fatalf("expected invalid command to fail: %s", data)
		}
	}
}
