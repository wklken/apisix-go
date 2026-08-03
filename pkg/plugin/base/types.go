package base

import (
	"fmt"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

type BasePlugin struct {
	Name           string
	Priority       int
	Schema         string
	MetadataSchema string
}

func (p *BasePlugin) GetName() string {
	return p.Name
}

func (p *BasePlugin) GetPriority() int {
	return p.Priority
}

func (p *BasePlugin) SetPriority(priority int) {
	p.Priority = priority
}

func (p *BasePlugin) GetSchema() string {
	return p.Schema
}

func (p *BasePlugin) GetMetadataSchema() string {
	return p.MetadataSchema
}

// type LoggerPlugin interface {
// Fire(entry map[string]any) error
// Consume()
// Send(log map[string]any)
// }

const (
	MAX_REQ_BODY  = 524288 // 512 KiB
	MAX_RESP_BODY = 524288 // 512 KiB
)

type BaseLoggerPlugin struct {
	BasePlugin

	FireChan   chan map[string]any
	AsyncBlock bool

	LogFormat map[string]string

	SendFunc       func(log map[string]any)
	BatchProcessor *logger_batch.Processor
	RouteID        string
	ServerAddr     string

	IncludeRequestBody  bool
	IncludeResponseBody bool
}

func (p *BaseLoggerPlugin) SetRouteContext(routeID string, serverAddr string) {
	p.RouteID = routeID
	p.ServerAddr = serverAddr
}

// InitLogger initializes the buffered fire channel, blocking policy and the
// per-plugin Send function.
func (p *BaseLoggerPlugin) InitLogger(send func(map[string]any)) {
	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = send
}

// BatchDefaults carries the per-plugin batch configuration values in seconds.
type BatchDefaults struct {
	BatchMaxSize       int
	MaxRetryCount      int
	RetryDelaySec      int
	RetryDelaySet      bool
	BufferDurationSec  int
	InactiveTimeoutSec int
	MaxPendingEntries  int
}

// ApplyBatchDefaults fills zero batch values with logger_batch defaults.
// RetryDelaySec is only defaulted when RetryDelaySet is false.
func ApplyBatchDefaults(d *BatchDefaults) {
	if d.BatchMaxSize == 0 {
		d.BatchMaxSize = logger_batch.DefaultBatchMaxSize
	}
	if d.RetryDelaySec == 0 && !d.RetryDelaySet {
		d.RetryDelaySec = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if d.BufferDurationSec == 0 {
		d.BufferDurationSec = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if d.InactiveTimeoutSec == 0 {
		d.InactiveTimeoutSec = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}
}

// NewBatchProcessor constructs a logger batch processor from second-based
// batch defaults.
func NewBatchProcessor(
	name string,
	d BatchDefaults,
	routeID, serverAddr string,
	deliver logger_batch.DeliveryFunc,
) *logger_batch.Processor {
	ApplyBatchDefaults(&d)
	return logger_batch.New(logger_batch.Config{
		Name:              name,
		BatchMaxSize:      d.BatchMaxSize,
		MaxRetryCount:     d.MaxRetryCount,
		RetryDelay:        time.Duration(d.RetryDelaySec) * time.Second,
		RetryDelaySet:     d.RetryDelaySet,
		BufferDuration:    time.Duration(d.BufferDurationSec) * time.Second,
		InactiveTimeout:   time.Duration(d.InactiveTimeoutSec) * time.Second,
		MaxPendingEntries: d.MaxPendingEntries,
		RouteID:           routeID,
		ServerAddr:        serverAddr,
	}, deliver)
}

func (p *BaseLoggerPlugin) Stop() {
	if p.BatchProcessor != nil {
		p.BatchProcessor.Stop()
	}
}

// func getRequest(r *http.Request, includeRequestBody bool) map[string]any {
// }

// func getResponse(w http.ResponseWriter, includeResponseBody bool) map[string]any {
// }

func (p *BaseLoggerPlugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		logFields := log.GetFields(r, p.LogFormat)

		// FIXME: if not LogFormat, will get full log,
		// reference: https://github.com/apache/apisix/blob/master/apisix/utils/log-util.lua#L136

		// logFields["request"] = getRequest(r, p.IncludeRequestBody)
		// logFields["response"] = getResponse(w, p.IncludeResponseBody)

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *BaseLoggerPlugin) Fire(entry map[string]any) error {
	if p.BatchProcessor != nil {
		p.BatchProcessor.Push(entry)
		return nil
	}

	select {
	case p.FireChan <- entry: // try and put into chan, if fail will to default
	default:
		if p.AsyncBlock {
			fmt.Println("the log buffered chan is full! will block")
			p.FireChan <- entry // Blocks the goroutine because buffer is full.
			return nil
		}
		fmt.Println("the log buffered chan is full! will drop")
		// Drop message by default.
	}
	return nil
}

// add a http log consumer here, to consume the log via a channel
func (p *BaseLoggerPlugin) Consume() {
	if p.BatchProcessor != nil {
		return
	}

	go func() {
		for log := range p.FireChan {
			p.SendFunc(log)
		}
	}()
}

// func (p *BaseLoggerPlugin) Send(log map[string]any) {
// 	logger.Errorf("the Send not implemented in sub-class: %s", p.Name)
// }
