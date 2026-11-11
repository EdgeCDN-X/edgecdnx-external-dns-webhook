package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	edgecdnx "github.com/EdgeCDN-X/edgecdnx-sdk-go"
)

type provider struct {
	api       *edgecdnx.Client
	projectID string
}

type permanentProviderError struct {
	err error
}

func (e *permanentProviderError) Error() string {
	return e.err.Error()
}

func (e *permanentProviderError) Unwrap() error {
	return e.err
}

type apiZone = v1alpha1.Zone
type apiRecord = v1alpha1.DNSEndpoint
type apiRecordSpec = v1alpha1.DNSEndpointSpec

type endpoint struct {
	DNSName          string                     `json:"dnsName"`
	Targets          []string                   `json:"targets"`
	RecordType       string                     `json:"recordType"`
	SetIdentifier    string                     `json:"setIdentifier,omitempty"`
	RecordTTL        int64                      `json:"recordTTL"`
	Labels           map[string]string          `json:"labels,omitempty"`
	ProviderSpecific []providerSpecificProperty `json:"providerSpecific,omitempty"`
}

type providerSpecificProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type changes struct {
	Create    []endpoint `json:"create"`
	UpdateOld []endpoint `json:"updateOld"`
	UpdateNew []endpoint `json:"updateNew"`
	Delete    []endpoint `json:"delete"`
}

// setIdentifierMarkerPrefix marks the extra TXT target that persists the
// setIdentifier of ExternalDNS TXT registry records. The DNSEndpoint API has
// no setIdentifier field, and ownership matching in the TXT registry requires
// the identifier to round-trip, so it is stored as an additional TXT string
// and stripped again when listing records.
const setIdentifierMarkerPrefix = "edgecdnx-set-identifier="

func newProvider(cfg config) (*provider, error) {
	if err := validateAPIConfig(cfg); err != nil {
		return nil, err
	}
	api, err := edgecdnx.NewClient(cfg.apiURL,
		edgecdnx.WithAPIKey(cfg.apiKey),
		edgecdnx.WithHTTPClient(&http.Client{
			Timeout: 10 * time.Second,
			Transport: &apiRoundTripper{
				next: http.DefaultTransport,
				key:  cfg.apiKey,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}),
	)
	if err != nil {
		return nil, err
	}
	return &provider{api: api, projectID: cfg.projectID}, nil
}

func (p *provider) zones(ctx context.Context) ([]apiZone, error) {
	zones, err := p.api.Zones.List(ctx, p.projectID)
	if err != nil {
		return nil, fmt.Errorf("fetch Project zones: %w", safeAPIError(err))
	}
	seenIDs := make(map[string]struct{}, len(zones))
	seenNames := make(map[string]struct{}, len(zones))
	for i := range zones {
		zones[i].Spec.Zone = normalizeDNSName(zones[i].Spec.Zone)
		if !validZone(zones[i].Spec.Zone) || !validResourceID(zones[i].Name) {
			return nil, errors.New("EdgeCDN-X API returned an invalid zone name or ID")
		}
		if _, exists := seenIDs[zones[i].Name]; exists {
			return nil, fmt.Errorf("EdgeCDN-X API returned duplicate zone ID %q", zones[i].Name)
		}
		if _, exists := seenNames[zones[i].Spec.Zone]; exists {
			return nil, fmt.Errorf("EdgeCDN-X API returned duplicate DNS zone %q", zones[i].Spec.Zone)
		}
		seenIDs[zones[i].Name] = struct{}{}
		seenNames[zones[i].Spec.Zone] = struct{}{}
	}
	sort.Slice(zones, func(i, j int) bool {
		return len(zones[i].Spec.Zone) > len(zones[j].Spec.Zone)
	})
	return zones, nil
}

func (p *provider) records(ctx context.Context) ([]endpoint, error) {
	zones, err := p.zones(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]endpoint, 0)
	var locations []v1alpha1.Location
	var profiles map[string]v1alpha1.HealthCheckProfile
	managedFetched := false
	for _, zone := range zones {
		apiRecords, err := p.api.DNSRecords.List(ctx, p.projectID, zone.Name)
		if err != nil {
			return nil, fmt.Errorf("fetch DNS endpoints for zone %q: %w", zone.Spec.Zone, safeAPIError(err))
		}
		for _, record := range apiRecords {
			if isManagedPolicy(record.Spec.RoutingPolicy) {
				if !dnsNameBelongsToZone(normalizeDNSName(record.Spec.DNSName), zone.Spec.Zone) {
					return nil, fmt.Errorf("DNS endpoint %q is outside zone %q", record.Name, zone.Spec.Zone)
				}
				if !managedFetched {
					locations, profiles, err = p.managedState(ctx)
					if err != nil {
						return nil, err
					}
					managedFetched = true
				}
				records = append(records, managedEndpointsFromRecord(record, locations, profiles)...)
				continue
			}
			converted, include, err := endpointFromAPI(record)
			if err != nil {
				return nil, fmt.Errorf("convert DNS endpoint %q: %w", record.Name, err)
			}
			if !include {
				continue
			}
			if !dnsNameBelongsToZone(converted.DNSName, zone.Spec.Zone) {
				return nil, fmt.Errorf("DNS endpoint %q is outside zone %q", record.Name, zone.Spec.Zone)
			}
			records = append(records, converted)
		}
	}
	return records, nil
}

// managedState fetches the project's Locations and HealthCheckProfiles once
// per listing, for reconstructing managed endpoints.
func (p *provider) managedState(ctx context.Context) ([]v1alpha1.Location, map[string]v1alpha1.HealthCheckProfile, error) {
	locations, err := p.api.Locations.List(ctx, p.projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch Project locations: %w", safeAPIError(err))
	}
	profileList, err := p.api.HealthCheckProfiles.List(ctx, p.projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch Project health check profiles: %w", safeAPIError(err))
	}
	profiles := make(map[string]v1alpha1.HealthCheckProfile, len(profileList))
	for _, profile := range profileList {
		profiles[profile.Name] = profile
	}
	return locations, profiles, nil
}

func endpointFromAPI(record apiRecord) (endpoint, bool, error) {
	spec := record.Spec
	dnsName := normalizeDNSName(spec.DNSName)
	recordType := strings.ToUpper(spec.RecordType)
	if !validDNSName(dnsName) {
		return endpoint{}, false, fmt.Errorf("invalid DNS name %q", spec.DNSName)
	}
	if !supportedRecordType(recordType) {
		return endpoint{}, false, fmt.Errorf("unsupported record type %q", spec.RecordType)
	}
	if spec.RecordTTL < 0 {
		return endpoint{}, false, fmt.Errorf("invalid TTL %d", spec.RecordTTL)
	}
	if len(spec.Targets) == 1 && strings.HasPrefix(spec.Targets[0], "LINK: ") {
		return endpoint{}, false, nil
	}
	if len(spec.Targets) == 0 {
		return endpoint{}, false, errors.New("record has no targets")
	}
	converted := endpoint{
		DNSName:    dnsName,
		Targets:    append([]string(nil), spec.Targets...),
		RecordType: recordType,
		RecordTTL:  int64(spec.RecordTTL),
	}
	if recordType == "TXT" {
		targets, setIdentifier := splitSetIdentifierMarker(converted.Targets)
		if len(targets) == 0 {
			return endpoint{}, false, errors.New("record has no targets")
		}
		converted.Targets = targets
		converted.SetIdentifier = setIdentifier
	}
	return converted, true, nil
}

// splitSetIdentifierMarker extracts the persisted setIdentifier from the
// targets of a stored TXT record.
func splitSetIdentifierMarker(targets []string) ([]string, string) {
	kept := make([]string, 0, len(targets))
	setIdentifier := ""
	for _, target := range targets {
		if value, found := strings.CutPrefix(target, setIdentifierMarkerPrefix); found {
			if setIdentifier == "" {
				setIdentifier = value
			}
			continue
		}
		kept = append(kept, target)
	}
	return kept, setIdentifier
}

func (p *provider) matchZone(zones []apiZone, dnsName string) (apiZone, error) {
	dnsName = normalizeDNSName(dnsName)
	for _, zone := range zones {
		if dnsNameBelongsToZone(dnsName, zone.Spec.Zone) {
			return zone, nil
		}
	}
	return apiZone{}, fmt.Errorf("no EdgeCDN-X Project zone covers %q", dnsName)
}

func (p *provider) apply(ctx context.Context, changes changes) error {
	if len(changes.UpdateOld) != len(changes.UpdateNew) {
		return permanentError("updateOld and updateNew endpoint counts do not match")
	}

	// Validate every endpoint's routing annotations before mutating anything,
	// so a malformed batch fails atomically with 400.
	createCfgs, err := parseConfigs(changes.Create)
	if err != nil {
		return err
	}
	updateOldCfgs, err := parseConfigs(changes.UpdateOld)
	if err != nil {
		return err
	}
	updateNewCfgs, err := parseConfigs(changes.UpdateNew)
	if err != nil {
		return err
	}
	deleteCfgs, err := parseConfigs(changes.Delete)
	if err != nil {
		return err
	}

	zones, err := p.zones(ctx)
	if err != nil {
		return err
	}

	for i, oldEndpoint := range changes.UpdateOld {
		newEndpoint := changes.UpdateNew[i]
		if sameEndpointIdentity(oldEndpoint, newEndpoint) {
			if updateNewCfgs[i].managed() {
				err = p.createManagedEndpoint(ctx, zones, newEndpoint, updateNewCfgs[i])
			} else {
				err = p.updateEndpoint(ctx, zones, newEndpoint)
			}
			if err != nil {
				return err
			}
			continue
		}
		sameKey := normalizeDNSName(oldEndpoint.DNSName) == normalizeDNSName(newEndpoint.DNSName) &&
			strings.EqualFold(oldEndpoint.RecordType, newEndpoint.RecordType)
		if sameKey && (updateOldCfgs[i].managed() || updateNewCfgs[i].managed()) {
			// The record changes identity or routing mode in place; remove the
			// old definition first so the new one does not conflict on create.
			if err := p.deleteAnyEndpoint(ctx, zones, oldEndpoint, updateOldCfgs[i]); err != nil {
				return err
			}
			if err := p.createAnyEndpoint(ctx, zones, newEndpoint, updateNewCfgs[i]); err != nil {
				return err
			}
			continue
		}
		if err := p.createAnyEndpoint(ctx, zones, newEndpoint, updateNewCfgs[i]); err != nil {
			return err
		}
		if err := p.deleteAnyEndpoint(ctx, zones, oldEndpoint, updateOldCfgs[i]); err != nil {
			return err
		}
	}
	for i, record := range changes.Create {
		if err := p.createAnyEndpoint(ctx, zones, record, createCfgs[i]); err != nil {
			return err
		}
	}
	for i, record := range changes.Delete {
		if err := p.deleteAnyEndpoint(ctx, zones, record, deleteCfgs[i]); err != nil {
			return err
		}
	}
	return nil
}

func parseConfigs(endpoints []endpoint) ([]*routingConfig, error) {
	configs := make([]*routingConfig, len(endpoints))
	for i := range endpoints {
		cfg, err := parseRoutingConfig(endpoints[i])
		if err != nil {
			return nil, err
		}
		configs[i] = cfg
	}
	return configs, nil
}

func (p *provider) createAnyEndpoint(ctx context.Context, zones []apiZone, record endpoint, cfg *routingConfig) error {
	if cfg.managed() {
		return p.createManagedEndpoint(ctx, zones, record, cfg)
	}
	return p.createEndpoint(ctx, zones, record)
}

func (p *provider) deleteAnyEndpoint(ctx context.Context, zones []apiZone, record endpoint, cfg *routingConfig) error {
	if cfg.managed() {
		return p.deleteManagedEndpoint(ctx, zones, record, cfg)
	}
	return p.deleteEndpoint(ctx, zones, record)
}

func (p *provider) createEndpoint(ctx context.Context, zones []apiZone, record endpoint) error {
	zone, apiRecord, err := p.toAPIRecord(zones, record)
	if err != nil {
		return err
	}
	_, err = p.api.DNSRecords.Create(ctx, p.projectID, zone.Name, edgecdnx.CreateDNSRecordRequest{
		DNSName:       apiRecord.DNSName,
		RoutingPolicy: apiRecord.RoutingPolicy,
		RecordTTL:     edgecdnx.Ptr(apiRecord.RecordTTL),
		RecordType:    apiRecord.RecordType,
		Targets:       apiRecord.Targets,
	})
	if edgecdnx.IsConflict(err) {
		existing, getErr := p.getRecord(ctx, zone, record)
		if getErr == nil && sameRecord(existing.Spec, apiRecord) {
			return nil
		}
		if getErr != nil {
			if edgecdnx.IsNotFound(getErr) {
				return translateAPIError("create DNS endpoint", err)
			}
			return translateAPIError("read existing DNS endpoint after create conflict", getErr)
		}
	}
	return translateAPIError("create DNS endpoint", err)
}

func (p *provider) updateEndpoint(ctx context.Context, zones []apiZone, desired endpoint) error {
	zone, desiredRecord, err := p.toAPIRecord(zones, desired)
	if err != nil {
		return err
	}
	recordID := dnsEndpointResourceName(desiredRecord.DNSName, desiredRecord.RecordType)
	for attempt := 0; attempt < 6; attempt++ {
		current, err := p.api.DNSRecords.Get(ctx, p.projectID, zone.Name, recordID)
		if edgecdnx.IsNotFound(err) {
			return p.createEndpoint(ctx, zones, desired)
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before update", err)
		}
		if current.Spec.RoutingPolicy != "" && current.Spec.RoutingPolicy != "Simple" {
			return permanentError("cannot update DNS endpoint %q with routing policy %q through ExternalDNS", current.Name, current.Spec.RoutingPolicy)
		}
		if sameRecord(current.Spec, desiredRecord) {
			return nil
		}
		if current.ResourceVersion == "" {
			return fmt.Errorf("DNS endpoint %q has no resourceVersion", current.Name)
		}
		_, err = p.api.DNSRecords.Update(ctx, p.projectID, zone.Name, recordID, edgecdnx.UpdateDNSRecordRequest{
			ResourceVersion: current.ResourceVersion,
			RecordTTL:       edgecdnx.Ptr(desiredRecord.RecordTTL),
			Targets:         desiredRecord.Targets,
		})
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

func (p *provider) deleteEndpoint(ctx context.Context, zones []apiZone, record endpoint) error {
	zone, recordValue, err := p.toAPIRecord(zones, record)
	if err != nil {
		return err
	}
	recordID := dnsEndpointResourceName(recordValue.DNSName, recordValue.RecordType)
	for attempt := 0; attempt < 6; attempt++ {
		current, err := p.api.DNSRecords.Get(ctx, p.projectID, zone.Name, recordID)
		if edgecdnx.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before delete", err)
		}
		if current.Spec.RoutingPolicy != "" && current.Spec.RoutingPolicy != "Simple" {
			return permanentError("cannot delete DNS endpoint %q with routing policy %q through ExternalDNS", current.Name, current.Spec.RoutingPolicy)
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

func (p *provider) getRecord(ctx context.Context, zone apiZone, record endpoint) (apiRecord, error) {
	recordID := dnsEndpointResourceName(normalizeDNSName(record.DNSName), strings.ToUpper(record.RecordType))
	current, err := p.api.DNSRecords.Get(ctx, p.projectID, zone.Name, recordID)
	if err != nil {
		return apiRecord{}, err
	}
	return *current, nil
}

func (p *provider) toAPIRecord(zones []apiZone, record endpoint) (apiZone, apiRecordSpec, error) {
	dnsName := normalizeDNSName(record.DNSName)
	recordType := strings.ToUpper(record.RecordType)
	if !validDNSName(dnsName) {
		return apiZone{}, apiRecordSpec{}, permanentError("invalid DNS name %q", record.DNSName)
	}
	if !supportedRecordType(recordType) {
		return apiZone{}, apiRecordSpec{}, permanentError("unsupported record type %q", record.RecordType)
	}
	if len(record.Targets) == 0 {
		return apiZone{}, apiRecordSpec{}, permanentError("DNS endpoint %q must have at least one target", dnsName)
	}
	for _, target := range record.Targets {
		if target == "" {
			return apiZone{}, apiRecordSpec{}, permanentError("DNS endpoint %q has an empty target", dnsName)
		}
	}
	targets := append([]string(nil), record.Targets...)
	if record.SetIdentifier != "" {
		if recordType != "TXT" {
			return apiZone{}, apiRecordSpec{}, permanentError("setIdentifier is not supported for %s DNS endpoint %q", recordType, dnsName)
		}
		if !validDNSSubdomain(record.SetIdentifier) {
			return apiZone{}, apiRecordSpec{}, permanentError("setIdentifier %q of DNS endpoint %q must be a valid DNS-1123 subdomain", record.SetIdentifier, dnsName)
		}
		// Persist the TXT registry record's setIdentifier as an extra target.
		targets = append(targets, setIdentifierMarkerPrefix+record.SetIdentifier)
	}
	zone, err := p.matchZone(zones, dnsName)
	if err != nil {
		return apiZone{}, apiRecordSpec{}, &permanentProviderError{err: err}
	}
	ttl := record.RecordTTL
	if ttl < 0 || int64(int(ttl)) != ttl {
		return apiZone{}, apiRecordSpec{}, permanentError("invalid TTL %d for DNS endpoint %q", ttl, dnsName)
	}
	return zone, apiRecordSpec{
		DNSName:       dnsName,
		RecordType:    recordType,
		RecordTTL:     int(ttl),
		RoutingPolicy: "Simple",
		Targets:       targets,
	}, nil
}

func translateAPIError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if status, ok := apiErrorStatus(err); ok {
		return fmt.Errorf("%s: %w", operation, &apiStatusError{status: status})
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func safeAPIError(err error) error {
	if status, ok := apiErrorStatus(err); ok {
		return &apiStatusError{status: status}
	}
	return err
}

type apiStatusError struct {
	status int
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("EdgeCDN-X API returned HTTP %d", e.status)
}

func apiErrorStatus(err error) (int, bool) {
	var safeErr *apiStatusError
	if errors.As(err, &safeErr) {
		return safeErr.status, true
	}
	var apiErr *edgecdnx.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	return 0, false
}

func permanentError(message string, args ...any) error {
	return &permanentProviderError{err: fmt.Errorf(message, args...)}
}

func sameRecord(current, desired apiRecordSpec) bool {
	if current.RecordTTL != desired.RecordTTL ||
		(current.RoutingPolicy != "" && current.RoutingPolicy != desired.RoutingPolicy) ||
		normalizeDNSName(current.DNSName) != normalizeDNSName(desired.DNSName) ||
		strings.ToUpper(current.RecordType) != desired.RecordType ||
		len(current.Targets) != len(desired.Targets) {
		return false
	}
	for i := range current.Targets {
		if current.Targets[i] != desired.Targets[i] {
			return false
		}
	}
	return true
}

func sameEndpointIdentity(a, b endpoint) bool {
	return normalizeDNSName(a.DNSName) == normalizeDNSName(b.DNSName) &&
		strings.EqualFold(a.RecordType, b.RecordType) &&
		a.SetIdentifier == b.SetIdentifier
}

func waitRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<attempt) * 10 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validResourceID(id string) bool {
	return validDNSSubdomain(id)
}

func validZone(name string) bool {
	return validDNSSubdomain(name)
}

func validDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	for index, label := range labels {
		if index == 0 && label == "*" && len(labels) > 1 {
			continue
		}
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

func supportedRecordType(recordType string) bool {
	switch recordType {
	case "A", "AAAA", "CNAME", "TXT", "MX", "SRV", "NS":
		return true
	default:
		return false
	}
}

func normalizeDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func dnsNameBelongsToZone(dnsName, zoneName string) bool {
	dnsName = normalizeDNSName(dnsName)
	zoneName = normalizeDNSName(zoneName)
	return dnsName == zoneName || strings.HasSuffix(dnsName, "."+zoneName)
}

func dnsEndpointResourceName(dnsName, recordType string) string {
	name := normalizeDNSName(dnsName) + "-" + strings.ToLower(recordType)
	if validDNSSubdomain(name) {
		return name
	}
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("dnsendpoint-%x", digest[:12])
}

func validDNSSubdomain(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}
