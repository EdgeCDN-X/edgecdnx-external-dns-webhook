package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const webhookAccept = webhookMediaType + ";version=1"

func TestNegotiateReturnsProjectZones(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != webhookAccept {
		t.Fatalf("Content-Type = %q, want %q", got, webhookAccept)
	}
	var result struct {
		Filters []string `json:"filters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Filters) != 1 || result.Filters[0] != ".example.com" {
		t.Fatalf("filters = %#v, want [\".example.com\"]", result.Filters)
	}
}

func TestNegotiateRejectsUnsupportedVersion(t *testing.T) {
	handler := newWebhookServer(&provider{})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", webhookMediaType+";version=2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotAcceptable)
	}
	if !strings.Contains(response.Body.String(), "error") {
		t.Fatalf("response body should contain a complete error: %s", response.Body)
	}
}

func TestNegotiateRejectsZeroQuality(t *testing.T) {
	handler := newWebhookServer(&provider{})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", webhookMediaType+";version=1;q=0.0")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotAcceptable)
	}
}

func TestRecordsConvertsAPIRecordsAndHidesServicePlaceholders(t *testing.T) {
	api := newFakeAPI()
	api.records["www.example.com-a"] = fakeRecord("www.example.com", "A", []string{"192.0.2.1"}, 120, "1")
	api.records["service.example.com-a"] = fakeRecord("service.example.com", "A", []string{"LINK: service"}, 10, "2")
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodGet, "/records", nil)
	request.Header.Set("Accept", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body)
	}
	var result []endpoint
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d endpoints, want 1: %#v", len(result), result)
	}
	if result[0].DNSName != "www.example.com" || result[0].RecordType != "A" ||
		result[0].RecordTTL != 120 || len(result[0].Targets) != 1 ||
		result[0].Targets[0] != "192.0.2.1" {
		t.Fatalf("unexpected endpoint: %#v", result[0])
	}
}

func TestApplyChangesUsesVersionedAPIUpdates(t *testing.T) {
	api := newFakeAPI()
	api.records[dnsEndpointResourceName("update.example.com", "A")] = fakeRecord(
		"update.example.com", "A", []string{"192.0.2.1"}, 60, "1")
	api.records[dnsEndpointResourceName("delete.example.com", "TXT")] = fakeRecord(
		"delete.example.com", "TXT", []string{"owned"}, 60, "2")
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	body := `{
		"create":[{"dnsName":"create.example.com","recordType":"A","recordTTL":0,"targets":["192.0.2.3"]}],
		"updateOld":[{"dnsName":"update.example.com","recordType":"A","recordTTL":60,"targets":["192.0.2.1"]}],
		"updateNew":[{"dnsName":"update.example.com","recordType":"A","recordTTL":120,"targets":["192.0.2.2"]}],
		"delete":[{"dnsName":"delete.example.com","recordType":"TXT","recordTTL":60,"targets":["owned"]}]
	}`
	request := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(body))
	request.Header.Set("Content-Type", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	created, exists := api.records[dnsEndpointResourceName("create.example.com", "A")]
	if !exists || created.Spec.RecordTTL != defaultRecordTTL || created.Spec.Targets[0] != "192.0.2.3" {
		t.Fatalf("created record = %#v, exists = %v", created, exists)
	}
	updated := api.records[dnsEndpointResourceName("update.example.com", "A")]
	if updated.Spec.RecordTTL != 120 || updated.Spec.Targets[0] != "192.0.2.2" || updated.Metadata.ResourceVersion == "1" {
		t.Fatalf("updated record = %#v", updated)
	}
	if _, exists := api.records[dnsEndpointResourceName("delete.example.com", "TXT")]; exists {
		t.Fatal("deleted record still exists")
	}
}

func TestApplyRejectsUnpairedUpdates(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(`{"updateOld":[{}]}`))
	request.Header.Set("Content-Type", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body)
	}
}

func TestApplyDoesNotDeleteNonSimpleRouting(t *testing.T) {
	api := newFakeAPI()
	record := fakeRecord("geo.example.com", "A", []string{"192.0.2.1"}, 60, "1")
	record.Spec.RoutingPolicy = "Geolocation"
	name := dnsEndpointResourceName("geo.example.com", "A")
	api.records[name] = record
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(
		`{"delete":[{"dnsName":"geo.example.com","recordType":"A","recordTTL":60,"targets":["192.0.2.1"]}]}`,
	))
	request.Header.Set("Content-Type", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, exists := api.records[name]; !exists {
		t.Fatal("non-Simple DNS endpoint was deleted")
	}
}

func testProvider(t *testing.T, apiURL string) *provider {
	t.Helper()
	api, err := newAPIClient(config{apiURL: apiURL, apiKey: "test-key", projectID: "test-project"})
	if err != nil {
		t.Fatalf("create API client: %v", err)
	}
	return &provider{api: api, projectID: "test-project"}
}

type fakeAPI struct {
	mu      sync.Mutex
	records map[string]apiRecord
	nextRV  int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{records: make(map[string]apiRecord), nextRV: 3}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "ApiKey test-key" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 && parts[0] == "project" && parts[1] == "test-project" && parts[2] == "zones" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, []apiZone{testZone()})
		return
	}
	if len(parts) < 5 || parts[0] != "project" || parts[1] != "test-project" ||
		parts[2] != "zones" || parts[3] != "example.com" || parts[4] != "dns-endpoints" {
		http.NotFound(w, r)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(parts) == 5 && r.Method == http.MethodGet {
		records := make([]apiRecord, 0, len(f.records))
		for _, record := range f.records {
			records = append(records, record)
		}
		writeJSON(w, http.StatusOK, records)
		return
	}
	if len(parts) == 5 && r.Method == http.MethodPost {
		var spec apiRecordSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		name := dnsEndpointResourceName(spec.DNSName, spec.RecordType)
		if _, exists := f.records[name]; exists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
			return
		}
		record := apiRecord{Metadata: apiMetadata{Name: name, ResourceVersion: f.version()}, Spec: spec}
		f.records[name] = record
		writeJSON(w, http.StatusCreated, record)
		return
	}
	if len(parts) != 6 {
		http.NotFound(w, r)
		return
	}
	name := parts[5]
	record, exists := f.records[name]
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, record)
	case http.MethodPatch:
		var patch struct {
			ResourceVersion string   `json:"resourceVersion"`
			RecordTTL       int      `json:"recordTTL"`
			Targets         []string `json:"targets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if patch.ResourceVersion != record.Metadata.ResourceVersion {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale version"})
			return
		}
		record.Spec.RecordTTL = patch.RecordTTL
		record.Spec.Targets = patch.Targets
		record.Metadata.ResourceVersion = f.version()
		f.records[name] = record
		writeJSON(w, http.StatusOK, record)
	case http.MethodDelete:
		if r.URL.Query().Get("resourceVersion") != record.Metadata.ResourceVersion {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale version"})
			return
		}
		delete(f.records, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) version() string {
	version := f.nextRV
	f.nextRV++
	return fmt.Sprint(version)
}

func fakeRecord(dnsName, recordType string, targets []string, ttl int, resourceVersion string) apiRecord {
	return apiRecord{
		Metadata: apiMetadata{
			Name:            dnsEndpointResourceName(dnsName, recordType),
			ResourceVersion: resourceVersion,
		},
		Spec: apiRecordSpec{
			DNSName:       dnsName,
			RoutingPolicy: "Simple",
			RecordTTL:     ttl,
			RecordType:    recordType,
			Targets:       targets,
		},
	}
}

func testZone() apiZone {
	zone := apiZone{Metadata: apiMetadata{Name: "example.com"}}
	zone.Spec.Zone = "example.com"
	return zone
}
