package main

import (
	"net"
	"strconv"
	"strings"
)

// API routing policy names (DNSEndpoint.spec.routingPolicy).
const (
	policySimple      = "Simple"
	policyGeolocation = "Geolocation"
	policyWeighted    = "Weighted"
	policyRoundRobin  = "RoundRobin"
	policyFailover    = "Failover"
)

// providerSpecific property names. ExternalDNS translates the
// external-dns.alpha.kubernetes.io/webhook-<name> annotation into a
// providerSpecific property named webhook/<name>.
const (
	propPrefix       = "webhook/edgecdnx-"
	propRouting      = "webhook/edgecdnx-routing"
	propWeight       = "webhook/edgecdnx-weight"
	propContinents   = "webhook/edgecdnx-continents"
	propCountries    = "webhook/edgecdnx-countries"
	propFallback     = "webhook/edgecdnx-fallback"
	propFailoverRole = "webhook/edgecdnx-failover-role"
	propHealthPath   = "webhook/edgecdnx-health-endpoint"
	propHealthProto  = "webhook/edgecdnx-health-protocol"
	propHealthPort   = "webhook/edgecdnx-health-port"
)

const (
	failoverRolePrimary   = "primary"
	failoverRoleSecondary = "secondary"

	maxLocationWeight = 1000
)

// annotationPolicies maps annotation values to API routing policies.
var annotationPolicies = map[string]string{
	"simple":   policySimple,
	"geo":      policyGeolocation,
	"wrr":      policyWeighted,
	"rr":       policyRoundRobin,
	"failover": policyFailover,
}

// policyAnnotations is the reverse of annotationPolicies.
var policyAnnotations = map[string]string{
	policySimple:      "simple",
	policyGeolocation: "geo",
	policyWeighted:    "wrr",
	policyRoundRobin:  "rr",
	policyFailover:    "failover",
}

// routingConfig is the parsed, validated form of the webhook/edgecdnx-*
// providerSpecific properties of one endpoint.
type routingConfig struct {
	policy      string // API policy name (Simple, Geolocation, ...)
	weight      int32  // Weighted
	continents  []string
	countries   []string
	fallback    string // Failover
	secondary   bool   // Failover role
	healthPath  string
	healthProto string
	healthPort  int32
}

// managed reports whether the endpoint requires Location/profile management.
func (c *routingConfig) managed() bool {
	return c != nil && c.policy != policySimple
}

func (c *routingConfig) hasHealth() bool {
	return c != nil && c.healthPath != ""
}

// parseRoutingConfig parses and validates the routing annotations of an
// endpoint. All errors are permanent (HTTP 400) errors.
func parseRoutingConfig(record endpoint) (*routingConfig, error) {
	dnsName := normalizeDNSName(record.DNSName)
	if strings.ToUpper(record.RecordType) == "TXT" {
		// ExternalDNS TXT registry records inherit the setIdentifier and
		// providerSpecific properties of the endpoint they track. They are
		// always stored as Simple records; the annotations are ignored.
		if record.SetIdentifier != "" && !validDNSSubdomain(record.SetIdentifier) {
			return nil, permanentError("set-identifier %q on endpoint %q must be a valid DNS-1123 subdomain", record.SetIdentifier, dnsName)
		}
		return &routingConfig{policy: policySimple}, nil
	}
	props := make(map[string]string)
	for _, prop := range record.ProviderSpecific {
		if !strings.HasPrefix(prop.Name, propPrefix) {
			continue
		}
		if _, exists := props[prop.Name]; exists {
			return nil, permanentError("duplicate property %q on endpoint %q", prop.Name, dnsName)
		}
		props[prop.Name] = prop.Value
	}

	cfg := &routingConfig{policy: policySimple}
	if rawRouting, exists := props[propRouting]; exists {
		policy, known := annotationPolicies[strings.ToLower(strings.TrimSpace(rawRouting))]
		if !known {
			return nil, permanentError("invalid routing %q on endpoint %q: must be one of simple, geo, wrr, rr, failover", rawRouting, dnsName)
		}
		cfg.policy = policy
	}
	delete(props, propRouting)

	if cfg.policy == policySimple {
		if record.SetIdentifier != "" {
			return nil, permanentError("setIdentifier requires a non-simple edgecdnx-routing annotation on endpoint %q", dnsName)
		}
		for name := range props {
			return nil, permanentError("property %q is not allowed with simple routing on endpoint %q", name, dnsName)
		}
		return cfg, nil
	}

	if record.SetIdentifier == "" {
		return nil, permanentError("set-identifier is required for %s routing on endpoint %q", policyAnnotations[cfg.policy], dnsName)
	}
	if !validDNSSubdomain(record.SetIdentifier) {
		return nil, permanentError("set-identifier %q on endpoint %q must be a valid DNS-1123 subdomain", record.SetIdentifier, dnsName)
	}
	recordType := strings.ToUpper(record.RecordType)
	if recordType != "A" && recordType != "AAAA" {
		return nil, permanentError("%s routing is only supported for A and AAAA records, got %q on endpoint %q", policyAnnotations[cfg.policy], record.RecordType, dnsName)
	}
	for _, target := range record.Targets {
		if !validIPForRecordType(target, recordType) {
			return nil, permanentError("target %q of endpoint %q is not a valid %s address", target, dnsName, recordType)
		}
	}

	var err error
	switch cfg.policy {
	case policyWeighted:
		err = cfg.parseWeighted(props, dnsName)
	case policyGeolocation:
		err = cfg.parseGeolocation(props, dnsName)
	case policyFailover:
		err = cfg.parseFailover(props, dnsName)
	case policyRoundRobin:
		err = rejectProps(props, dnsName, "rr", propWeight, propContinents, propCountries, propFallback, propFailoverRole)
	}
	if err != nil {
		return nil, err
	}
	for _, name := range []string{propWeight, propContinents, propCountries, propFallback, propFailoverRole} {
		delete(props, name)
	}

	if err := cfg.parseHealth(props, dnsName); err != nil {
		return nil, err
	}
	for _, name := range []string{propHealthPath, propHealthProto, propHealthPort} {
		delete(props, name)
	}

	for name := range props {
		return nil, permanentError("unknown property %q on endpoint %q", name, dnsName)
	}
	return cfg, nil
}

func (c *routingConfig) parseWeighted(props map[string]string, dnsName string) error {
	if err := rejectProps(props, dnsName, "wrr", propContinents, propCountries, propFallback, propFailoverRole); err != nil {
		return err
	}
	rawWeight, exists := props[propWeight]
	if !exists {
		return permanentError("weight is required for wrr routing on endpoint %q", dnsName)
	}
	weight, err := strconv.Atoi(strings.TrimSpace(rawWeight))
	if err != nil || weight < 0 || weight > maxLocationWeight {
		return permanentError("weight %q on endpoint %q must be an integer between 0 and %d", rawWeight, dnsName, maxLocationWeight)
	}
	c.weight = int32(weight)
	return nil
}

func (c *routingConfig) parseGeolocation(props map[string]string, dnsName string) error {
	if err := rejectProps(props, dnsName, "geo", propWeight, propFallback, propFailoverRole); err != nil {
		return err
	}
	var err error
	if c.continents, err = parseCodeList(props[propContinents], "continent", dnsName); err != nil {
		return err
	}
	if c.countries, err = parseCodeList(props[propCountries], "country", dnsName); err != nil {
		return err
	}
	if len(c.continents) == 0 && len(c.countries) == 0 {
		return permanentError("geo routing on endpoint %q requires continents and/or countries", dnsName)
	}
	return nil
}

func (c *routingConfig) parseFailover(props map[string]string, dnsName string) error {
	if err := rejectProps(props, dnsName, "failover", propWeight, propContinents, propCountries); err != nil {
		return err
	}
	role := failoverRolePrimary
	if rawRole, exists := props[propFailoverRole]; exists {
		role = strings.ToLower(strings.TrimSpace(rawRole))
	}
	switch role {
	case failoverRolePrimary:
	case failoverRoleSecondary:
		c.secondary = true
	default:
		return permanentError("failover-role on endpoint %q must be primary or secondary", dnsName)
	}
	fallback, exists := props[propFallback]
	if !exists {
		if !c.secondary {
			return permanentError("fallback is required for primary failover routing on endpoint %q", dnsName)
		}
		return nil
	}
	fallback = strings.TrimSpace(fallback)
	if !validDNSSubdomain(fallback) {
		return permanentError("fallback %q on endpoint %q must be a valid DNS-1123 subdomain", fallback, dnsName)
	}
	c.fallback = fallback
	return nil
}

func (c *routingConfig) parseHealth(props map[string]string, dnsName string) error {
	path, exists := props[propHealthPath]
	if !exists {
		if err := rejectProps(props, dnsName, "routing without health-endpoint", propHealthProto, propHealthPort); err != nil {
			return err
		}
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return permanentError("health-endpoint %q on endpoint %q must be a request path starting with /", path, dnsName)
	}
	c.healthPath = path

	c.healthProto = "http"
	if rawProto, exists := props[propHealthProto]; exists {
		proto := strings.ToLower(strings.TrimSpace(rawProto))
		if proto != "http" && proto != "https" {
			return permanentError("health-protocol %q on endpoint %q must be http or https", rawProto, dnsName)
		}
		c.healthProto = proto
	}

	c.healthPort = 80
	if c.healthProto == "https" {
		c.healthPort = 443
	}
	if rawPort, exists := props[propHealthPort]; exists {
		port, err := strconv.Atoi(strings.TrimSpace(rawPort))
		if err != nil || port < 1 || port > 65535 {
			return permanentError("health-port %q on endpoint %q must be an integer between 1 and 65535", rawPort, dnsName)
		}
		c.healthPort = int32(port)
	}
	return nil
}

func rejectProps(props map[string]string, dnsName, policy string, names ...string) error {
	for _, name := range names {
		if _, exists := props[name]; exists {
			return permanentError("property %q is not allowed with %s routing on endpoint %q", name, policy, dnsName)
		}
	}
	return nil
}

func parseCodeList(raw, kind, dnsName string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	codes := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		code := strings.ToUpper(strings.TrimSpace(part))
		if len(code) != 2 || !isAlpha(code) {
			return nil, permanentError("invalid %s code %q on endpoint %q", kind, part, dnsName)
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
}

func isAlpha(value string) bool {
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func validIPForRecordType(target, recordType string) bool {
	ip := net.ParseIP(target)
	if ip == nil {
		return false
	}
	if recordType == "A" {
		return ip.To4() != nil
	}
	return ip.To4() == nil
}

// normalizeIP returns the canonical textual form of an IP address so stored
// node IPs always compare equal to ExternalDNS targets.
func normalizeIP(target string) string {
	if ip := net.ParseIP(target); ip != nil {
		return ip.String()
	}
	return target
}

// serializeProviderSpecific renders the canonical providerSpecific properties
// for a managed routing configuration. The output must round-trip through
// parseRoutingConfig so ExternalDNS never sees a permanent diff.
func serializeProviderSpecific(cfg *routingConfig) []providerSpecificProperty {
	if !cfg.managed() {
		return nil
	}
	props := []providerSpecificProperty{{Name: propRouting, Value: policyAnnotations[cfg.policy]}}
	switch cfg.policy {
	case policyWeighted:
		props = append(props, providerSpecificProperty{Name: propWeight, Value: strconv.Itoa(int(cfg.weight))})
	case policyGeolocation:
		if len(cfg.continents) > 0 {
			props = append(props, providerSpecificProperty{Name: propContinents, Value: strings.Join(cfg.continents, ",")})
		}
		if len(cfg.countries) > 0 {
			props = append(props, providerSpecificProperty{Name: propCountries, Value: strings.Join(cfg.countries, ",")})
		}
	case policyFailover:
		if cfg.secondary {
			props = append(props, providerSpecificProperty{Name: propFailoverRole, Value: failoverRoleSecondary})
		}
		if cfg.fallback != "" {
			props = append(props, providerSpecificProperty{Name: propFallback, Value: cfg.fallback})
		}
	}
	if cfg.hasHealth() {
		props = append(props,
			providerSpecificProperty{Name: propHealthPath, Value: cfg.healthPath},
			providerSpecificProperty{Name: propHealthProto, Value: cfg.healthProto},
			providerSpecificProperty{Name: propHealthPort, Value: strconv.Itoa(int(cfg.healthPort))},
		)
	}
	return props
}

// canonicalizeEndpoint validates an endpoint's routing properties and rewrites
// them into their canonical form (defaults filled in, codes uppercased) so the
// desired state ExternalDNS computes matches the records the webhook returns.
func canonicalizeEndpoint(record endpoint) (endpoint, error) {
	cfg, err := parseRoutingConfig(record)
	if err != nil {
		return endpoint{}, err
	}
	kept := make([]providerSpecificProperty, 0, len(record.ProviderSpecific))
	for _, prop := range record.ProviderSpecific {
		if !strings.HasPrefix(prop.Name, propPrefix) {
			kept = append(kept, prop)
		}
	}
	kept = append(kept, serializeProviderSpecific(cfg)...)
	if len(kept) == 0 {
		kept = nil
	}
	record.ProviderSpecific = kept
	if cfg.managed() {
		targets := make([]string, 0, len(record.Targets))
		for _, target := range record.Targets {
			targets = append(targets, normalizeIP(target))
		}
		record.Targets = targets
	}
	return record, nil
}
