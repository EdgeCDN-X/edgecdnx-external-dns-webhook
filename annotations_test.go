package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func props(pairs ...string) []providerSpecificProperty {
	if len(pairs)%2 != 0 {
		panic("props requires name/value pairs")
	}
	properties := make([]providerSpecificProperty, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		properties = append(properties, providerSpecificProperty{Name: pairs[i], Value: pairs[i+1]})
	}
	return properties
}

func managedTestEndpoint(recordType, setIdentifier string, targets []string, properties []providerSpecificProperty) endpoint {
	return endpoint{
		DNSName:          "app.example.com",
		RecordType:       recordType,
		SetIdentifier:    setIdentifier,
		RecordTTL:        60,
		Targets:          targets,
		ProviderSpecific: properties,
	}
}

func TestParseRoutingConfigValidationMatrix(t *testing.T) {
	ipv4 := []string{"203.0.113.10"}
	for _, test := range []struct {
		name     string
		endpoint endpoint
		wantErr  string
	}{
		{
			name:     "simple without properties",
			endpoint: managedTestEndpoint("A", "", ipv4, nil),
		},
		{
			name:     "simple rejects setIdentifier",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, nil),
			wantErr:  "setIdentifier requires",
		},
		{
			name:     "simple rejects edgecdnx properties",
			endpoint: managedTestEndpoint("A", "", ipv4, props(propWeight, "70")),
			wantErr:  "not allowed with simple routing",
		},
		{
			name:     "unknown routing value",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "fancy")),
			wantErr:  "invalid routing",
		},
		{
			name:     "non-simple requires setIdentifier",
			endpoint: managedTestEndpoint("A", "", ipv4, props(propRouting, "geo", propContinents, "EU")),
			wantErr:  "set-identifier is required",
		},
		{
			name:     "non-simple rejects invalid setIdentifier",
			endpoint: managedTestEndpoint("A", "Not_Valid", ipv4, props(propRouting, "rr")),
			wantErr:  "DNS-1123",
		},
		{
			name:     "non-simple rejects CNAME",
			endpoint: managedTestEndpoint("CNAME", "k8s-fra", []string{"other.example.com"}, props(propRouting, "rr")),
			wantErr:  "only supported for A and AAAA",
		},
		{
			name:     "non-simple rejects non-IP target",
			endpoint: managedTestEndpoint("A", "k8s-fra", []string{"not-an-ip"}, props(propRouting, "rr")),
			wantErr:  "not a valid A address",
		},
		{
			name:     "non-simple rejects IPv6 target on A record",
			endpoint: managedTestEndpoint("A", "k8s-fra", []string{"2001:db8::10"}, props(propRouting, "rr")),
			wantErr:  "not a valid A address",
		},
		{
			name:     "wrr requires weight",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "wrr")),
			wantErr:  "weight is required",
		},
		{
			name:     "wrr rejects out of range weight",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "wrr", propWeight, "1001")),
			wantErr:  "between 0 and 1000",
		},
		{
			name:     "wrr accepts weight",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "wrr", propWeight, "70")),
		},
		{
			name:     "wrr rejects geo properties",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "wrr", propWeight, "70", propContinents, "EU")),
			wantErr:  "not allowed with wrr",
		},
		{
			name:     "geo requires continents or countries",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "geo")),
			wantErr:  "requires continents and/or countries",
		},
		{
			name:     "geo rejects invalid code",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "geo", propCountries, "GBR")),
			wantErr:  "invalid country code",
		},
		{
			name:     "geo accepts codes",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "geo", propContinents, "eu", propCountries, "gb, ch")),
		},
		{
			name:     "rr rejects weight",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "rr", propWeight, "70")),
			wantErr:  "not allowed",
		},
		{
			name:     "failover primary requires fallback",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "failover")),
			wantErr:  "fallback is required",
		},
		{
			name:     "failover secondary without fallback",
			endpoint: managedTestEndpoint("A", "k8s-ams", ipv4, props(propRouting, "failover", propFailoverRole, "secondary")),
		},
		{
			name:     "failover rejects unknown role",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "failover", propFailoverRole, "tertiary")),
			wantErr:  "must be primary or secondary",
		},
		{
			name:     "failover role rejected on geo",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "geo", propContinents, "EU", propFailoverRole, "secondary")),
			wantErr:  "not allowed with geo",
		},
		{
			name:     "health protocol without endpoint rejected",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "rr", propHealthProto, "https")),
			wantErr:  "not allowed",
		},
		{
			name:     "health path must start with slash",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "rr", propHealthPath, "healthz")),
			wantErr:  "starting with /",
		},
		{
			name:     "health port range",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "rr", propHealthPath, "/healthz", propHealthPort, "70000")),
			wantErr:  "between 1 and 65535",
		},
		{
			name:     "unknown edgecdnx property",
			endpoint: managedTestEndpoint("A", "k8s-fra", ipv4, props(propRouting, "rr", propPrefix+"frobnicate", "yes")),
			wantErr:  "unknown property",
		},
		{
			name:     "txt registry record ignores annotations",
			endpoint: managedTestEndpoint("TXT", "k8s-fra", []string{`"heritage=external-dns,external-dns/owner=fra"`}, props(propRouting, "geo", propContinents, "EU")),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseRoutingConfig(test.endpoint)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("parseRoutingConfig: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseRoutingConfig = %+v, want error containing %q", cfg, test.wantErr)
			}
			var permanentErr *permanentProviderError
			if !errors.As(err, &permanentErr) {
				t.Fatalf("error %v is not permanent (would not map to 400)", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestHealthDefaults(t *testing.T) {
	cfg, err := parseRoutingConfig(managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
		props(propRouting, "rr", propHealthPath, "/healthz")))
	if err != nil {
		t.Fatalf("parseRoutingConfig: %v", err)
	}
	if cfg.healthProto != "http" || cfg.healthPort != 80 {
		t.Fatalf("defaults = %s:%d, want http:80", cfg.healthProto, cfg.healthPort)
	}
	cfg, err = parseRoutingConfig(managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"},
		props(propRouting, "rr", propHealthPath, "/healthz", propHealthProto, "https")))
	if err != nil {
		t.Fatalf("parseRoutingConfig: %v", err)
	}
	if cfg.healthPort != 443 {
		t.Fatalf("https default port = %d, want 443", cfg.healthPort)
	}
}

func TestSerializationRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties []providerSpecificProperty
	}{
		{"wrr", props(propRouting, "wrr", propWeight, "70")},
		{"geo", props(propRouting, "geo", propContinents, "EU", propCountries, "GB,CH")},
		{"rr", props(propRouting, "rr")},
		{"failover primary", props(propRouting, "failover", propFallback, "k8s-ams")},
		{"failover secondary", props(propRouting, "failover", propFailoverRole, "secondary")},
		{"rr with health", props(propRouting, "rr", propHealthPath, "/healthz", propHealthProto, "https", propHealthPort, "443")},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := managedTestEndpoint("A", "k8s-fra", []string{"203.0.113.10"}, test.properties)
			cfg, err := parseRoutingConfig(record)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			record.ProviderSpecific = serializeProviderSpecific(cfg)
			reparsed, err := parseRoutingConfig(record)
			if err != nil {
				t.Fatalf("reparse serialized properties: %v", err)
			}
			if !reflect.DeepEqual(cfg, reparsed) {
				t.Fatalf("round trip changed config: %+v != %+v", cfg, reparsed)
			}
		})
	}
}

func TestCanonicalizeEndpointNormalizes(t *testing.T) {
	record := managedTestEndpoint("AAAA", "k8s-fra", []string{"2001:DB8:0:0:0:0:0:10"},
		props(propRouting, "GEO", propContinents, "eu", propCountries, " gb ,ch "))
	record.ProviderSpecific = append(record.ProviderSpecific, providerSpecificProperty{Name: "webhook/other", Value: "kept"})
	canonical, err := canonicalizeEndpoint(record)
	if err != nil {
		t.Fatalf("canonicalizeEndpoint: %v", err)
	}
	want := append(props("webhook/other", "kept"),
		props(propRouting, "geo", propContinents, "EU", propCountries, "GB,CH")...)
	if !reflect.DeepEqual(canonical.ProviderSpecific, want) {
		t.Fatalf("providerSpecific = %+v, want %+v", canonical.ProviderSpecific, want)
	}
	if canonical.Targets[0] != "2001:db8::10" {
		t.Fatalf("target not normalized: %q", canonical.Targets[0])
	}
}

func TestEndpointLabelValue(t *testing.T) {
	if got := endpointLabelValue("app.example.com"); got != "app.example.com" {
		t.Fatalf("endpointLabelValue = %q", got)
	}
	wildcard := endpointLabelValue("*.example.com")
	if !strings.HasPrefix(wildcard, "sha256-") || !validLabelValue(wildcard) {
		t.Fatalf("wildcard label value = %q", wildcard)
	}
	long := endpointLabelValue(strings.Repeat("a", 60) + ".example.com")
	if !strings.HasPrefix(long, "sha256-") || !validLabelValue(long) {
		t.Fatalf("long label value = %q", long)
	}
}
