package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatCommandPostsArgumentsAndPrintsHumanOutput(t *testing.T) {
	var received chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.URL.Path != "/chat" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set(chatTraceIDHeader, "42")
		_ = json.NewEncoder(w).Encode(chatResponse{Reply: "assistant reply"})
	}))
	defer server.Close()

	t.Setenv(chatGatewayURLEnv, "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	err := runChatCommand([]string{"--url", server.URL + "/", "hello", "from", "terminal"}, strings.NewReader("ignored"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if received != (chatRequest{SenderID: "cli-user", Message: "hello from terminal"}) {
		t.Fatalf("request=%#v", received)
	}
	if stdout.String() != "assistant reply\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if stderr.String() != "Trace ID: 42\n" {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestChatCommandReadsStdinAndPrintsJSON(t *testing.T) {
	var received chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set(chatTraceIDHeader, "73")
		_ = json.NewEncoder(w).Encode(chatResponse{Reply: "structured reply"})
	}))
	defer server.Close()

	t.Setenv(chatGatewayURLEnv, server.URL+"/")
	var stdout, stderr bytes.Buffer
	err := runChatCommand([]string{"--sender-id", "debug-session", "--json"}, strings.NewReader("  list my tasks\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if received != (chatRequest{SenderID: "debug-session", Message: "list my tasks"}) {
		t.Fatalf("request=%#v", received)
	}
	var output chatCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output != (chatCommandOutput{Reply: "structured reply", TraceID: 73}) {
		t.Fatalf("output=%#v", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestChatCommandRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "empty message", args: nil, want: "message is required"},
		{name: "empty sender", args: []string{"--sender-id", " ", "hello"}, want: "--sender-id must not be empty"},
		{name: "zero timeout", args: []string{"--timeout", "0s", "hello"}, want: "--timeout must be positive"},
		{name: "unsupported scheme", args: []string{"--url", "file:///tmp/gateway", "hello"}, want: "scheme must be http or https"},
		{name: "missing host", args: []string{"--url", "http:///gateway", "hello"}, want: "host is required"},
		{name: "query", args: []string{"--url", "http://localhost:18789?debug=1", "hello"}, want: "query and fragment are not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runChatCommand(test.args, strings.NewReader("  \n"), &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want containing %q", err, test.want)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestChatCommandReportsGatewayErrorWithTrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(chatTraceIDHeader, "91")
		http.Error(w, "agent generation failed", http.StatusInternalServerError)
	}))
	defer server.Close()

	err := runChatCommand([]string{"--url", server.URL, "hello"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "500 Internal Server Error: agent generation failed (trace ID 91)") {
		t.Fatalf("error=%v", err)
	}
}

func TestChatCommandRejectsInvalidSuccessResponses(t *testing.T) {
	tests := []struct {
		name    string
		traceID string
		body    string
		want    string
	}{
		{name: "missing trace", body: `{"reply":"hello"}`, want: "missing X-OpenClaw-Trace-ID"},
		{name: "invalid trace", traceID: "zero", body: `{"reply":"hello"}`, want: "invalid X-OpenClaw-Trace-ID"},
		{name: "malformed JSON", traceID: "1", body: `{`, want: "decode chat response"},
		{name: "empty reply", traceID: "1", body: `{"reply":" "}`, want: "empty reply"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.traceID != "" {
					w.Header().Set(chatTraceIDHeader, test.traceID)
				}
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			err := runChatCommand([]string{"--url", server.URL, "hello"}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want containing %q", err, test.want)
			}
		})
	}
}

func TestChatCommandTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set(chatTraceIDHeader, "1")
		_ = json.NewEncoder(w).Encode(chatResponse{Reply: "too late"})
	}))
	defer server.Close()

	started := time.Now()
	err := runChatCommand([]string{"--url", server.URL, "--timeout", "20ms", "hello"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestChatEndpointPreservesBasePath(t *testing.T) {
	got, err := chatEndpoint("https://gateway.example.test/openclaw/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://gateway.example.test/openclaw/chat" {
		t.Fatalf("endpoint=%q", got)
	}
}
