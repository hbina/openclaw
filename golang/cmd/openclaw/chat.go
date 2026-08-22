package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGatewayURL     = "http://127.0.0.1:18789"
	defaultChatTimeout    = 10 * time.Minute
	maxChatResponseBytes  = 1 << 20
	maxChatErrorBodyBytes = 64 << 10
	chatTraceIDHeader     = "X-OpenClaw-Trace-ID"
	chatGatewayURLEnv     = "OPENCLAW_GATEWAY_URL"
)

type chatRequest struct {
	SenderID string `json:"sender_id"`
	Message  string `json:"message"`
}

type chatResponse struct {
	Reply string `json:"reply"`
}

type chatCommandOutput struct {
	Reply   string `json:"reply"`
	TraceID int64  `json:"trace_id"`
}

func runChatCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	defaultURL := strings.TrimSpace(os.Getenv(chatGatewayURLEnv))
	if defaultURL == "" {
		defaultURL = defaultGatewayURL
	}

	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	gatewayURL := flags.String("url", defaultURL, "Gateway base URL")
	senderID := flags.String("sender-id", "cli-user", "CLI conversation routing key")
	timeout := flags.Duration("timeout", defaultChatTimeout, "request timeout")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if strings.TrimSpace(*senderID) == "" {
		return fmt.Errorf("--sender-id must not be empty")
	}

	message, err := chatMessage(flags.Args(), stdin)
	if err != nil {
		return err
	}
	endpoint, err := chatEndpoint(*gatewayURL)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(chatRequest{SenderID: strings.TrimSpace(*senderID), Message: message})
	if err != nil {
		return fmt.Errorf("encode chat request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create chat request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("send chat request: %w", err)
	}
	defer response.Body.Close()

	traceID, traceErr := parseChatTraceID(response.Header.Get(chatTraceIDHeader))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxChatErrorBodyBytes))
		if readErr != nil {
			return fmt.Errorf("gateway returned %s and its error body could not be read: %w", response.Status, readErr)
		}
		detail := strings.TrimSpace(string(body))
		traceSuffix := ""
		if traceErr == nil {
			traceSuffix = fmt.Sprintf(" (trace ID %d)", traceID)
		}
		if detail == "" {
			return fmt.Errorf("gateway returned %s%s", response.Status, traceSuffix)
		}
		return fmt.Errorf("gateway returned %s: %s%s", response.Status, detail, traceSuffix)
	}
	if traceErr != nil {
		return traceErr
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxChatResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read chat response: %w", err)
	}
	if len(body) > maxChatResponseBytes {
		return fmt.Errorf("chat response exceeds %d bytes", maxChatResponseBytes)
	}
	var output chatResponse
	if err := json.Unmarshal(body, &output); err != nil {
		return fmt.Errorf("decode chat response: %w", err)
	}
	if strings.TrimSpace(output.Reply) == "" {
		return fmt.Errorf("chat response contains an empty reply")
	}

	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(chatCommandOutput{Reply: output.Reply, TraceID: traceID}); err != nil {
			return fmt.Errorf("write chat output: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(stdout, output.Reply); err != nil {
		return fmt.Errorf("write chat reply: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "Trace ID: %d\n", traceID); err != nil {
		return fmt.Errorf("write chat trace ID: %w", err)
	}
	return nil
}

func chatMessage(args []string, stdin io.Reader) (string, error) {
	message := strings.TrimSpace(strings.Join(args, " "))
	if len(args) == 0 {
		body, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read chat message from stdin: %w", err)
		}
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		return "", fmt.Errorf("message is required as arguments or stdin")
	}
	return message, nil
}

func chatEndpoint(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("invalid --url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid --url: scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid --url: host is required")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid --url: query and fragment are not supported")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func parseChatTraceID(raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("chat response is missing %s", chatTraceIDHeader)
	}
	traceID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || traceID < 1 {
		return 0, fmt.Errorf("chat response has invalid %s %q", chatTraceIDHeader, raw)
	}
	return traceID, nil
}
