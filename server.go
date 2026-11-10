package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const (
	webhookMediaType = "application/external.dns.webhook+json"
	webhookVersion   = "1"
	maxWebhookBody   = 32 << 20
)

type webhookServer struct {
	provider *provider
}

func newWebhookServer(provider *provider) http.Handler {
	mux := http.NewServeMux()
	server := &webhookServer{provider: provider}
	mux.HandleFunc("/", server.negotiate)
	mux.HandleFunc("/records", server.records)
	mux.HandleFunc("/adjustendpoints", server.adjustEndpoints)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *webhookServer) negotiate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !acceptsWebhookV1(r.Header.Get("Accept")) {
		writeError(w, http.StatusNotAcceptable, "supported media type is "+webhookMediaType+";version="+webhookVersion)
		return
	}
	zones, err := s.provider.zones(r.Context())
	if err != nil {
		writeProviderError(w, err)
		return
	}
	filters := make([]string, 0, len(zones))
	for _, zone := range zones {
		filters = append(filters, "."+zone.Spec.Zone)
	}
	writeJSON(w, http.StatusOK, map[string][]string{"filters": filters})
}

func (s *webhookServer) records(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !acceptsWebhookV1(r.Header.Get("Accept")) {
			writeError(w, http.StatusNotAcceptable, "unsupported webhook media type")
			return
		}
		records, err := s.provider.records(r.Context())
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, records)
	case http.MethodPost:
		if !hasWebhookContentType(r.Header.Get("Content-Type")) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported webhook media type")
			return
		}
		var request changes
		if err := decodeBody(w, r, &request); err != nil {
			writeDecodeError(w, err)
			return
		}
		if err := s.provider.apply(r.Context(), request); err != nil {
			writeProviderError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *webhookServer) adjustEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !hasWebhookContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported webhook media type")
		return
	}
	var endpoints []endpoint
	if err := decodeBody(w, r, &endpoints); err != nil {
		writeDecodeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, endpoints)
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return fmt.Errorf("invalid JSON request body: %w", err)
	}
	return nil
}

func acceptsWebhookV1(header string) bool {
	for _, candidate := range strings.Split(header, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(candidate))
		if err != nil {
			continue
		}
		if mediaType != webhookMediaType || params["version"] != webhookVersion {
			continue
		}
		quality := 1.0
		if rawQuality, exists := params["q"]; exists {
			parsedQuality, err := strconv.ParseFloat(rawQuality, 64)
			if err != nil || parsedQuality <= 0 {
				continue
			}
			quality = parsedQuality
		}
		if quality > 0 {
			return true
		}
	}
	return false
}

func hasWebhookContentType(header string) bool {
	mediaType, params, err := mime.ParseMediaType(header)
	return err == nil && mediaType == webhookMediaType && params["version"] == webhookVersion
}

func writeProviderError(w http.ResponseWriter, err error) {
	var permanentErr *permanentProviderError
	if errors.As(err, &permanentErr) {
		writeError(w, http.StatusBadRequest, permanentErr.Error())
		return
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.webhookStatus(), err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 32 MiB")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", webhookMediaType+";version="+webhookVersion)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write webhook response", "error", err)
	}
}
