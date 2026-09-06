package compiler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestCredentialMetadataSurvivesAuthenticationAndConsumerBindings(t *testing.T) {
	observed := make(chan resource.Consumer, 3)
	opa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input struct {
				Consumer resource.Consumer `json:"consumer"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "bad input", 400)
			return
		}
		observed <- input.Input.Consumer
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	defer opa.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Credential", r.Header.Get("X-Credential-Identifier"))
		w.Header().Set("Consumer", r.Header.Get("X-Consumer-Username"))
		w.WriteHeader(204)
	}))
	defer upstream.Close()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &consumerPreparationBroker{resolved: map[string]string{"$ENV://PIPELINE_KEY": "second-key"}}
	effective := workerTestEffective()
	effective.Config.Apisix.ID = "credential-pipeline"
	effective.Config.Plugins = []string{"key-auth", "opa"}
	factory, err := NewWorkerCompilerFactory(
		effective,
		testutil.NewSecretMaterializer(broker, catalog),
		workerTestRuntimeObservers(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	desired := mustGenerationSnapshot(t, 93, []generation.Resource{
		resourceValue("consumers", "rose", `{"username":"rose","plugins":{"key-auth":{"key":"parent-key"}}}`),
		resourceValue("consumers", "rose/credentials/first", `{"plugins":{"key-auth":{"key":"first-key"}}}`),
		resourceValue("consumers", "rose/credentials/second", `{"plugins":{"key-auth":{"key":"$ENV://PIPELINE_KEY"}}}`),
		resourceValue(
			"routes",
			"credential",
			fmt.Sprintf(
				`{"id":"credential","uri":"/","plugins":{"key-auth":{},"opa":{"host":%q,"policy":"allow","with_consumer":true}},"upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
				opa.URL,
				strings.TrimPrefix(upstream.URL, "http://"),
			),
		),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for _, test := range []struct{ key, id string }{{"parent-key", ""}, {"first-key", "first"}, {"second-key", "second"}} {
		request := httptest.NewRequest("GET", "http://gateway.test/", nil)
		request.Header.Set("apikey", test.key)
		response := httptest.NewRecorder()
		prepared.HTTP().Handler().ServeHTTP(response, request)
		if response.Code != 204 {
			t.Fatalf("credential %s status=%d body=%s", test.id, response.Code, response.Body.String())
		}
		if response.Header().Get("Consumer") != "rose" || response.Header().Get("Credential") != test.id {
			t.Errorf("credential %s upstream identity=%v", test.id, response.Header())
		}
		select {
		case consumer := <-observed:
			auth, ok := consumer.AuthConf.(map[string]any)
			if consumer.Username != "rose" || consumer.CredentialID != test.id || !ok || auth["key"] != test.key {
				t.Errorf("credential %s OPA consumer metadata does not identify the selected credential", test.id)
			}
		default:
			t.Fatal("OPA did not observe the authenticated consumer")
		}
	}
}

func TestNestedCredentialEnvelopeCannotSynthesizeConsumers(t *testing.T) {
	for _, test := range []struct{ name, raw string }{
		{"username", `{"username":"rose/credentials/bad","plugins":{"key-auth":{"key":"bad-key"}}}`},
		{"multiple plugins", `{"plugins":{"key-auth":{"key":"bad-key"},"basic-auth":{"username":"bad","password":"bad"}}}`},
		{"group", `{"group_id":"other","plugins":{"key-auth":{"key":"bad-key"}}}`},
		{"unknown field", `{"other":true,"plugins":{"key-auth":{"key":"bad-key"}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory, _ := newConsumerAttemptFactory(t, &consumerPreparationBroker{})
			desired := mustGenerationSnapshot(
				t,
				94,
				[]generation.Resource{
					resourceValue("consumers", "rose", `{"username":"rose"}`),
					resourceValue("consumers", "rose/credentials/bad", test.raw),
				},
				nil,
			)
			prepared, err := factory.prepareGenerationSecrets(
				context.Background(),
				ticketForSnapshot(desired, generation.DomainHTTP),
				desired,
				nil,
			)
			if err != nil {
				t.Fatalf("invalid credential must be quarantined without rejecting its parent generation: %v", err)
			}
			defer func() {
				if err := prepared.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			if _, ok := prepared.consumers.ConsumerByPluginKey("key-auth", "bad-key"); ok {
				t.Fatal("invalid nested credential became an authentication binding")
			}
			if _, ok := prepared.consumers.ConsumerByID("rose/credentials/bad"); ok {
				t.Fatal("nested credential synthesized an independent consumer")
			}
			if _, ok := prepared.consumers.ConsumerByID("rose"); !ok {
				t.Fatal("quarantining a bad credential removed its valid parent")
			}
		})
	}
}
