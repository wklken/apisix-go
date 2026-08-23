package config

import (
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
)

type CompatibilityTarget string

type SecurityProfile string

type QualificationProfile string

const (
	CompatibilityAPISIX317       CompatibilityTarget  = "apisix-3.17"
	SecurityCompat               SecurityProfile      = "compat"
	SecurityStrict               SecurityProfile      = "strict"
	QualificationNone            QualificationProfile = ""
	QualificationHTTPDataPlaneV1 QualificationProfile = "http-data-plane-v1"
)

type ProfileSelection struct {
	Compatibility CompatibilityTarget
	Security      SecurityProfile
	Qualification QualificationProfile
}

func (p ProfileSelection) Validate(manifest *capability.Manifest) error {
	if manifest == nil {
		return fmt.Errorf("capability manifest must not be nil")
	}
	if p.Compatibility != CompatibilityTarget(manifest.Target.Name) {
		return fmt.Errorf("compatibility_target is unsupported")
	}
	if !slices.Contains([]SecurityProfile{SecurityCompat, SecurityStrict}, p.Security) {
		return fmt.Errorf("security_profile is unsupported")
	}
	if p.Qualification == QualificationNone {
		return nil
	}
	qualification, ok := manifest.Qualification(string(p.Qualification))
	if !ok {
		return fmt.Errorf("qualification_profile is unsupported")
	}
	qualified := manifest.QualifiedPlugins(string(p.Qualification))
	if !slices.Equal(qualification.RequiredPlugins, qualified) {
		return fmt.Errorf("qualification_profile %q has unqualified required plugins", p.Qualification)
	}
	return nil
}
