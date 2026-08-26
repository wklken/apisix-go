package base_test

import (
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestScopedSecretAccessExposesNoMutableAuthorityFields(t *testing.T) {
	for field := range reflect.TypeFor[base.ScopedSecretAccess]().Fields() {
		if field.IsExported() {
			t.Fatalf("ScopedSecretAccess exports mutable authority field %q", field.Name)
		}
	}
}
