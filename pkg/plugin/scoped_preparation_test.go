package plugin

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestSupportsScopedSecretMaterializationUnknownIsRedacted(t *testing.T) {
	const factory = "secret://vault/plugin-factory"

	supported, err := SupportsScopedSecretMaterialization(factory)
	if supported {
		t.Fatal("SupportsScopedSecretMaterialization(unknown) = true, want false")
	}
	if err == nil {
		t.Fatal("SupportsScopedSecretMaterialization(unknown) error = nil")
	}
	if strings.Contains(err.Error(), factory) {
		t.Fatalf("unknown factory leaked in error: %v", err)
	}
}

func TestSupportsScopedSecretMaterializationRejectsEmptyAndNilFactories(t *testing.T) {
	if _, err := SupportsScopedSecretMaterialization(""); err == nil {
		t.Fatal("SupportsScopedSecretMaterialization(empty) error = nil")
	}
	if _, err := scopedSecretMaterializationSupportFromFactory("nil-factory", nil); err == nil {
		t.Fatal("scopedSecretMaterializationSupportFromFactory(nil) error = nil")
	}
}

func TestSupportsScopedSecretMaterializationReturnsFalseForRealFactoryWithoutSupport(t *testing.T) {
	supported, err := SupportsScopedSecretMaterialization("echo")
	if err != nil {
		t.Fatalf("SupportsScopedSecretMaterialization(echo) error = %v", err)
	}
	if supported {
		t.Fatal("SupportsScopedSecretMaterialization(echo) = true, want false")
	}
}

func TestRawResolverFactoriesSupportScopedSecretMaterialization(t *testing.T) {
	for _, factory := range []string{
		"ai-rate-limiting", "csrf", "kafka-proxy", "response-rewrite",
		"elasticsearch-logger", "error-log-logger", "google-cloud-logging",
		"http-logger", "kafka-logger", "lago", "loggly", "rocketmq-logger",
		"sls-logger", "splunk-hec-logging", "tencent-cloud-cls",
	} {
		supported, err := SupportsScopedSecretMaterialization(factory)
		if err != nil || !supported {
			t.Errorf("factory %s scoped support = %v/%v", factory, supported, err)
		}
	}
}

func TestSupportsScopedSecretMaterializationOnlyConstructsDualInterfaceFactory(t *testing.T) {
	plugin := &scopedPreparationPoisonPlugin{}
	constructorCalls := 0

	supported, err := scopedSecretMaterializationSupportFromFactory("dual-interface", func() Plugin {
		constructorCalls++
		return plugin
	})
	if err != nil {
		t.Fatalf("scopedSecretMaterializationSupportFromFactory() error = %v", err)
	}
	if !supported {
		t.Fatal("scopedSecretMaterializationSupportFromFactory() = false, want true")
	}
	if constructorCalls != 1 {
		t.Fatalf("factory constructor calls = %d, want 1", constructorCalls)
	}
	if plugin.initCalls != 0 || plugin.postInitCalls != 0 || plugin.handlerCalls != 0 ||
		plugin.configCalls != 0 || plugin.schemaCalls != 0 || plugin.metadataCalls != 0 ||
		plugin.priorityCalls != 0 || plugin.nameCalls != 0 || plugin.scopedCalls != 0 ||
		plugin.legacyCalls != 0 {
		t.Fatalf("dual-interface lifecycle calls = %#v, want all zero", plugin)
	}
}

func TestNewFactoryInstancePreservesExactRegistryIdentity(t *testing.T) {
	for _, factory := range []string{"otel", "request-context"} {
		instance, err := NewFactoryInstance(factory, base.Dependencies{})
		if err != nil || instance.Factory() != factory || instance.Plugin() == nil {
			t.Fatalf("NewFactoryInstance(%q) = %#v/%v", factory, instance, err)
		}
	}
	pre, err := NewFactoryInstance("serverless-pre-function", base.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	post, err := NewFactoryInstance("serverless-post-function", base.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.TypeOf(pre.Plugin()) != reflect.TypeOf(post.Plugin()) {
		t.Fatal("serverless regression fixture no longer shares one concrete plugin type")
	}
	if pre.Factory() == post.Factory() {
		t.Fatal("distinct serverless factories lost their exact registry identities")
	}
	if _, err := NewFactoryInstance("secret://vault/plugin-factory", base.Dependencies{}); err == nil ||
		strings.Contains(err.Error(), "secret://vault/plugin-factory") {
		t.Fatalf("unknown factory error = %v, want redacted error", err)
	}
	echo := New("echo", base.Dependencies{})
	typedNil := reflect.Zero(reflect.TypeOf(echo)).Interface().(Plugin)
	if !isNilPlugin(typedNil) {
		t.Fatal("typed nil plugin was not rejected")
	}
	var poison *scopedPreparationPoisonPlugin
	if _, err := scopedSecretMaterializationSupportFromFactory("typed-nil", func() Plugin {
		return poison
	}); err == nil {
		t.Fatal("scopedSecretMaterializationSupportFromFactory(typed nil) error = nil")
	}
}

type scopedPreparationPoisonPlugin struct {
	initCalls     int
	postInitCalls int
	handlerCalls  int
	configCalls   int
	schemaCalls   int
	metadataCalls int
	priorityCalls int
	nameCalls     int
	scopedCalls   int
	legacyCalls   int
}

func (p *scopedPreparationPoisonPlugin) Init() error {
	p.initCalls++
	return nil
}

func (p *scopedPreparationPoisonPlugin) PostInit() error {
	p.postInitCalls++
	return nil
}

func (p *scopedPreparationPoisonPlugin) Handler(next http.Handler) http.Handler {
	p.handlerCalls++
	return next
}

func (p *scopedPreparationPoisonPlugin) Config() any {
	p.configCalls++
	return nil
}

func (p *scopedPreparationPoisonPlugin) GetSchema() string {
	p.schemaCalls++
	return ""
}

func (p *scopedPreparationPoisonPlugin) GetMetadataSchema() string {
	p.metadataCalls++
	return ""
}

func (p *scopedPreparationPoisonPlugin) GetPriority() int {
	p.priorityCalls++
	return 0
}

func (p *scopedPreparationPoisonPlugin) GetName() string {
	p.nameCalls++
	return "dual-interface"
}

func (p *scopedPreparationPoisonPlugin) MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error {
	p.scopedCalls++
	return nil
}

func (p *scopedPreparationPoisonPlugin) MaterializeSecrets() error {
	p.legacyCalls++
	return nil
}
