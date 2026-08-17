package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorUsesConfiguredResponsesUpstream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "upstream_api": "responses",
	}}
	req := cliproxyexecutor.Request{Model: "qwen3.8-max", Payload: []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "input").Exists() || gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("request was not translated to Responses: %s", gotBody)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("translated response content = %q, want ok; payload=%s", got, resp.Payload)
	}
}

func TestOpenAICompatExecutorAutoSelectsProtocolAndBailianCache(t *testing.T) {
	tests := []struct {
		name        string
		source      sdktranslator.Format
		cacheMode   string
		wantPath    string
		wantSession bool
		cacheMarker bool
	}{
		{name: "responses uses session cache", source: sdktranslator.FormatOpenAIResponse, cacheMode: "auto", wantPath: "/v1/responses", wantSession: true},
		{name: "chat keeps chat protocol", source: sdktranslator.FormatOpenAI, cacheMode: "auto", wantPath: "/v1/chat/completions", cacheMarker: true},
		{name: "cache can be disabled", source: sdktranslator.FormatOpenAIResponse, cacheMode: "off", wantPath: "/v1/responses"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotPath, gotSessionCache string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotSessionCache = r.Header.Get("x-dashscope-session-cache")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/responses") {
					_, _ = w.Write([]byte(`{"id":"resp_auto","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"chat_auto","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
				"base_url": server.URL + "/v1", "api_key": "test", "upstream_api": "auto",
				"vendor": "aliyun-bailian", "cache_mode": test.cacheMode,
			}}
			payload := []byte(`{"model":"qwen-plus","messages":[{"role":"user","content":"hi"}]}`)
			if test.cacheMarker {
				payload = []byte(`{"model":"qwen-plus","messages":[{"role":"system","content":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"hi"}]}`)
			}
			if test.source == sdktranslator.FormatOpenAIResponse {
				payload = []byte(`{"model":"qwen-plus","input":"hi"}`)
			}
			metadata := make(map[string]any)
			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "qwen-plus", Payload: payload}, cliproxyexecutor.Options{
				SourceFormat: test.source, ResponseFormat: test.source, Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != test.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, test.wantPath)
			}
			if got := gotSessionCache == "enable"; got != test.wantSession {
				t.Fatalf("session cache enabled = %v, want %v", got, test.wantSession)
			}
			if test.wantPath == "/v1/responses" {
				if got := metadata[cliproxyexecutor.ResponseAffinityIDMetadataKey]; got != "resp_auto" {
					t.Fatalf("response affinity ID = %#v, want resp_auto", got)
				}
			}
			if test.cacheMarker && gjson.GetBytes(gotBody, "messages.0.content.0.cache_control.type").String() != "ephemeral" {
				t.Fatalf("explicit cache marker was not preserved: %s", gotBody)
			}
		})
	}
}

func TestOpenAICompatExecutorStreamsConfiguredResponsesUpstream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{
		"base_url": server.URL + "/v1", "api_key": "test", "upstream_api": "responses",
	}}
	req := cliproxyexecutor.Request{Model: "qwen3.8-max", Payload: []byte(`{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)}
	metadata := make(map[string]any)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI, Stream: true, Metadata: metadata}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("Responses request retained chat-only stream_options: %s", gotBody)
	}
	if !gjson.GetBytes(gotBody, "input").Exists() || gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("stream request was not translated to Responses: %s", gotBody)
	}
	if got := metadata[cliproxyexecutor.ResponseAffinityIDMetadataKey]; got != "resp_1" {
		t.Fatalf("stream response affinity ID = %#v, want resp_1", got)
	}
}
