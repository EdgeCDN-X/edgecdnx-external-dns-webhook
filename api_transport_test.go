package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type repeatedByteReader struct{}

func (repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestAPIRoundTripperRejectsOversizedResponse(t *testing.T) {
	transport := &apiRoundTripper{next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(io.LimitReader(repeatedByteReader{}, maxAPIResponseSize+1)),
		}, nil
	})}
	request, err := http.NewRequest(http.MethodGet, "https://api.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil || !strings.Contains(err.Error(), "exceeds 32 MiB") {
		t.Fatalf("RoundTrip error = %v, want response-size error", err)
	}
}

func TestAPIRoundTripperRedactsDebugLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	transport := &apiRoundTripper{
		key: "secret-key",
		next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"echo":"secret-key"}`)),
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodPost, "https://api.example.test", bytes.NewBufferString(`{"key":"secret-key"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "ApiKey secret-key")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	if strings.Contains(logs.String(), "secret-key") || strings.Contains(logs.String(), "Authorization") {
		t.Fatalf("debug logs leaked credentials: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("debug logs did not contain redacted payload details: %s", logs.String())
	}
}
