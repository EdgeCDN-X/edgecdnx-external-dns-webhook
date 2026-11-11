package main

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	edgecdnx "github.com/EdgeCDN-X/edgecdnx-sdk-go"
)

// managedEndpointsFromRecord reconstructs ExternalDNS endpoints from a
// non-Simple DNS record and the managed Locations serving its hostname. One
// endpoint is emitted per Location (setIdentifier). Locations without an
// ownership label, without IPs of the record's family, or records with an
// unknown routing policy are skipped (treated as externally managed).
func managedEndpointsFromRecord(record apiRecord, locations []v1alpha1.Location, profiles map[string]v1alpha1.HealthCheckProfile) []endpoint {
	dnsName := normalizeDNSName(record.Spec.DNSName)
	recordType := strings.ToUpper(record.Spec.RecordType)
	if recordType != "A" && recordType != "AAAA" {
		return nil
	}
	labelValue := endpointLabelValue(dnsName)
	endpoints := make([]endpoint, 0)
	for _, location := range locations {
		if !locationServesEndpoint(location, labelValue) {
			continue
		}
		ipv4s, ipv6s := extractNodeIPs(location)
		targets := ipv4s
		if recordType == "AAAA" {
			targets = ipv6s
		}
		if len(targets) == 0 {
			continue
		}
		cfg := configFromResources(record, location, profiles)
		if cfg == nil {
			continue
		}
		endpoints = append(endpoints, endpoint{
			DNSName:          dnsName,
			RecordType:       recordType,
			SetIdentifier:    location.Name,
			RecordTTL:        int64(record.Spec.RecordTTL),
			Targets:          targets,
			ProviderSpecific: serializeProviderSpecific(cfg),
		})
	}
	return endpoints
}

// createManagedEndpoint provisions or converges the HealthCheckProfile,
// Location, and DNSEndpoint for one managed endpoint. All operations are
// idempotent so ExternalDNS retries converge.
func (p *provider) createManagedEndpoint(ctx context.Context, zones []apiZone, record endpoint, cfg *routingConfig) error {
	dnsName := normalizeDNSName(record.DNSName)
	recordType := strings.ToUpper(record.RecordType)
	if !validDNSName(dnsName) {
		return permanentError("invalid DNS name %q", record.DNSName)
	}
	if len(record.Targets) == 0 {
		return permanentError("DNS endpoint %q must have at least one target", dnsName)
	}
	ttl := record.RecordTTL
	if ttl < 0 || int64(int(ttl)) != ttl {
		return permanentError("invalid TTL %d for DNS endpoint %q", ttl, dnsName)
	}
	zone, err := p.matchZone(zones, dnsName)
	if err != nil {
		return &permanentProviderError{err: err}
	}
	labelValue := endpointLabelValue(dnsName)
	setIdentifier := record.SetIdentifier

	existingLocation, locationExists, err := p.getManagedLocation(ctx, setIdentifier, labelValue)
	if err != nil {
		return err
	}

	if cfg.hasHealth() {
		if err := p.upsertHealthCheckProfile(ctx, cfg, setIdentifier, dnsName); err != nil {
			return err
		}
	}

	targets := make([]string, 0, len(record.Targets))
	for _, target := range record.Targets {
		if !validIPForRecordType(target, recordType) {
			return permanentError("target %q of endpoint %q is not a valid %s address", target, dnsName, recordType)
		}
		targets = append(targets, normalizeIP(target))
	}
	var ipv4s, ipv6s []string
	if recordType == "A" {
		ipv4s = targets
	} else {
		ipv6s = targets
	}
	if locationExists {
		// Preserve the other IP family maintained by the sibling A/AAAA record.
		existingV4, existingV6 := extractNodeIPs(existingLocation)
		if recordType == "A" {
			ipv6s = existingV6
		} else {
			ipv4s = existingV4
		}
	}
	staleProfile := !cfg.hasHealth() && locationExists && locationReferencesProfile(existingLocation, setIdentifier)

	desired := desiredLocation(cfg, setIdentifier, labelValue, ipv4s, ipv6s)
	if err := p.upsertLocation(ctx, existingLocation, locationExists, desired, labelValue); err != nil {
		return err
	}
	if staleProfile {
		if err := p.deleteHealthCheckProfile(ctx, setIdentifier); err != nil {
			return err
		}
	}

	return p.upsertManagedRecord(ctx, zone, dnsName, recordType, int(ttl), cfg, labelValue, setIdentifier)
}

// deleteManagedEndpoint removes one setIdentifier's IP family from its
// Location, garbage-collects the Location and its profile when empty, and
// deletes the DNSEndpoint when no managed Location serves the record anymore.
func (p *provider) deleteManagedEndpoint(ctx context.Context, zones []apiZone, record endpoint, cfg *routingConfig) error {
	dnsName := normalizeDNSName(record.DNSName)
	recordType := strings.ToUpper(record.RecordType)
	zone, err := p.matchZone(zones, dnsName)
	if err != nil {
		return &permanentProviderError{err: err}
	}
	labelValue := endpointLabelValue(dnsName)
	setIdentifier := record.SetIdentifier

	existingLocation, locationExists, err := p.getManagedLocation(ctx, setIdentifier, labelValue)
	if err != nil {
		return err
	}
	locationDeleted := false
	if locationExists {
		ipv4s, ipv6s := extractNodeIPs(existingLocation)
		if recordType == "A" {
			ipv4s = nil
		} else {
			ipv6s = nil
		}
		if len(ipv4s)+len(ipv6s) == 0 {
			hadProfile := locationReferencesProfile(existingLocation, setIdentifier)
			if err := p.api.Locations.Delete(ctx, p.projectID, setIdentifier); err != nil && !edgecdnx.IsNotFound(err) {
				return translateAPIError("delete location", err)
			}
			if hadProfile {
				if err := p.deleteHealthCheckProfile(ctx, setIdentifier); err != nil {
					return err
				}
			}
			locationDeleted = true
		} else {
			groups := prunedNodeGroups(existingLocation, ipv4s, ipv6s)
			if _, err := p.api.Locations.Update(ctx, p.projectID, setIdentifier, edgecdnx.UpdateLocationRequest{
				NodeGroups: &groups,
			}); err != nil {
				return translateAPIError("update location", err)
			}
		}
	}

	recordID := dnsEndpointResourceName(dnsName, recordType)
	for attempt := 0; attempt < 6; attempt++ {
		current, err := p.api.DNSRecords.Get(ctx, p.projectID, zone.Name, recordID)
		if edgecdnx.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before delete", err)
		}
		if !isManagedPolicy(current.Spec.RoutingPolicy) {
			return permanentError("cannot delete DNS endpoint %q: it uses Simple routing but the endpoint carries routing annotations", current.Name)
		}
		deleteRecord := false
		if current.Spec.RoutingPolicy == policyFailover && locationDeleted &&
			len(current.Spec.Targets) > 0 && current.Spec.Targets[0] == setIdentifier {
			// The failover primary is gone; the API cannot clear targets, so
			// delete the record. Remaining secondaries recreate it on their
			// next sync.
			deleteRecord = true
		} else {
			remaining, err := p.remainingFamilyLocations(ctx, labelValue, recordType)
			if err != nil {
				return err
			}
			deleteRecord = !remaining
		}
		if !deleteRecord {
			return nil
		}
		if current.ResourceVersion == "" {
			return fmt.Errorf("DNS endpoint %q has no resourceVersion", current.Name)
		}
		err = p.api.DNSRecords.DeleteWithOptions(ctx, p.projectID, zone.Name, recordID, edgecdnx.DeleteDNSRecordOptions{
			ResourceVersion: current.ResourceVersion,
		})
		if edgecdnx.IsNotFound(err) {
			return nil
		}
		if edgecdnx.IsConflict(err) {
			if err := waitRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		return translateAPIError("delete DNS endpoint", err)
	}
	return fmt.Errorf("DNS endpoint %q changed during all delete attempts", recordID)
}

// getManagedLocation reads a Location by setIdentifier and verifies ownership:
// the webhook never adopts Locations it did not create or that belong to a
// different hostname.
func (p *provider) getManagedLocation(ctx context.Context, setIdentifier, labelValue string) (v1alpha1.Location, bool, error) {
	existing, err := p.api.Locations.Get(ctx, p.projectID, setIdentifier)
	if edgecdnx.IsNotFound(err) {
		return v1alpha1.Location{}, false, nil
	}
	if err != nil {
		return v1alpha1.Location{}, false, translateAPIError("read location", err)
	}
	if !isManagedLocation(*existing) {
		return v1alpha1.Location{}, false, permanentError("location %q exists but is not managed by this webhook", setIdentifier)
	}
	if existing.Labels[endpointLabelKey] != labelValue {
		return v1alpha1.Location{}, false, permanentError("location %q is already used for a different DNS endpoint", setIdentifier)
	}
	return *existing, true, nil
}

func (p *provider) upsertLocation(ctx context.Context, existing v1alpha1.Location, exists bool, desired edgecdnx.CreateLocationRequest, labelValue string) error {
	if !exists {
		_, err := p.api.Locations.Create(ctx, p.projectID, desired)
		if err == nil {
			return nil
		}
		if !edgecdnx.IsConflict(err) {
			return translateAPIError("create location", err)
		}
		// Lost a create race; re-read, verify ownership, and fall through to update.
		current, exists, err := p.getManagedLocation(ctx, desired.Name, labelValue)
		if err != nil {
			return err
		}
		if !exists {
			return permanentError("location %q create conflicted but the location is missing", desired.Name)
		}
		existing = current
	}
	if !locationNeedsUpdate(existing, desired) {
		return nil
	}
	_, err := p.api.Locations.Update(ctx, p.projectID, desired.Name, edgecdnx.UpdateLocationRequest{
		Labels:            &desired.Labels,
		NodeGroups:        &desired.NodeGroups,
		GeoLookup:         &desired.GeoLookup,
		Weight:            edgecdnx.Ptr(desired.Weight),
		FallbackLocations: edgecdnx.Ptr(stringSliceOrEmpty(desired.FallbackLocations)),
	})
	return translateAPIError("update location", err)
}

func (p *provider) upsertHealthCheckProfile(ctx context.Context, cfg *routingConfig, setIdentifier, dnsName string) error {
	desired := desiredHealthProbes(cfg, dnsName)
	existing, err := p.api.HealthCheckProfiles.Get(ctx, p.projectID, setIdentifier)
	if edgecdnx.IsNotFound(err) {
		_, err := p.api.HealthCheckProfiles.Create(ctx, p.projectID, edgecdnx.CreateHealthCheckProfileRequest{
			Name:   setIdentifier,
			Probes: desired,
		})
		if err == nil || edgecdnx.IsConflict(err) {
			// A conflicting create means another replica won the race; the
			// profile content is deterministic, so converge via update below.
			if err == nil {
				return nil
			}
			existing, err = p.api.HealthCheckProfiles.Get(ctx, p.projectID, setIdentifier)
			if err != nil {
				return translateAPIError("read health check profile", err)
			}
		} else {
			return translateAPIError("create health check profile", err)
		}
	} else if err != nil {
		return translateAPIError("read health check profile", err)
	}
	if !profileNeedsUpdate(*existing, desired) {
		return nil
	}
	_, err = p.api.HealthCheckProfiles.Update(ctx, p.projectID, setIdentifier, edgecdnx.UpdateHealthCheckProfileRequest{
		Probes: &desired,
	})
	return translateAPIError("update health check profile", err)
}

func (p *provider) deleteHealthCheckProfile(ctx context.Context, setIdentifier string) error {
	err := p.api.HealthCheckProfiles.Delete(ctx, p.projectID, setIdentifier)
	if err != nil && !edgecdnx.IsNotFound(err) {
		return translateAPIError("delete health check profile", err)
	}
	return nil
}

// upsertManagedRecord converges the shared DNSEndpoint of a hostname. Multiple
// clusters run this concurrently for the same record, so it must be
// commutative: identical desired state is a no-op, conflicting state is a
// permanent error.
func (p *provider) upsertManagedRecord(ctx context.Context, zone apiZone, dnsName, recordType string, ttl int, cfg *routingConfig, labelValue, setIdentifier string) error {
	recordID := dnsEndpointResourceName(dnsName, recordType)
	desiredSelector := desiredRouteSelector(cfg, labelValue)
	for attempt := 0; attempt < 6; attempt++ {
		current, err := p.api.DNSRecords.Get(ctx, p.projectID, zone.Name, recordID)
		if edgecdnx.IsNotFound(err) {
			request := edgecdnx.CreateDNSRecordRequest{
				DNSName:       dnsName,
				RecordType:    recordType,
				RecordTTL:     edgecdnx.Ptr(ttl),
				RoutingPolicy: cfg.policy,
				RouteSelector: desiredSelector,
			}
			if cfg.policy == policyFailover && !cfg.secondary {
				request.Targets = []string{setIdentifier}
			}
			_, err := p.api.DNSRecords.Create(ctx, p.projectID, zone.Name, request)
			if err == nil {
				return nil
			}
			if edgecdnx.IsConflict(err) {
				// Another cluster created the record concurrently; re-read and converge.
				if err := waitRetry(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return translateAPIError("create DNS endpoint", err)
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before update", err)
		}
		if !isManagedPolicy(current.Spec.RoutingPolicy) {
			return permanentError("DNS endpoint %q already exists with Simple routing; remove the conflicting record first", current.Name)
		}
		update := edgecdnx.UpdateDNSRecordRequest{ResourceVersion: current.ResourceVersion}
		changed := false
		if current.Spec.RoutingPolicy != cfg.policy {
			// A routing policy change is only safe when no other cluster still
			// maintains a Location (and therefore annotations) for this hostname.
			sole, err := p.soleLocationForEndpoint(ctx, labelValue, setIdentifier)
			if err != nil {
				return err
			}
			if !sole {
				return permanentError("DNS endpoint %q uses routing policy %q; refusing to change it to %q while other locations serve it",
					current.Name, current.Spec.RoutingPolicy, cfg.policy)
			}
			update.RoutingPolicy = cfg.policy
			changed = true
		}
		if current.Spec.RecordTTL != ttl {
			update.RecordTTL = edgecdnx.Ptr(ttl)
			changed = true
		}
		if desiredSelector != nil && !sameRouteSelector(current.Spec.RouteSelector, desiredSelector) {
			update.RouteSelector = desiredSelector
			changed = true
		}
		if cfg.policy == policyFailover && !cfg.secondary {
			if current.Spec.RoutingPolicy == policyFailover &&
				len(current.Spec.Targets) > 0 && current.Spec.Targets[0] != setIdentifier {
				return permanentError("DNS endpoint %q already has failover primary %q; only one primary is allowed",
					current.Name, current.Spec.Targets[0])
			}
			if len(current.Spec.Targets) == 0 || current.Spec.Targets[0] != setIdentifier {
				update.Targets = []string{setIdentifier}
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if current.ResourceVersion == "" {
			return fmt.Errorf("DNS endpoint %q has no resourceVersion", current.Name)
		}
		_, err = p.api.DNSRecords.Update(ctx, p.projectID, zone.Name, recordID, update)
		if edgecdnx.IsConflict(err) {
			if err := waitRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		return translateAPIError("update DNS endpoint", err)
	}
	return fmt.Errorf("DNS endpoint %q changed during all update attempts", recordID)
}

// soleLocationForEndpoint reports whether setIdentifier is the only managed
// Location serving the hostname identified by labelValue.
func (p *provider) soleLocationForEndpoint(ctx context.Context, labelValue, setIdentifier string) (bool, error) {
	locations, err := p.api.Locations.List(ctx, p.projectID)
	if err != nil {
		return false, translateAPIError("list locations", err)
	}
	for _, location := range locations {
		if location.Name != setIdentifier && locationServesEndpoint(location, labelValue) {
			return false, nil
		}
	}
	return true, nil
}

// remainingFamilyLocations reports whether any managed Location still provides
// IPs of the record's family for the hostname identified by labelValue.
func (p *provider) remainingFamilyLocations(ctx context.Context, labelValue, recordType string) (bool, error) {
	locations, err := p.api.Locations.List(ctx, p.projectID)
	if err != nil {
		return false, translateAPIError("list locations", err)
	}
	for _, location := range locations {
		if !locationServesEndpoint(location, labelValue) {
			continue
		}
		ipv4s, ipv6s := extractNodeIPs(location)
		if recordType == "A" && len(ipv4s) > 0 {
			return true, nil
		}
		if recordType == "AAAA" && len(ipv6s) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// locationReferencesProfile reports whether any node group of the Location
// references the webhook-managed profile (named after the setIdentifier).
func locationReferencesProfile(location v1alpha1.Location, profileName string) bool {
	for _, group := range location.Spec.NodeGroups {
		if group.HealthCheck != nil && group.HealthCheck.Name == profileName {
			return true
		}
	}
	return false
}

// prunedNodeGroups rebuilds the Location's node groups with the remaining IP
// families, preserving the first group's configuration.
func prunedNodeGroups(location v1alpha1.Location, ipv4s, ipv6s []string) []v1alpha1.NodeGroupSpec {
	group := v1alpha1.NodeGroupSpec{Name: defaultNodeGroupName, Flavor: ""}
	if len(location.Spec.NodeGroups) > 0 {
		group = location.Spec.NodeGroups[0]
	}
	group.Nodes = buildNodes(ipv4s, ipv6s)
	return []v1alpha1.NodeGroupSpec{group}
}

func stringSliceOrEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
