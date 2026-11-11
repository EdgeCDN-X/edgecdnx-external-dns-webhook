package main

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"

	v1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	edgecdnx "github.com/EdgeCDN-X/edgecdnx-sdk-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "edgecdnx-external-dns-webhook"
	endpointLabelKey    = "dns.edgecdnx.com/endpoint"

	defaultNodeGroupName = "default"

	continentAttribute  = "geoip/continent/code"
	countryAttribute    = "geoip/country/code"
	geoDefaultAttribute = "default"

	continentAttributeWeight  = 1000
	countryAttributeWeight    = 100
	geoDefaultAttributeWeight = 1
)

// endpointLabelValue derives the label value identifying all Locations that
// serve a hostname. Hostnames that are not valid label values (wildcards,
// names longer than 63 characters) are replaced with a digest.
func endpointLabelValue(dnsName string) string {
	value := normalizeDNSName(dnsName)
	if validLabelValue(value) {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256-%x", digest[:8])
}

func validLabelValue(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for i, r := range value {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 || i == len(value)-1 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func managedLocationLabels(labelValue string) map[string]string {
	return map[string]string{
		managedByLabelKey: managedByLabelValue,
		endpointLabelKey:  labelValue,
	}
}

// isManagedLocation reports whether a Location is owned by this webhook.
func isManagedLocation(location v1alpha1.Location) bool {
	return location.Labels[managedByLabelKey] == managedByLabelValue
}

// locationServesEndpoint reports whether a managed Location belongs to the
// hostname identified by labelValue.
func locationServesEndpoint(location v1alpha1.Location, labelValue string) bool {
	return isManagedLocation(location) && location.Labels[endpointLabelKey] == labelValue
}

func isManagedPolicy(policy string) bool {
	return policy != "" && policy != policySimple
}

// nodeName returns the deterministic name of the index-th node in the default
// node group.
func nodeName(index int) string {
	if index == 0 {
		return defaultNodeGroupName
	}
	return fmt.Sprintf("%s-%d", defaultNodeGroupName, index+1)
}

// buildNodes zips the IPv4 and IPv6 address lists into node specs.
func buildNodes(ipv4s, ipv6s []string) []v1alpha1.NodeSpec {
	count := max(len(ipv4s), len(ipv6s))
	nodes := make([]v1alpha1.NodeSpec, 0, count)
	for i := 0; i < count; i++ {
		node := v1alpha1.NodeSpec{Name: nodeName(i)}
		if i < len(ipv4s) {
			node.Ipv4 = ipv4s[i]
		}
		if i < len(ipv6s) {
			node.Ipv6 = ipv6s[i]
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// extractNodeIPs collects the IPv4 and IPv6 addresses of all nodes of a
// Location, in node order.
func extractNodeIPs(location v1alpha1.Location) (ipv4s, ipv6s []string) {
	collect := func(nodes []v1alpha1.NodeSpec) {
		for _, node := range nodes {
			if node.Ipv4 != "" {
				ipv4s = append(ipv4s, normalizeIP(node.Ipv4))
			}
			if node.Ipv6 != "" {
				ipv6s = append(ipv6s, normalizeIP(node.Ipv6))
			}
		}
	}
	for _, group := range location.Spec.NodeGroups {
		collect(group.Nodes)
	}
	collect(location.Spec.Nodes)
	return ipv4s, ipv6s
}

func desiredGeoLookup(cfg *routingConfig) v1alpha1.GeoLookupSpec {
	if cfg.policy != policyGeolocation {
		return v1alpha1.GeoLookupSpec{}
	}
	attributes := make(map[string]v1alpha1.GeoLookupAttributeSpec, 3)
	if len(cfg.continents) > 0 {
		attributes[continentAttribute] = geoAttributeSpec(continentAttributeWeight, cfg.continents)
	}
	if len(cfg.countries) > 0 {
		attributes[countryAttribute] = geoAttributeSpec(countryAttributeWeight, cfg.countries)
	}
	if cfg.geoDefault {
		// Catch-all candidate: the plugin scores the "default" attribute's
		// weight for every request regardless of its origin.
		attributes[geoDefaultAttribute] = v1alpha1.GeoLookupAttributeSpec{Weight: geoDefaultAttributeWeight}
	}
	return v1alpha1.GeoLookupSpec{Attributes: attributes}
}

func geoAttributeSpec(weight int, codes []string) v1alpha1.GeoLookupAttributeSpec {
	values := make([]v1alpha1.GeoLookupAttributeValuesSpec, 0, len(codes))
	for _, code := range codes {
		values = append(values, v1alpha1.GeoLookupAttributeValuesSpec{Value: code})
	}
	return v1alpha1.GeoLookupAttributeSpec{Weight: weight, Values: values}
}

// desiredLocation builds the full desired Location for one setIdentifier.
func desiredLocation(cfg *routingConfig, setIdentifier, labelValue string, ipv4s, ipv6s []string) edgecdnx.CreateLocationRequest {
	group := v1alpha1.NodeGroupSpec{
		Name:   defaultNodeGroupName,
		Flavor: "",
		Nodes:  buildNodes(ipv4s, ipv6s),
	}
	if cfg.hasHealth() {
		group.HealthCheck = &corev1.LocalObjectReference{Name: setIdentifier}
	}
	request := edgecdnx.CreateLocationRequest{
		Name:       setIdentifier,
		Labels:     managedLocationLabels(labelValue),
		NodeGroups: []v1alpha1.NodeGroupSpec{group},
		GeoLookup:  desiredGeoLookup(cfg),
	}
	if cfg.policy == policyWeighted {
		request.Weight = cfg.weight
	}
	if cfg.policy == policyFailover && cfg.fallback != "" {
		request.FallbackLocations = []string{cfg.fallback}
	}
	return request
}

// locationNeedsUpdate compares the managed subset of an existing Location with
// the desired state.
func locationNeedsUpdate(existing v1alpha1.Location, desired edgecdnx.CreateLocationRequest) bool {
	if !reflect.DeepEqual(existing.Labels, desired.Labels) {
		return true
	}
	if existing.Spec.Weight != desired.Weight {
		return true
	}
	if !sameStringSlices(existing.Spec.FallbackLocations, desired.FallbackLocations) {
		return true
	}
	if !sameGeoLookup(existing.Spec.GeoLookup, desired.GeoLookup) {
		return true
	}
	return !sameNodeGroups(existing.Spec.NodeGroups, desired.NodeGroups) || len(existing.Spec.Nodes) > 0
}

func sameGeoLookup(current, desired v1alpha1.GeoLookupSpec) bool {
	if current.Weight != desired.Weight {
		return false
	}
	if len(current.Attributes) != len(desired.Attributes) {
		return false
	}
	for name, desiredAttribute := range desired.Attributes {
		currentAttribute, exists := current.Attributes[name]
		if !exists || !reflect.DeepEqual(currentAttribute, desiredAttribute) {
			return false
		}
	}
	return true
}

func sameNodeGroups(current, desired []v1alpha1.NodeGroupSpec) bool {
	if len(current) != len(desired) {
		return false
	}
	for i := range desired {
		if current[i].Name != desired[i].Name || current[i].Flavor != desired[i].Flavor {
			return false
		}
		if (current[i].HealthCheck == nil) != (desired[i].HealthCheck == nil) {
			return false
		}
		if current[i].HealthCheck != nil && current[i].HealthCheck.Name != desired[i].HealthCheck.Name {
			return false
		}
		if len(current[i].Nodes) != len(desired[i].Nodes) {
			return false
		}
		for j := range desired[i].Nodes {
			if current[i].Nodes[j].Name != desired[i].Nodes[j].Name ||
				normalizeIP(current[i].Nodes[j].Ipv4) != desired[i].Nodes[j].Ipv4 ||
				normalizeIP(current[i].Nodes[j].Ipv6) != desired[i].Nodes[j].Ipv6 {
				return false
			}
		}
	}
	return true
}

func sameStringSlices(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	for i := range desired {
		if current[i] != desired[i] {
			return false
		}
	}
	return true
}

// desiredHealthCheckProfile builds the HTTP(S) health check profile attached
// to a managed Location's default node group.
func desiredHealthCheckProfile(cfg *routingConfig, setIdentifier, dnsName string) edgecdnx.CreateHealthCheckProfileRequest {
	return edgecdnx.CreateHealthCheckProfileRequest{
		Name:   setIdentifier,
		Probes: desiredHealthProbes(cfg, dnsName),
	}
}

func desiredHealthProbes(cfg *routingConfig, dnsName string) []v1alpha1.HealthCheckProbeSpec {
	return []v1alpha1.HealthCheckProbeSpec{{
		Name: "http",
		Type: v1alpha1.HealthCheckProbeTypeHTTP,
		HTTP: &v1alpha1.HTTPHealthCheckProbeSpec{
			Protocol: cfg.healthProto,
			Port:     cfg.healthPort,
			Path:     cfg.healthPath,
			Host:     normalizeDNSName(dnsName),
			Stack:    v1alpha1.StackTypeDual,
		},
	}}
}

func profileNeedsUpdate(existing v1alpha1.HealthCheckProfile, desired []v1alpha1.HealthCheckProbeSpec) bool {
	if len(existing.Spec.Probes) != len(desired) {
		return true
	}
	for i := range desired {
		current := existing.Spec.Probes[i]
		if current.Name != desired[i].Name || current.Type != desired[i].Type {
			return true
		}
		if (current.HTTP == nil) != (desired[i].HTTP == nil) {
			return true
		}
		if current.HTTP != nil {
			if current.HTTP.Protocol != desired[i].HTTP.Protocol ||
				current.HTTP.Port != desired[i].HTTP.Port ||
				current.HTTP.Path != desired[i].HTTP.Path ||
				current.HTTP.Host != desired[i].HTTP.Host ||
				current.HTTP.Stack != desired[i].HTTP.Stack {
				return true
			}
		}
	}
	return false
}

// desiredRouteSelector returns the routeSelector for label-selected policies.
func desiredRouteSelector(cfg *routingConfig, labelValue string) *metav1.LabelSelector {
	switch cfg.policy {
	case policyGeolocation, policyWeighted, policyRoundRobin:
		return &metav1.LabelSelector{MatchLabels: map[string]string{endpointLabelKey: labelValue}}
	default:
		return nil
	}
}

func sameRouteSelector(current, desired *metav1.LabelSelector) bool {
	if current == nil || desired == nil {
		return (current == nil) == (desired == nil)
	}
	return reflect.DeepEqual(current.MatchLabels, desired.MatchLabels) &&
		len(current.MatchExpressions) == 0 && len(desired.MatchExpressions) == 0
}

// configFromResources reconstructs the routingConfig of a Location managed by
// this webhook, so GET /records round-trips the original annotations.
// Returns nil when the record's routing policy is not one the webhook manages.
func configFromResources(record apiRecord, location v1alpha1.Location, profiles map[string]v1alpha1.HealthCheckProfile) *routingConfig {
	annotation, known := policyAnnotations[record.Spec.RoutingPolicy]
	if !known || annotation == "simple" {
		return nil
	}
	cfg := &routingConfig{policy: record.Spec.RoutingPolicy}
	switch cfg.policy {
	case policyWeighted:
		cfg.weight = int32(location.Spec.Weight)
	case policyGeolocation:
		cfg.continents = geoAttributeValues(location.Spec.GeoLookup, continentAttribute)
		cfg.countries = geoAttributeValues(location.Spec.GeoLookup, countryAttribute)
		_, cfg.geoDefault = location.Spec.GeoLookup.Attributes[geoDefaultAttribute]
	case policyFailover:
		cfg.secondary = len(record.Spec.Targets) == 0 || record.Spec.Targets[0] != location.Name
		if len(location.Spec.FallbackLocations) > 0 {
			cfg.fallback = location.Spec.FallbackLocations[0]
		}
	}

	for _, group := range location.Spec.NodeGroups {
		if group.HealthCheck == nil {
			continue
		}
		profile, exists := profiles[group.HealthCheck.Name]
		if !exists {
			continue
		}
		for _, probe := range profile.Spec.Probes {
			if probe.Type != v1alpha1.HealthCheckProbeTypeHTTP || probe.HTTP == nil {
				continue
			}
			cfg.healthPath = probe.HTTP.Path
			cfg.healthProto = probe.HTTP.Protocol
			cfg.healthPort = probe.HTTP.Port
			break
		}
		break
	}
	return cfg
}

func geoAttributeValues(geoLookup v1alpha1.GeoLookupSpec, attribute string) []string {
	spec, exists := geoLookup.Attributes[attribute]
	if !exists {
		return nil
	}
	values := make([]string, 0, len(spec.Values))
	for _, value := range spec.Values {
		values = append(values, strings.ToUpper(value.Value))
	}
	return values
}
