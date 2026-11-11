package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func postChanges(t *testing.T, handler http.Handler, request changes) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/records", bytes.NewReader(payload))
	httpRequest.Header.Set("Content-Type", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	return response
}

func getRecords(t *testing.T, handler http.Handler) []endpoint {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/records", nil)
	request.Header.Set("Accept", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /records = %d: %s", response.Code, response.Body)
	}
	var endpoints []endpoint
	if err := json.Unmarshal(response.Body.Bytes(), &endpoints); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	return endpoints
}

func mustAccept(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body)
	}
}

func mustReject(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body)
	}
}

func weightedEndpoint(setIdentifier, ip, weight string, health bool) endpoint {
	properties := props(propRouting, "wrr", propWeight, weight)
	if health {
		properties = append(properties, props(propHealthPath, "/healthz", propHealthProto, "https", propHealthPort, "443")...)
	}
	return managedTestEndpoint("A", setIdentifier, []string{ip}, properties)
}

func TestWeightedMultiClusterProvisioning(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	// Frankfurt cluster syncs first.
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{weightedEndpoint("k8s-fra", "203.0.113.10", "70", true)}}))
	// Amsterdam cluster syncs second and converges on the shared record.
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{weightedEndpoint("k8s-ams", "198.51.100.20", "30", true)}}))

	api.mu.Lock()
	defer api.mu.Unlock()
	fra, exists := api.locations["k8s-fra"]
	if !exists {
		t.Fatal("location k8s-fra missing")
	}
	if fra.Labels[endpointLabelKey] != "app.example.com" || fra.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("location labels = %v", fra.Labels)
	}
	if fra.Spec.Weight != 70 {
		t.Fatalf("fra weight = %d, want 70", fra.Spec.Weight)
	}
	if len(fra.Spec.NodeGroups) != 1 || len(fra.Spec.NodeGroups[0].Nodes) != 1 ||
		fra.Spec.NodeGroups[0].Nodes[0].Ipv4 != "203.0.113.10" {
		t.Fatalf("fra node groups = %+v", fra.Spec.NodeGroups)
	}
	if fra.Spec.NodeGroups[0].HealthCheck == nil || fra.Spec.NodeGroups[0].HealthCheck.Name != "k8s-fra" {
		t.Fatalf("fra health check ref = %+v", fra.Spec.NodeGroups[0].HealthCheck)
	}
	ams := api.locations["k8s-ams"]
	if ams.Spec.Weight != 30 || ams.Spec.NodeGroups[0].Nodes[0].Ipv4 != "198.51.100.20" {
		t.Fatalf("ams location = %+v", ams.Spec)
	}

	profile, exists := api.profiles["k8s-fra"]
	if !exists || len(profile.Spec.Probes) != 1 {
		t.Fatalf("profile = %+v, exists = %v", profile, exists)
	}
	probe := profile.Spec.Probes[0]
	if probe.Type != v1alpha1.HealthCheckProbeTypeHTTP || probe.HTTP == nil ||
		probe.HTTP.Protocol != "https" || probe.HTTP.Port != 443 ||
		probe.HTTP.Path != "/healthz" || probe.HTTP.Host != "app.example.com" {
		t.Fatalf("probe = %+v", probe)
	}

	record, exists := api.records[dnsEndpointResourceName("app.example.com", "A")]
	if !exists {
		t.Fatal("shared DNS endpoint missing")
	}
	if record.Spec.RoutingPolicy != policyWeighted {
		t.Fatalf("routing policy = %q", record.Spec.RoutingPolicy)
	}
	if record.Spec.RouteSelector == nil || record.Spec.RouteSelector.MatchLabels[endpointLabelKey] != "app.example.com" {
		t.Fatalf("route selector = %+v", record.Spec.RouteSelector)
	}
	if len(record.Spec.Targets) != 0 {
		t.Fatalf("weighted record must not have targets: %+v", record.Spec.Targets)
	}
}

func TestGeoRecordsRoundTripAndIdempotency(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	create := managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
		props(propRouting, "geo", propContinents, "EU", propCountries, "GB,CH", propGeoDefault, "true",
			propHealthPath, "/healthz", propHealthProto, "https", propHealthPort, "443"))
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{create}}))

	api.mu.Lock()
	locationVersion := api.locations["k8s-fra"].ResourceVersion
	recordVersion := api.records[dnsEndpointResourceName("app.example.com", "A")].ResourceVersion
	geoLookup := api.locations["k8s-fra"].Spec.GeoLookup
	api.mu.Unlock()
	continent := geoLookup.Attributes[continentAttribute]
	if continent.Weight != continentAttributeWeight || len(continent.Values) != 1 || continent.Values[0].Value != "EU" {
		t.Fatalf("continent attribute = %+v", continent)
	}
	country := geoLookup.Attributes[countryAttribute]
	if country.Weight != countryAttributeWeight || len(country.Values) != 2 {
		t.Fatalf("country attribute = %+v", country)
	}
	geoDefault, hasGeoDefault := geoLookup.Attributes[geoDefaultAttribute]
	if !hasGeoDefault || geoDefault.Weight != geoDefaultAttributeWeight || len(geoDefault.Values) != 0 {
		t.Fatalf("default attribute = %+v (present=%v)", geoDefault, hasGeoDefault)
	}

	// GET /records must reconstruct the endpoint exactly as ExternalDNS sent it.
	records := getRecords(t, handler)
	if len(records) != 1 {
		t.Fatalf("got %d records: %+v", len(records), records)
	}
	got := records[0]
	if got.DNSName != "app.example.com" || got.RecordType != "A" || got.SetIdentifier != "k8s-fra" ||
		len(got.Targets) != 1 || got.Targets[0] != "203.0.113.10" {
		t.Fatalf("reconstructed endpoint = %+v", got)
	}
	wantProps := props(propRouting, "geo", propContinents, "EU", propCountries, "GB,CH", propGeoDefault, "true",
		propHealthPath, "/healthz", propHealthProto, "https", propHealthPort, "443")
	if len(got.ProviderSpecific) != len(wantProps) {
		t.Fatalf("providerSpecific = %+v, want %+v", got.ProviderSpecific, wantProps)
	}
	for i := range wantProps {
		if got.ProviderSpecific[i] != wantProps[i] {
			t.Fatalf("providerSpecific[%d] = %+v, want %+v", i, got.ProviderSpecific[i], wantProps[i])
		}
	}

	// Re-applying the identical create must be a no-op (idempotent upserts).
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{create}}))
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.locations["k8s-fra"].ResourceVersion != locationVersion {
		t.Fatal("idempotent re-create modified the location")
	}
	if api.records[dnsEndpointResourceName("app.example.com", "A")].ResourceVersion != recordVersion {
		t.Fatal("idempotent re-create modified the DNS endpoint")
	}
}

func TestFailoverPrimarySecondaryAnyOrder(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	secondary := managedTestEndpoint("A", "k8s-ams", []string{"198.51.100.20"},
		props(propRouting, "failover", propFailoverRole, "secondary"))
	primary := managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
		props(propRouting, "failover", propFallback, "k8s-ams"))

	// The standby cluster syncs first: record exists without a primary.
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{secondary}}))
	recordID := dnsEndpointResourceName("app.example.com", "A")
	api.mu.Lock()
	record := api.records[recordID]
	api.mu.Unlock()
	if record.Spec.RoutingPolicy != policyFailover || len(record.Spec.Targets) != 0 {
		t.Fatalf("record after secondary = %+v", record.Spec)
	}

	// The primary registers afterwards and claims targets[0].
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{primary}}))
	api.mu.Lock()
	record = api.records[recordID]
	fra := api.locations["k8s-fra"]
	api.mu.Unlock()
	if len(record.Spec.Targets) != 1 || record.Spec.Targets[0] != "k8s-fra" {
		t.Fatalf("record targets = %+v, want [k8s-fra]", record.Spec.Targets)
	}
	if len(fra.Spec.FallbackLocations) != 1 || fra.Spec.FallbackLocations[0] != "k8s-ams" {
		t.Fatalf("fra fallbackLocations = %+v", fra.Spec.FallbackLocations)
	}

	// A competing primary from a third cluster is rejected.
	competing := managedTestEndpoint("A", "k8s-oth", []string{"192.0.2.30"},
		props(propRouting, "failover", propFallback, "k8s-fra"))
	mustReject(t, postChanges(t, handler, changes{Create: []endpoint{competing}}))
}

func TestManagedValidationFailuresReturn400(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	for name, record := range map[string]endpoint{
		"geo without setIdentifier": managedTestEndpoint("A", "", []string{"203.0.113.10"},
			props(propRouting, "geo", propContinents, "EU")),
		"wrr without weight": managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
			props(propRouting, "wrr")),
		"failover primary without fallback": managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
			props(propRouting, "failover")),
		"cname with routing": managedTestEndpoint("CNAME", "k8s-fra", []string{"other.example.com"},
			props(propRouting, "rr")),
	} {
		t.Run(name, func(t *testing.T) {
			mustReject(t, postChanges(t, handler, changes{Create: []endpoint{record}}))
		})
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.locations) != 0 || len(api.records) != 0 {
		t.Fatalf("validation failures must not create resources: %d locations, %d records", len(api.locations), len(api.records))
	}
}

func TestManagedDeleteLifecycle(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	fra := weightedEndpoint("k8s-fra", "203.0.113.10", "70", true)
	ams := weightedEndpoint("k8s-ams", "198.51.100.20", "30", true)
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{fra, ams}}))

	recordID := dnsEndpointResourceName("app.example.com", "A")

	// Removing one cluster keeps the shared record for the remaining one.
	mustAccept(t, postChanges(t, handler, changes{Delete: []endpoint{fra}}))
	api.mu.Lock()
	_, fraLocation := api.locations["k8s-fra"]
	_, fraProfile := api.profiles["k8s-fra"]
	_, recordExists := api.records[recordID]
	api.mu.Unlock()
	if fraLocation || fraProfile {
		t.Fatalf("fra location/profile not garbage collected: location=%v profile=%v", fraLocation, fraProfile)
	}
	if !recordExists {
		t.Fatal("shared record deleted while k8s-ams still serves the hostname")
	}

	// Removing the last cluster deletes the record too.
	mustAccept(t, postChanges(t, handler, changes{Delete: []endpoint{ams}}))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.locations) != 0 || len(api.profiles) != 0 || len(api.records) != 0 {
		t.Fatalf("lifecycle leak: %d locations, %d profiles, %d records",
			len(api.locations), len(api.profiles), len(api.records))
	}
}

func TestDualStackLocationMerge(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	recordA := managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"}, props(propRouting, "rr"))
	recordAAAA := managedTestEndpoint("AAAA", "k8s-fra", []string{"2001:db8::10"}, props(propRouting, "rr"))
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{recordA, recordAAAA}}))

	api.mu.Lock()
	location := api.locations["k8s-fra"]
	api.mu.Unlock()
	if len(location.Spec.NodeGroups) != 1 || len(location.Spec.NodeGroups[0].Nodes) != 1 {
		t.Fatalf("node groups = %+v", location.Spec.NodeGroups)
	}
	node := location.Spec.NodeGroups[0].Nodes[0]
	if node.Ipv4 != "203.0.113.10" || node.Ipv6 != "2001:db8::10" {
		t.Fatalf("node = %+v, want both families", node)
	}

	// Deleting the A record keeps the Location alive for the AAAA record.
	mustAccept(t, postChanges(t, handler, changes{Delete: []endpoint{recordA}}))
	api.mu.Lock()
	defer api.mu.Unlock()
	location, exists := api.locations["k8s-fra"]
	if !exists {
		t.Fatal("location deleted while AAAA family remains")
	}
	node = location.Spec.NodeGroups[0].Nodes[0]
	if node.Ipv4 != "" || node.Ipv6 != "2001:db8::10" {
		t.Fatalf("node after A delete = %+v", node)
	}
	if _, exists := api.records[dnsEndpointResourceName("app.example.com", "A")]; exists {
		t.Fatal("A record not deleted")
	}
	if _, exists := api.records[dnsEndpointResourceName("app.example.com", "AAAA")]; !exists {
		t.Fatal("AAAA record must survive the A record deletion")
	}
}

func TestUnmanagedLocationIsNotAdopted(t *testing.T) {
	api := newFakeAPI()
	api.locations["k8s-fra"] = v1alpha1.Location{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-fra", ResourceVersion: "1"},
		Spec: v1alpha1.LocationSpec{
			NodeGroups: []v1alpha1.NodeGroupSpec{{Name: "ssd", Flavor: "", Nodes: []v1alpha1.NodeSpec{{Name: "n1", Ipv4: "192.0.2.99"}}}},
		},
	}
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	mustReject(t, postChanges(t, handler, changes{Create: []endpoint{weightedEndpoint("k8s-fra", "203.0.113.10", "70", false)}}))

	api.mu.Lock()
	defer api.mu.Unlock()
	location := api.locations["k8s-fra"]
	if location.ResourceVersion != "1" || location.Spec.NodeGroups[0].Name != "ssd" {
		t.Fatalf("unmanaged location was modified: %+v", location)
	}
}

func TestTXTRegistryRecordRoundTripsSetIdentifier(t *testing.T) {
	api := newFakeAPI()
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()
	handler := newWebhookServer(testProvider(t, apiServer.URL))

	heritage := "\"heritage=external-dns,external-dns/owner=fra\""
	// The TXT registry copies setIdentifier and providerSpecific from the
	// tracked endpoint onto the ownership record.
	txt := endpoint{
		DNSName:          "fra-a-app.example.com",
		RecordType:       "TXT",
		SetIdentifier:    "k8s-fra",
		RecordTTL:        60,
		Targets:          []string{heritage},
		ProviderSpecific: props(propRouting, "geo", propContinents, "EU"),
	}
	mustAccept(t, postChanges(t, handler, changes{Create: []endpoint{txt}}))

	api.mu.Lock()
	stored := api.records[dnsEndpointResourceName("fra-a-app.example.com", "TXT")]
	api.mu.Unlock()
	if stored.Spec.RoutingPolicy != "Simple" {
		t.Fatalf("TXT routing policy = %q, want Simple", stored.Spec.RoutingPolicy)
	}
	if len(stored.Spec.Targets) != 2 || stored.Spec.Targets[1] != setIdentifierMarkerPrefix+"k8s-fra" {
		t.Fatalf("stored TXT targets = %+v", stored.Spec.Targets)
	}

	records := getRecords(t, handler)
	if len(records) != 1 {
		t.Fatalf("got %d records: %+v", len(records), records)
	}
	got := records[0]
	if got.SetIdentifier != "k8s-fra" || len(got.Targets) != 1 || got.Targets[0] != heritage {
		t.Fatalf("TXT round trip = %+v", got)
	}

	mustAccept(t, postChanges(t, handler, changes{Delete: []endpoint{txt}}))
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.records) != 0 {
		t.Fatal("TXT record not deleted")
	}
}

func TestAdjustEndpointsValidatesAndCanonicalizes(t *testing.T) {
	handler := newWebhookServer(&provider{})

	// Invalid annotations fail fast with 400.
	payload, _ := json.Marshal([]endpoint{managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"}, props(propRouting, "wrr"))})
	request := httptest.NewRequest(http.MethodPost, "/adjustendpoints", bytes.NewReader(payload))
	request.Header.Set("Content-Type", webhookAccept)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "weight is required") {
		t.Fatalf("unexpected error body: %s", response.Body)
	}

	// Valid annotations are canonicalized (defaults filled, codes uppercased).
	payload, _ = json.Marshal([]endpoint{managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
		props(propRouting, "geo", propCountries, "gb,ch", propHealthPath, "/healthz"))})
	request = httptest.NewRequest(http.MethodPost, "/adjustendpoints", bytes.NewReader(payload))
	request.Header.Set("Content-Type", webhookAccept)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}
	var adjusted []endpoint
	if err := json.Unmarshal(response.Body.Bytes(), &adjusted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := props(propRouting, "geo", propCountries, "GB,CH",
		propHealthPath, "/healthz", propHealthProto, "http", propHealthPort, "80")
	if len(adjusted) != 1 || len(adjusted[0].ProviderSpecific) != len(want) {
		t.Fatalf("adjusted = %+v, want props %+v", adjusted, want)
	}
	for i := range want {
		if adjusted[0].ProviderSpecific[i] != want[i] {
			t.Fatalf("prop[%d] = %+v, want %+v", i, adjusted[0].ProviderSpecific[i], want[i])
		}
	}
}
