package kafka_logger

import (
	"testing"
	"time"
)

func TestProducerTimeLingerUsesSeconds(t *testing.T) {
	p := &Plugin{config: Config{
		BrokerList:         map[string]int{"127.0.0.1": 9092},
		KafkaTopic:         "apisix-logs",
		ProducerTimeLinger: 1,
		Timeout:            3,
	}}
	p.applyDefaults()
	writer, err := p.newWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if writer.BatchTimeout != time.Second {
		t.Fatalf("BatchTimeout=%v, APISIX 3.17 flush_time for producer_time_linger=1 is 1000ms", writer.BatchTimeout)
	}
}
