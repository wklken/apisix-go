package base

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/log"
)

type BasePlugin struct {
	Name     string
	Priority int
	Schema   string
}

func (p *BasePlugin) GetName() string {
	return p.Name
}

func (p *BasePlugin) GetPriority() int {
	return p.Priority
}

func (p *BasePlugin) GetSchema() string {
	return p.Schema
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

	SendFunc func(log map[string]any)

	IncludeRequestBody  bool
	IncludeResponseBody bool
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

		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *BaseLoggerPlugin) Fire(entry map[string]any) error {
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
	go func() {
		for {
			select {
			case log := <-p.FireChan:
				p.SendFunc(log)
			}
		}
	}()
}

// func (p *BaseLoggerPlugin) Send(log map[string]any) {
// 	logger.Errorf("the Send not implemented in sub-class: %s", p.Name)
// }
