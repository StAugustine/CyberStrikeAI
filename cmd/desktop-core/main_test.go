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
	resourceDir := writeTestResources(t, root, "test-version")
	appendTestResourceConfig(t, resourceDir, "test-version", `multi_agent:
  eino_middleware:
    run_retry_max_attempts: 1
    run_retry_max_backoff_sec: 1
  sub_agents:
    - id: desktop-specialist
      name: Desktop Specialist
      description: Verifies the desktop multi-Agent runtime.
      instruction: Return a concise desktop confirmation.
`)
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
		"multi_agent": map[string]interface{}{
			"enabled": true,
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
		"message": "desktop-tool-execution",
		"finalization": map[string]interface{}{
			"requireExecutionEvidence": false,
		},
	})
	if !desktopSSEHasEvent(toolEvents, "response") || !desktopSSEHasEvent(toolEvents, "done") {
		t.Fatalf("desktop tool execution events = %#v", toolEvents)
	}
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
	status, body = desktopJSONRequest(t, http.MethodGet, restarted.URL+"api/conversations/"+conversationID, restartedToken, nil)
	if status != http.StatusOK {
		t.Fatalf("persisted conversation after core restart status = %d, body = %#v", status, body)
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
