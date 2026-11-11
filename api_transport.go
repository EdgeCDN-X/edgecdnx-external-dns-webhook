package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const maxAPIResponseSize = 32 << 20

type apiRoundTripper struct {
	next http.RoundTripper
	key  string
}

func (t *apiRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	debug := slog.Default().Enabled(req.Context(), slog.LevelDebug)
	if debug {
		slog.DebugContext(req.Context(), "EdgeCDN-X API request", "method", req.Method,
			"url", req.URL.String(), "body", t.redact(requestBody(req)))
	}
	start := time.Now()

	resp, err := next.RoundTrip(req)
	if err != nil {
		if debug {
			slog.DebugContext(req.Context(), "EdgeCDN-X API request failed", "method", req.Method,
				"url", req.URL.String(), "duration", time.Since(start), "error", t.redact(err.Error()))
		}
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("EdgeCDN-X API transport returned no response")
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read EdgeCDN-X API response: %w", err)
	}
	if len(data) > maxAPIResponseSize {
		return nil, errors.New("EdgeCDN-X API response exceeds 32 MiB")
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))

	if debug {
		body := string(data)
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			body = string(data[:min(len(data), 4<<10)])
		}
		slog.DebugContext(req.Context(), "EdgeCDN-X API response", "method", req.Method,
			"url", req.URL.String(), "status", resp.StatusCode, "duration", time.Since(start),
			"body", t.redact(body))
	}
	return resp, nil
}

func requestBody(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	body, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return ""
	}
	return string(data)
}

func (t *apiRoundTripper) redact(value string) string {
	if t.key == "" {
		return value
	}
	return strings.ReplaceAll(value, t.key, "[REDACTED]")
}
