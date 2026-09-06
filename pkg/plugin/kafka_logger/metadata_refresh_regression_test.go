package kafka_logger

import (
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

func TestProducerMetadataRefreshIntervalUsesSeconds(t *testing.T) {
	p := &Plugin{config: Config{BrokerList: map[string]int{"127.0.0.1": 9092}, MetaRefreshInterval: 47}}
	p.applyDefaults()
	writer, err := p.newWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.MetadataTTL != 47*time.Second {
		t.Fatalf("transport=%#v, want metadata TTL 47 seconds", writer.Transport)
	}
}
