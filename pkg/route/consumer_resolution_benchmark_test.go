package route

import (
	"runtime"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
)

func BenchmarkConsumerBindingsForKeyWarmHit(b *testing.B) {
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	key := plugin.ConsumerCacheKey{
		ConsumerID: "benchmark-consumer",
		RouteID:    "benchmark-route",
	}
	ready := make(chan struct{})
	close(ready)
	builder.consumerResolution.entries.Store(key, &consumerResolutionTemplate{ready: ready})

	b.ReportAllocs()
	b.ResetTimer()
	var sink []plugin.Binding
	for b.Loop() {
		bindings, err := builder.consumerBindingsForKey(key, func() ([]plugin.Binding, error) {
			b.Fatal("warm cache unexpectedly initialized")
			return nil, nil
		})
		if err != nil {
			b.Fatal(err)
		}
		sink = bindings
	}
	b.StopTimer()
	runtime.KeepAlive(sink)
}
