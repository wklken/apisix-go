package config

import (
	"fmt"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
)

func ValidateQualificationPlugins(
	enabled []string,
	selection ProfileSelection,
	manifest *capability.Manifest,
) error {
	if err := selection.Validate(manifest); err != nil {
		return err
	}
	if selection.Qualification == QualificationNone {
		return nil
	}

	profile, ok := manifest.Qualification(string(selection.Qualification))
	if !ok {
		return fmt.Errorf(
			"qualification_profile %q is not declared by the capability manifest",
			selection.Qualification,
		)
	}

	want := append([]string(nil), profile.RequiredPlugins...)
	got := append([]string(nil), enabled...)
	sort.Strings(want)
	sort.Strings(got)

	duplicates := duplicateNames(got)
	if len(duplicates) != 0 {
		return fmt.Errorf(
			"qualification_profile %s: duplicate enabled plugins %v",
			selection.Qualification,
			duplicates,
		)
	}

	missing, unexpected := membershipDifference(want, got)
	if len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf(
			"qualification_profile %s: missing plugins %v; unexpected plugins %v",
			selection.Qualification,
			missing,
			unexpected,
		)
	}
	return nil
}

func duplicateNames(sortedNames []string) []string {
	duplicates := make([]string, 0)
	for index := 1; index < len(sortedNames); index++ {
		if sortedNames[index] == sortedNames[index-1] &&
			(len(duplicates) == 0 || duplicates[len(duplicates)-1] != sortedNames[index]) {
			duplicates = append(duplicates, sortedNames[index])
		}
	}
	return duplicates
}

func membershipDifference(want, got []string) ([]string, []string) {
	missing := make([]string, 0)
	unexpected := make([]string, 0)
	for wantIndex, gotIndex := 0, 0; wantIndex < len(want) || gotIndex < len(got); {
		switch {
		case wantIndex == len(want):
			unexpected = append(unexpected, got[gotIndex:]...)
			return missing, unexpected
		case gotIndex == len(got):
			missing = append(missing, want[wantIndex:]...)
			return missing, unexpected
		case want[wantIndex] == got[gotIndex]:
			wantIndex++
			gotIndex++
		case want[wantIndex] < got[gotIndex]:
			missing = append(missing, want[wantIndex])
			wantIndex++
		default:
			unexpected = append(unexpected, got[gotIndex])
			gotIndex++
		}
	}
	return missing, unexpected
}
