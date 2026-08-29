package producer

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"

	rocketmqerrors "github.com/apache/rocketmq-client-go/v2/errors"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

func TestSendSyncStopsBeforeRetryWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	producer := &defaultProducer{
		options: producerOptions{},
	}

	err := producer.sendSync(ctx, &primitive.Message{Topic: "topic"}, primitive.NewSendResult())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sendSync() error = %v, want context cancellation", err)
	}
	if errors.Is(err, rocketmqerrors.ErrRequestTimeout) {
		t.Fatal("sendSync() converted caller cancellation to a request timeout")
	}
}

func TestSendOneWayStopsBeforeRetryWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	producer := &defaultProducer{
		options: producerOptions{},
	}

	err := producer.sendOneWay(ctx, &primitive.Message{Topic: "topic"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sendOneWay() error = %v, want context cancellation", err)
	}
}

func TestTLSOptionsPropagateToEachProducerRemotingConfig(t *testing.T) {
	first := defaultProducerOptions()
	second := defaultProducerOptions()
	roots := x509.NewCertPool()
	WithTls(true)(&first)
	WithTlsVerify(true)(&first)
	WithTlsRootCAs(roots)(&first)

	if !first.RemotingClientConfig.UseTls || !first.RemotingClientConfig.TLSVerify ||
		first.RemotingClientConfig.TLSRootCAs != roots {
		t.Fatalf("TLS options were not propagated: %#v", first.RemotingClientConfig)
	}
	if second.RemotingClientConfig.UseTls || second.RemotingClientConfig.TLSVerify ||
		second.RemotingClientConfig.TLSRootCAs != nil {
		t.Fatalf("TLS options leaked into another producer: %#v", second.RemotingClientConfig)
	}
}
