package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyberstrike-ai/internal/desktopcredentials"
	"cyberstrike-ai/internal/desktopprotocol"
	"cyberstrike-ai/internal/desktopruntime"
	"github.com/gin-gonic/gin"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDesktopCoreLocalAdminGoldenPath(t *testing.T) {
	root := t.TempDir()
	cancelRequestStarted := make(chan struct{}, 1)
	liveResponseStarted := make(chan struct{}, 1)
	releaseLiveResponse := make(chan struct{})
	releaseLive := func() {
		select {
		case <-releaseLiveResponse:
		default:
			close(releaseLiveResponse)
		}
	}
	t.Cleanup(releaseLive)
	upstream := newDesktopFakeAI(t, cancelRequestStarted, liveResponseStarted, releaseLiveResponse)
	t.Cleanup(upstream.Close)
	externalMCP, externalMCPCalls := newDesktopFakeMCP(t)
	t.Cleanup(externalMCP.Close)
	infoCollect := newDesktopFakeInfoCollect(t)
	t.Cleanup(infoCollect.Close)
	credentialStore := newRecordingCredentialStore()
	credentialStore.values["desktop-knowledge"] = "desktop-embedding-secret"
	resourceDir := writeTestResources(t, root, "test-version")
	replaceTestResourceConfig(t, resourceDir, "test-version", "knowledge:\n  enabled: false\n", fmt.Sprintf(`knowledge:
  enabled: true
  embedding:
    provider: openai
    model: text-embedding-3-small
    base_url: %s/v1
    api_key: keyring://desktop-knowledge
  indexing:
    max_retries: 1
    retry_delay_ms: 1
openai:
  base_url: %s/v1
  api_key: keyring://desktop-knowledge
  model: desktop-test-model
`, upstream.URL, upstream.URL))
	appendTestResourceConfig(t, resourceDir, "test-version", `multi_agent:
  eino_middleware:
    run_retry_max_attempts: 1
    run_retry_max_backoff_sec: 1
  sub_agents:
    - id: desktop-specialist
      name: Desktop Specialist
      description: Verifies the desktop multi-Agent runtime.
      instruction: Return a concise desktop confirmation.
audit:
  enabled: true
  retention_days: 15
  max_detail_bytes: 4096
`)
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
	type sseResult struct {
		events []map[string]interface{}
		err    error
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
		`id="vulnerability-alert-settings"`,
		`id="dashboard-section-access"`,
		`window.open('/api-docs'`,
		`window.open('https://github.com/Ed1s0nZ/CyberStrikeAI'`,
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
		{http.MethodGet, "api/vulnerability-alerts/subscription", token},
		{http.MethodPut, "api/vulnerability-alerts/subscription", token},
		{http.MethodPost, "api/terminal/run", token},
		{http.MethodGet, "api/webshell/connections", token},
		{http.MethodGet, "api/c2/listeners", token},
	} {
		if status := desktopStatusRequest(t, request.method, ready.URL+request.path, request.token); status != http.StatusNotFound {
			t.Fatalf("excluded desktop route %s /%s status = %d, want 404", request.method, request.path, status)
		}
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/fofa/search", token, map[string]interface{}{
		"provider": "fofa",
		"query":    `domain="desktop.example.test"`,
	})
	need, _ := body["need"].([]interface{})
	if status != http.StatusBadRequest || len(need) != 1 || need[0] != "fofa.api_key" {
		t.Fatalf("unconfigured desktop info collection status = %d, body = %#v", status, body)
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
		"multi_agent": map[string]interface{}{
			"enabled": true,
		},
		"fofa": map[string]interface{}{
			"base_url": infoCollect.URL + "/fofa",
			"api_key":  "desktop-fofa-secret",
		},
		"zoomeye": map[string]interface{}{
			"base_url": infoCollect.URL + "/zoomeye",
			"api_key":  "desktop-zoomeye-secret",
		},
		"quake": map[string]interface{}{
			"base_url": infoCollect.URL + "/quake",
			"api_key":  "desktop-quake-secret",
		},
		"shodan": map[string]interface{}{
			"base_url": infoCollect.URL,
			"api_key":  "desktop-shodan-secret",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("desktop AI config update status = %d, body = %#v", status, body)
	}
	for _, secret := range []string{
		"stream-secret",
		"desktop-fofa-secret",
		"desktop-zoomeye-secret",
		"desktop-quake-secret",
		"desktop-shodan-secret",
	} {
		if !desktopCredentialStoreContains(credentialStore, secret) {
			t.Fatalf("desktop credential store does not contain %q: %#v", secret, credentialStore.values)
		}
	}
	if len(credentialStore.values) != 6 || credentialStore.values["desktop-knowledge"] != "desktop-embedding-secret" {
		t.Fatalf("desktop AI credential store values = %#v", credentialStore.values)
	}
	configPath := filepath.Join(options.Roots.ConfigDir, "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read protected desktop config: %v", err)
	}
	for _, secret := range []string{
		"stream-secret",
		"desktop-embedding-secret",
		"desktop-fofa-secret",
		"desktop-zoomeye-secret",
		"desktop-quake-secret",
		"desktop-shodan-secret",
	} {
		if bytes.Contains(configData, []byte(secret)) {
			t.Fatalf("desktop configuration persisted plaintext secret %q", secret)
		}
	}
	desktopAssertInfoCollect(t, ready.URL, token)

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
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/groups", token, map[string]string{
		"name": "Desktop Golden Group",
		"icon": "📁",
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop conversation group status = %d, body = %#v", status, body)
	}
	groupID, _ := body["id"].(string)
	if groupID == "" {
		t.Fatalf("create desktop conversation group did not return an id: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, ready.URL+"api/groups/"+groupID, token, map[string]string{
		"name": "Desktop Persistent Group",
		"icon": "🖥️",
	})
	if status != http.StatusOK || body["name"] != "Desktop Persistent Group" {
		t.Fatalf("update desktop conversation group status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, ready.URL+"api/groups/"+groupID+"/pinned", token, map[string]bool{"pinned": true})
	if status != http.StatusOK {
		t.Fatalf("pin desktop conversation group status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/groups/conversations", token, map[string]string{
		"conversationId": conversationID,
		"groupId":        groupID,
	})
	if status != http.StatusOK {
		t.Fatalf("add desktop conversation to group status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, ready.URL+"api/groups/"+groupID+"/conversations/"+conversationID+"/pinned", token, map[string]bool{"pinned": true})
	if status != http.StatusOK {
		t.Fatalf("pin desktop grouped conversation status = %d, body = %#v", status, body)
	}
	status, groupedConversations := desktopJSONArrayRequest(t, ready.URL+"api/groups/"+groupID+"/conversations", token)
	if status != http.StatusOK || !desktopJSONArrayContains(groupedConversations, "id", conversationID) || !desktopJSONArrayContains(groupedConversations, "groupPinned", true) {
		t.Fatalf("desktop grouped conversations status = %d, body = %#v", status, groupedConversations)
	}

	attachmentContent := []byte("desktop attachment content\n")
	status, uploadBody := desktopMultipartUploadRequest(t, ready.URL+"api/chat-uploads", token, "desktop-note.txt", attachmentContent, map[string]string{
		"conversationId": conversationID,
	})
	if status != http.StatusOK || uploadBody["ok"] != true {
		t.Fatalf("desktop attachment upload status = %d, body = %#v", status, uploadBody)
	}
	attachmentRelativePath, _ := uploadBody["relativePath"].(string)
	attachmentAbsolutePath, _ := uploadBody["absolutePath"].(string)
	if attachmentRelativePath == "" || !filepath.IsAbs(attachmentAbsolutePath) {
		t.Fatalf("desktop attachment paths = relative %q, absolute %q", attachmentRelativePath, attachmentAbsolutePath)
	}
	managedUploadsRoot := filepath.Join(options.Roots.DataDir, "chat_uploads")
	managedRelativePath, err := filepath.Rel(managedUploadsRoot, attachmentAbsolutePath)
	if err != nil || managedRelativePath == ".." || strings.HasPrefix(managedRelativePath, ".."+string(filepath.Separator)) {
		t.Fatalf("desktop attachment escaped managed uploads root %q: %q", managedUploadsRoot, attachmentAbsolutePath)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/chat-uploads/content?path="+url.QueryEscape(attachmentRelativePath), token, nil)
	if status != http.StatusOK || body["content"] != string(attachmentContent) {
		t.Fatalf("read desktop attachment status = %d, body = %#v", status, body)
	}
	status, _, downloadedAttachment := desktopBodyRequest(t, http.MethodGet, ready.URL+"api/chat-uploads/download?path="+url.QueryEscape(attachmentRelativePath), token, nil, "")
	if status != http.StatusOK || !bytes.Equal(downloadedAttachment, attachmentContent) {
		t.Fatalf("download desktop attachment status = %d, body = %q", status, downloadedAttachment)
	}
	attachmentEvents := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
		"conversationId": conversationID,
		"message":        "desktop-attachment-message",
		"attachments": []map[string]string{{
			"fileName":   "desktop-note.txt",
			"mimeType":   "text/plain",
			"serverPath": attachmentAbsolutePath,
		}},
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if !desktopSSEHasEvent(attachmentEvents, "response") || !desktopSSEHasEvent(attachmentEvents, "done") {
		t.Fatalf("desktop attachment Agent events = %#v", attachmentEvents)
	}
	outsideAttachmentPath := filepath.Join(options.Roots.TempDir, "outside-attachment.txt")
	if err := os.WriteFile(outsideAttachmentPath, []byte("outside-attachment-secret"), 0600); err != nil {
		t.Fatalf("write out-of-root desktop attachment: %v", err)
	}
	outsideAttachmentEvents := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
		"conversationId": conversationID,
		"message":        "desktop-outside-attachment",
		"attachments": []map[string]string{{
			"fileName":   "outside-attachment.txt",
			"mimeType":   "text/plain",
			"serverPath": outsideAttachmentPath,
		}},
	})
	outsideAttachmentData, err := json.Marshal(outsideAttachmentEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !desktopSSEHasEvent(outsideAttachmentEvents, "error") || !desktopSSEHasEvent(outsideAttachmentEvents, "done") || desktopSSEHasEvent(outsideAttachmentEvents, "response") || bytes.Contains(outsideAttachmentData, []byte("outside-attachment-secret")) || bytes.Contains(outsideAttachmentData, []byte(outsideAttachmentPath)) {
		t.Fatalf("out-of-root desktop attachment events = %#v", outsideAttachmentEvents)
	}
	managementFixture := desktopCreateManagementFixture(t, ready.URL, token, conversationID)
	extensionFixture := desktopCreateExtensionFixture(t, ready.URL, token, filepath.Join(options.Roots.DataDir, "resources"))
	liveBody, err := json.Marshal(map[string]interface{}{
		"message": "desktop-live-sse",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	liveRequest, err := http.NewRequest(http.MethodPost, ready.URL+"api/eino-agent/stream", bytes.NewReader(liveBody))
	if err != nil {
		t.Fatal(err)
	}
	liveRequest.Header.Set("Authorization", "Bearer "+token)
	liveRequest.Header.Set("Content-Type", "application/json")
	type httpResult struct {
		response *http.Response
		err      error
	}
	liveHTTPResult := make(chan httpResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(liveRequest)
		liveHTTPResult <- httpResult{response: response, err: requestErr}
	}()
	select {
	case <-liveResponseStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("desktop live SSE upstream did not start")
	}
	var liveResponse *http.Response
	select {
	case result := <-liveHTTPResult:
		if result.err != nil {
			t.Fatalf("open desktop live SSE: %v", result.err)
		}
		liveResponse = result.response
	case <-time.After(2 * time.Second):
		t.Fatal("desktop live SSE response was buffered before model completion")
	}
	if liveResponse.StatusCode != http.StatusOK || !strings.HasPrefix(liveResponse.Header.Get("Content-Type"), "text/event-stream") {
		_ = liveResponse.Body.Close()
		t.Fatalf("desktop live SSE status = %d, content type = %q", liveResponse.StatusCode, liveResponse.Header.Get("Content-Type"))
	}
	firstLiveEvent := make(chan map[string]interface{}, 1)
	liveStreamDone := make(chan sseResult, 1)
	go func() {
		defer liveResponse.Body.Close()
		events := make([]map[string]interface{}, 0, 8)
		scanner := bufio.NewScanner(liveResponse.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]interface{}
			if decodeErr := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); decodeErr != nil {
				liveStreamDone <- sseResult{err: decodeErr}
				return
			}
			events = append(events, event)
			select {
			case firstLiveEvent <- event:
			default:
			}
		}
		liveStreamDone <- sseResult{events: events, err: scanner.Err()}
	}()
	select {
	case event := <-firstLiveEvent:
		if event["type"] == "done" {
			t.Fatalf("desktop live SSE completed before release: %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desktop live SSE emitted no event before model completion")
	}
	releaseLive()
	var liveResult sseResult
	select {
	case liveResult = <-liveStreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("desktop live SSE did not complete after release")
	}
	if liveResult.err != nil || !desktopSSEHasEvent(liveResult.events, "response") || !desktopSSEHasEvent(liveResult.events, "done") {
		logData, _ := os.ReadFile(filepath.Join(options.Roots.LogDir, "cyberstrike-ai.log"))
		t.Fatalf("desktop live SSE result = %#v, error = %v, log = %s", liveResult.events, liveResult.err, logData)
	}
	for _, mode := range []string{"deep", "plan_execute", "supervisor"} {
		events := desktopSSERequest(t, ready.URL+"api/multi-agent/stream", token, map[string]interface{}{
			"message":       "desktop-" + mode + "-mode",
			"orchestration": mode,
			"finalization": map[string]interface{}{
				"requireExecutionEvidence": false,
			},
		})
		if !desktopSSEHasEvent(events, "conversation") || !desktopSSEHasEvent(events, "response") || !desktopSSEHasEvent(events, "done") {
			t.Fatalf("desktop %s Agent events = %#v", mode, events)
		}
	}
	toolEvents := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
		"conversationId": conversationID,
		"message":        "desktop-tool-execution",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if !desktopSSEHasEvent(toolEvents, "response") || !desktopSSEHasEvent(toolEvents, "done") {
		t.Fatalf("desktop tool execution events = %#v", toolEvents)
	}
	managementFixture = desktopExerciseAttackChainPromotion(t, ready.URL, token, conversationID, managementFixture)
	status, monitorBody := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/monitor?tool=query_assets", token, nil)
	if status != http.StatusOK {
		t.Fatalf("desktop monitor query_assets status = %d, body = %#v", status, monitorBody)
	}
	executions, _ := monitorBody["executions"].([]interface{})
	if len(executions) != 1 {
		t.Fatalf("desktop monitor query_assets executions = %#v", executions)
	}
	execution, _ := executions[0].(map[string]interface{})
	executionID, _ := execution["id"].(string)
	if executionID == "" || execution["toolName"] != "query_assets" || execution["status"] != "completed" {
		t.Fatalf("desktop monitor query_assets execution = %#v", execution)
	}
	status, executionDetail := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/monitor/execution/"+executionID, token, nil)
	if status != http.StatusOK || executionDetail["id"] != executionID || executionDetail["status"] != "completed" {
		t.Fatalf("desktop monitor execution detail status = %d, body = %#v", status, executionDetail)
	}
	status, notificationSummary := desktopJSONRequest(t, http.MethodGet, ready.URL+"api/notifications/summary?since=0&limit=200", token, nil)
	if status != http.StatusOK {
		t.Fatalf("desktop notification summary status = %d, body = %#v", status, notificationSummary)
	}
	completedNotificationID := desktopNotificationID(notificationSummary, "task_completed")
	if completedNotificationID == "" {
		t.Fatalf("desktop notification summary has no completed task: %#v", notificationSummary)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/notifications/read", token, map[string]interface{}{
		"eventIds": []string{completedNotificationID},
	})
	if status != http.StatusOK {
		t.Fatalf("desktop notification mark-read status = %d, body = %#v", status, body)
	}
	status, notificationSummary = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/notifications/summary?since=0&limit=200", token, nil)
	if status != http.StatusOK || desktopNotificationContainsID(notificationSummary, completedNotificationID) {
		t.Fatalf("desktop read notification remained visible: status = %d, body = %#v", status, notificationSummary)
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
	for _, failure := range []struct {
		message     string
		wantMessage string
	}{
		{message: "desktop-rate-limit", wantMessage: "模型服务限流或额度不足，请稍后重试。"},
		{message: "desktop-server-failure", wantMessage: "模型服务暂时不可用，请稍后重试。"},
		{message: "desktop-stream-interruption", wantMessage: "模型服务连接中断，请检查网络或服务地址。"},
	} {
		failureEvents := desktopSSERequest(t, ready.URL+"api/eino-agent/stream", token, map[string]interface{}{
			"message": failure.message,
			"finalization": map[string]interface{}{
				"requireExecutionEvidence": false,
			},
		})
		failureConversationID := ""
		foundSafeError := false
		foundDone := false
		for _, event := range failureEvents {
			switch event["type"] {
			case "conversation":
				data, _ := event["data"].(map[string]interface{})
				failureConversationID, _ = data["conversationId"].(string)
			case "error":
				foundSafeError = event["message"] == failure.wantMessage
			case "done":
				foundDone = true
			case "response":
				t.Fatalf("%s desktop SSE returned a success response: %#v", failure.message, failureEvents)
			}
		}
		if failureConversationID == "" || !foundSafeError || !foundDone || !desktopSSEHasEvent(failureEvents, "eino_run_retry") {
			t.Fatalf("%s desktop SSE events = %#v", failure.message, failureEvents)
		}
		failureData, err := json.Marshal(failureEvents)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(failureData, []byte("stream-secret")) || bytes.Contains(failureData, []byte("upstream-secret-marker")) {
			t.Fatalf("%s desktop SSE exposed upstream data: %s", failure.message, failureData)
		}
		status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/conversations/"+failureConversationID, token, nil)
		if status != http.StatusOK {
			t.Fatalf("%s persisted conversation status = %d, body = %#v", failure.message, status, body)
		}
		persistedFailure, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(persistedFailure, []byte(failure.wantMessage)) || bytes.Contains(persistedFailure, []byte("stream-secret")) || bytes.Contains(persistedFailure, []byte("upstream-secret-marker")) {
			t.Fatalf("%s persisted conversation was not safely redacted: %s", failure.message, persistedFailure)
		}
		status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks", token, nil)
		if status != http.StatusOK {
			t.Fatalf("%s active task status = %d, body = %#v", failure.message, status, body)
		}
		if taskStatus, found := desktopTaskStatus(body, failureConversationID); found {
			t.Fatalf("%s task remained active with status %q: %#v", failure.message, taskStatus, body)
		}
		status, body = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/agent-loop/tasks/completed", token, nil)
		if status != http.StatusOK {
			t.Fatalf("%s completed task status = %d, body = %#v", failure.message, status, body)
		}
		if taskStatus, found := desktopTaskStatus(body, failureConversationID); !found || taskStatus != "failed" {
			t.Fatalf("%s completed task status = %q, found = %v, body = %#v", failure.message, taskStatus, found, body)
		}
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
	operationsFixture := desktopCreateOperationsFixture(
		t,
		ready.URL,
		token,
		filepath.Join(options.Roots.DataDir, "chat_uploads"),
		externalMCP.URL+"/mcp",
		externalMCPCalls,
		cancelRequestStarted,
	)

	status, body = desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/logout", token, nil)
	if status != http.StatusOK {
		t.Fatalf("local admin logout status = %d, body = %#v", status, body)
	}
	status, _ = desktopJSONRequest(t, http.MethodGet, ready.URL+"api/auth/validate", token, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token validation status = %d", status)
	}
	status, restartLogin := desktopJSONRequest(t, http.MethodPost, ready.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "desktop-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("pre-restart local admin login status = %d, body = %#v", status, restartLogin)
	}
	restartToken, _ := restartLogin["token"].(string)
	if restartToken == "" {
		t.Fatalf("pre-restart local admin login did not return a token: %#v", restartLogin)
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

	restartStdinReader, restartStdinWriter := io.Pipe()
	restartStdoutReader, restartStdoutWriter := io.Pipe()
	restartDone := make(chan error, 1)
	go func() {
		restartDone <- runDesktopCore(context.Background(), restartStdinReader, restartStdoutWriter, options)
		_ = restartStdoutWriter.Close()
		_ = restartStdinReader.Close()
	}()
	t.Cleanup(func() {
		_ = restartStdinWriter.Close()
		_ = restartStdoutReader.Close()
	})

	restartDecoder := json.NewDecoder(restartStdoutReader)
	var restarted desktopprotocol.Handshake
	decodeWithTimeout(t, restartDecoder, &restarted)
	if err := restarted.Validate(); err != nil {
		t.Fatalf("restarted READY validation: %v", err)
	}
	if restarted.Type != desktopprotocol.MessageReady || restarted.AppVersion != options.AppVersion {
		t.Fatalf("unexpected restarted handshake: %#v", restarted)
	}
	status, _ = desktopJSONRequest(t, http.MethodGet, restarted.URL+"api/auth/validate", restartToken, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("pre-restart token validation after core restart status = %d, want 401", status)
	}
	status, restartedLogin := desktopJSONRequest(t, http.MethodPost, restarted.URL+"api/auth/login", "", map[string]string{
		"username": "admin",
		"password": "desktop-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("post-restart local admin login status = %d, body = %#v", status, restartedLogin)
	}
	restartedToken, _ := restartedLogin["token"].(string)
	if restartedToken == "" {
		t.Fatalf("post-restart local admin login did not return a token: %#v", restartedLogin)
	}
	desktopAssertInfoCollect(t, restarted.URL, restartedToken)
	status, body = desktopJSONRequest(t, http.MethodGet, restarted.URL+"api/conversations/"+conversationID, restartedToken, nil)
	if status != http.StatusOK {
		t.Fatalf("persisted conversation after core restart status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, restarted.URL+"api/groups/"+groupID, restartedToken, nil)
	if status != http.StatusOK || body["name"] != "Desktop Persistent Group" || body["pinned"] != true {
		t.Fatalf("persisted desktop group after core restart status = %d, body = %#v", status, body)
	}
	status, groupedConversations = desktopJSONArrayRequest(t, restarted.URL+"api/groups/"+groupID+"/conversations", restartedToken)
	if status != http.StatusOK || !desktopJSONArrayContains(groupedConversations, "id", conversationID) || !desktopJSONArrayContains(groupedConversations, "groupPinned", true) {
		t.Fatalf("persisted desktop group mapping after core restart status = %d, body = %#v", status, groupedConversations)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, restarted.URL+"api/chat-uploads/content?path="+url.QueryEscape(attachmentRelativePath), restartedToken, nil)
	if status != http.StatusOK || body["content"] != string(attachmentContent) {
		t.Fatalf("persisted desktop attachment after core restart status = %d, body = %#v", status, body)
	}
	desktopAssertManagementFixture(t, restarted.URL, restartedToken, conversationID, managementFixture)
	desktopAssertExtensionFixture(t, restarted.URL, restartedToken, extensionFixture)
	desktopAssertOperationsFixture(t, restarted.URL, restartedToken, operationsFixture)
	desktopDeleteOperationsFixture(t, restarted.URL, restartedToken, operationsFixture)
	desktopDeleteExtensionFixture(t, restarted.URL, restartedToken, extensionFixture)
	desktopDeleteManagementFixture(t, restarted.URL, restartedToken, conversationID, managementFixture)
	status, body = desktopJSONRequest(t, http.MethodDelete, restarted.URL+"api/groups/"+groupID+"/conversations/"+conversationID, restartedToken, nil)
	if status != http.StatusOK {
		t.Fatalf("remove persisted desktop group mapping status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodDelete, restarted.URL+"api/groups/"+groupID, restartedToken, nil)
	if status != http.StatusOK {
		t.Fatalf("delete persisted desktop group status = %d, body = %#v", status, body)
	}
	status, groupedConversations = desktopJSONArrayRequest(t, restarted.URL+"api/groups", restartedToken)
	if status != http.StatusOK || desktopJSONArrayContains(groupedConversations, "id", groupID) {
		t.Fatalf("deleted desktop group remained visible: status = %d, body = %#v", status, groupedConversations)
	}
	status, body = desktopJSONRequest(t, http.MethodDelete, restarted.URL+"api/chat-uploads", restartedToken, map[string]string{"path": attachmentRelativePath})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete persisted desktop attachment status = %d, body = %#v", status, body)
	}

	if err := json.NewEncoder(restartStdinWriter).Encode(desktopprotocol.Command{
		Type:            desktopprotocol.CommandShutdown,
		ProtocolVersion: desktopprotocol.Version,
	}); err != nil {
		t.Fatalf("write restarted shutdown command: %v", err)
	}
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restarted runDesktopCore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restarted desktop core did not shut down")
	}
}

func newDesktopFakeAI(t *testing.T, cancelRequestStarted chan<- struct{}, liveResponseStarted chan<- struct{}, releaseLiveResponse <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/embeddings" {
			if request.Header.Get("Authorization") != "Bearer desktop-embedding-secret" {
				t.Errorf("desktop fake embedding authorization = %q", request.Header.Get("Authorization"))
				http.Error(response, "unexpected authorization", http.StatusUnauthorized)
				return
			}
			var payload map[string]interface{}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode desktop fake embedding request: %v", err)
				http.Error(response, "invalid request", http.StatusBadRequest)
				return
			}
			inputCount := 1
			if inputs, ok := payload["input"].([]interface{}); ok && len(inputs) > 0 {
				inputCount = len(inputs)
			}
			data := make([]map[string]interface{}, inputCount)
			for index := range data {
				data[index] = map[string]interface{}{
					"object":    "embedding",
					"embedding": []float64{1, 0, 0},
					"index":     index,
				}
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]interface{}{
				"object": "list",
				"data":   data,
				"model":  "text-embedding-3-small",
				"usage":  map[string]int{"prompt_tokens": inputCount, "total_tokens": inputCount},
			})
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("desktop fake AI path = %q", request.URL.Path)
			http.Error(response, "unexpected path", http.StatusBadRequest)
			return
		}
		authorization := request.Header.Get("Authorization")
		if authorization != "Bearer stream-secret" && authorization != "Bearer desktop-embedding-secret" {
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
		streaming, _ := payload["stream"].(bool)
		if bytes.Contains(requestData, []byte("desktop-auth-failure")) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{"error":{"message":"upstream-secret-marker"}}`)
			return
		}
		if streaming && bytes.Contains(requestData, []byte("desktop-rate-limit")) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(response, `{"error":{"message":"upstream-secret-marker","type":"rate_limit_error"}}`)
			return
		}
		if streaming && bytes.Contains(requestData, []byte("desktop-server-failure")) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(response, `{"error":{"message":"upstream-secret-marker","type":"server_error"}}`)
			return
		}
		if streaming && bytes.Contains(requestData, []byte("desktop-stream-interruption")) {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-interrupted\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(response, "data: {\"id\":\n\n")
			return
		}
		if bytes.Contains(requestData, []byte("desktop-live-sse")) && streaming {
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-live\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			if flusher, ok := response.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case liveResponseStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseLiveResponse:
			case <-request.Context().Done():
				return
			}
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-live\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"desktop live reply\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(response, "data: {\"id\":\"chatcmpl-desktop-live\",\"object\":\"chat.completion.chunk\",\"model\":\"desktop-test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
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
		if desktopPayloadHasTool(payload, "plan") && !desktopPayloadHasTool(payload, "respond") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-plan", "plan", `{"steps":["Return a desktop plan-execute confirmation."]}`)
			return
		}
		if desktopPayloadHasTool(payload, "respond") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-respond", "respond", `{"response":"desktop plan-execute reply"}`)
			return
		}
		if bytes.Contains(requestData, []byte("构建攻击链图")) {
			desktopWriteChatCompletionResponse(response, `{"nodes":[{"id":"node_1","type":"target","label":"desktop.example.test","risk_score":40,"metadata":{"target":"desktop.example.test"}},{"id":"node_2","type":"action","label":"Query desktop assets","risk_score":0,"metadata":{"tool_name":"query_assets","findings":["desktop.example.test"]}}],"edges":[{"source":"node_1","target":"node_2","type":"leads_to","weight":3}]}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-external-mcp-call")) &&
			desktopPayloadHasTool(payload, "desktop-golden-mcp__desktop_echo") &&
			!desktopPayloadHasRole(payload, "tool") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-external-mcp", "desktop-golden-mcp__desktop_echo", `{"text":"golden"}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-external-mcp-call")) &&
			desktopPayloadHasRole(payload, "tool") &&
			!bytes.Contains(requestData, []byte("desktop-mcp:golden")) {
			t.Error("desktop external MCP result was not returned to the model")
		}
		if bytes.Contains(requestData, []byte("desktop-knowledge-retrieval")) &&
			desktopPayloadHasTool(payload, "search_knowledge_base") &&
			!desktopPayloadHasRole(payload, "tool") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-knowledge", "search_knowledge_base", `{"query":"Persisted desktop knowledge content","risk_type":"desktop-golden"}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-knowledge-retrieval")) &&
			desktopPayloadHasRole(payload, "tool") &&
			!bytes.Contains(requestData, []byte("Persisted desktop knowledge content.")) {
			t.Error("desktop knowledge retrieval result was not returned to the model")
		}
		if bytes.Contains(requestData, []byte("desktop-skill-runtime")) &&
			desktopPayloadHasTool(payload, "skill") &&
			!desktopPayloadHasRole(payload, "tool") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-skill", "skill", `{"skill":"desktop-golden-skill"}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-skill-runtime")) &&
			desktopPayloadHasRole(payload, "tool") &&
			!bytes.Contains(requestData, []byte("Desktop Persistent Skill")) {
			t.Error("desktop Skill content was not returned to the model")
		}
		if bytes.Contains(requestData, []byte("desktop-tool-execution")) &&
			desktopPayloadHasTool(payload, "query_assets") &&
			!desktopPayloadHasRole(payload, "tool") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-tool-execution", "query_assets", `{}`)
			return
		}
		if bytes.Contains(requestData, []byte("desktop-hitl-approval")) &&
			!bytes.Contains(requestData, []byte("[HITL Reject]")) &&
			desktopPayloadHasTool(payload, "query_assets") {
			desktopWriteToolCallResponse(response, payload, "call-desktop-hitl", "query_assets", `{}`)
			return
		}
		if streaming {
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

func newDesktopFakeMCP(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	calls := make(chan string, 4)
	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "desktop-golden-mcp",
		Version: "1.0.0",
	}, nil)
	type echoArgs struct {
		Text string `json:"text"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "desktop_echo",
		Description: "Echo text through the desktop external MCP golden path.",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args echoArgs) (*sdkmcp.CallToolResult, any, error) {
		select {
		case calls <- args.Text:
		default:
			t.Error("desktop fake MCP call buffer is full")
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "desktop-mcp:" + args.Text}},
		}, nil, nil
	})
	streamable := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{
		JSONResponse: true,
		Stateless:    true,
	})
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mcp" {
			http.Error(response, "desktop fake MCP unavailable", http.StatusServiceUnavailable)
			return
		}
		streamable.ServeHTTP(response, request)
	})), calls
}

func newDesktopFakeInfoCollect(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/fofa":
			if request.URL.Query().Get("key") != "desktop-fofa-secret" || request.URL.Query().Get("email") != "" {
				t.Error("desktop fake FOFA request used unexpected credential fields")
				http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			queryData, err := base64.StdEncoding.DecodeString(request.URL.Query().Get("qbase64"))
			if err != nil {
				t.Errorf("decode desktop fake FOFA query: %v", err)
				http.Error(response, `{"error":"invalid query"}`, http.StatusBadRequest)
				return
			}
			switch string(queryData) {
			case "desktop-info-failure":
				response.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(response, `{"error":"desktop-fofa-secret upstream detail"}`)
				return
			case "desktop-info-malformed":
				_, _ = io.WriteString(response, `{`)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]interface{}{
				"error":   false,
				"size":    1,
				"page":    1,
				"total":   1,
				"results": [][]interface{}{{"https://desktop.example.test", "192.0.2.10"}},
			})
		case "/zoomeye":
			if request.Header.Get("API-KEY") != "desktop-zoomeye-secret" {
				t.Errorf("desktop fake ZoomEye API key = %q", request.Header.Get("API-KEY"))
				http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]interface{}{
				"code":     60000,
				"query":    `domain="desktop.example.test"`,
				"total":    1,
				"page":     1,
				"pagesize": 1,
				"data": []map[string]interface{}{{
					"ip":   "192.0.2.11",
					"port": 443,
				}},
			})
		case "/quake":
			if request.Header.Get("X-QuakeToken") != "desktop-quake-secret" {
				t.Errorf("desktop fake Quake API key = %q", request.Header.Get("X-QuakeToken"))
				http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]interface{}{
				"code":        0,
				"total_count": 1,
				"data": []map[string]interface{}{{
					"ip":   "192.0.2.12",
					"port": 8443,
				}},
			})
		case "/shodan/host/search":
			if request.URL.Query().Get("key") != "desktop-shodan-secret" {
				t.Errorf("desktop fake Shodan key = %q", request.URL.Query().Get("key"))
				http.Error(response, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]interface{}{
				"total": 1,
				"matches": []map[string]interface{}{{
					"ip_str": "192.0.2.13",
					"port":   9443,
				}},
			})
		default:
			t.Errorf("desktop fake info collection path = %q", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
}

func desktopAssertInfoCollect(t *testing.T, baseURL, token string) {
	t.Helper()
	for _, testCase := range []struct {
		provider string
		query    string
		fields   string
	}{
		{provider: "fofa", query: `domain="desktop.example.test"`, fields: "host,ip"},
		{provider: "zoomeye", query: `domain="desktop.example.test"`, fields: "ip,port"},
		{provider: "quake", query: `domain:"desktop.example.test"`, fields: "ip,port"},
		{provider: "shodan", query: "hostname:desktop.example.test", fields: "ip_str,port"},
	} {
		status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/fofa/search", token, map[string]interface{}{
			"provider": testCase.provider,
			"query":    testCase.query,
			"fields":   testCase.fields,
			"size":     1,
			"page":     1,
			"full":     true,
		})
		results, _ := body["results"].([]interface{})
		if status != http.StatusOK ||
			body["provider"] != testCase.provider ||
			body["results_count"] != float64(1) ||
			len(results) != 1 {
			t.Fatalf("desktop %s info collection status = %d, body = %#v", testCase.provider, status, body)
		}
	}

	for _, testCase := range []struct {
		query       string
		wantMessage string
	}{
		{query: "desktop-info-failure", wantMessage: "429"},
		{query: "desktop-info-malformed", wantMessage: "解析 FOFA 响应失败"},
	} {
		status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/fofa/search", token, map[string]interface{}{
			"provider": "fofa",
			"query":    testCase.query,
			"fields":   "host,ip",
		})
		errorText, _ := body["error"].(string)
		data, _ := json.Marshal(body)
		if status != http.StatusBadGateway ||
			!strings.Contains(errorText, testCase.wantMessage) ||
			bytes.Contains(data, []byte("desktop-fofa-secret")) {
			t.Fatalf("desktop FOFA failure status = %d, body = %#v", status, body)
		}
	}

	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/config", token, nil)
	credentialStatus, _ := body["credential_status"].(map[string]interface{})
	if status != http.StatusOK {
		t.Fatalf("desktop info collection credential status = %d, body = %#v", status, body)
	}
	for _, path := range []string{"fofa.api_key", "zoomeye.api_key", "quake.api_key", "shodan.api_key"} {
		if credentialStatus[path] != true {
			t.Fatalf("desktop info collection credential %s not configured: %#v", path, credentialStatus)
		}
	}
	data, _ := json.Marshal(body)
	for _, secret := range []string{"desktop-fofa-secret", "desktop-zoomeye-secret", "desktop-quake-secret", "desktop-shodan-secret"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("desktop config API exposed info collection secret %q", secret)
		}
	}
}

func desktopWriteToolCallResponse(response http.ResponseWriter, payload map[string]interface{}, callID, name, arguments string) {
	toolCall := map[string]interface{}{
		"id":   callID,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}
	if streaming, _ := payload["stream"].(bool); streaming {
		response.Header().Set("Content-Type", "text/event-stream")
		first, _ := json.Marshal(map[string]interface{}{
			"id":     "chatcmpl-desktop-tool",
			"object": "chat.completion.chunk",
			"model":  "desktop-test-model",
			"choices": []interface{}{map[string]interface{}{
				"index": 0,
				"delta": map[string]interface{}{
					"role": "assistant",
					"tool_calls": []interface{}{map[string]interface{}{
						"index":    0,
						"id":       callID,
						"type":     "function",
						"function": toolCall["function"],
					}},
				},
				"finish_reason": nil,
			}},
		})
		last, _ := json.Marshal(map[string]interface{}{
			"id":     "chatcmpl-desktop-tool",
			"object": "chat.completion.chunk",
			"model":  "desktop-test-model",
			"choices": []interface{}{map[string]interface{}{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			}},
		})
		_, _ = fmt.Fprintf(response, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, last)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]interface{}{
		"id":     "chatcmpl-desktop-tool",
		"object": "chat.completion",
		"model":  "desktop-test-model",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": []interface{}{toolCall},
			},
			"finish_reason": "tool_calls",
		}},
	})
}

func desktopWriteChatCompletionResponse(response http.ResponseWriter, content string) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]interface{}{
		"id":     "chatcmpl-desktop-text",
		"object": "chat.completion",
		"model":  "desktop-test-model",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
	})
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

func desktopSSEHasEvent(events []map[string]interface{}, eventType string) bool {
	for _, event := range events {
		if event["type"] == eventType {
			return true
		}
	}
	return false
}

func desktopNotificationID(summary map[string]interface{}, notificationType string) string {
	items, _ := summary["items"].([]interface{})
	for _, item := range items {
		notification, _ := item.(map[string]interface{})
		if notification["type"] == notificationType {
			id, _ := notification["id"].(string)
			return id
		}
	}
	return ""
}

func desktopNotificationContainsID(summary map[string]interface{}, id string) bool {
	items, _ := summary["items"].([]interface{})
	for _, item := range items {
		notification, _ := item.(map[string]interface{})
		if notification["id"] == id {
			return true
		}
	}
	return false
}

type desktopManagementFixture struct {
	projectID        string
	assetID          string
	vulnerabilityID  string
	factID           string
	relatedFactID    string
	factEdgeID       string
	promotedFactIDs  []string
	promotedFactKeys []string
}

func desktopCreateManagementFixture(t *testing.T, baseURL, token, conversationID string) desktopManagementFixture {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects", token, map[string]string{
		"name":        "Desktop Golden Project",
		"description": "Desktop project persistence check",
		"scope_json":  `{"domains":["desktop.example.test"]}`,
		"status":      "active",
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop project status = %d, body = %#v", status, body)
	}
	fixture := desktopManagementFixture{}
	fixture.projectID, _ = body["id"].(string)
	if fixture.projectID == "" {
		t.Fatalf("create desktop project did not return an id: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/projects/"+fixture.projectID, token, map[string]interface{}{
		"name":   "Desktop Persistent Project",
		"pinned": true,
	})
	if status != http.StatusOK || body["name"] != "Desktop Persistent Project" || body["pinned"] != true {
		t.Fatalf("update desktop project status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/conversations/"+conversationID+"/project", token, map[string]string{"projectId": fixture.projectID})
	if status != http.StatusOK || body["projectId"] != fixture.projectID {
		t.Fatalf("bind desktop conversation project status = %d, body = %#v", status, body)
	}

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/assets/import", token, map[string]interface{}{
		"source": "desktop-golden",
		"assets": []map[string]interface{}{{
			"project_id": fixture.projectID,
			"host":       "desktop.example.test",
			"domain":     "desktop.example.test",
			"port":       443,
			"protocol":   "https",
			"title":      "Desktop managed asset",
			"status":     "active",
			"tags":       []string{"desktop"},
		}},
	})
	if status != http.StatusOK || body["created"] != float64(1) {
		t.Fatalf("import desktop asset status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets?host=desktop.example.test&project_id="+url.QueryEscape(fixture.projectID), token, nil)
	if status != http.StatusOK {
		t.Fatalf("list desktop assets status = %d, body = %#v", status, body)
	}
	fixture.assetID = desktopNestedItemID(body, "assets", "host", "desktop.example.test")
	if fixture.assetID == "" {
		t.Fatalf("imported desktop asset not found: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/assets/bulk", token, map[string]interface{}{
		"asset_ids": []string{fixture.assetID},
		"status":    "inactive",
		"add_tags":  []string{"persistent"},
	})
	if status != http.StatusOK || body["updated"] != float64(1) {
		t.Fatalf("update desktop asset status = %d, body = %#v", status, body)
	}
	desktopExerciseAssetLifecycle(t, baseURL, token, conversationID, fixture.projectID, fixture.assetID)

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/vulnerabilities", token, map[string]string{
		"conversation_id":    conversationID,
		"project_id":         fixture.projectID,
		"conversation_tag":   "desktop-golden-conversation",
		"task_tag":           "desktop-golden-task",
		"title":              "Desktop Golden Vulnerability",
		"description":        "Desktop vulnerability persistence check",
		"severity":           "high",
		"status":             "open",
		"type":               "desktop-test",
		"target":             "desktop.example.test",
		"preconditions":      "Desktop target is reachable.",
		"reproduction_steps": "1. Open the desktop test target.\n2. Verify the golden finding.",
		"evidence":           "desktop-golden-evidence",
		"impact":             "Desktop golden impact",
		"recommendation":     "Apply the desktop golden fix.",
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop vulnerability status = %d, body = %#v", status, body)
	}
	fixture.vulnerabilityID, _ = body["id"].(string)
	if fixture.vulnerabilityID == "" {
		t.Fatalf("create desktop vulnerability did not return an id: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/vulnerabilities/"+fixture.vulnerabilityID, token, map[string]string{
		"status":       "confirmed",
		"retest_notes": "Desktop retest pending",
	})
	if status != http.StatusOK || body["status"] != "confirmed" {
		t.Fatalf("update desktop vulnerability status = %d, body = %#v", status, body)
	}
	desktopExerciseVulnerabilityReporting(t, baseURL, token, conversationID, fixture.projectID, fixture.vulnerabilityID)
	desktopAssertAssetLifecycle(t, baseURL, token, conversationID, fixture.projectID, fixture.assetID)

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/facts", token, map[string]interface{}{
		"fact_key":                 "target.primary_domain",
		"category":                 "target",
		"summary":                  "Desktop primary domain",
		"body":                     "desktop.example.test",
		"confidence":               "confirmed",
		"pinned":                   true,
		"related_vulnerability_id": fixture.vulnerabilityID,
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop project fact status = %d, body = %#v", status, body)
	}
	fixture.factID, _ = body["id"].(string)
	if fixture.factID == "" {
		t.Fatalf("create desktop project fact did not return an id: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/facts", token, map[string]interface{}{
		"fact_key":   "finding.desktop_surface",
		"category":   "finding",
		"summary":    "Desktop exposed surface",
		"body":       "The desktop golden target exposes an HTTPS service.",
		"confidence": "confirmed",
	})
	if status != http.StatusOK {
		t.Fatalf("create related desktop project fact status = %d, body = %#v", status, body)
	}
	fixture.relatedFactID, _ = body["id"].(string)
	if fixture.relatedFactID == "" {
		t.Fatalf("create related desktop project fact did not return an id: %#v", body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/fact-edges", token, map[string]string{
		"source_fact_key": "target.primary_domain",
		"target_fact_key": "finding.desktop_surface",
		"edge_type":       "discovered_on",
		"confidence":      "confirmed",
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop project fact edge status = %d, body = %#v", status, body)
	}
	fixture.factEdgeID, _ = body["id"].(string)
	if fixture.factEdgeID == "" {
		t.Fatalf("create desktop project fact edge did not return an id: %#v", body)
	}
	status, factEdges := desktopJSONArrayRequest(t, baseURL+"api/projects/"+fixture.projectID+"/fact-edges", token)
	if status != http.StatusOK || !desktopJSONArrayContains(factEdges, "id", fixture.factEdgeID) {
		t.Fatalf("list desktop project fact edges status = %d, body = %#v", status, factEdges)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/fact-graph?view=full", token, nil)
	if status != http.StatusOK || desktopNestedItem(body, "nodes", "fact_key", "target.primary_domain") == nil || desktopNestedItem(body, "nodes", "fact_key", "finding.desktop_surface") == nil || desktopNestedItem(body, "edges", "id", fixture.factEdgeID) == nil {
		t.Fatalf("desktop project fact graph status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/facts/deprecate", token, map[string]string{"fact_key": "finding.desktop_surface"})
	if status != http.StatusOK || body["success"] != true {
		t.Fatalf("deprecate desktop project fact status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/fact-graph?view=full", token, nil)
	if status != http.StatusOK || desktopNestedItem(body, "nodes", "fact_key", "finding.desktop_surface") != nil || desktopNestedItem(body, "edges", "id", fixture.factEdgeID) != nil {
		t.Fatalf("deprecated desktop project fact remained in default graph: status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/fact-graph?view=full&exclude_deprecated=0", token, nil)
	deprecatedEdge := desktopNestedItem(body, "edges", "id", fixture.factEdgeID)
	if status != http.StatusOK || desktopNestedItem(body, "nodes", "fact_key", "finding.desktop_surface") == nil || deprecatedEdge == nil || deprecatedEdge["confidence"] != "deprecated" {
		t.Fatalf("deprecated desktop project fact missing from full graph: status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/facts/restore", token, map[string]string{
		"fact_key":   "finding.desktop_surface",
		"confidence": "confirmed",
	})
	if status != http.StatusOK || body["success"] != true {
		t.Fatalf("restore desktop project fact status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/fact-edges", token, map[string]string{
		"source_fact_key": "target.primary_domain",
		"target_fact_key": "finding.desktop_surface",
		"edge_type":       "discovered_on",
		"confidence":      "confirmed",
	})
	if status != http.StatusOK || body["id"] != fixture.factEdgeID || body["confidence"] != "confirmed" {
		t.Fatalf("restore desktop project fact edge status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/stats", token, nil)
	if status != http.StatusOK {
		t.Fatalf("desktop project stats status = %d, body = %#v", status, body)
	}
	return fixture
}

func desktopAssertManagementFixture(t *testing.T, baseURL, token, conversationID string, fixture desktopManagementFixture) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID, token, nil)
	if status != http.StatusOK || body["name"] != "Desktop Persistent Project" || body["pinned"] != true {
		t.Fatalf("persisted desktop project status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/conversations/"+conversationID, token, nil)
	if status != http.StatusOK || body["projectId"] != fixture.projectID {
		t.Fatalf("persisted desktop conversation project status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets?q=desktop.example.test", token, nil)
	asset := desktopNestedItem(body, "assets", "id", fixture.assetID)
	if status != http.StatusOK || asset == nil || asset["status"] != "inactive" || asset["project_id"] != fixture.projectID {
		t.Fatalf("persisted desktop asset status = %d, body = %#v", status, body)
	}
	desktopAssertAssetLifecycle(t, baseURL, token, conversationID, fixture.projectID, fixture.assetID)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/"+fixture.vulnerabilityID, token, nil)
	if status != http.StatusOK || body["status"] != "confirmed" || body["project_id"] != fixture.projectID {
		t.Fatalf("persisted desktop vulnerability status = %d, body = %#v", status, body)
	}
	desktopAssertVulnerabilityReporting(t, baseURL, token, fixture.projectID, fixture.vulnerabilityID)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/facts?fact_key=target.primary_domain", token, nil)
	if status != http.StatusOK || body["id"] != fixture.factID || body["related_vulnerability_id"] != fixture.vulnerabilityID {
		t.Fatalf("persisted desktop project fact status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/facts?fact_key=finding.desktop_surface&include_links=1", token, nil)
	if status != http.StatusOK || body["id"] != fixture.relatedFactID || body["confidence"] != "confirmed" || desktopNestedItem(body, "incoming_links", "id", fixture.factEdgeID) == nil {
		t.Fatalf("persisted related desktop project fact status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/attack-chain/"+conversationID, token, nil)
	if status != http.StatusOK || len(desktopNestedItems(body, "nodes")) != 2 || len(desktopNestedItems(body, "edges")) != 1 {
		t.Fatalf("persisted desktop attack chain status = %d, body = %#v", status, body)
	}
	for index, factKey := range fixture.promotedFactKeys {
		status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/facts?fact_key="+url.QueryEscape(factKey), token, nil)
		if status != http.StatusOK || body["id"] != fixture.promotedFactIDs[index] || body["source_conversation_id"] != conversationID {
			t.Fatalf("persisted promoted desktop fact %q status = %d, body = %#v", factKey, status, body)
		}
	}
}

func desktopDeleteManagementFixture(t *testing.T, baseURL, token, conversationID string, fixture desktopManagementFixture) {
	t.Helper()
	type managementRequest struct {
		method string
		target string
		body   interface{}
	}
	requests := []managementRequest{
		{method: http.MethodDelete, target: baseURL + "api/projects/" + fixture.projectID + "/fact-edges/" + fixture.factEdgeID},
	}
	for _, promotedFactID := range fixture.promotedFactIDs {
		requests = append(requests, managementRequest{method: http.MethodDelete, target: baseURL + "api/projects/" + fixture.projectID + "/facts/" + promotedFactID})
	}
	requests = append(requests,
		managementRequest{method: http.MethodDelete, target: baseURL + "api/projects/" + fixture.projectID + "/facts/" + fixture.relatedFactID},
		managementRequest{method: http.MethodDelete, target: baseURL + "api/projects/" + fixture.projectID + "/facts/" + fixture.factID},
		managementRequest{method: http.MethodDelete, target: baseURL + "api/vulnerabilities/" + fixture.vulnerabilityID},
		managementRequest{method: http.MethodDelete, target: baseURL + "api/assets/" + fixture.assetID},
		managementRequest{method: http.MethodPut, target: baseURL + "api/conversations/" + conversationID + "/project", body: map[string]string{"projectId": ""}},
		managementRequest{method: http.MethodDelete, target: baseURL + "api/projects/" + fixture.projectID},
	)
	for _, request := range requests {
		status, body := desktopJSONRequest(t, request.method, request.target, token, request.body)
		if status != http.StatusOK {
			t.Fatalf("delete desktop management fixture %s %s status = %d, body = %#v", request.method, request.target, status, body)
		}
	}
	status, _ := desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID, token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("deleted desktop project status = %d, want 404", status)
	}
}

func desktopExerciseAttackChainPromotion(t *testing.T, baseURL, token, conversationID string, fixture desktopManagementFixture) desktopManagementFixture {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/attack-chain/"+conversationID+"/regenerate", token, nil)
	if status != http.StatusOK || len(desktopNestedItems(body, "nodes")) != 2 || len(desktopNestedItems(body, "edges")) != 1 {
		t.Fatalf("regenerate desktop attack chain status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/projects/"+fixture.projectID+"/promote-attack-chain/"+conversationID, token, nil)
	if status != http.StatusOK || body["facts_created"] != float64(2) || body["edges_created"] != float64(1) {
		t.Fatalf("promote desktop attack chain status = %d, body = %#v", status, body)
	}
	keys, _ := body["fact_keys"].([]interface{})
	if len(keys) != 2 {
		t.Fatalf("promote desktop attack chain fact keys = %#v", body)
	}
	for _, value := range keys {
		factKey, _ := value.(string)
		if factKey == "" {
			t.Fatalf("promote desktop attack chain returned invalid fact key: %#v", body)
		}
		status, fact := desktopJSONRequest(t, http.MethodGet, baseURL+"api/projects/"+fixture.projectID+"/facts?fact_key="+url.QueryEscape(factKey), token, nil)
		factID, _ := fact["id"].(string)
		if status != http.StatusOK || factID == "" || fact["source_conversation_id"] != conversationID {
			t.Fatalf("promoted desktop attack chain fact %q status = %d, body = %#v", factKey, status, fact)
		}
		fixture.promotedFactKeys = append(fixture.promotedFactKeys, factKey)
		fixture.promotedFactIDs = append(fixture.promotedFactIDs, factID)
	}
	graph, _ := body["graph"].(map[string]interface{})
	if desktopNestedItem(graph, "nodes", "fact_key", fixture.promotedFactKeys[0]) == nil || desktopNestedItem(graph, "nodes", "fact_key", fixture.promotedFactKeys[1]) == nil || len(desktopNestedItems(graph, "edges")) < 2 {
		t.Fatalf("promoted desktop attack chain graph = %#v", body)
	}
	return fixture
}

func desktopExerciseAssetLifecycle(t *testing.T, baseURL, token, conversationID, projectID, assetID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/assets/import", token, map[string]interface{}{
		"source": "desktop-duplicate",
		"assets": []map[string]interface{}{{
			"host":               "desktop.example.test",
			"domain":             "desktop.example.test",
			"port":               8443,
			"protocol":           "https",
			"title":              "Desktop duplicate asset",
			"responsible_person": "Desktop Owner",
			"status":             "active",
			"tags":               []string{"duplicate"},
		}},
	})
	if status != http.StatusOK || body["created"] != float64(1) {
		t.Fatalf("create desktop duplicate asset status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets?domain=desktop.example.test&port=8443", token, nil)
	duplicateID := desktopNestedItemID(body, "assets", "port", float64(8443))
	if status != http.StatusOK || duplicateID == "" {
		t.Fatalf("find desktop duplicate asset status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/assets/merge", token, map[string]interface{}{
		"asset_ids":  []string{assetID, duplicateID},
		"primary_id": assetID,
	})
	mergedAsset, _ := body["asset"].(map[string]interface{})
	if status != http.StatusOK || body["merged"] != float64(1) || mergedAsset["id"] != assetID || mergedAsset["responsible_person"] != "Desktop Owner" || !desktopJSONArrayValueContains(mergedAsset["tags"], "duplicate") {
		t.Fatalf("merge desktop duplicate asset status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets?domain=desktop.example.test&port=8443", token, nil)
	if status != http.StatusOK || body["total"] != float64(0) {
		t.Fatalf("merged desktop duplicate remained visible: status = %d, body = %#v", status, body)
	}

	for _, targetProjectID := range []string{"", projectID} {
		status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/assets/project-binding", token, map[string]interface{}{
			"asset_ids":  []string{assetID},
			"project_id": targetProjectID,
		})
		if status != http.StatusOK || body["updated"] != float64(1) || body["project_id"] != targetProjectID {
			t.Fatalf("bind desktop asset project %q status = %d, body = %#v", targetProjectID, status, body)
		}
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/assets/scan-links", token, map[string]interface{}{
		"scans": []map[string]string{{
			"asset_id":        assetID,
			"conversation_id": conversationID,
		}},
	})
	if status != http.StatusOK || body["updated"] != float64(1) {
		t.Fatalf("record desktop asset scan status = %d, body = %#v", status, body)
	}
}

func desktopAssertAssetLifecycle(t *testing.T, baseURL, token, conversationID, projectID, assetID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets?host=desktop.example.test&port=443&project_id="+url.QueryEscape(projectID)+"&scan_state=scanned", token, nil)
	asset := desktopNestedItem(body, "assets", "id", assetID)
	if status != http.StatusOK || body["total"] != float64(1) || asset == nil ||
		asset["last_scan_conversation_id"] != conversationID || asset["last_scan_at"] == nil ||
		asset["vulnerability_count"] != float64(1) || asset["risk_level"] != "high" ||
		asset["responsible_person"] != "Desktop Owner" ||
		!desktopJSONArrayValueContains(asset["tags"], "persistent") ||
		!desktopJSONArrayValueContains(asset["tags"], "duplicate") {
		t.Fatalf("desktop asset lifecycle status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets/selection?risk_level=high&project_id="+url.QueryEscape(projectID), token, nil)
	if status != http.StatusOK || body["total"] != float64(1) || desktopNestedItem(body, "assets", "id", assetID) == nil {
		t.Fatalf("desktop high-risk asset selection status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/assets/stats?days=30", token, nil)
	coverage, _ := body["coverage"].(map[string]interface{})
	if status != http.StatusOK || body["total"] != float64(1) || coverage["scanned"] != float64(1) || coverage["never_scanned"] != float64(0) {
		t.Fatalf("desktop asset scan stats status = %d, body = %#v", status, body)
	}
}

func desktopExerciseVulnerabilityReporting(t *testing.T, baseURL, token, conversationID, projectID, vulnerabilityID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/vulnerabilities", token, map[string]string{
		"conversation_id":  conversationID,
		"project_id":       projectID,
		"conversation_tag": "desktop-golden-conversation",
		"task_tag":         "desktop-batch-delete",
		"title":            "Desktop Batch Vulnerability",
		"description":      "Desktop batch deletion check",
		"severity":         "critical",
		"status":           "open",
		"type":             "desktop-batch-test",
		"target":           "batch.desktop.example.test",
	})
	batchID, _ := body["id"].(string)
	if status != http.StatusOK || batchID == "" {
		t.Fatalf("create desktop batch vulnerability status = %d, body = %#v", status, body)
	}

	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/stats?project_id="+url.QueryEscape(projectID), token, nil)
	bySeverity, _ := body["by_severity"].(map[string]interface{})
	byStatus, _ := body["by_status"].(map[string]interface{})
	if status != http.StatusOK || body["total"] != float64(2) || bySeverity["high"] != float64(1) || bySeverity["critical"] != float64(1) || byStatus["confirmed"] != float64(1) || byStatus["open"] != float64(1) {
		t.Fatalf("desktop vulnerability aggregate stats status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/filter-options", token, nil)
	if status != http.StatusOK ||
		!desktopJSONArrayValueContains(body["vulnerability_ids"], vulnerabilityID) ||
		!desktopJSONArrayValueContains(body["vulnerability_ids"], batchID) ||
		!desktopJSONArrayValueContains(body["conversation_tags"], "desktop-golden-conversation") ||
		!desktopJSONArrayValueContains(body["task_tags"], "desktop-batch-delete") {
		t.Fatalf("desktop vulnerability filter options status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/export?project_id="+url.QueryEscape(projectID)+"&group_by=task&mode=split", token, nil)
	files := desktopNestedItems(body, "files")
	exportData, _ := json.Marshal(files)
	if status != http.StatusOK || body["total"] != float64(2) || len(files) != 2 || !bytes.Contains(exportData, []byte("Desktop Batch Vulnerability")) {
		t.Fatalf("desktop vulnerability split export status = %d, body = %#v", status, body)
	}

	status, body = desktopJSONRequest(t, http.MethodDelete, baseURL+"api/vulnerabilities/batch?task_tag=desktop-batch-delete", token, nil)
	if status != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("desktop vulnerability batch delete status = %d, body = %#v", status, body)
	}
	status, _ = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/"+batchID, token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("deleted desktop batch vulnerability status = %d, want 404", status)
	}
	desktopAssertVulnerabilityReporting(t, baseURL, token, projectID, vulnerabilityID)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs?category=vulnerability&action=delete_batch", token, nil)
	auditTotal, _ := body["total"].(float64)
	if status != http.StatusOK || auditTotal < 1 {
		t.Fatalf("desktop vulnerability batch audit status = %d, body = %#v", status, body)
	}
}

func desktopAssertVulnerabilityReporting(t *testing.T, baseURL, token, projectID, vulnerabilityID string) {
	t.Helper()
	query := "project_id=" + url.QueryEscape(projectID) + "&id=" + url.QueryEscape(vulnerabilityID)
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/stats?"+query, token, nil)
	bySeverity, _ := body["by_severity"].(map[string]interface{})
	byStatus, _ := body["by_status"].(map[string]interface{})
	if status != http.StatusOK || body["total"] != float64(1) || bySeverity["high"] != float64(1) || byStatus["confirmed"] != float64(1) {
		t.Fatalf("desktop vulnerability filtered stats status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities?"+query, token, nil)
	vulnerability := desktopNestedItem(body, "vulnerabilities", "id", vulnerabilityID)
	if status != http.StatusOK || body["total"] != float64(1) || vulnerability == nil || vulnerability["task_tag"] != "desktop-golden-task" {
		t.Fatalf("desktop vulnerability filtered list status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/vulnerabilities/export?"+query+"&group_by=conversation&mode=summary", token, nil)
	files := desktopNestedItems(body, "files")
	if status != http.StatusOK || body["total"] != float64(1) || len(files) != 1 {
		t.Fatalf("desktop vulnerability summary export status = %d, body = %#v", status, body)
	}
	file, _ := files[0].(map[string]interface{})
	content, _ := file["content"].(string)
	if !strings.Contains(content, "Desktop Golden Vulnerability") ||
		!strings.Contains(content, vulnerabilityID) ||
		!strings.Contains(content, "desktop-golden-evidence") ||
		!strings.Contains(content, "Desktop retest pending") ||
		strings.Contains(content, "stream-secret") ||
		strings.Contains(content, "desktop-embedding-secret") {
		t.Fatalf("desktop vulnerability summary export file = %#v", file)
	}
}

type desktopExtensionFixture struct {
	roleName           string
	rolePath           string
	skillName          string
	skillPath          string
	agentFilename      string
	agentPath          string
	workflowID         string
	importedWorkflowID string
	workflowImportID   string
	workflowRunID      string
	knowledgeItemID    string
	knowledgeItemPath  string
	knowledgeLogID     string
}

func desktopCreateExtensionFixture(t *testing.T, baseURL, token, managedResourcesRoot string) desktopExtensionFixture {
	t.Helper()
	fixture := desktopExtensionFixture{
		roleName:      "desktop-golden-role",
		skillName:     "desktop-golden-skill",
		agentFilename: "desktop-golden-agent.md",
		workflowID:    "desktop-golden-workflow",
	}
	workflowGraph := map[string]interface{}{
		"nodes": []map[string]interface{}{
			{"id": "start-1", "type": "start", "label": "Start", "position": map[string]int{"x": 0, "y": 0}, "config": map[string]interface{}{}},
			{"id": "output-1", "type": "output", "label": "Output", "position": map[string]int{"x": 0, "y": 120}, "config": map[string]interface{}{
				"output_key":     "result",
				"source_binding": map[string]string{"from": "inputs", "field": "message"},
			}},
		},
		"edges":  []map[string]string{{"id": "edge-1", "source": "start-1", "target": "output-1"}},
		"config": map[string]int{"schema_version": 1},
	}
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/workflows/validate", token, map[string]interface{}{"graph": workflowGraph})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("validate desktop workflow status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/workflows/dry-run", token, map[string]interface{}{
		"graph":  workflowGraph,
		"inputs": map[string]string{"message": "desktop workflow input"},
	})
	if status != http.StatusOK || body["result"] == nil {
		t.Fatalf("dry-run desktop workflow status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/workflows", token, map[string]interface{}{
		"id":          fixture.workflowID,
		"name":        "Desktop Golden Workflow",
		"description": "Desktop workflow persistence check",
		"version":     1,
		"enabled":     true,
		"graph":       workflowGraph,
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop workflow status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/workflows/"+fixture.workflowID, token, map[string]interface{}{
		"name":        "Desktop Persistent Workflow",
		"description": "Desktop workflow updated before restart",
		"version":     2,
		"enabled":     true,
		"graph":       workflowGraph,
	})
	workflow, _ := body["workflow"].(map[string]interface{})
	if status != http.StatusOK || workflow["name"] != "Desktop Persistent Workflow" || workflow["version"] != float64(2) {
		t.Fatalf("update desktop workflow status = %d, body = %#v", status, body)
	}
	fixture.importedWorkflowID, fixture.workflowImportID = desktopExerciseWorkflowPackage(
		t,
		baseURL,
		token,
		fixture.workflowID,
		workflowGraph,
	)

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/roles", token, map[string]interface{}{
		"name":             fixture.roleName,
		"description":      "Desktop role persistence check",
		"user_prompt":      "Use the desktop golden workflow.",
		"tools":            []string{"query_assets"},
		"workflow_id":      fixture.workflowID,
		"workflow_version": "latest",
		"workflow_policy":  "auto",
		"enabled":          true,
	})
	if status != http.StatusOK {
		t.Fatalf("create desktop role status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/roles/"+url.PathEscape(fixture.roleName), token, map[string]interface{}{
		"name":             fixture.roleName,
		"description":      "Desktop Persistent Role",
		"user_prompt":      "Use the persisted desktop golden workflow.",
		"tools":            []string{"query_assets"},
		"workflow_id":      fixture.workflowID,
		"workflow_version": "latest",
		"workflow_policy":  "auto",
		"enabled":          true,
	})
	role, _ := body["role"].(map[string]interface{})
	if status != http.StatusOK || role["description"] != "Desktop Persistent Role" {
		t.Fatalf("update desktop role status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/eino-agent", token, map[string]interface{}{
		"message": "desktop workflow execution",
		"role":    fixture.roleName,
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	fixture.workflowRunID, _ = body["workflowRunId"].(string)
	if status != http.StatusOK ||
		fixture.workflowRunID == "" ||
		body["agentMode"] != "workflow" ||
		body["workflowStatus"] != "completed" ||
		body["awaitingHitl"] != false {
		t.Fatalf("execute desktop role workflow status = %d, body = %#v", status, body)
	}
	desktopAssertWorkflowRun(t, baseURL, token, fixture.workflowRunID, fixture.workflowID)
	fixture.rolePath = filepath.Join(managedResourcesRoot, "roles", fixture.roleName+".yaml")
	desktopAssertManagedPath(t, managedResourcesRoot, fixture.rolePath)
	if _, err := os.Stat(fixture.rolePath); err != nil {
		t.Fatalf("stat managed desktop role: %v", err)
	}

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/skills", token, map[string]string{
		"name":        fixture.skillName,
		"description": "Desktop skill persistence check",
		"content":     "# Desktop Golden Skill\n\nFollow the initial desktop procedure.",
	})
	skill, _ := body["skill"].(map[string]interface{})
	fixture.skillPath, _ = skill["path"].(string)
	if status != http.StatusOK || fixture.skillPath == "" {
		t.Fatalf("create desktop skill status = %d, body = %#v", status, body)
	}
	desktopAssertManagedPath(t, managedResourcesRoot, fixture.skillPath)
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/skills/"+fixture.skillName, token, map[string]string{
		"description": "Desktop Persistent Skill",
		"content":     "# Desktop Persistent Skill\n\nFollow the persisted desktop procedure.",
	})
	if status != http.StatusOK {
		t.Fatalf("update desktop skill status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/skills/"+fixture.skillName+"/file", token, map[string]string{
		"path":    "references/desktop.md",
		"content": "desktop skill reference",
	})
	if status != http.StatusOK || body["path"] != "references/desktop.md" {
		t.Fatalf("write desktop skill package file status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/"+fixture.skillName+"/files", token, nil)
	if status != http.StatusOK || desktopNestedItem(body, "files", "path", "SKILL.md") == nil || desktopNestedItem(body, "files", "path", "references/desktop.md") == nil {
		t.Fatalf("list desktop skill package files status = %d, body = %#v", status, body)
	}
	skillEvents := desktopSSERequest(t, baseURL+"api/eino-agent/stream", token, map[string]interface{}{
		"message": "desktop-skill-runtime",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if !desktopSSEHasEvent(skillEvents, "response") || !desktopSSEHasEvent(skillEvents, "done") {
		t.Fatalf("desktop Skill runtime events = %#v", skillEvents)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/stats", token, nil)
	skillStats := desktopNestedItem(body, "stats", "skill_name", fixture.skillName)
	if status != http.StatusOK || skillStats == nil || skillStats["total_calls"] != float64(1) || skillStats["success_calls"] != float64(1) || skillStats["failed_calls"] != float64(0) {
		t.Fatalf("desktop Skill runtime stats status = %d, body = %#v", status, body)
	}

	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/multi-agent/markdown-agents", token, nil)
	agentsDir, _ := body["dir"].(string)
	if status != http.StatusOK || agentsDir == "" {
		t.Fatalf("list desktop markdown agents status = %d, body = %#v", status, body)
	}
	desktopAssertManagedPath(t, managedResourcesRoot, agentsDir)
	fixture.agentPath = filepath.Join(agentsDir, fixture.agentFilename)
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/multi-agent/markdown-agents", token, map[string]interface{}{
		"filename":       fixture.agentFilename,
		"id":             "desktop-golden-agent",
		"name":           "Desktop Golden Agent",
		"description":    "Desktop Agent persistence check",
		"tools":          []string{"query_assets"},
		"instruction":    "Verify the initial desktop state.",
		"bind_role":      fixture.roleName,
		"max_iterations": 3,
	})
	if status != http.StatusOK || body["filename"] != fixture.agentFilename {
		t.Fatalf("create desktop markdown agent status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/multi-agent/markdown-agents/"+fixture.agentFilename, token, map[string]interface{}{
		"id":             "desktop-golden-agent",
		"name":           "Desktop Persistent Agent",
		"description":    "Desktop Agent updated before restart",
		"tools":          []string{"query_assets"},
		"instruction":    "Verify the persisted desktop state.",
		"bind_role":      fixture.roleName,
		"max_iterations": 4,
	})
	if status != http.StatusOK {
		t.Fatalf("update desktop markdown agent status = %d, body = %#v", status, body)
	}
	desktopAssertManagedPath(t, managedResourcesRoot, fixture.agentPath)

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/knowledge/items", token, map[string]string{
		"category": "desktop-golden",
		"title":    "desktop-golden-note",
		"content":  "Initial desktop knowledge content.",
	})
	fixture.knowledgeItemID, _ = body["id"].(string)
	if status != http.StatusOK || fixture.knowledgeItemID == "" {
		t.Fatalf("create desktop knowledge item status = %d, body = %#v", status, body)
	}
	desktopWaitForKnowledgeIndex(t, baseURL, token, fixture.knowledgeItemID)
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/knowledge/items/"+fixture.knowledgeItemID, token, map[string]string{
		"category": "desktop-golden",
		"title":    "desktop-persistent-note",
		"content":  "Persisted desktop knowledge content.",
	})
	fixture.knowledgeItemPath, _ = body["filePath"].(string)
	if status != http.StatusOK || body["title"] != "desktop-persistent-note" || fixture.knowledgeItemPath == "" {
		t.Fatalf("update desktop knowledge item status = %d, body = %#v", status, body)
	}
	desktopAssertManagedPath(t, managedResourcesRoot, fixture.knowledgeItemPath)
	desktopWaitForKnowledgeIndex(t, baseURL, token, fixture.knowledgeItemID)
	fixture.knowledgeLogID = desktopExerciseKnowledgeRetrieval(t, baseURL, token, fixture.knowledgeItemID)

	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs?category=workflow&resource_id="+url.QueryEscape(fixture.workflowID), token, nil)
	auditTotal, _ := body["total"].(float64)
	if status != http.StatusOK || auditTotal < 2 || desktopNestedItem(body, "logs", "resourceId", fixture.workflowID) == nil {
		t.Fatalf("desktop workflow audit status = %d, body = %#v", status, body)
	}
	return fixture
}

func desktopExerciseWorkflowPackage(
	t *testing.T,
	baseURL, token, workflowID string,
	workflowGraph map[string]interface{},
) (string, string) {
	t.Helper()
	status, headers, archiveData := desktopBodyRequest(
		t,
		http.MethodGet,
		baseURL+"api/workflows/"+url.PathEscape(workflowID)+"/package",
		token,
		nil,
		"",
	)
	packageHash := headers.Get("X-Workflow-Package-SHA256")
	if status != http.StatusOK ||
		!strings.HasPrefix(headers.Get("Content-Type"), "application/zip") ||
		!strings.HasPrefix(packageHash, "sha256:") ||
		headers.Get("ETag") != `"`+packageHash+`"` {
		t.Fatalf("export desktop workflow package status = %d, headers = %#v", status, headers)
	}
	desktopAssertWorkflowPackageArchive(t, archiveData, workflowID)

	status, body := desktopJSONRequest(t, http.MethodPut, baseURL+"api/workflows/"+url.PathEscape(workflowID), token, map[string]interface{}{
		"name":        "Desktop Persistent Workflow",
		"description": "Desktop workflow changed after package export",
		"version":     3,
		"enabled":     true,
		"graph":       workflowGraph,
	})
	workflow, _ := body["workflow"].(map[string]interface{})
	if status != http.StatusOK || workflow["version"] != float64(3) {
		t.Fatalf("prepare desktop workflow package conflict status = %d, body = %#v", status, body)
	}

	status, body = desktopMultipartUploadRequest(
		t,
		baseURL+"api/workflow-package-inspections",
		token,
		workflowID+".csapkg.zip",
		archiveData,
		nil,
	)
	inspection, _ := body["inspection"].(map[string]interface{})
	inspectionID, _ := inspection["id"].(string)
	conflict, _ := inspection["conflict"].(map[string]interface{})
	if status != http.StatusCreated || inspectionID == "" || conflict["state"] != "id_conflict" {
		t.Fatalf("inspect desktop workflow package status = %d, body = %#v", status, body)
	}

	importedWorkflowID := "desktop-imported-workflow"
	requestBody := map[string]interface{}{
		"inspection_id": inspectionID,
		"resolution": map[string]string{
			"action":          "rename",
			"new_workflow_id": importedWorkflowID,
		},
		"confirm_overwrite": false,
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/workflow-package-imports", token, requestBody)
	errorBody, _ := body["error"].(map[string]interface{})
	if status != http.StatusBadRequest || errorBody["code"] != "WFPKG_IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("desktop workflow package missing idempotency status = %d, body = %#v", status, body)
	}

	idempotencyKey := "00000000-0000-4000-8000-000000000001"
	status, body = desktopJSONRequestWithHeaders(
		t,
		http.MethodPost,
		baseURL+"api/workflow-package-imports",
		token,
		requestBody,
		map[string]string{"Idempotency-Key": idempotencyKey},
	)
	importResult, _ := body["import"].(map[string]interface{})
	importID, _ := importResult["id"].(string)
	if status != http.StatusCreated ||
		importID == "" ||
		importResult["result"] != "renamed" ||
		importResult["target_workflow_id"] != importedWorkflowID {
		t.Fatalf("import desktop workflow package status = %d, body = %#v", status, body)
	}
	status, replayBody := desktopJSONRequestWithHeaders(
		t,
		http.MethodPost,
		baseURL+"api/workflow-package-imports",
		token,
		requestBody,
		map[string]string{"Idempotency-Key": idempotencyKey},
	)
	replayImport, _ := replayBody["import"].(map[string]interface{})
	if status != http.StatusOK || replayImport["id"] != importID {
		t.Fatalf("replay desktop workflow package import status = %d, body = %#v", status, replayBody)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflow-package-imports/"+url.PathEscape(importID), token, nil)
	persistedImport, _ := body["import"].(map[string]interface{})
	if status != http.StatusOK || persistedImport["id"] != importID || persistedImport["result"] != "renamed" {
		t.Fatalf("read desktop workflow package import status = %d, body = %#v", status, body)
	}
	return importedWorkflowID, importID
}

func desktopAssertWorkflowPackageArchive(t *testing.T, data []byte, workflowID string) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read desktop workflow package: %v", err)
	}
	wantEntries := map[string]bool{
		"checksums.sha256":                  false,
		"manifest.json":                     false,
		"workflows/" + workflowID + ".json": false,
	}
	for _, file := range archive.File {
		if _, exists := wantEntries[file.Name]; exists {
			wantEntries[file.Name] = true
		}
	}
	for name, found := range wantEntries {
		if !found {
			t.Fatalf("desktop workflow package missing %q", name)
		}
	}
}

func desktopAssertWorkflowRun(t *testing.T, baseURL, token, runID, workflowID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflows/runs/"+url.PathEscape(runID), token, nil)
	run, _ := body["run"].(map[string]interface{})
	if status != http.StatusOK || run["id"] != runID || run["workflow_id"] != workflowID || run["status"] != "completed" {
		t.Fatalf("desktop workflow run status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflows/runs/"+url.PathEscape(runID)+"/replay", token, nil)
	steps, _ := body["steps"].([]interface{})
	if status != http.StatusOK || body["workflowRunId"] != runID || len(steps) < 2 {
		t.Fatalf("desktop workflow replay status = %d, body = %#v", status, body)
	}
}

func desktopAssertExtensionFixture(t *testing.T, baseURL, token string, fixture desktopExtensionFixture) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflows/"+fixture.workflowID, token, nil)
	workflow, _ := body["workflow"].(map[string]interface{})
	if status != http.StatusOK ||
		workflow["name"] != "Desktop Persistent Workflow" ||
		workflow["description"] != "Desktop workflow changed after package export" ||
		workflow["version"] != float64(3) {
		t.Fatalf("persisted desktop workflow status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflows/"+fixture.importedWorkflowID, token, nil)
	importedWorkflow, _ := body["workflow"].(map[string]interface{})
	if status != http.StatusOK || importedWorkflow["id"] != fixture.importedWorkflowID || importedWorkflow["version"] != float64(1) {
		t.Fatalf("persisted imported desktop workflow status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/workflow-package-imports/"+fixture.workflowImportID, token, nil)
	workflowImport, _ := body["import"].(map[string]interface{})
	if status != http.StatusOK || workflowImport["id"] != fixture.workflowImportID || workflowImport["target_workflow_id"] != fixture.importedWorkflowID {
		t.Fatalf("persisted desktop workflow package import status = %d, body = %#v", status, body)
	}
	desktopAssertWorkflowRun(t, baseURL, token, fixture.workflowRunID, fixture.workflowID)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/roles/"+url.PathEscape(fixture.roleName), token, nil)
	role, _ := body["role"].(map[string]interface{})
	if status != http.StatusOK || role["description"] != "Desktop Persistent Role" || role["workflow_id"] != fixture.workflowID {
		t.Fatalf("persisted desktop role status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/"+fixture.skillName, token, nil)
	skill, _ := body["skill"].(map[string]interface{})
	content, _ := skill["content"].(string)
	if status != http.StatusOK || skill["description"] != "Desktop Persistent Skill" || skill["path"] != fixture.skillPath || !strings.Contains(content, "persisted desktop procedure") {
		t.Fatalf("persisted desktop skill status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/"+fixture.skillName+"/file?path=references%2Fdesktop.md", token, nil)
	if status != http.StatusOK || body["content"] != "desktop skill reference" {
		t.Fatalf("persisted desktop skill package file status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/stats", token, nil)
	skillStats := desktopNestedItem(body, "stats", "skill_name", fixture.skillName)
	if status != http.StatusOK || skillStats == nil || skillStats["total_calls"] != float64(1) || skillStats["success_calls"] != float64(1) {
		t.Fatalf("persisted desktop Skill runtime stats status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/multi-agent/markdown-agents/"+fixture.agentFilename, token, nil)
	if status != http.StatusOK || body["name"] != "Desktop Persistent Agent" || body["bind_role"] != fixture.roleName || body["max_iterations"] != float64(4) {
		t.Fatalf("persisted desktop markdown agent status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/items/"+fixture.knowledgeItemID, token, nil)
	if status != http.StatusOK || body["title"] != "desktop-persistent-note" || body["content"] != "Persisted desktop knowledge content." || body["filePath"] != fixture.knowledgeItemPath {
		t.Fatalf("persisted desktop knowledge item status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/stats", token, nil)
	knowledgeTotal, _ := body["total_items"].(float64)
	if status != http.StatusOK || body["enabled"] != true || knowledgeTotal < 1 {
		t.Fatalf("persisted desktop knowledge stats status = %d, body = %#v", status, body)
	}
	desktopAssertKnowledgeSearch(t, baseURL, token, fixture.knowledgeItemID)
	desktopAssertKnowledgeRetrievalLog(t, baseURL, token, fixture.knowledgeLogID, fixture.knowledgeItemID)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs?category=workflow&resource_id="+url.QueryEscape(fixture.workflowID), token, nil)
	auditTotal, _ := body["total"].(float64)
	if status != http.StatusOK || auditTotal < 2 {
		t.Fatalf("persisted desktop audit status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs?category=workflow_package", token, nil)
	packageAuditTotal, _ := body["total"].(float64)
	if status != http.StatusOK || packageAuditTotal < 3 {
		t.Fatalf("persisted desktop workflow package audit status = %d, body = %#v", status, body)
	}
}

func desktopDeleteExtensionFixture(t *testing.T, baseURL, token string, fixture desktopExtensionFixture) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodDelete, baseURL+"api/knowledge/retrieval-logs/"+fixture.knowledgeLogID, token, nil)
	if status != http.StatusOK {
		t.Fatalf("delete desktop knowledge retrieval log status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodDelete, baseURL+"api/skills/"+fixture.skillName+"/stats", token, nil)
	if status != http.StatusOK {
		t.Fatalf("clear desktop Skill runtime stats status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/skills/stats", token, nil)
	skillStats := desktopNestedItem(body, "stats", "skill_name", fixture.skillName)
	if status != http.StatusOK || skillStats == nil || skillStats["total_calls"] != float64(0) {
		t.Fatalf("cleared desktop Skill runtime stats status = %d, body = %#v", status, body)
	}
	for _, request := range []struct {
		target string
	}{
		{target: baseURL + "api/knowledge/items/" + fixture.knowledgeItemID},
		{target: baseURL + "api/multi-agent/markdown-agents/" + fixture.agentFilename},
		{target: baseURL + "api/skills/" + fixture.skillName},
		{target: baseURL + "api/roles/" + url.PathEscape(fixture.roleName)},
		{target: baseURL + "api/workflows/" + fixture.importedWorkflowID},
		{target: baseURL + "api/workflows/" + fixture.workflowID},
	} {
		status, body := desktopJSONRequest(t, http.MethodDelete, request.target, token, nil)
		if status != http.StatusOK {
			t.Fatalf("delete desktop extension fixture %s status = %d, body = %#v", request.target, status, body)
		}
	}
	for _, request := range []struct {
		target string
	}{
		{target: baseURL + "api/knowledge/items/" + fixture.knowledgeItemID},
		{target: baseURL + "api/multi-agent/markdown-agents/" + fixture.agentFilename},
		{target: baseURL + "api/skills/" + fixture.skillName},
		{target: baseURL + "api/roles/" + url.PathEscape(fixture.roleName)},
		{target: baseURL + "api/workflows/" + fixture.importedWorkflowID},
		{target: baseURL + "api/workflows/" + fixture.workflowID},
	} {
		status, _ := desktopJSONRequest(t, http.MethodGet, request.target, token, nil)
		if status != http.StatusNotFound {
			t.Fatalf("deleted desktop extension resource %s status = %d, want 404", request.target, status)
		}
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/retrieval-logs?limit=100", token, nil)
	if status != http.StatusOK || desktopNestedItem(body, "logs", "id", fixture.knowledgeLogID) != nil {
		t.Fatalf("deleted desktop knowledge retrieval log remained visible: status = %d, body = %#v", status, body)
	}
	for _, path := range []string{fixture.rolePath, fixture.skillPath, fixture.agentPath, fixture.knowledgeItemPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("deleted desktop managed resource remains at %q: %v", path, err)
		}
	}
}

func desktopAssertManagedPath(t *testing.T, root, path string) {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("desktop managed path escaped root %q: %q", root, path)
	}
}

func desktopWaitForKnowledgeIndex(t *testing.T, baseURL, token, itemID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/index-status", token, nil)
		if status == http.StatusOK && body["indexed_items"] == float64(1) && body["is_complete"] == true {
			return
		}
		if body["last_error"] != nil {
			t.Fatalf("index desktop knowledge item %s: %#v", itemID, body)
		}
		time.Sleep(25 * time.Millisecond)
	}
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/index-status", token, nil)
	t.Fatalf("desktop knowledge item %s was not indexed: status = %d, body = %#v", itemID, status, body)
}

func desktopExerciseKnowledgeRetrieval(t *testing.T, baseURL, token, itemID string) string {
	t.Helper()
	desktopAssertKnowledgeSearch(t, baseURL, token, itemID)
	events := desktopSSERequest(t, baseURL+"api/eino-agent/stream", token, map[string]interface{}{
		"message": "desktop-knowledge-retrieval",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	conversationID := ""
	for _, event := range events {
		if event["type"] != "conversation" {
			continue
		}
		data, _ := event["data"].(map[string]interface{})
		conversationID, _ = data["conversationId"].(string)
	}
	if conversationID == "" || !desktopSSEHasEvent(events, "response") || !desktopSSEHasEvent(events, "done") {
		t.Fatalf("desktop knowledge retrieval Agent events = %#v", events)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/retrieval-logs?conversationId="+url.QueryEscape(conversationID)+"&limit=10", token, nil)
		log := desktopNestedItem(body, "logs", "query", "Persisted desktop knowledge content")
		if status == http.StatusOK && log != nil {
			logID, _ := log["id"].(string)
			if logID == "" || log["riskType"] != "desktop-golden" || !desktopJSONArrayValueContains(log["retrievedItems"], itemID) {
				t.Fatalf("desktop knowledge retrieval log = %#v", log)
			}
			return logID
		}
		time.Sleep(25 * time.Millisecond)
	}
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/retrieval-logs?conversationId="+url.QueryEscape(conversationID)+"&limit=10", token, nil)
	t.Fatalf("desktop knowledge retrieval log was not recorded: status = %d, body = %#v", status, body)
	return ""
}

func desktopAssertKnowledgeSearch(t *testing.T, baseURL, token, itemID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/knowledge/search", token, map[string]interface{}{
		"query":     "Persisted desktop knowledge content",
		"riskType":  "desktop-golden",
		"topK":      5,
		"threshold": 0.99,
	})
	for _, raw := range desktopNestedItems(body, "results") {
		result, _ := raw.(map[string]interface{})
		item, _ := result["item"].(map[string]interface{})
		if status == http.StatusOK && item["id"] == itemID && result["score"] == float64(1) {
			return
		}
	}
	t.Fatalf("desktop knowledge semantic search status = %d, body = %#v", status, body)
}

func desktopAssertKnowledgeRetrievalLog(t *testing.T, baseURL, token, logID, itemID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/knowledge/retrieval-logs?limit=100", token, nil)
	log := desktopNestedItem(body, "logs", "id", logID)
	if status != http.StatusOK || log == nil || log["query"] != "Persisted desktop knowledge content" || !desktopJSONArrayValueContains(log["retrievedItems"], itemID) {
		t.Fatalf("persisted desktop knowledge retrieval log status = %d, body = %#v", status, body)
	}
}

func desktopJSONArrayValueContains(value interface{}, want string) bool {
	items, _ := value.([]interface{})
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type desktopOperationsFixture struct {
	queueID          string
	singleQueueID    string
	pausedQueueID    string
	firstTaskID      string
	addedTaskID      string
	externalMCPName  string
	externalMCPURL   string
	externalMCPCalls <-chan string
	fileDirectory    string
	fileRelativePath string
	fileAbsolutePath string
	auditLogID       string
}

func desktopCreateOperationsFixture(
	t *testing.T,
	baseURL, token, managedUploadsRoot, externalMCPURL string,
	externalMCPCalls <-chan string,
	cancelRequestStarted <-chan struct{},
) desktopOperationsFixture {
	t.Helper()
	fixture := desktopOperationsFixture{
		externalMCPName:  "desktop-golden-mcp",
		externalMCPURL:   externalMCPURL,
		externalMCPCalls: externalMCPCalls,
		fileDirectory:    "desktop-golden-files",
	}
	status, body := desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks", token, map[string]interface{}{
		"title":        "Desktop Golden Batch",
		"tasks":        []string{"desktop-tool-execution batch one", "desktop-tool-execution batch two"},
		"agentMode":    "eino_single",
		"scheduleMode": "manual",
		"executeNow":   false,
		"concurrency":  2,
	})
	fixture.queueID, _ = body["queueId"].(string)
	queue, _ := body["queue"].(map[string]interface{})
	tasks := desktopNestedItems(queue, "tasks")
	if status != http.StatusOK || fixture.queueID == "" || len(tasks) != 2 {
		t.Fatalf("create desktop batch queue status = %d, body = %#v", status, body)
	}
	firstTask, _ := tasks[0].(map[string]interface{})
	secondTask, _ := tasks[1].(map[string]interface{})
	fixture.firstTaskID, _ = firstTask["id"].(string)
	secondTaskID, _ := secondTask["id"].(string)
	if fixture.firstTaskID == "" || secondTaskID == "" {
		t.Fatalf("desktop batch task ids = %#v", tasks)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/batch-tasks/"+fixture.queueID+"/tasks/"+fixture.firstTaskID, token, map[string]string{
		"message": "desktop-tool-execution persisted batch one",
	})
	if status != http.StatusOK {
		t.Fatalf("update desktop batch task status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.queueID+"/tasks", token, map[string]string{
		"message": "desktop-tool-execution added batch task",
	})
	addedTask, _ := body["task"].(map[string]interface{})
	fixture.addedTaskID, _ = addedTask["id"].(string)
	if status != http.StatusOK || fixture.addedTaskID == "" {
		t.Fatalf("add desktop batch task status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodDelete, baseURL+"api/batch-tasks/"+fixture.queueID+"/tasks/"+secondTaskID, token, nil)
	if status != http.StatusOK {
		t.Fatalf("delete desktop batch task status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/batch-tasks/"+fixture.queueID+"/metadata", token, map[string]interface{}{
		"title":       "Desktop Persistent Batch",
		"role":        "",
		"agentMode":   "eino_single",
		"concurrency": 2,
	})
	queue, _ = body["queue"].(map[string]interface{})
	if status != http.StatusOK || queue["title"] != "Desktop Persistent Batch" {
		t.Fatalf("update desktop batch metadata status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/batch-tasks/"+fixture.queueID+"/schedule", token, map[string]string{
		"scheduleMode": "cron",
		"cronExpr":     "0 0 * * *",
	})
	if status != http.StatusOK {
		t.Fatalf("update desktop batch schedule status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/batch-tasks/"+fixture.queueID+"/schedule-enabled", token, map[string]bool{"scheduleEnabled": false})
	if status != http.StatusOK {
		t.Fatalf("disable desktop batch schedule status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.queueID+"/start", token, nil)
	if status != http.StatusOK {
		t.Fatalf("start desktop batch queue status = %d, body = %#v", status, body)
	}
	desktopWaitForBatchQueue(t, baseURL, token, fixture.queueID, "completed")

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks", token, map[string]interface{}{
		"title":        "Desktop Single Task Batch",
		"tasks":        []string{"desktop-tool-execution single task", "desktop-tool-execution pending sibling"},
		"agentMode":    "eino_single",
		"scheduleMode": "manual",
		"executeNow":   false,
		"concurrency":  1,
	})
	fixture.singleQueueID, _ = body["queueId"].(string)
	singleQueue, _ := body["queue"].(map[string]interface{})
	singleTasks := desktopNestedItems(singleQueue, "tasks")
	if status != http.StatusOK || fixture.singleQueueID == "" || len(singleTasks) != 2 {
		t.Fatalf("create single-task desktop batch queue status = %d, body = %#v", status, body)
	}
	singleTask, _ := singleTasks[0].(map[string]interface{})
	singleTaskID, _ := singleTask["id"].(string)
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.singleQueueID+"/tasks/"+singleTaskID+"/run", token, nil)
	if status != http.StatusOK || body["autoStarted"] != true {
		t.Fatalf("run single desktop batch task status = %d, body = %#v", status, body)
	}
	desktopWaitForBatchQueue(t, baseURL, token, fixture.singleQueueID, "paused")
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+fixture.singleQueueID, token, nil)
	singleQueue, _ = body["queue"].(map[string]interface{})
	if status != http.StatusOK || desktopNestedItem(singleQueue, "tasks", "status", "completed") == nil || desktopNestedItem(singleQueue, "tasks", "status", "pending") == nil {
		t.Fatalf("single desktop batch task result status = %d, body = %#v", status, body)
	}

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks", token, map[string]interface{}{
		"title":        "Desktop Paused Batch",
		"tasks":        []string{"desktop-cancel-running-agent"},
		"agentMode":    "eino_single",
		"scheduleMode": "manual",
		"executeNow":   false,
		"concurrency":  1,
	})
	fixture.pausedQueueID, _ = body["queueId"].(string)
	if status != http.StatusOK || fixture.pausedQueueID == "" {
		t.Fatalf("create pausable desktop batch queue status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.pausedQueueID+"/start", token, nil)
	if status != http.StatusOK {
		t.Fatalf("start pausable desktop batch queue status = %d, body = %#v", status, body)
	}
	select {
	case <-cancelRequestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("pausable desktop batch task did not reach fake AI")
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.pausedQueueID+"/pause", token, nil)
	if status != http.StatusOK {
		t.Fatalf("pause running desktop batch queue status = %d, body = %#v", status, body)
	}
	desktopWaitForBatchTaskStatus(t, baseURL, token, fixture.pausedQueueID, "cancelled")

	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/external-mcp/"+fixture.externalMCPName, token, map[string]interface{}{
		"config": map[string]interface{}{
			"type":        "http",
			"url":         fixture.externalMCPURL + "/unavailable",
			"description": "Desktop unavailable MCP",
			"timeout":     2,
			"disabled":    true,
		},
	})
	if status != http.StatusOK {
		t.Fatalf("create unavailable desktop external MCP status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/external-mcp/"+fixture.externalMCPName+"/start", token, nil)
	if status != http.StatusOK {
		t.Fatalf("start unavailable desktop external MCP status = %d, body = %#v", status, body)
	}
	desktopWaitForExternalMCP(t, baseURL, token, fixture.externalMCPName, "error", 0, true)
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/external-mcp/"+fixture.externalMCPName+"/stop", token, nil)
	if status != http.StatusOK {
		t.Fatalf("stop unavailable desktop external MCP status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/external-mcp/"+fixture.externalMCPName, token, map[string]interface{}{
		"config": map[string]interface{}{
			"type":        "http",
			"url":         fixture.externalMCPURL,
			"description": "Desktop Persistent MCP",
			"timeout":     2,
			"max_retries": 1,
			"disabled":    true,
		},
	})
	if status != http.StatusOK {
		t.Fatalf("recover desktop external MCP config status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/external-mcp/"+fixture.externalMCPName+"/start", token, nil)
	if status != http.StatusOK {
		t.Fatalf("start recovered desktop external MCP status = %d, body = %#v", status, body)
	}
	desktopWaitForExternalMCP(t, baseURL, token, fixture.externalMCPName, "connected", 1, false)
	desktopAssertExternalMCPTool(t, baseURL, token, fixture.externalMCPName)
	desktopInvokeExternalMCP(t, baseURL, token, fixture.externalMCPName, fixture.externalMCPCalls)

	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/chat-uploads/mkdir", token, map[string]string{
		"parent": "",
		"name":   fixture.fileDirectory,
	})
	if status != http.StatusOK || body["relativePath"] != fixture.fileDirectory {
		t.Fatalf("create desktop file directory status = %d, body = %#v", status, body)
	}
	status, upload := desktopMultipartUploadRequest(t, baseURL+"api/chat-uploads", token, "desktop-file.txt", []byte("initial desktop file"), map[string]string{
		"relativeDir": fixture.fileDirectory,
	})
	uploadedPath, _ := upload["relativePath"].(string)
	if status != http.StatusOK || uploadedPath == "" {
		t.Fatalf("upload desktop managed file status = %d, body = %#v", status, upload)
	}
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/chat-uploads/rename", token, map[string]string{
		"path":    uploadedPath,
		"newName": "desktop-persistent-file.txt",
	})
	fixture.fileRelativePath, _ = body["relativePath"].(string)
	if status != http.StatusOK || fixture.fileRelativePath == "" {
		t.Fatalf("rename desktop managed file status = %d, body = %#v", status, body)
	}
	fixture.fileAbsolutePath = filepath.Join(managedUploadsRoot, filepath.FromSlash(fixture.fileRelativePath))
	desktopAssertManagedPath(t, managedUploadsRoot, fixture.fileAbsolutePath)
	status, body = desktopJSONRequest(t, http.MethodPut, baseURL+"api/chat-uploads/content", token, map[string]string{
		"path":    fixture.fileRelativePath,
		"content": "Persisted desktop managed file.",
	})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("edit desktop managed file status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/chat-uploads?search=desktop-persistent-file", token, nil)
	if status != http.StatusOK || desktopNestedItem(body, "files", "relativePath", fixture.fileRelativePath) == nil {
		t.Fatalf("list desktop managed file status = %d, body = %#v", status, body)
	}
	archiveStatus, archiveHeaders, archiveData := desktopBodyRequest(t, http.MethodGet, baseURL+"api/chat-uploads/export?search=desktop-persistent-file", token, nil, "")
	if archiveStatus != http.StatusOK || !strings.HasPrefix(archiveHeaders.Get("Content-Type"), "application/zip") {
		t.Fatalf("export desktop managed file status = %d, headers = %#v, body = %q", archiveStatus, archiveHeaders, archiveData)
	}
	desktopAssertChatFilesArchive(t, archiveData, "Persisted desktop managed file.")
	fixture.auditLogID = desktopAssertAuditLifecycle(t, baseURL, token, fixture.queueID)
	return fixture
}

func desktopAssertOperationsFixture(t *testing.T, baseURL, token string, fixture desktopOperationsFixture) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+fixture.queueID, token, nil)
	queue, _ := body["queue"].(map[string]interface{})
	if status != http.StatusOK || queue["title"] != "Desktop Persistent Batch" || queue["status"] != "completed" || queue["scheduleMode"] != "cron" || queue["cronExpr"] != "0 0 * * *" || queue["scheduleEnabled"] != false {
		t.Fatalf("persisted desktop batch queue status = %d, body = %#v", status, body)
	}
	firstTask := desktopNestedItem(queue, "tasks", "id", fixture.firstTaskID)
	addedTask := desktopNestedItem(queue, "tasks", "id", fixture.addedTaskID)
	if len(desktopNestedItems(queue, "tasks")) != 2 || firstTask == nil || addedTask == nil || firstTask["status"] != "completed" || addedTask["status"] != "completed" {
		t.Fatalf("persisted desktop batch tasks = %#v", queue["tasks"])
	}
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/batch-tasks/"+fixture.queueID+"/rerun", token, nil)
	if status != http.StatusOK {
		t.Fatalf("rerun persisted desktop batch queue status = %d, body = %#v", status, body)
	}
	desktopWaitForBatchQueue(t, baseURL, token, fixture.queueID, "completed")
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+fixture.singleQueueID, token, nil)
	singleQueue, _ := body["queue"].(map[string]interface{})
	if status != http.StatusOK || singleQueue["status"] != "paused" || desktopNestedItem(singleQueue, "tasks", "status", "completed") == nil || desktopNestedItem(singleQueue, "tasks", "status", "pending") == nil {
		t.Fatalf("persisted single-task desktop batch queue status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+fixture.pausedQueueID, token, nil)
	pausedQueue, _ := body["queue"].(map[string]interface{})
	if status != http.StatusOK || pausedQueue["status"] != "paused" {
		t.Fatalf("persisted paused desktop batch queue status = %d, body = %#v", status, body)
	}
	pausedTasks := desktopNestedItems(pausedQueue, "tasks")
	if len(pausedTasks) != 1 || desktopNestedItem(pausedQueue, "tasks", "status", "cancelled") == nil {
		t.Fatalf("persisted paused desktop batch task = %#v", pausedQueue)
	}

	body = desktopWaitForExternalMCP(t, baseURL, token, fixture.externalMCPName, "connected", 1, false)
	externalConfig, _ := body["config"].(map[string]interface{})
	if externalConfig["description"] != "Desktop Persistent MCP" ||
		externalConfig["type"] != "http" ||
		externalConfig["url"] != fixture.externalMCPURL ||
		externalConfig["external_mcp_enable"] != true {
		t.Fatalf("persisted enabled desktop external MCP body = %#v", body)
	}
	desktopAssertExternalMCPTool(t, baseURL, token, fixture.externalMCPName)
	desktopInvokeExternalMCP(t, baseURL, token, fixture.externalMCPName, fixture.externalMCPCalls)
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/external-mcp/"+fixture.externalMCPName+"/stop", token, nil)
	if status != http.StatusOK {
		t.Fatalf("stop persisted desktop external MCP status = %d, body = %#v", status, body)
	}
	desktopWaitForExternalMCP(t, baseURL, token, fixture.externalMCPName, "disabled", 0, false)
	status, body = desktopJSONRequest(t, http.MethodPost, baseURL+"api/external-mcp/"+fixture.externalMCPName+"/start", token, nil)
	if status != http.StatusOK {
		t.Fatalf("restart persisted desktop external MCP status = %d, body = %#v", status, body)
	}
	desktopWaitForExternalMCP(t, baseURL, token, fixture.externalMCPName, "connected", 1, false)
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/chat-uploads/content?path="+url.QueryEscape(fixture.fileRelativePath), token, nil)
	if status != http.StatusOK || body["content"] != "Persisted desktop managed file." {
		t.Fatalf("persisted desktop managed file status = %d, body = %#v", status, body)
	}
	desktopAssertPersistedAudit(t, baseURL, token, fixture.queueID, fixture.auditLogID)
}

func desktopDeleteOperationsFixture(t *testing.T, baseURL, token string, fixture desktopOperationsFixture) {
	t.Helper()
	for _, request := range []struct {
		method string
		target string
		body   interface{}
	}{
		{method: http.MethodDelete, target: baseURL + "api/batch-tasks/" + fixture.queueID},
		{method: http.MethodDelete, target: baseURL + "api/batch-tasks/" + fixture.singleQueueID},
		{method: http.MethodDelete, target: baseURL + "api/batch-tasks/" + fixture.pausedQueueID},
		{method: http.MethodDelete, target: baseURL + "api/external-mcp/" + fixture.externalMCPName},
		{method: http.MethodDelete, target: baseURL + "api/chat-uploads", body: map[string]string{"path": fixture.fileDirectory}},
	} {
		status, body := desktopJSONRequest(t, request.method, request.target, token, request.body)
		if status != http.StatusOK {
			t.Fatalf("delete desktop operations fixture %s status = %d, body = %#v", request.target, status, body)
		}
	}
	for _, target := range []string{
		baseURL + "api/batch-tasks/" + fixture.queueID,
		baseURL + "api/batch-tasks/" + fixture.singleQueueID,
		baseURL + "api/batch-tasks/" + fixture.pausedQueueID,
		baseURL + "api/external-mcp/" + fixture.externalMCPName,
		baseURL + "api/chat-uploads/content?path=" + url.QueryEscape(fixture.fileRelativePath),
	} {
		status, _ := desktopJSONRequest(t, http.MethodGet, target, token, nil)
		if status != http.StatusNotFound {
			t.Fatalf("deleted desktop operations resource %s status = %d, want 404", target, status)
		}
	}
	if _, err := os.Stat(fixture.fileAbsolutePath); !os.IsNotExist(err) {
		t.Fatalf("deleted desktop managed file remains at %q: %v", fixture.fileAbsolutePath, err)
	}
}

func desktopWaitForExternalMCP(
	t *testing.T,
	baseURL, token, name, wantStatus string,
	wantToolCount int,
	requireError bool,
) map[string]interface{} {
	t.Helper()
	target := baseURL + "api/external-mcp/" + url.PathEscape(name)
	deadline := time.Now().Add(10 * time.Second)
	var status int
	var body map[string]interface{}
	for time.Now().Before(deadline) {
		status, body = desktopJSONRequest(t, http.MethodGet, target, token, nil)
		errorText, _ := body["error"].(string)
		toolCount, _ := body["tool_count"].(float64)
		if status == http.StatusOK &&
			body["status"] == wantStatus &&
			int(toolCount) == wantToolCount &&
			(!requireError || strings.TrimSpace(errorText) != "") {
			return body
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(
		"desktop external MCP %s did not reach status %s with %d tools: HTTP %d, body = %#v",
		name,
		wantStatus,
		wantToolCount,
		status,
		body,
	)
	return nil
}

func desktopAssertExternalMCPTool(t *testing.T, baseURL, token, name string) {
	t.Helper()
	status, body := desktopJSONRequest(
		t,
		http.MethodGet,
		baseURL+"api/config/tools?page=1&page_size=100&search=desktop_echo",
		token,
		nil,
	)
	tool := desktopNestedItem(body, "tools", "name", "desktop_echo")
	if status != http.StatusOK ||
		tool == nil ||
		tool["external_mcp"] != name ||
		tool["is_external"] != true ||
		tool["enabled"] != true {
		t.Fatalf("desktop external MCP tool listing status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(
		t,
		http.MethodGet,
		baseURL+"api/config/tools/desktop_echo/schema?external_mcp="+url.QueryEscape(name),
		token,
		nil,
	)
	if schema, _ := body["input_schema"].(map[string]interface{}); status != http.StatusOK || schema == nil {
		t.Fatalf("desktop external MCP tool schema status = %d, body = %#v", status, body)
	}
}

func desktopInvokeExternalMCP(t *testing.T, baseURL, token, name string, calls <-chan string) {
	t.Helper()
	events := desktopSSERequest(t, baseURL+"api/eino-agent/stream", token, map[string]interface{}{
		"message": "desktop-external-mcp-call",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if !desktopSSEHasEvent(events, "response") || !desktopSSEHasEvent(events, "done") {
		t.Fatalf("desktop external MCP Agent events = %#v", events)
	}
	select {
	case text := <-calls:
		if text != "golden" {
			t.Fatalf("desktop external MCP argument = %q, want golden", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desktop external MCP server did not receive the Agent tool call")
	}
	status, body := desktopJSONRequest(
		t,
		http.MethodGet,
		baseURL+"api/monitor?tool="+url.QueryEscape(name+"__desktop_echo"),
		token,
		nil,
	)
	execution := desktopNestedItem(body, "executions", "toolName", name+"::desktop_echo")
	if status != http.StatusOK || execution == nil || execution["status"] != "completed" {
		t.Fatalf("desktop external MCP execution monitor status = %d, body = %#v", status, body)
	}
}

func desktopWaitForBatchQueue(t *testing.T, baseURL, token, queueID, wantStatus string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+queueID, token, nil)
		queue, _ := body["queue"].(map[string]interface{})
		if status == http.StatusOK && queue["status"] == wantStatus {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+queueID, token, nil)
	t.Fatalf("desktop batch queue %s did not reach %s: status = %d, body = %#v", queueID, wantStatus, status, body)
}

func desktopWaitForBatchTaskStatus(t *testing.T, baseURL, token, queueID, wantStatus string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+queueID, token, nil)
		queue, _ := body["queue"].(map[string]interface{})
		if status == http.StatusOK && queue["status"] == "paused" && desktopNestedItem(queue, "tasks", "status", wantStatus) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/batch-tasks/"+queueID, token, nil)
	t.Fatalf("desktop batch queue %s task did not reach %s: status = %d, body = %#v", queueID, wantStatus, status, body)
}

func desktopAssertChatFilesArchive(t *testing.T, data []byte, wantContent string) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read desktop chat files archive: %v", err)
	}
	foundManifest := false
	foundContent := false
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("open desktop chat files archive entry %q: %v", file.Name, err)
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatalf("read desktop chat files archive entry %q: %v", file.Name, err)
		}
		if file.Name == "manifest.json" {
			foundManifest = bytes.Contains(content, []byte(`"fileCount": 1`))
		}
		if string(content) == wantContent {
			foundContent = true
		}
	}
	if !foundManifest || !foundContent {
		t.Fatalf("desktop chat files archive missing manifest or content: files = %#v", archive.File)
	}
}

func desktopAssertAuditLifecycle(t *testing.T, baseURL, token, resourceID string) string {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/meta", token, nil)
	if status != http.StatusOK || body["enabled"] != true || body["retention_days"] != float64(15) || body["max_export"] != float64(5000) {
		t.Fatalf("desktop audit metadata status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/summary?category=task&resource_id="+url.QueryEscape(resourceID), token, nil)
	auditTotal, _ := body["total"].(float64)
	if status != http.StatusOK || auditTotal < 1 || body["has_filters"] != true {
		t.Fatalf("desktop audit summary status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs?category=task&resource_id="+url.QueryEscape(resourceID), token, nil)
	logItem := desktopNestedItem(body, "logs", "resourceId", resourceID)
	logID, _ := logItem["id"].(string)
	logsTotal, _ := body["total"].(float64)
	if status != http.StatusOK || logsTotal < 1 || logID == "" {
		t.Fatalf("desktop audit filtered logs status = %d, body = %#v", status, body)
	}
	status, detail := desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs/"+url.PathEscape(logID), token, nil)
	logDetail, _ := detail["log"].(map[string]interface{})
	if status != http.StatusOK || logDetail["id"] != logID || logDetail["resourceId"] != resourceID || logDetail["resourceAvailable"] != true {
		t.Fatalf("desktop audit detail status = %d, body = %#v", status, detail)
	}
	detailData, _ := json.Marshal(detail)
	if bytes.Contains(detailData, []byte("stream-secret")) || bytes.Contains(detailData, []byte("desktop-embedding-secret")) {
		t.Fatalf("desktop audit detail exposed credentials: %s", detailData)
	}
	status, missing := desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs/audit_missing_desktop", token, nil)
	if status != http.StatusNotFound || missing["error"] == "" {
		t.Fatalf("missing desktop audit detail status = %d, body = %#v", status, missing)
	}

	status, headers, data := desktopBodyRequest(t, http.MethodGet, baseURL+"api/audit/logs/export?resource_id="+url.QueryEscape(resourceID), token, nil, "")
	if status != http.StatusOK || !strings.HasPrefix(headers.Get("Content-Type"), "application/json") || !bytes.Contains(data, []byte(resourceID)) || bytes.Contains(data, []byte("stream-secret")) || bytes.Contains(data, []byte("desktop-embedding-secret")) {
		t.Fatalf("desktop JSON audit export status = %d, headers = %#v, body = %q", status, headers, data)
	}
	status, headers, data = desktopBodyRequest(t, http.MethodGet, baseURL+"api/audit/logs/export?format=csv&resource_id="+url.QueryEscape(resourceID), token, nil, "")
	if status != http.StatusOK || !strings.HasPrefix(headers.Get("Content-Type"), "text/csv") || !bytes.Contains(data, []byte("id,created_at,level,category,action,result,actor")) || !bytes.Contains(data, []byte(resourceID)) {
		t.Fatalf("desktop CSV audit export status = %d, headers = %#v, body = %q", status, headers, data)
	}
	return logID
}

func desktopAssertPersistedAudit(t *testing.T, baseURL, token, resourceID, logID string) {
	t.Helper()
	status, body := desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/meta", token, nil)
	if status != http.StatusOK || body["enabled"] != true || body["retention_days"] != float64(15) {
		t.Fatalf("persisted desktop audit metadata status = %d, body = %#v", status, body)
	}
	status, body = desktopJSONRequest(t, http.MethodGet, baseURL+"api/audit/logs/"+url.PathEscape(logID), token, nil)
	logDetail, _ := body["log"].(map[string]interface{})
	if status != http.StatusOK || logDetail["id"] != logID || logDetail["resourceId"] != resourceID || logDetail["resourceAvailable"] != true {
		t.Fatalf("persisted desktop audit detail status = %d, body = %#v", status, body)
	}
}

func desktopNestedItems(body map[string]interface{}, key string) []interface{} {
	items, _ := body[key].([]interface{})
	return items
}

func desktopNestedItem(body map[string]interface{}, key, matchKey string, matchValue interface{}) map[string]interface{} {
	for _, raw := range desktopNestedItems(body, key) {
		item, _ := raw.(map[string]interface{})
		if item[matchKey] == matchValue {
			return item
		}
	}
	return nil
}

func desktopNestedItemID(body map[string]interface{}, key, matchKey string, matchValue interface{}) string {
	item := desktopNestedItem(body, key, matchKey, matchValue)
	id, _ := item["id"].(string)
	return id
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

func desktopPayloadHasRole(payload map[string]interface{}, role string) bool {
	messages, _ := payload["messages"].([]interface{})
	for _, item := range messages {
		message, _ := item.(map[string]interface{})
		if message["role"] == role {
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

func desktopJSONArrayRequest(t *testing.T, target, token string) (int, []map[string]interface{}) {
	t.Helper()
	status, _, data := desktopBodyRequest(t, http.MethodGet, target, token, nil, "")
	payload := make([]map[string]interface{}, 0)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("decode desktop array response: %v", err)
		}
	}
	return status, payload
}

func desktopJSONArrayContains(items []map[string]interface{}, key string, value interface{}) bool {
	for _, item := range items {
		if item[key] == value {
			return true
		}
	}
	return false
}

func desktopMultipartUploadRequest(t *testing.T, target, token, filename string, content []byte, fields map[string]string) (int, map[string]interface{}) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create desktop multipart file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write desktop multipart file: %v", err)
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatalf("write desktop multipart field %q: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close desktop multipart body: %v", err)
	}
	status, _, data := desktopBodyRequest(t, http.MethodPost, target, token, &body, writer.FormDataContentType())
	payload := make(map[string]interface{})
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("decode desktop multipart response: %v", err)
		}
	}
	return status, payload
}

func desktopBodyRequest(t *testing.T, method, target, token string, body io.Reader, contentType string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("create desktop body request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send desktop body request: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read desktop body response: %v", err)
	}
	return response.StatusCode, response.Header.Clone(), data
}

func desktopJSONRequest(t *testing.T, method, target, token string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	return desktopJSONRequestWithHeaders(t, method, target, token, body, nil)
}

func desktopJSONRequestWithHeaders(
	t *testing.T,
	method, target, token string,
	body interface{},
	headers map[string]string,
) (int, map[string]interface{}) {
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
	for name, value := range headers {
		request.Header.Set(name, value)
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

func desktopCredentialStoreContains(store *recordingCredentialStore, secret string) bool {
	for _, value := range store.values {
		if value == secret {
			return true
		}
	}
	return false
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

func replaceTestResourceConfig(t *testing.T, resourceDir, version, oldValue, newValue string) {
	t.Helper()
	configPath := filepath.Join(resourceDir, "config.example.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(configData), oldValue, newValue, 1)
	if updated == string(configData) {
		t.Fatalf("test resource config does not contain %q", oldValue)
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	appendTestResourceConfig(t, resourceDir, version, "")
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
