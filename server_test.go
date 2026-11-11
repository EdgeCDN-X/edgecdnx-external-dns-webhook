package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	edgecdnx "github.com/EdgeCDN-X/edgecdnx-sdk-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestAPIErrorStatusIsPreservedWithoutEchoingResponseBody(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"API echoed test-key"}`))
	}))
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body)
	}
	if !strings.Contains(response.Body.String(), "HTTP 403") || strings.Contains(response.Body.String(), "test-key") {
		t.Fatalf("API error should retain its status without exposing the response body: %s", response.Body)
	}
}

func TestAPIRedirectIsRejected(t *testing.T) {
	requests := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/project/test-project/zones" {
			t.Errorf("unexpected follow-up request path %q", r.URL.Path)
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	defer apiServer.Close()

	handler := newWebhookServer(testProvider(t, apiServer.URL))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusFound, response.Body)
	}
	if requests != 1 {
		t.Fatalf("API redirect was followed %d times, want once", requests)
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
	if !exists || created.Spec.RecordTTL != 0 || created.Spec.Targets[0] != "192.0.2.3" {
		t.Fatalf("created record = %#v, exists = %v", created, exists)
	}
	updated := api.records[dnsEndpointResourceName("update.example.com", "A")]
	if updated.Spec.RecordTTL != 120 || updated.Spec.Targets[0] != "192.0.2.2" || updated.ResourceVersion == "1" {
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
	api, err := newProvider(config{apiURL: apiURL, apiKey: "test-key", projectID: "test-project"})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return api
}

type fakeAPI struct {
	mu        sync.Mutex
	records   map[string]apiRecord
	locations map[string]v1alpha1.Location
	profiles  map[string]v1alpha1.HealthCheckProfile
	nextRV    int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		records:   make(map[string]apiRecord),
		locations: make(map[string]v1alpha1.Location),
		profiles:  make(map[string]v1alpha1.HealthCheckProfile),
		nextRV:    3,
	}
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
	if len(parts) >= 3 && parts[0] == "project" && parts[1] == "test-project" && parts[2] == "locations" {
		f.serveLocations(w, r, parts[3:])
		return
	}
	if len(parts) >= 3 && parts[0] == "project" && parts[1] == "test-project" && parts[2] == "healthcheckprofiles" {
		f.serveProfiles(w, r, parts[3:])
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
		record := apiRecord{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: f.version()}, Spec: spec}
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
			ResourceVersion string                `json:"resourceVersion"`
			RecordTTL       *int                  `json:"recordTTL"`
			Targets         []string              `json:"targets"`
			RoutingPolicy   string                `json:"routingPolicy"`
			RouteSelector   *metav1.LabelSelector `json:"routeSelector"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if patch.ResourceVersion != record.ResourceVersion {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale version"})
			return
		}
		if patch.RecordTTL != nil {
			record.Spec.RecordTTL = *patch.RecordTTL
		}
		if patch.Targets != nil {
			record.Spec.Targets = patch.Targets
		}
		if patch.RoutingPolicy != "" {
			record.Spec.RoutingPolicy = patch.RoutingPolicy
		}
		if patch.RouteSelector != nil {
			record.Spec.RouteSelector = patch.RouteSelector
		}
		record.ResourceVersion = f.version()
		f.records[name] = record
		writeJSON(w, http.StatusOK, record)
	case http.MethodDelete:
		if r.URL.Query().Get("resourceVersion") != record.ResourceVersion {
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

func (f *fakeAPI) serveLocations(w http.ResponseWriter, r *http.Request, rest []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		locations := make([]v1alpha1.Location, 0, len(f.locations))
		for _, location := range f.locations {
			locations = append(locations, location)
		}
		writeJSON(w, http.StatusOK, locations)
	case len(rest) == 0 && r.Method == http.MethodPost:
		var request edgecdnx.CreateLocationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if _, exists := f.locations[request.Name]; exists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
			return
		}
		location := v1alpha1.Location{
			ObjectMeta: metav1.ObjectMeta{Name: request.Name, Labels: request.Labels, ResourceVersion: f.version()},
			Spec: v1alpha1.LocationSpec{
				NodeGroups:        request.NodeGroups,
				GeoLookup:         request.GeoLookup,
				Weight:            request.Weight,
				FallbackLocations: request.FallbackLocations,
			},
		}
		f.locations[request.Name] = location
		writeJSON(w, http.StatusCreated, location)
	case len(rest) == 1:
		name := rest[0]
		location, exists := f.locations[name]
		if !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, location)
		case http.MethodPatch:
			var request edgecdnx.UpdateLocationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			if request.Labels != nil {
				location.Labels = *request.Labels
			}
			if request.NodeGroups != nil {
				location.Spec.NodeGroups = *request.NodeGroups
			}
			if request.GeoLookup != nil {
				location.Spec.GeoLookup = *request.GeoLookup
			}
			if request.Weight != nil {
				location.Spec.Weight = *request.Weight
			}
			if request.FallbackLocations != nil {
				location.Spec.FallbackLocations = *request.FallbackLocations
			}
			location.ResourceVersion = f.version()
			f.locations[name] = location
			writeJSON(w, http.StatusOK, location)
		case http.MethodDelete:
			delete(f.locations, name)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) serveProfiles(w http.ResponseWriter, r *http.Request, rest []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		profiles := make([]v1alpha1.HealthCheckProfile, 0, len(f.profiles))
		for _, profile := range f.profiles {
			profiles = append(profiles, profile)
		}
		writeJSON(w, http.StatusOK, profiles)
	case len(rest) == 0 && r.Method == http.MethodPost:
		var request edgecdnx.CreateHealthCheckProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if _, exists := f.profiles[request.Name]; exists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
			return
		}
		profile := v1alpha1.HealthCheckProfile{
			ObjectMeta: metav1.ObjectMeta{Name: request.Name, ResourceVersion: f.version()},
			Spec:       v1alpha1.HealthCheckProfileSpec{Probes: request.Probes},
		}
		f.profiles[request.Name] = profile
		writeJSON(w, http.StatusCreated, profile)
	case len(rest) == 1:
		name := rest[0]
		profile, exists := f.profiles[name]
		if !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, profile)
		case http.MethodPatch:
			var request edgecdnx.UpdateHealthCheckProfileRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			if request.Probes != nil {
				profile.Spec.Probes = *request.Probes
			}
			profile.ResourceVersion = f.version()
			f.profiles[name] = profile
			writeJSON(w, http.StatusOK, profile)
		case http.MethodDelete:
			delete(f.profiles, name)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func fakeRecord(dnsName, recordType string, targets []string, ttl int, resourceVersion string) apiRecord {
	return apiRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:            dnsEndpointResourceName(dnsName, recordType),
			ResourceVersion: resourceVersion,
		},
		Spec: v1alpha1.DNSEndpointSpec{
			DNSName:       dnsName,
			RoutingPolicy: "Simple",
			RecordTTL:     ttl,
			RecordType:    recordType,
			Targets:       targets,
		},
	}
}

func testZone() apiZone {
	return apiZone{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com"},
		Spec:       v1alpha1.ZoneSpec{Zone: "example.com"},
	}
}
