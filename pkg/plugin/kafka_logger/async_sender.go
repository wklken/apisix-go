package kafka_logger

import (
	"context"
	"errors"
	"sync"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var errKafkaSenderClosed = errors.New("kafka sender is closed")

type kafkaBatchWriter interface {
	WriteMessages(context.Context, ...kafka.Message) error
	Close() error
}

// The APISIX producer bounds queued messages, excluding a batch already being
// delivered. kafka-go's asynchronous Writer has no bounded queue, so this
// generation-owned worker admits messages before calling a synchronous writer.
type asyncKafkaSender struct {
	writer    kafkaBatchWriter
	queue     chan kafka.Message
	trigger   chan struct{}
	stop      chan struct{}
	done      chan struct{}
	batchSize int
	linger    time.Duration
	timeout   time.Duration
	mu        sync.Mutex
	closed    bool
	closeErr  error
}

func newAsyncKafkaSender(
	owner *runtime.TaskOwner,
	writer kafkaBatchWriter,
	capacity, batchSize int,
	linger, timeout time.Duration,
) (*asyncKafkaSender, error) {
	if owner == nil {
		return nil, runtime.ErrTaskOwnerRequired
	}
	s := &asyncKafkaSender{
		writer: writer, queue: make(chan kafka.Message, capacity), trigger: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}), batchSize: batchSize, linger: linger, timeout: timeout,
	}
	if err := owner.Go("producer", s.run); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *asyncKafkaSender) Send(ctx context.Context, message kafkaMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errKafkaSenderClosed
	}
	select {
	case s.queue <- kafka.Message{Topic: message.Topic, Key: append([]byte(nil), message.Key...), Value: append([]byte(nil), message.Value...)}:
		if len(s.queue) >= s.batchSize {
			select {
			case s.trigger <- struct{}{}:
			default:
			}
		}
		return nil
	default:
		return errors.New("buffer overflow")
	}
}

func (s *asyncKafkaSender) Close() error {
	s.stopAdmission()
	<-s.done
	return s.closeErr
}

func (s *asyncKafkaSender) stopAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
}

func (s *asyncKafkaSender) run(ctx context.Context) error {
	defer close(s.done)
	defer func() { s.closeErr = s.writer.Close() }()
	defer s.stopAdmission()
	ticker := time.NewTicker(s.linger)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAdmission()
			s.flushOnClose(ctx)
			return nil
		case <-s.stop:
			s.flushOnClose(ctx)
			return nil
		case <-ticker.C:
		case <-s.trigger:
			// flush can consume several full batches itself. Their buffered wake
			// must not flush a later partial batch before the linger interval.
			if len(s.queue) < s.batchSize {
				continue
			}
		}
		s.flush(ctx, false)
	}
}

func (s *asyncKafkaSender) flushOnClose(ctx context.Context) {
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	s.flush(drainCtx, true)
}

func (s *asyncKafkaSender) flush(ctx context.Context, drain bool) {
	for {
		batch := make([]kafka.Message, 0, min(len(s.queue), s.batchSize))
		for len(batch) < s.batchSize {
			select {
			case message := <-s.queue:
				batch = append(batch, message)
			default:
				goto deliver
			}
		}
	deliver:
		if len(batch) == 0 {
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.writer.WriteMessages(sendCtx, batch...)
		cancel()
		if err != nil {
			logger.Errorf("kafka logger asynchronous delivery failed")
		}
		if !drain && (ctx.Err() != nil || len(s.queue) < s.batchSize) {
			return
		}
	}
}
