package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxAPIResponseSize = 32 << 20

type apiClient struct {
	baseURL string
	key     string
	http    *http.Client
}

type apiError struct {
	method string
	status int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("EdgeCDN-X API %s returned HTTP %d", e.method, e.status)
}

func hasAPIStatus(err error, status int) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == status
}

// redact masks the API key anywhere it might appear in text destined for logs.
func (a *apiClient) redact(s string) string {
	return strings.ReplaceAll(s, a.key, "[REDACTED]")
}

func (a *apiClient) request(ctx context.Context, method, requestPath string, payload, result any) error {
	var body io.Reader
	var requestBody []byte
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode EdgeCDN-X API request: %w", err)
		}
		requestBody = data
		body = bytes.NewReader(data)
	}

	requestURL := a.baseURL + requestPath
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return fmt.Errorf("create EdgeCDN-X API request: %w", err)
	}
	// Never log request headers: Authorization carries the API key.
	req.Header.Set("Authorization", "ApiKey "+a.key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	debug := slog.Default().Enabled(ctx, slog.LevelDebug)
	if debug {
		slog.DebugContext(ctx, "EdgeCDN-X API request", "method", method, "url", requestURL,
			"body", a.redact(string(requestBody)))
	}
	start := time.Now()
	resp, err := a.http.Do(req)
	if err != nil {
		if debug {
			slog.DebugContext(ctx, "EdgeCDN-X API request failed", "method", method, "url", requestURL,
				"duration", time.Since(start), "error", a.redact(err.Error()))
		}
		return fmt.Errorf("call EdgeCDN-X API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if debug {
			// Read a bounded body for debug logging only; returned errors never
			// include bodies, which proxies or servers may echo (keys are redacted here).
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			slog.DebugContext(ctx, "EdgeCDN-X API response", "method", method, "url", requestURL,
				"status", resp.StatusCode, "duration", time.Since(start), "body", a.redact(string(data)))
		}
		return &apiError{method: method, status: resp.StatusCode}
	}
	if result == nil {
		if debug {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			slog.DebugContext(ctx, "EdgeCDN-X API response", "method", method, "url", requestURL,
				"status", resp.StatusCode, "duration", time.Since(start), "body", a.redact(string(data)))
		}
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return fmt.Errorf("read EdgeCDN-X API response: %w", err)
	}
	if len(data) > maxAPIResponseSize {
		return errors.New("EdgeCDN-X API response exceeds 32 MiB")
	}
	if debug {
		slog.DebugContext(ctx, "EdgeCDN-X API response", "method", method, "url", requestURL,
			"status", resp.StatusCode, "duration", time.Since(start), "body", a.redact(string(data)))
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode EdgeCDN-X API response: %w", err)
	}
	return nil
}

func (a *apiClient) projectPath(projectID string) string {
	return "/project/" + url.PathEscape(projectID)
}

func (a *apiClient) zonesPath(projectID string) string {
	return a.projectPath(projectID) + "/zones"
}

func (a *apiClient) zonePath(projectID, zoneID string) string {
	return a.zonesPath(projectID) + "/" + url.PathEscape(zoneID)
}

func (a *apiClient) endpointsPath(projectID, zoneID string) string {
	return a.zonePath(projectID, zoneID) + "/dns-endpoints"
}

func (a *apiClient) endpointPath(projectID, zoneID, endpointID string) string {
	return a.endpointsPath(projectID, zoneID) + "/" + url.PathEscape(endpointID)
}

func (e *apiError) webhookStatus() int {
	if e.status >= http.StatusInternalServerError {
		return http.StatusBadGateway
	}
	return e.status
}
