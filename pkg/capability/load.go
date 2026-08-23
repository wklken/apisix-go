package capability

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	expectedTargetName    = "apisix-3.17"
	expectedTargetVersion = "3.17.0"
	expectedSourceCommit  = "9ef2ecab67f652d38365049613610ef649bb4ad0"
	expectedTargetImage   = "apache/apisix:3.17.0"
)

//go:embed manifest.yaml
var manifestYAML []byte

func Load() (*Manifest, error) { return Parse(manifestYAML) }

func Parse(data []byte) (*Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode manifest: multiple YAML documents are not supported")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return &manifest, nil
}

func (m *Manifest) Plugin(name string) (PluginCapability, bool) {
	if m == nil {
		return PluginCapability{}, false
	}
	index, ok := m.pluginsByName[name]
	if !ok {
		return PluginCapability{}, false
	}
	if index < 0 || index >= len(m.Plugins) {
		return PluginCapability{}, false
	}
	return clonePlugin(m.Plugins[index]), true
}

func (m *Manifest) Qualification(name string) (QualificationProfile, bool) {
	if m == nil {
		return QualificationProfile{}, false
	}
	index, ok := m.profilesByName[name]
	if !ok {
		return QualificationProfile{}, false
	}
	if index < 0 || index >= len(m.QualificationProfiles) {
		return QualificationProfile{}, false
	}
	return cloneQualification(m.QualificationProfiles[index]), true
}

func (m *Manifest) QualifiedPlugins(profile string) []string {
	if m == nil {
		return nil
	}
	qualification, ok := m.Qualification(profile)
	if !ok {
		return nil
	}

	qualified := make([]string, 0, len(qualification.RequiredPlugins))
	for _, key := range qualification.RequiredPlugins {
		plugin, ok := m.Plugin(key)
		if !ok || plugin.Namespace != NamespaceAPISIX || plugin.Behavior != BehaviorFull {
			continue
		}
		if !supportsDomain(plugin.Domains, qualification.Domains) {
			continue
		}
		if !hasRequiredEvidence(plugin.Evidence, qualification.RequiredEvidence) {
			continue
		}
		qualified = append(qualified, key)
	}
	return qualified
}

func (m *Manifest) validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("schema_version %d is unsupported; want 1", m.SchemaVersion)
	}
	if m.Target.Name != expectedTargetName ||
		m.Target.Version != expectedTargetVersion ||
		m.Target.SourceCommit != expectedSourceCommit ||
		m.Target.Image != expectedTargetImage {
		return fmt.Errorf(
			"target must be %s %s at %s with image %s",
			expectedTargetName, expectedTargetVersion, expectedSourceCommit, expectedTargetImage,
		)
	}

	divergenceIDs, err := validateDivergences(m.Divergences)
	if err != nil {
		return err
	}

	m.pluginsByName = make(map[string]int, len(m.Plugins))
	factoryByKey := make(map[string]int)
	for index, plugin := range m.Plugins {
		if strings.TrimSpace(plugin.Name) == "" {
			return fmt.Errorf("plugins[%d]: name must not be blank", index)
		}
		if _, exists := m.pluginsByName[plugin.Name]; exists {
			return fmt.Errorf("duplicate plugin id %q", plugin.Name)
		}
		m.pluginsByName[plugin.Name] = index
		if err := validatePlugin(plugin); err != nil {
			return fmt.Errorf("plugin %q: %w", plugin.Name, err)
		}
		for _, id := range plugin.DivergenceIDs {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("plugin %q: divergence id must not be blank", plugin.Name)
			}
			if _, exists := divergenceIDs[id]; !exists {
				return fmt.Errorf("plugin %q: divergence %q is absent from the top-level ledger", plugin.Name, id)
			}
		}
		if duplicateStrings(plugin.DivergenceIDs) {
			return fmt.Errorf("plugin %q: duplicate divergence id", plugin.Name)
		}
		for _, factory := range plugin.Factories {
			if _, exists := factoryByKey[factory.Key]; exists {
				return fmt.Errorf("duplicate factory id %q", factory.Key)
			}
			if other, exists := m.pluginsByName[factory.Key]; exists && other != index {
				return fmt.Errorf("factory id %q collides with plugin id", factory.Key)
			}
			factoryByKey[factory.Key] = index
			m.pluginsByName[factory.Key] = index
		}
	}

	m.profilesByName = make(map[string]int, len(m.QualificationProfiles))
	for index, profile := range m.QualificationProfiles {
		if strings.TrimSpace(profile.Name) == "" {
			return fmt.Errorf("qualification_profiles[%d]: name must not be blank", index)
		}
		if _, exists := m.profilesByName[profile.Name]; exists {
			return fmt.Errorf("duplicate profile id %q", profile.Name)
		}
		m.profilesByName[profile.Name] = index
		if err := validateQualification(profile, factoryByKey); err != nil {
			return fmt.Errorf("qualification profile %q: %w", profile.Name, err)
		}
	}
	return nil
}

func validateDivergences(divergences []Divergence) (map[string]struct{}, error) {
	ids := make(map[string]struct{}, len(divergences))
	for index, divergence := range divergences {
		if strings.TrimSpace(divergence.ID) == "" {
			return nil, fmt.Errorf("divergences[%d]: id must not be blank", index)
		}
		if _, exists := ids[divergence.ID]; exists {
			return nil, fmt.Errorf("duplicate divergence id %q", divergence.ID)
		}
		ids[divergence.ID] = struct{}{}
		if !validDivergenceStatus(divergence.Status) {
			return nil, fmt.Errorf("divergence %q: unknown divergence status %q", divergence.ID, divergence.Status)
		}
		if divergence.Status == DivergenceAccepted {
			if strings.TrimSpace(divergence.ADR) == "" || strings.TrimSpace(divergence.OwnerApprovalRef) == "" {
				return nil, fmt.Errorf("accepted divergence %q requires adr and owner_approval_ref", divergence.ID)
			}
		}
	}
	return ids, nil
}

func validatePlugin(plugin PluginCapability) error {
	if !validNamespace(plugin.Namespace) {
		return fmt.Errorf("unknown namespace %q", plugin.Namespace)
	}
	if plugin.Namespace == NamespaceAPISIX && len(plugin.Domains) == 0 {
		return errors.New("apisix plugin must declare a domain")
	}
	seenDomains := make(map[Domain]struct{}, len(plugin.Domains))
	for _, domain := range plugin.Domains {
		if !validDomain(domain) {
			return fmt.Errorf("unknown domain %q", domain)
		}
		if _, exists := seenDomains[domain]; exists {
			return fmt.Errorf("duplicate domain %q", domain)
		}
		seenDomains[domain] = struct{}{}
	}

	if !validBehavior(plugin.Behavior) {
		return fmt.Errorf("unknown behavior %q", plugin.Behavior)
	}
	switch plugin.Behavior {
	case BehaviorFull:
		if len(plugin.KnownGaps) != 0 {
			return errors.New("full behavior must not declare known gaps")
		}
	case BehaviorPartial, BehaviorDeferred:
		if len(plugin.KnownGaps) == 0 {
			return fmt.Errorf("%s behavior requires known gaps", plugin.Behavior)
		}
	}
	for _, gap := range plugin.KnownGaps {
		if strings.TrimSpace(gap) == "" {
			return errors.New("known gaps must not be blank")
		}
	}

	for _, evidence := range evidenceClaims(plugin.Evidence) {
		if err := validateEvidence(evidence.kind, evidence.claim); err != nil {
			return err
		}
	}
	for _, factory := range plugin.Factories {
		if strings.TrimSpace(factory.Key) == "" {
			return errors.New("factory key must not be blank")
		}
		if strings.TrimSpace(factory.ImportPath) == "" ||
			strings.TrimSpace(factory.ImportAlias) == "" ||
			strings.TrimSpace(factory.Constructor) == "" {
			return fmt.Errorf("factory %q requires import_path, import_alias, and constructor", factory.Key)
		}
	}
	return nil
}

func validateEvidence(kind EvidenceKind, claim EvidenceClaim) error {
	if !validEvidenceState(claim.State) {
		return fmt.Errorf("evidence %q: unknown state %q", kind, claim.State)
	}
	if claim.State == EvidenceVerified {
		if len(claim.Refs) == 0 {
			return fmt.Errorf("evidence %q: verified claim requires refs", kind)
		}
		for _, ref := range claim.Refs {
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("evidence %q: verified refs must not be blank", kind)
			}
		}
		return nil
	}
	if claim.State == EvidenceNotApplicable && !concreteReason(claim.Reason) {
		return fmt.Errorf("evidence %q: not_applicable claim requires a concrete applicability reason", kind)
	}
	if strings.TrimSpace(claim.Owner) == "" || strings.TrimSpace(claim.Reason) == "" {
		return fmt.Errorf("evidence %q: non-verified claim requires owner and reason", kind)
	}
	return nil
}

func validateQualification(profile QualificationProfile, factoryByKey map[string]int) error {
	if !sortedUniqueStrings(profile.Domains) {
		return errors.New("domains must be sorted and unique")
	}
	for _, domain := range profile.Domains {
		if !validDomain(Domain(domain)) {
			return fmt.Errorf("unknown domain %q", domain)
		}
	}
	if !sortedUniqueStrings(profile.RequiredPlugins) {
		return errors.New("required_plugins must be sorted and unique")
	}
	for _, plugin := range profile.RequiredPlugins {
		if strings.TrimSpace(plugin) == "" {
			return errors.New("required_plugins must not contain blank keys")
		}
		if _, exists := factoryByKey[plugin]; !exists {
			return fmt.Errorf("required_plugins key %q has no factory", plugin)
		}
	}
	if !sortedUniqueEvidence(profile.RequiredEvidence) {
		return errors.New("required_evidence must be sorted and unique")
	}
	for _, kind := range profile.RequiredEvidence {
		if !validEvidenceKind(kind) {
			return fmt.Errorf("unknown required evidence kind %q", kind)
		}
	}
	return nil
}

func evidenceClaims(evidence Evidence) []struct {
	kind  EvidenceKind
	claim EvidenceClaim
} {
	return []struct {
		kind  EvidenceKind
		claim EvidenceClaim
	}{
		{EvidenceSchema, evidence.Schema},
		{EvidenceUnit, evidence.Unit},
		{EvidenceUpstream, evidence.Upstream},
		{EvidenceDifferential, evidence.Differential},
		{EvidenceRealDependency, evidence.RealDependency},
		{EvidenceFailure, evidence.Failure},
		{EvidenceRecovery, evidence.Recovery},
	}
}

func hasRequiredEvidence(evidence Evidence, required []EvidenceKind) bool {
	for _, kind := range required {
		claim := evidenceClaim(evidence, kind)
		if claim.State != EvidenceVerified && claim.State != EvidenceNotApplicable {
			return false
		}
	}
	return true
}

func evidenceClaim(evidence Evidence, kind EvidenceKind) EvidenceClaim {
	switch kind {
	case EvidenceSchema:
		return evidence.Schema
	case EvidenceUnit:
		return evidence.Unit
	case EvidenceUpstream:
		return evidence.Upstream
	case EvidenceDifferential:
		return evidence.Differential
	case EvidenceRealDependency:
		return evidence.RealDependency
	case EvidenceFailure:
		return evidence.Failure
	case EvidenceRecovery:
		return evidence.Recovery
	default:
		return EvidenceClaim{}
	}
}

func supportsDomain(pluginDomains []Domain, profileDomains []string) bool {
	for _, pluginDomain := range pluginDomains {
		for _, profileDomain := range profileDomains {
			if pluginDomain == Domain(profileDomain) {
				return true
			}
		}
	}
	return false
}

func concreteReason(reason string) bool {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	if normalized == "" {
		return false
	}

	words := make([]string, 0, 4)
	var word strings.Builder
	var compact strings.Builder
	descriptiveCount := 0
	flushWord := func() {
		if word.Len() == 0 {
			return
		}
		words = append(words, word.String())
		word.Reset()
	}
	for _, char := range normalized {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			descriptiveCount++
			compact.WriteRune(char)
			word.WriteRune(char)
			continue
		}
		flushWord()
	}
	flushWord()
	if descriptiveCount < 3 || len(words) < 2 {
		return false
	}

	for _, token := range words {
		switch token {
		case "tbd", "todo", "pending", "unknown", "placeholder", "unspecified", "na":
			return false
		}
	}
	if strings.Contains(normalized, "n/a") {
		return false
	}
	for index := 1; index < len(words); index++ {
		if words[index-1] == "n" && words[index] == "a" {
			return false
		}
	}

	compactReason := compact.String()
	switch compactReason {
	case "tbd", "todo", "pending", "unknown", "placeholder", "unspecified", "na",
		"none", "nil", "null", "later", "missing", "deferred", "flaky", "stale",
		"reason", "status", "notapplicable", "notspecified", "notprovided":
		return false
	}
	switch strings.Join(words, " ") {
	case "no reason", "no status", "status reason", "not applicable", "not specified", "not provided":
		return false
	}
	return true
}

func sortedUniqueStrings(values []string) bool {
	if !sort.StringsAreSorted(values) {
		return false
	}
	return !duplicateStrings(values)
}

func duplicateStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func sortedUniqueEvidence(values []EvidenceKind) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func validNamespace(namespace Namespace) bool {
	return namespace == NamespaceAPISIX || namespace == NamespaceGoV1
}

func validDomain(domain Domain) bool {
	return domain == DomainHTTP || domain == DomainStream
}

func validBehavior(behavior BehaviorStatus) bool {
	return behavior == BehaviorFull || behavior == BehaviorPartial ||
		behavior == BehaviorNotApplicable || behavior == BehaviorDeferred
}

func validEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidenceSchema, EvidenceUnit, EvidenceUpstream, EvidenceDifferential,
		EvidenceRealDependency, EvidenceFailure, EvidenceRecovery:
		return true
	default:
		return false
	}
}

func validEvidenceState(state EvidenceState) bool {
	switch state {
	case EvidenceVerified, EvidenceMissing, EvidenceDeferred, EvidenceFlaky,
		EvidenceStale, EvidenceNotApplicable:
		return true
	default:
		return false
	}
}

func validDivergenceStatus(status DivergenceStatus) bool {
	return status == DivergenceProposed || status == DivergenceAccepted || status == DivergenceRetired
}

func clonePlugin(plugin PluginCapability) PluginCapability {
	plugin.Domains = append([]Domain(nil), plugin.Domains...)
	plugin.Factories = append([]Factory(nil), plugin.Factories...)
	plugin.Phases = append([]string(nil), plugin.Phases...)
	plugin.Scopes = append([]string(nil), plugin.Scopes...)
	plugin.KnownGaps = append([]string(nil), plugin.KnownGaps...)
	plugin.Evidence = cloneEvidence(plugin.Evidence)
	plugin.DivergenceIDs = append([]string(nil), plugin.DivergenceIDs...)
	plugin.SupportedPlatforms = append([]string(nil), plugin.SupportedPlatforms...)
	return plugin
}

func cloneEvidence(evidence Evidence) Evidence {
	evidence.Schema.Refs = append([]string(nil), evidence.Schema.Refs...)
	evidence.Unit.Refs = append([]string(nil), evidence.Unit.Refs...)
	evidence.Upstream.Refs = append([]string(nil), evidence.Upstream.Refs...)
	evidence.Differential.Refs = append([]string(nil), evidence.Differential.Refs...)
	evidence.RealDependency.Refs = append([]string(nil), evidence.RealDependency.Refs...)
	evidence.Failure.Refs = append([]string(nil), evidence.Failure.Refs...)
	evidence.Recovery.Refs = append([]string(nil), evidence.Recovery.Refs...)
	return evidence
}

func cloneQualification(profile QualificationProfile) QualificationProfile {
	profile.Domains = append([]string(nil), profile.Domains...)
	profile.RequiredPlugins = append([]string(nil), profile.RequiredPlugins...)
	profile.RequiredEvidence = append([]EvidenceKind(nil), profile.RequiredEvidence...)
	return profile
}
