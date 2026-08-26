package runtime

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type metadataTestSecret string

func (value metadataTestSecret) Use(use func(string) error) error {
	return use(string(value))
}

func TestMetadataViewScopesSecretsToFactoryAndRevokesOnClose(t *testing.T) {
	view, err := NewMetadataViewWithSecrets(map[string]MetadataDocument{
		"factory-a": {
			Document: []byte(`{"token":"plugin_metadata#sha256:redacted"}`),
			Secrets:  map[string]MetadataSecret{"/token": metadataTestSecret("private-a")},
		},
		"factory-b": {Document: []byte(`{"token":"public-b"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	scoped := view.ForFactory("factory-a")
	var own map[string]string
	if found, err := scoped.Decode("factory-a", &own); err != nil || !found || own["token"] != "private-a" {
		t.Fatalf("own Decode() = (%v, %v, %#v)", found, err, own)
	}
	var sibling map[string]string
	if found, err := scoped.Decode("factory-b", &sibling); err != nil || found {
		t.Fatalf("sibling Decode() = (%v, %v), want hidden", found, err)
	}
	if got := string(view.state.documents["factory-a"].document); strings.Contains(got, "private-a") {
		t.Fatalf("shared metadata document contains plaintext: %q", got)
	}

	view.Close()
	if found, err := scoped.Decode("factory-a", &own); found || !errors.Is(err, errMetadataViewClosed) {
		t.Fatalf("closed Decode() = (%v, %v), want revoked authority", found, err)
	}
}

func TestMetadataViewZeroAndEmpty(t *testing.T) {
	var zero MetadataView
	for _, view := range []MetadataView{zero, mustMetadataView(t, nil), mustMetadataView(t, map[string][]byte{})} {
		var target map[string]any
		found, err := view.Decode("missing", &target)
		if err != nil || found {
			t.Fatalf("Decode() = (%v, %v), want (false, nil)", found, err)
		}
	}
}

func TestMetadataViewCanonicalizesDocuments(t *testing.T) {
	first := mustMetadataView(t, map[string][]byte{
		"factory": []byte("  {\"name\":\"value\",\"number\":9007199254740993}  "),
	})
	second := mustMetadataView(t, map[string][]byte{
		"factory": []byte("{\"number\":9007199254740993, \"name\": \"value\"}"),
	})

	var firstTarget, secondTarget map[string]any
	found, err := first.Decode("factory", &firstTarget)
	if err != nil || !found {
		t.Fatalf("first Decode() = (%v, %v)", found, err)
	}
	found, err = second.Decode("factory", &secondTarget)
	if err != nil || !found {
		t.Fatalf("second Decode() = (%v, %v)", found, err)
	}
	if !reflect.DeepEqual(firstTarget, secondTarget) {
		t.Fatalf("canonical decode mismatch: %#v != %#v", firstTarget, secondTarget)
	}
	if !bytes.Equal(first.state.documents["factory"].document, second.state.documents["factory"].document) {
		t.Fatalf(
			"canonical documents differ: %q != %q",
			first.state.documents["factory"].document,
			second.state.documents["factory"].document,
		)
	}

	number, ok := firstTarget["number"].(interface{ String() string })
	if !ok || number.String() != "9007199254740993" {
		t.Fatalf("large number = %#v, want exact JSON number", firstTarget["number"])
	}
}

func TestMetadataViewCopiesInput(t *testing.T) {
	document := []byte(`{"secret":"before"}`)
	metadata := map[string][]byte{"factory": document}
	view := mustMetadataView(t, metadata)

	document[11] = 'a'
	metadata["factory"] = []byte(`{"secret":"after"}`)

	var target map[string]string
	found, err := view.Decode("factory", &target)
	if err != nil || !found || target["secret"] != "before" {
		t.Fatalf("Decode() = (%v, %v, %#v), want original document", found, err, target)
	}
}

func TestMetadataViewTargetMutationDoesNotChangeView(t *testing.T) {
	view := mustMetadataView(t, map[string][]byte{"factory": []byte(`{"value":"stable"}`)})
	var target map[string]string
	if found, err := view.Decode("factory", &target); err != nil || !found {
		t.Fatalf("first Decode() = (%v, %v)", found, err)
	}
	target["value"] = "mutated"

	var second map[string]string
	if found, err := view.Decode("factory", &second); err != nil || !found {
		t.Fatalf("second Decode() = (%v, %v)", found, err)
	}
	if second["value"] != "stable" {
		t.Fatalf("second Decode() = %#v, want stable value", second)
	}
}

func TestMetadataViewDecodeAbsentAndInvalidArguments(t *testing.T) {
	view := mustMetadataView(t, map[string][]byte{"factory": []byte(`{"value":1}`)})

	var target map[string]any
	if found, err := view.Decode("absent", &target); err != nil || found {
		t.Fatalf("absent Decode() = (%v, %v), want (false, nil)", found, err)
	}
	if found, err := view.Decode("factory", nil); found || !errors.Is(err, errMetadataTargetRequired) {
		t.Fatalf("nil target Decode() = (%v, %v)", found, err)
	}
	if found, err := view.Decode("factory", target); found || !errors.Is(err, errMetadataTargetRequired) {
		t.Fatalf("non-pointer target Decode() = (%v, %v)", found, err)
	}
	var nilTarget *map[string]any
	if found, err := view.Decode("factory", nilTarget); found || !errors.Is(err, errMetadataTargetRequired) {
		t.Fatalf("typed nil target Decode() = (%v, %v)", found, err)
	}
	if found, err := view.Decode("", &target); found || !errors.Is(err, errMetadataFactoryRequired) {
		t.Fatalf("empty factory Decode() = (%v, %v)", found, err)
	}
}

func TestMetadataViewRejectsInvalidDocumentsWithRedactedErrors(t *testing.T) {
	cases := []struct {
		name     string
		document string
	}{
		{name: "empty", document: ""},
		{name: "whitespace", document: " \t\n"},
		{name: "null", document: "null"},
		{name: "array", document: "[]"},
		{name: "scalar", document: "42"},
		{name: "invalid", document: `{"secret":"plaintext"`},
		{name: "multiple", document: `{"first":1}{"second":"plaintext"}`},
		{name: "trailing", document: `{"first":1} plaintext"`},
		{name: "leading non-json whitespace", document: "\u00a0{\"secret\":\"plaintext\"}"},
		{name: "trailing non-json whitespace", document: "{\"secret\":\"plaintext\"}\u00a0"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMetadataView(map[string][]byte{"factory": []byte(tt.document)})
			if !errors.Is(err, errMetadataDocumentInvalid) {
				t.Fatalf("NewMetadataView() error = %v, want redacted document error", err)
			}
			if strings.Contains(err.Error(), "plaintext") ||
				(tt.document != "" && strings.Contains(err.Error(), tt.document)) {
				t.Fatalf("error leaked metadata: %q", err)
			}
		})
	}

	if _, err := NewMetadataView(map[string][]byte{
		"": []byte(`{"secret":"plaintext"}`),
	}); !errors.Is(err, errMetadataFactoryRequired) {
		t.Fatalf("empty factory error = %v", err)
	}
}

func TestMetadataViewDecodeIsConcurrent(t *testing.T) {
	view := mustMetadataView(t, map[string][]byte{"factory": []byte(`{"value":9007199254740993}`)})
	const decodes = 64
	var wg sync.WaitGroup
	errs := make(chan error, decodes)
	for range decodes {
		wg.Go(func() {
			var target map[string]any
			found, err := view.Decode("factory", &target)
			if err != nil {
				errs <- err
				return
			}
			if !found {
				errs <- errors.New("metadata unexpectedly absent")
				return
			}
			number, ok := target["value"].(interface{ String() string })
			if !ok || number.String() != "9007199254740993" {
				errs <- errors.New("large integer was not preserved")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func mustMetadataView(t *testing.T, metadata map[string][]byte) MetadataView {
	t.Helper()
	view, err := NewMetadataView(metadata)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}
