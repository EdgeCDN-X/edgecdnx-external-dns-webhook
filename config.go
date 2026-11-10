package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type config struct {
	listenAddress string
	apiURL        string
	apiKey        string
	projectID     string
}

func configFromEnv() (config, error) {
	listenAddress := os.Getenv("WEBHOOK_LISTEN_ADDRESS")
	if listenAddress == "" {
		listenAddress = "0.0.0.0:8888"
	}

	apiURL := os.Getenv("EDGECDNX_API_URL")
	parsedURL, err := url.Parse(apiURL)
	if err != nil || parsedURL.Host == "" ||
		(parsedURL.Scheme != "https" && parsedURL.Scheme != "http") ||
		parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return config{}, errors.New("EDGECDNX_API_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}

	apiKey := os.Getenv("EDGECDNX_API_KEY")
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, " \t\r\n") {
		return config{}, errors.New("EDGECDNX_API_KEY must contain an API key")
	}

	projectID := os.Getenv("EDGECDNX_PROJECT_ID")
	if !validResourceID(projectID) {
		return config{}, errors.New("EDGECDNX_PROJECT_ID must contain a valid Project ID")
	}

	return config{
		listenAddress: listenAddress,
		apiURL:        strings.TrimRight(apiURL, "/"),
		apiKey:        apiKey,
		projectID:     projectID,
	}, nil
}

func newAPIClient(cfg config) (*apiClient, error) {
	parsedURL, err := url.Parse(cfg.apiURL)
	if err != nil || parsedURL.Host == "" ||
		(parsedURL.Scheme != "https" && parsedURL.Scheme != "http") ||
		parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return nil, fmt.Errorf("invalid API URL")
	}
	if strings.TrimSpace(cfg.apiKey) == "" || strings.ContainsAny(cfg.apiKey, " \t\r\n") {
		return nil, errors.New("API key must not be empty or contain whitespace")
	}
	return &apiClient{
		baseURL: strings.TrimRight(cfg.apiURL, "/"),
		key:     cfg.apiKey,
		http: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}
