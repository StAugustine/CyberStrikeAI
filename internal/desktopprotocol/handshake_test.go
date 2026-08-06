package desktopprotocol

import "testing"

func TestParseReadyAcceptsUnknownFields(t *testing.T) {
	message, err := Parse([]byte(`{
		"type":"READY",
		"protocol_version":1,
		"url":"http://127.0.0.1:43123/",
		"app_version":"v1.2.3",
		"future_field":{"enabled":true}
	}`))
	if err != nil {
		t.Fatalf("parse READY: %v", err)
	}
	if message.Type != MessageReady || message.URL != "http://127.0.0.1:43123/" {
		t.Fatalf("unexpected READY message: %#v", message)
	}
}

func TestParseBootstrapRequired(t *testing.T) {
	message, err := Parse([]byte(`{
		"type":"BOOTSTRAP_REQUIRED",
		"protocol_version":1,
		"app_version":"v1.2.3"
	}`))
	if err != nil {
		t.Fatalf("parse BOOTSTRAP_REQUIRED: %v", err)
	}
	if message.Type != MessageBootstrapRequired || message.URL != "" {
		t.Fatalf("unexpected BOOTSTRAP_REQUIRED message: %#v", message)
	}
}

func TestParseCredentialMigrationRequired(t *testing.T) {
	message, err := Parse([]byte(`{
		"type":"CREDENTIAL_MIGRATION_REQUIRED",
		"protocol_version":1,
		"app_version":"v1.2.3",
		"credential_paths":["ai.channels.primary.api_key","fofa.api_key"]
	}`))
	if err != nil {
		t.Fatalf("parse CREDENTIAL_MIGRATION_REQUIRED: %v", err)
	}
	if message.Type != MessageCredentialMigrationRequired || len(message.CredentialPaths) != 2 {
		t.Fatalf("unexpected CREDENTIAL_MIGRATION_REQUIRED message: %#v", message)
	}
}

func TestParseRejectsIncompatibleOrUnsafeMessages(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "protocol version", data: `{"type":"READY","protocol_version":2,"url":"http://127.0.0.1:43123/","app_version":"v1"}`},
		{name: "message type", data: `{"type":"UNKNOWN","protocol_version":1,"app_version":"v1"}`},
		{name: "missing app version", data: `{"type":"READY","protocol_version":1,"url":"http://127.0.0.1:43123/"}`},
		{name: "non-loopback URL", data: `{"type":"READY","protocol_version":1,"url":"http://localhost:43123/","app_version":"v1"}`},
		{name: "URL credentials", data: `{"type":"READY","protocol_version":1,"url":"http://user@127.0.0.1:43123/","app_version":"v1"}`},
		{name: "bootstrap URL", data: `{"type":"BOOTSTRAP_REQUIRED","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"v1"}`},
		{name: "ready credential paths", data: `{"type":"READY","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"v1","credential_paths":["fofa.api_key"]}`},
		{name: "bootstrap credential paths", data: `{"type":"BOOTSTRAP_REQUIRED","protocol_version":1,"app_version":"v1","credential_paths":["fofa.api_key"]}`},
		{name: "migration URL", data: `{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"url":"http://127.0.0.1:43123/","app_version":"v1","credential_paths":["fofa.api_key"]}`},
		{name: "missing migration paths", data: `{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"v1"}`},
		{name: "empty migration path", data: `{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"v1","credential_paths":[""]}`},
		{name: "duplicate migration path", data: `{"type":"CREDENTIAL_MIGRATION_REQUIRED","protocol_version":1,"app_version":"v1","credential_paths":["fofa.api_key","fofa.api_key"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.data)); err == nil {
				t.Fatal("expected invalid handshake to be rejected")
			}
		})
	}
}
