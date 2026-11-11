package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type provider struct {
	api       *apiClient
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

type apiMetadata struct {
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion"`
}

type apiZone struct {
	Metadata apiMetadata `json:"metadata"`
	Spec     struct {
		Zone string `json:"zone"`
	} `json:"spec"`
}

type apiRecordSpec struct {
	DNSName       string   `json:"dnsName"`
	RoutingPolicy string   `json:"routingPolicy"`
	RecordTTL     int      `json:"recordTTL"`
	RecordType    string   `json:"recordType"`
	Targets       []string `json:"targets"`
}

type apiRecord struct {
	Metadata apiMetadata   `json:"metadata"`
	Spec     apiRecordSpec `json:"spec"`
}

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

func newProvider(cfg config) (*provider, error) {
	api, err := newAPIClient(cfg)
	if err != nil {
		return nil, err
	}
	return &provider{api: api, projectID: cfg.projectID}, nil
}

func (p *provider) zones(ctx context.Context) ([]apiZone, error) {
	var zones []apiZone
	if err := p.api.request(ctx, http.MethodGet, p.api.zonesPath(p.projectID), nil, &zones); err != nil {
		return nil, fmt.Errorf("fetch Project zones: %w", err)
	}
	seenIDs := make(map[string]struct{}, len(zones))
	seenNames := make(map[string]struct{}, len(zones))
	for i := range zones {
		zones[i].Spec.Zone = normalizeDNSName(zones[i].Spec.Zone)
		if !validZone(zones[i].Spec.Zone) || !validResourceID(zones[i].Metadata.Name) {
			return nil, errors.New("EdgeCDN-X API returned an invalid zone name or ID")
		}
		if _, exists := seenIDs[zones[i].Metadata.Name]; exists {
			return nil, fmt.Errorf("EdgeCDN-X API returned duplicate zone ID %q", zones[i].Metadata.Name)
		}
		if _, exists := seenNames[zones[i].Spec.Zone]; exists {
			return nil, fmt.Errorf("EdgeCDN-X API returned duplicate DNS zone %q", zones[i].Spec.Zone)
		}
		seenIDs[zones[i].Metadata.Name] = struct{}{}
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
	for _, zone := range zones {
		var apiRecords []apiRecord
		if err := p.api.request(ctx, http.MethodGet, p.api.endpointsPath(p.projectID, zone.Metadata.Name), nil, &apiRecords); err != nil {
			return nil, fmt.Errorf("fetch DNS endpoints for zone %q: %w", zone.Spec.Zone, err)
		}
		for _, record := range apiRecords {
			converted, include, err := endpointFromAPI(record)
			if err != nil {
				return nil, fmt.Errorf("convert DNS endpoint %q: %w", record.Metadata.Name, err)
			}
			if !include {
				continue
			}
			if !dnsNameBelongsToZone(converted.DNSName, zone.Spec.Zone) {
				return nil, fmt.Errorf("DNS endpoint %q is outside zone %q", record.Metadata.Name, zone.Spec.Zone)
			}
			records = append(records, converted)
		}
	}
	return records, nil
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
	return endpoint{
		DNSName:    dnsName,
		Targets:    append([]string(nil), spec.Targets...),
		RecordType: recordType,
		RecordTTL:  int64(spec.RecordTTL),
	}, true, nil
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

	zones, err := p.zones(ctx)
	if err != nil {
		return err
	}

	for i, oldEndpoint := range changes.UpdateOld {
		newEndpoint := changes.UpdateNew[i]
		if sameEndpointIdentity(oldEndpoint, newEndpoint) {
			err = p.updateEndpoint(ctx, zones, newEndpoint)
			if err != nil {
				return err
			}
			continue
		}
		if err := p.createEndpoint(ctx, zones, newEndpoint); err != nil {
			return err
		}
		if err := p.deleteEndpoint(ctx, zones, oldEndpoint); err != nil {
			return err
		}
	}
	for _, record := range changes.Create {
		if err := p.createEndpoint(ctx, zones, record); err != nil {
			return err
		}
	}
	for _, record := range changes.Delete {
		if err := p.deleteEndpoint(ctx, zones, record); err != nil {
			return err
		}
	}
	return nil
}

func (p *provider) createEndpoint(ctx context.Context, zones []apiZone, record endpoint) error {
	zone, apiRecord, err := p.toAPIRecord(zones, record)
	if err != nil {
		return err
	}
	err = p.api.request(ctx, http.MethodPost, p.api.endpointsPath(p.projectID, zone.Metadata.Name), apiRecord, nil)
	if hasAPIStatus(err, http.StatusConflict) {
		existing, getErr := p.getRecord(ctx, zone, record)
		if getErr == nil && sameRecord(existing.Spec, apiRecord) {
			return nil
		}
		if getErr != nil {
			if hasAPIStatus(getErr, http.StatusNotFound) {
				return translateAPIError("create DNS endpoint", err)
			}
			return getErr
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
	recordPath := p.api.endpointPath(p.projectID, zone.Metadata.Name, recordID)
	for attempt := 0; attempt < 6; attempt++ {
		var current apiRecord
		err := p.api.request(ctx, http.MethodGet, recordPath, nil, &current)
		if hasAPIStatus(err, http.StatusNotFound) {
			return p.createEndpoint(ctx, zones, desired)
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before update", err)
		}
		if current.Spec.RoutingPolicy != "" && current.Spec.RoutingPolicy != "Simple" {
			return permanentError("cannot update DNS endpoint %q with routing policy %q through ExternalDNS", current.Metadata.Name, current.Spec.RoutingPolicy)
		}
		if sameRecord(current.Spec, desiredRecord) {
			return nil
		}
		if current.Metadata.ResourceVersion == "" {
			return fmt.Errorf("DNS endpoint %q has no resourceVersion", current.Metadata.Name)
		}
		payload := struct {
			ResourceVersion string   `json:"resourceVersion"`
			RecordTTL       int      `json:"recordTTL"`
			Targets         []string `json:"targets"`
		}{
			ResourceVersion: current.Metadata.ResourceVersion,
			RecordTTL:       desiredRecord.RecordTTL,
			Targets:         desiredRecord.Targets,
		}
		err = p.api.request(ctx, http.MethodPatch, recordPath, payload, nil)
		if hasAPIStatus(err, http.StatusConflict) {
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
	recordPath := p.api.endpointPath(p.projectID, zone.Metadata.Name, recordID)
	for attempt := 0; attempt < 6; attempt++ {
		var current apiRecord
		err := p.api.request(ctx, http.MethodGet, recordPath, nil, &current)
		if hasAPIStatus(err, http.StatusNotFound) {
			return nil
		}
		if err != nil {
			return translateAPIError("read DNS endpoint before delete", err)
		}
		if current.Spec.RoutingPolicy != "" && current.Spec.RoutingPolicy != "Simple" {
			return permanentError("cannot delete DNS endpoint %q with routing policy %q through ExternalDNS", current.Metadata.Name, current.Spec.RoutingPolicy)
		}
		if current.Metadata.ResourceVersion == "" {
			return fmt.Errorf("DNS endpoint %q has no resourceVersion", current.Metadata.Name)
		}
		deleteURL := recordPath + "?resourceVersion=" + url.QueryEscape(current.Metadata.ResourceVersion)
		err = p.api.request(ctx, http.MethodDelete, deleteURL, nil, nil)
		if hasAPIStatus(err, http.StatusNotFound) {
			return nil
		}
		if hasAPIStatus(err, http.StatusConflict) {
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
	var current apiRecord
	recordID := dnsEndpointResourceName(normalizeDNSName(record.DNSName), strings.ToUpper(record.RecordType))
	err := p.api.request(ctx, http.MethodGet, p.api.endpointPath(p.projectID, zone.Metadata.Name, recordID), nil, &current)
	return current, err
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
	if record.SetIdentifier != "" {
		return apiZone{}, apiRecordSpec{}, permanentError("setIdentifier is not supported for DNS endpoint %q", dnsName)
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
		Targets:       append([]string(nil), record.Targets...),
	}, nil
}

func translateAPIError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s: EdgeCDN-X API returned HTTP %d", operation, apiErr.status)
	}
	return fmt.Errorf("%s: %w", operation, err)
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
