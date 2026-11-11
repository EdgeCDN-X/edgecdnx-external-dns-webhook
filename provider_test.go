package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWildcardDNSNames(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"*.spaceup.tbotech.sk", true},
		{"www.example.com", true},
		{"_acme-challenge.example.com", true},
		{"*", false},
		{"*.", false},
		{"*foo.example.com", false},
		{"foo*.example.com", false},
		{"www.*.example.com", false},
		{"*.*.example.com", false},
		{"*..example.com", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validDNSName(test.name); got != test.valid {
				t.Fatalf("validDNSName(%q) = %v, want %v", test.name, got, test.valid)
			}
		})
	}
}

func TestZoneValidationRemainsStrict(t *testing.T) {
	for _, name := range []string{"*.example.com", "_dns.example.com", "*"} {
		if validZone(name) {
			t.Errorf("validZone(%q) = true, want false", name)
		}
	}
	if !validZone("example.com") {
		t.Error("validZone(example.com) = false, want true")
	}
}

func TestWildcardEndpointConversion(t *testing.T) {
	record := fakeRecord("*.spaceup.tbotech.sk", "A", []string{"192.0.2.1"}, 60, "1")
	converted, include, err := endpointFromAPI(record)
	if err != nil || !include {
		t.Fatalf("endpointFromAPI: include=%v, error=%v", include, err)
	}
	zone := apiZone{ObjectMeta: metav1.ObjectMeta{Name: "tbotech.sk"}}
	zone.Spec.Zone = "tbotech.sk"
	p := &provider{}
	matched, spec, err := p.toAPIRecord([]apiZone{zone}, converted)
	if err != nil {
		t.Fatalf("toAPIRecord: %v", err)
	}
	if matched.Name != zone.Name || !sameRecord(record.Spec, spec) {
		t.Fatalf("wildcard round trip changed record: zone=%+v, spec=%+v", matched, spec)
	}
}
