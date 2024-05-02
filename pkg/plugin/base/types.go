package base

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
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

type BaseLoggerPlugin struct {
	BasePlugin

	fireChan   chan map[string]any
	asyncBlock bool

	logFormat map[string]string
}

func (p *BaseLoggerPlugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		logFields := log.GetFields(r, p.logFormat)
		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *BaseLoggerPlugin) Fire(entry map[string]any) error {
	select {
	case p.fireChan <- entry: // try and put into chan, if fail will to default
	default:
		if p.asyncBlock {
			fmt.Println("the log buffered chan is full! will block")
			p.fireChan <- entry // Blocks the goroutine because buffer is full.
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
			case log := <-p.fireChan:
				p.Send(log)
				// consume the log
			}
		}
	}()
}

func (p *BaseLoggerPlugin) Send(log map[string]any) {
	logger.Errorf("the Send not implemented in sub-class: %s", p.Name)
}
