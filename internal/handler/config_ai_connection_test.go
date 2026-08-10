package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOpenAIConnectionReturnsSafeDesktopDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		upstream   int
		success    bool
		statusCode int
	}{
		{name: "success", upstream: http.StatusOK, success: true},
		{name: "unauthorized", upstream: http.StatusUnauthorized, statusCode: http.StatusUnauthorized},
		{name: "rate limited", upstream: http.StatusTooManyRequests, statusCode: http.StatusTooManyRequests},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("upstream path = %q", request.URL.Path)
					http.Error(response, "unexpected path", http.StatusBadRequest)
					return
				}
				if request.Header.Get("Authorization") != "Bearer request-secret" {
					t.Errorf("upstream authorization header = %q", request.Header.Get("Authorization"))
					http.Error(response, "unexpected authorization", http.StatusBadRequest)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(test.upstream)
				if test.upstream == http.StatusOK {
					_, _ = response.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
					return
				}
				_, _ = response.Write([]byte(`{"error":{"message":"upstream-secret-marker"}}`))
			}))
			defer upstream.Close()

			handler, _, _, _ := newDesktopConfigHandler(t)
			body, err := json.Marshal(TestOpenAIRequest{
				Provider: "openai_compatible",
				BaseURL:  upstream.URL + "/v1",
				APIKey:   "request-secret",
				Model:    "test-model",
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/config/test-openai", bytes.NewReader(body))
			context.Request.Header.Set("Content-Type", "application/json")
			handler.TestOpenAI(context)

			if recorder.Code != http.StatusOK {
				t.Fatalf("TestOpenAI status = %d: %s", recorder.Code, recorder.Body.String())
			}
			var result struct {
				Success    bool `json:"success"`
				StatusCode int  `json:"status_code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success != test.success || result.StatusCode != test.statusCode {
				t.Fatalf("TestOpenAI result = %+v, body = %s", result, recorder.Body.String())
			}
			if bytes.Contains(recorder.Body.Bytes(), []byte("request-secret")) || bytes.Contains(recorder.Body.Bytes(), []byte("upstream-secret-marker")) {
				t.Fatalf("TestOpenAI exposed sensitive upstream data: %s", recorder.Body.String())
			}
		})
	}
}
