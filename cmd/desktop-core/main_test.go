package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopcredentials"
	"cyberstrike-ai/internal/desktopprotocol"
	"cyberstrike-ai/internal/desktopruntime"
	"github.com/gin-gonic/gin"
)

func TestDesktopCoreLocalAdminGoldenPath(t *testing.T) {
	root := t.TempDir()
	cancelRequestStarted := make(chan struct{}, 1)
	upstream := newDesktopFakeAI(t, cancelRequestStarted)
	t.Cleanup(upstream.Close)
	resourceDir := writeTestResources(t, root, "test-version")
	credentialStore := newRecordingCredentialStore()
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir:     resourceDir,
		AppVersion:      "test-version",
		CredentialStore: credentialStore,
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create desktop stdout pipe: %v", err)
	}
	previousGinWriter := gin.DefaultWriter
	gin.DefaultWriter = stdoutWriter
	done := make(chan error, 1)
	go func() {
		done <- runDesktopCore(context.Background(), stdinReader, stdoutWriter, options)
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
	}()
	t.Cleanup(func() {
		gin.DefaultWriter = previousGinWriter
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})

	var stdoutTranscript bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(stdoutReader, &stdoutTranscript))
	var bootstrap desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &bootstrap)
	if bootstrap.Type != desktopprotocol.MessageBootstrapRequired || bootstrap.AppVersion != options.AppVersion {
		t.Fatalf("unexpected bootstrap handshake: %#v", bootstrap)
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandBootstrap,
		ProtocolVersion: desktopprotocol.Version,
		Password:        "desktop-secret",
	}); err != nil {
		t.Fatalf("write bootstrap command: %v", err)
	}

	var ready desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &ready)
	if err := ready.Validate(); err != nil {
		t.Fatalf("READY validation: %v", err)
	}
	if ready.Type != desktopprotocol.MessageReady || ready.AppVersion != options.AppVersion {
		t.Fatalf("unexpected READY handshake: %#v", ready)
	}
	response, err := http.Get(ready.URL + "health/ready")
	if err != nil {
		t.Fatalf("GET health ready: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health ready status = %d", response.StatusCode)
	}
	desktopPage, err := http.Get(ready.URL)
	if err != nil {
		t.Fatalf("GET desktop page: %v", err)
	}
	desktopPageBody, err := io.ReadAll(desktopPage.Body)
	_ = desktopPage.Body.Close()
	if err != nil {
		t.Fatalf("read desktop page: %v", err)
	}
	if desktopPage.StatusCode != http.StatusOK {
		t.Fatalf("desktop page status = %d, want 200", desktopPage.StatusCode)
	}
	for _, marker := range []string{
		`data-page="webshell"`,
		`data-page="c2"`,
		`data-page="platform-rbac"`,
		`data-section="robots"`,
		`id="settings-section-terminal"`,
		`/static/js/c2.js`,
		`/static/js/terminal.js`,
		`/static/js/webshell.js`,
		`id="robot-account-binding-modal"`,
	} {
		if bytes.Contains(desktopPageBody, []byte(marker)) {
			t.Fatalf("desktop page contains out-of-scope marker %q", marker)
		}
	}
	if !bytes.Contains(desktopPageBody, []byte(`/static/js/desktop-setup.js`)) {
		t.Fatal("desktop page does not contain desktop setup script")
	}

	status, body := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/conversations", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated conversations status = %d", status)
	}
	status, _ = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "wrong-password",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-password login status = %d", status)
	}
	status, login := desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "desktop-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("local admin login status = %d, body = %#v", status, login)
	}
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("local admin login did not return a token: %#v", login)
	}
	user, _ := login["user"].(map[string]interface{})
	if user["username"] != "admin" {
		t.Fatalf("local admin login returned unexpected user: %#v", login)
	}
	permissions, _ := login["permissions"].([]interface{})
	if len(permissions) == 0 {
		t.Fatalf("local admin login returned no permissions: %#v", login)
	}

	for _, path := range []string{
		"api/auth/validate",
		"api/conversations",
		"api/monitor/stats",
		"api/notifications/summary",
		"api/config",
	} {
		status, body := desktopJSONRequest(t, http.MethodGet, ready.URL+path, token, nil)
		if status != http.StatusOK {
			t.Fatalf("authenticated GET /%s status = %d, body = %#v", path, status, body)
		}
	}
	for _, request := range []struct {
		method string
		path   string
		token  string
	}{
		{http.MethodGet, "api/rbac/users", token},
		{http.MethodPost, "api/robot/test", token},
		{http.MethodPost, "api/robot/lark", ""},
		{http.MethodPost, "api/terminal/run", token},
		{http.MethodGet, "api/webshell/connections", token},
		{http.MethodGet, "api/c2/listeners", token},
	} {
		if status := desktopStatusRequest(t, request.method, ready.URL+request.path, request.token); status != http.StatusNotFound {
			t.Fatalf("excluded desktop route %s /%s status = %d, want 404", request.method, request.path, status)
		}
	}

	status, body = desktopJSONRequest(t, http.MethodPut, ready.URL+"api/config", token, map[string]interface{}{
		"ai": map[string]interface{}{
			"default_channel": "desktop-test",
			"channels": map[string]interface{}{
				"desktop-test": map[string]interface{}{
					"name":                  "Desktop Test",
					"provider":              "openai_compatible",
					"base_url":              upstream.URL + "/v1",
					"api_key":               "stream-secret",
					"model":                 "desktop-test-model",
					"max_total_tokens":      4096,
					"max_completion_tokens": 256,
				},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("desktop AI config update status = %d, body = %#v", status, body)
	}
	if len(credentialStore.values) != 1 {
		t.Fatalf("desktop AI credential store values = %#v", credentialStore.values)
	}
	configPath := filepath.Join(options.Roots.ConfigDir, "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read protected desktop config: %v", err)
	}
	if bytes.Contains(configData, []byte("stream-secret")) {
		t.Fatal("desktop AI configuration persisted plaintext")
	}

	events := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
		"message": "Reply with a short desktop streaming confirmation.",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	conversationID := ""
	foundResponse := false
	foundDone := false
	for _, event := range events {
		eventType, _ := event["type"].(string)
		switch eventType {
		case "conversation":
			data, _ := event["data"].(map[string]interface{})
			conversationID, _ = data["conversationId"].(string)
		case "response":
			foundResponse = true
		case "done":
			foundDone = true
		}
	}
	if conversationID == "" || !foundResponse || !foundDone {
		t.Fatalf("desktop SSE golden path events = %#v", events)
	}
	eventData, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(eventData, []byte("stream-secret")) {
		t.Fatal("desktop SSE exposed the AI credential")
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/conversations/"+conversationID, token, nil)
	if status != http.StatusOK {
		t.Fatalf("persisted desktop conversation status = %d, body = %#v", status, body)
	}
	failureEvents := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
		"message": "desktop-auth-failure",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	foundError := false
	foundFailureDone := false
	failureConversationID := ""
	for _, event := range failureEvents {
		switch event["type"] {
		case "conversation":
			data, _ := event["data"].(map[string]interface{})
			failureConversationID, _ = data["conversationId"].(string)
		case "error":
			foundError = true
		case "done":
			foundFailureDone = true
		case "response":
			t.Fatalf("authentication-failed desktop SSE returned a success response: %#v", failureEvents)
		}
	}
	if failureConversationID == "" || !foundError || !foundFailureDone {
		t.Fatalf("authentication-failed desktop SSE events = %#v", failureEvents)
	}
	failureData, err := json.Marshal(failureEvents)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(failureData, []byte("stream-secret")) || bytes.Contains(failureData, []byte("upstream-secret-marker")) {
		t.Fatalf("authentication-failed desktop SSE exposed upstream data: %s", failureData)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/conversations/"+failureConversationID, token, nil)
	if status != http.StatusOK {
		t.Fatalf("failed desktop conversation status = %d, body = %#v", status, body)
	}
	persistedFailure, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persistedFailure, []byte("stream-secret")) || bytes.Contains(persistedFailure, []byte("upstream-secret-marker")) {
		t.Fatalf("failed desktop conversation persisted upstream data: %s", persistedFailure)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/conversations", token, map[string]string{
		"title": "Desktop cancellation",
	})
	if status != http.StatusOK {
		t.Fatalf("create cancellation conversation status = %d, body = %#v", status, body)
	}
	cancelConversationID, _ := body["id"].(string)
	if cancelConversationID == "" {
		t.Fatalf("create cancellation conversation did not return an id: %#v", body)
	}
	type sseResult struct {
		events []map[string]interface{}
		err    error
	}
	cancelStreamResult := make(chan sseResult, 1)
	go func() {
		events, err := desktopSSERequestResult(ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
			"conversationId": cancelConversationID,
			"message":        "desktop-cancel-running-agent",
			"finalization": map[string]interface{}{
				"requireExecutionEvidence": false,
			},
		})
		cancelStreamResult <- sseResult{events: events, err: err}
	}()
	select {
	case <-cancelRequestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("desktop Agent did not reach the cancellable upstream request")
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks", token, nil)
	if status != http.StatusOK {
		t.Fatalf("active desktop tasks status = %d, body = %#v", status, body)
	}
	if taskStatus, found := desktopTaskStatus(body, cancelConversationID); !found || taskStatus != "running" {
		t.Fatalf("active desktop task status = %q, found = %v, body = %#v", taskStatus, found, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/agent-loop/cancel", token, map[string]string{
		"conversationId": cancelConversationID,
	})
	if status != http.StatusOK || body["status"] != "cancelling" {
		t.Fatalf("cancel desktop Agent status = %d, body = %#v", status, body)
	}
	var cancelledResult sseResult
	select {
	case cancelledResult = <-cancelStreamResult:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled desktop Agent stream did not finish")
	}
	if cancelledResult.err != nil {
		t.Fatalf("cancelled desktop Agent stream: %v", cancelledResult.err)
	}
	foundCancelled := false
	foundCancelDone := false
	for _, event := range cancelledResult.events {
		switch event["type"] {
		case "cancelled":
			foundCancelled = true
		case "done":
			foundCancelDone = true
		case "response":
			t.Fatalf("cancelled desktop Agent returned a success response: %#v", cancelledResult.events)
		}
	}
	if !foundCancelled || !foundCancelDone {
		t.Fatalf("cancelled desktop Agent events = %#v", cancelledResult.events)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks", token, nil)
	if status != http.StatusOK {
		t.Fatalf("post-cancel active desktop tasks status = %d, body = %#v", status, body)
	}
	if taskStatus, found := desktopTaskStatus(body, cancelConversationID); found {
		t.Fatalf("cancelled desktop task remained active with status %q: %#v", taskStatus, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks/completed", token, nil)
	if status != http.StatusOK {
		t.Fatalf("completed desktop tasks status = %d, body = %#v", status, body)
	}
	if taskStatus, found := desktopTaskStatus(body, cancelConversationID); !found || taskStatus != "cancelled" {
		t.Fatalf("completed desktop task status = %q, found = %v, body = %#v", taskStatus, found, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/conversations", token, map[string]string{
		"title": "Desktop HITL",
	})
	if status != http.StatusOK {
		t.Fatalf("create HITL conversation status = %d, body = %#v", status, body)
	}
	hitlConversationID, _ := body["id"].(string)
	if hitlConversationID == "" {
		t.Fatalf("create HITL conversation did not return an id: %#v", body)
	}
	hitlStreamResult := make(chan sseResult, 1)
	go func() {
		events, err := desktopSSERequestResult(ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
			"conversationId": hitlConversationID,
			"message":        "desktop-hitl-approval",
			"hitl": map[string]interface{}{
				"enabled":        true,
				"mode":           "approval",
				"reviewer":       "human",
				"sensitiveTools": []string{},
				"timeoutSeconds": 30,
			},
			"finalization": map[string]interface{}{
				"requireExecutionEvidence": false,
			},
		})
		hitlStreamResult <- sseResult{events: events, err: err}
	}()
	interruptID := ""
	pendingDeadline := time.Now().Add(5 * time.Second)
	for interruptID == "" && time.Now().Before(pendingDeadline) {
		status, pending := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/hitl/pending", token, nil)
		if status != http.StatusOK {
			t.Fatalf("desktop HITL pending status = %d, body = %#v", status, pending)
		}
		interruptID = desktopHITLInterruptID(pending, hitlConversationID)
		if interruptID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if interruptID == "" {
		select {
		case earlyResult := <-hitlStreamResult:
			logData, _ := os.ReadFile(filepath.Join(options.Roots.LogDir, "cyberstrike-ai.log"))
			t.Fatalf("desktop HITL interrupt did not become pending: err = %v, events = %#v, log = %s", earlyResult.err, earlyResult.events, logData)
		default:
			t.Fatal("desktop HITL interrupt did not become pending")
		}
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/hitl/decision", token, map[string]string{
		"interruptId": interruptID,
		"decision":    "reject",
		"comment":     "desktop rejection check",
	})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("desktop HITL rejection status = %d, body = %#v", status, body)
	}
	var hitlResult sseResult
	select {
	case hitlResult = <-hitlStreamResult:
	case <-time.After(5 * time.Second):
		t.Fatal("desktop HITL stream did not resume after rejection")
	}
	if hitlResult.err != nil {
		t.Fatalf("desktop HITL stream: %v", hitlResult.err)
	}
	foundHITLInterrupt := false
	foundHITLRejected := false
	foundHITLResponse := false
	foundHITLDone := false
	for _, event := range hitlResult.events {
		switch event["type"] {
		case "hitl_interrupt":
			foundHITLInterrupt = true
		case "hitl_rejected":
			foundHITLRejected = true
		case "response":
			foundHITLResponse = true
		case "done":
			foundHITLDone = true
		}
	}
	if !foundHITLInterrupt || !foundHITLRejected || !foundHITLResponse || !foundHITLDone {
		t.Fatalf("desktop HITL events = %#v", hitlResult.events)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/hitl/pending", token, nil)
	if status != http.StatusOK || desktopHITLInterruptID(body, hitlConversationID) != "" {
		t.Fatalf("resolved desktop HITL remained pending: status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks", token, nil)
	if status != http.StatusOK {
		t.Fatalf("post-HITL active desktop tasks status = %d, body = %#v", status, body)
	}
	if taskStatus, found := desktopTaskStatus(body, hitlConversationID); found {
		t.Fatalf("resolved desktop HITL task remained active with status %q: %#v", taskStatus, body)
	}

	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/logout", token, nil)
	if status != http.StatusOK {
		t.Fatalf("local admin logout status = %d, body = %#v", status, body)
	}
	status, _ = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/auth/validate", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token validation status = %d", status)
	}

	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandShutdown,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write shutdown command: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDesktopCore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("desktop core did not shut down")
	}
	if _, err := io.Copy(&stdoutTranscript, stdoutReader); err != nil {
		t.Fatalf("drain desktop stdout: %v", err)
	}
	protocolLines := strings.Split(strings.TrimSpace(stdoutTranscript.String()), "\n")
	if len(protocolLines) != 2 {
		t.Fatalf("desktop stdout must contain exactly two protocol messages, got %d: %q", len(protocolLines), stdoutTranscript.String())
	}
	for index, line := range protocolLines {
		var handshake desktopprotocol.Handshake
		if err := json.Unmarshal([]byte(line), &handshake); err != nil {
			t.Fatalf("desktop stdout message %d is not valid JSON: %v", index, err)
		}
	}
	if strings.Contains(stdoutTranscript.String(), "desktop-secret") {
		t.Fatal("bootstrap password leaked to desktop stdout")
	}
	assertSecretNotPersisted(t, root, "desktop-secret")
}

func newDesktopFakeAI(t *testing.T, cancelRequestStarted chan<- struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("desktop fake AI path = %q", request.URL.Path)
			http.Error(response, "unexpected path", http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer stream-secret" {
			t.Errorf("desktop fake AI authorization = %q", request.Header.Get("Authorization"))
			http.Error(response, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode desktop fake AI request: %v", err)
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		requestData, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode desktop fake AI request: %v", err)
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		if toolName := desktopForbiddenTool(payload); toolName != "" {
			t.Errorf("desktop fake AI received excluded tool %q", toolName)
			http.Error(response, "excluded desktop tool", http.StatusInternalServerError)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-auth-failure")) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{"error":{"message":"upstream-secret-marker"}}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-cancel-running-agent")) {
			select {
			case cancelRequestStarted <- struct{}{}:
			default:
			}
			<-request.Context().Done()
			return
		}
		if bytes.Contains(requestData, []byte("desktop-hitl-approval")) &&
			!bytes.Contains(requestData, []byte("[HITL Reject]")) &&
			desktopPayloadHasTool(payload, "query_assets") {
			if streaming, _ := payload["stream"].(bool); streaming {
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-hitl\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-desktop-hitl\",\"type\":\"function\",\"function\":{\"name\":\"query_assets\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n")
				_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-hitl\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				_, _ = io.WriteString(response, "data: [DONE]\n\n")
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"id":"chatcmpl-desktop-hitl","object":"chat.completion","model":"desktop-test-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-desktop-hitl","type":"function","function":{"name":"query_assets","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if streaming, _ := payload["stream"].(bool); streaming {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"desktop streamed reply\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"chatcmpl-desktop","object":"chat.completion","model":"desktop-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"desktop streamed reply"},"finish_reason":"stop"}]}`)
	}))
}

func desktopSSERequest(t *testing.T, target, token string, body interface{}) []map[string]interface{} {
	t.Helper()
	events, err := desktopSSERequestResult(target, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func desktopSSERequestResult(target, token string, body interface{}) ([]map[string]interface{}, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode desktop SSE request: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create desktop SSE request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send desktop SSE request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("desktop SSE status = %d: %s", response.StatusCode, data)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return nil, fmt.Errorf("desktop SSE content type = %q", response.Header.Get("Content-Type"))
	}
	events := make([]map[string]interface{}, 0, 8)
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return nil, fmt.Errorf("decode desktop SSE event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read desktop SSE response: %w", err)
	}
	return events, nil
}

func desktopTaskStatus(body map[string]interface{}, conversationID string) (string, bool) {
	tasks, _ := body["tasks"].([]interface{})
	for _, item := range tasks {
		task, _ := item.(map[string]interface{})
		if task["conversationId"] == conversationID {
			status, _ := task["status"].(string)
			return status, true
		}
	}
	return "", false
}

func desktopHITLInterruptID(body map[string]interface{}, conversationID string) string {
	items, _ := body["items"].([]interface{})
	for _, item := range items {
		interrupt, _ := item.(map[string]interface{})
		if interrupt["conversationId"] == conversationID {
			interruptID, _ := interrupt["interruptId"].(string)
			if interruptID == "" {
				interruptID, _ = interrupt["id"].(string)
			}
			return interruptID
		}
	}
	return ""
}

func desktopPayloadHasTool(payload map[string]interface{}, toolName string) bool {
	tools, _ := payload["tools"].([]interface{})
	for _, item := range tools {
		tool, _ := item.(map[string]interface{})
		function, _ := tool["function"].(map[string]interface{})
		if function["name"] == toolName {
			return true
		}
	}
	return false
}

func desktopForbiddenTool(payload map[string]interface{}) string {
	tools, _ := payload["tools"].([]interface{})
	for _, item := range tools {
		tool, _ := item.(map[string]interface{})
		function, _ := tool["function"].(map[string]interface{})
		name, _ := function["name"].(string)
		lower := strings.ToLower(name)
		for _, prefix := range []string{"c2_", "manage_webshell_", "robot_", "webshell_"} {
			if strings.HasPrefix(lower, prefix) {
				return name
			}
		}
	}
	return ""
}

func desktopStatusRequest(t *testing.T, method, target, token string) int {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("create desktop status request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send desktop status request: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func desktopJSONRequest(t *testing.T, method, target, token string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode desktop request body: %v", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, requestBody)
	if err != nil {
		t.Fatalf("create desktop request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send desktop request: %v", err)
	}
	defer response.Body.Close()
	payload := make(map[string]interface{})
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("decode desktop response: %v", err)
	}
	return response.StatusCode, payload
}

func TestDesktopCoreMigratesCredentialsOnlyAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "test-version")
	appendTestResourceConfig(t, resourceDir, "test-version", "fofa:\n  api_key: migration-secret\n")
	store := newRecordingCredentialStore()
	options := runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir:     resourceDir,
		AppVersion:      "test-version",
		CredentialStore: store,
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runDesktopCore(context.Background(), stdinReader, stdoutWriter, options)
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})

	var stdoutTranscript bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(stdoutReader, &stdoutTranscript))
	var migration desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &migration)
	if err := migration.Validate(); err != nil {
		t.Fatalf("migration validation: %v", err)
	}
	if migration.Type != desktopprotocol.MessageCredentialMigrationRequired || len(migration.CredentialPaths) != 1 || migration.CredentialPaths[0] != desktopcredentials.PathFOFA {
		t.Fatalf("unexpected migration handshake: %#v", migration)
	}
	if len(store.values) != 0 {
		t.Fatalf("credential store changed before confirmation: %v", store.values)
	}
	if strings.Contains(stdoutTranscript.String(), "migration-secret") {
		t.Fatal("migration handshake exposed plaintext")
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandMigrateCredentials,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write migration command: %v", err)
	}

	var bootstrap desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &bootstrap)
	if bootstrap.Type != desktopprotocol.MessageBootstrapRequired {
		t.Fatalf("unexpected bootstrap handshake: %#v", bootstrap)
	}
	if len(store.values) != 1 {
		t.Fatalf("credential store values after confirmation = %v", store.values)
	}
	paths, err := desktopruntime.ResolvePaths(options.Roots)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configData, []byte("migration-secret")) || !bytes.Contains(configData, []byte(desktopcredentials.ReferencePrefix)) {
		t.Fatalf("migrated config did not contain only a credential reference: %q", configData)
	}

	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandBootstrap,
		ProtocolVersion: desktopprotocol.Version,
		Password:        "desktop-secret",
	}); err != nil {
		t.Fatalf("write bootstrap command: %v", err)
	}
	var ready desktopprotocol.Handshake
	decodeWithTimeout(t, decoder, &ready)
	if ready.Type != desktopprotocol.MessageReady {
		t.Fatalf("unexpected READY handshake: %#v", ready)
	}
	if err := json.NewEncoder(stdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandShutdown,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write shutdown command: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDesktopCore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("desktop core did not shut down")
	}
	if strings.Contains(stdoutTranscript.String(), "migration-secret") {
		t.Fatal("desktop stdout exposed migrated plaintext")
	}
}

func TestDesktopCoreRejectsResourceVersionMismatch(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "resource-version")
	err := runDesktopCore(context.Background(), emptyReader{}, io.Discard, runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir: resourceDir,
		AppVersion:  "different-version",
	})
	if err == nil {
		t.Fatal("expected resource version mismatch")
	}
}

func TestDesktopCoreFailsClosedWhenCredentialStoreIsUnavailable(t *testing.T) {
	root := t.TempDir()
	resourceDir := writeTestResources(t, root, "test-version")
	appendTestResourceConfig(t, resourceDir, "test-version", "fofa:\n  api_key: migration-secret\n")

	stdin := bytes.NewBufferString(`{"type":"MIGRATE_CREDENTIALS","protocol_version":1}` + "\n")
	err := runDesktopCore(context.Background(), stdin, io.Discard, runOptions{
		Roots: desktopruntime.Roots{
			DataDir:   filepath.Join(root, "data"),
			ConfigDir: filepath.Join(root, "config"),
			CacheDir:  filepath.Join(root, "cache"),
			LogDir:    filepath.Join(root, "logs"),
			TempDir:   filepath.Join(root, "temp"),
		},
		ResourceDir:     resourceDir,
		AppVersion:      "test-version",
		CredentialStore: failingCredentialStore{},
	})
	if err == nil || !strings.Contains(err.Error(), "store desktop credential fofa.api_key") {
		t.Fatalf("unexpected credential store error: %v", err)
	}
	if strings.Contains(err.Error(), "migration-secret") {
		t.Fatal("credential store error exposed plaintext")
	}
}

type failingCredentialStore struct{}

func (failingCredentialStore) Get(string) (string, error) {
	return "", errors.New("credential store unavailable")
}

func (failingCredentialStore) Set(string, string) error {
	return errors.New("credential store unavailable")
}

func (failingCredentialStore) Delete(string) error { return nil }

type recordingCredentialStore struct {
	values map[string]string
}

func newRecordingCredentialStore() *recordingCredentialStore {
	return &recordingCredentialStore{values: make(map[string]string)}
}

func (s *recordingCredentialStore) Get(account string) (string, error) {
	value, ok := s.values[account]
	if !ok {
		return "", errors.New("credential not found")
	}
	return value, nil
}

func (s *recordingCredentialStore) Set(account, secret string) error {
	s.values[account] = secret
	return nil
}

func (s *recordingCredentialStore) Delete(account string) error {
	delete(s.values, account)
	return nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func writeTestResources(t *testing.T, root, version string) string {
	t.Helper()
	resourceDir := filepath.Join(root, "bundled-defaults")
	if err := os.MkdirAll(resourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`version: test
server:
  host: 127.0.0.1
  port: 0
auth:
  session_duration_hours: 12
log:
  level: error
  output: stdout
knowledge:
  enabled: false
c2:
  enabled: false
`)
	if err := os.WriteFile(filepath.Join(resourceDir, "config.example.yaml"), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(configData)
	manifest := desktopruntime.ResourceManifest{
		SchemaVersion: 1,
		AppVersion:    version,
		Files: []desktopruntime.ResourceFile{{
			Path:   "config.example.yaml",
			SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	return resourceDir
}

func appendTestResourceConfig(t *testing.T, resourceDir, version, extra string) {
	t.Helper()
	configPath := filepath.Join(resourceDir, "config.example.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configData = append(configData, []byte(extra)...)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(configData)
	manifestData, err := json.Marshal(desktopruntime.ResourceManifest{
		SchemaVersion: 1,
		AppVersion:    version,
		Files: []desktopruntime.ResourceFile{{
			Path:   "config.example.yaml",
			SHA256: hex.EncodeToString(sum[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeWithTimeout(t *testing.T, decoder *json.Decoder, target any) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- decoder.Decode(target) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode desktop handshake: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for desktop handshake")
	}
}

func assertSecretNotPersisted(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("bootstrap password persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan desktop files for plaintext password: %v", err)
	}
}
