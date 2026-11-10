package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func (a *apiClient) request(ctx context.Context, method, requestPath string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode EdgeCDN-X API request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+requestPath, body)
	if err != nil {
		return fmt.Errorf("create EdgeCDN-X API request: %w", err)
	}
	req.Header.Set("Authorization", "ApiKey "+a.key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("call EdgeCDN-X API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &apiError{method: method, status: resp.StatusCode}
	}
	if result == nil {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return fmt.Errorf("read EdgeCDN-X API response: %w", err)
	}
	if len(data) > maxAPIResponseSize {
		return errors.New("EdgeCDN-X API response exceeds 32 MiB")
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
