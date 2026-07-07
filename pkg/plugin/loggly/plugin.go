package loggly

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
}

const (
	priority = 411
	name     = "loggly"
)

const schema = `
{
  "type": "object",
  "properties": {
    "customer_token": {
      "type": "string"
    },
    "severity": {
      "type": "string",
      "default": "INFO",
      "enum": ["DEBUG", "INFO", "NOTICE", "WARNING", "ERR", "CRIT", "ALERT", "EMEGR", "debug", "info", "notice", "warning", "err", "crit", "alert", "emegr"]
    },
    "severity_map": {
      "type": "object"
    },
    "tags": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      },
      "default": ["apisix"]
    },
    "log_format": {
      "type": "object"
    },
    "host": {
      "type": "string",
      "default": "logs-01.loggly.com"
    },
    "port": {
      "type": "integer",
      "default": 514
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
    }
  },
  "required": ["customer_token"]
}
`

type Config struct {
	CustomerToken string            `json:"customer_token"`
	Severity      string            `json:"severity,omitempty"`
	SeverityMap   map[string]string `json:"severity_map,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Host          string            `json:"host,omitempty"`
	Port          int               `json:"port,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
}

var severityValues = map[string]int{
	"EMEGR":   0,
	"ALERT":   1,
	"CRIT":    2,
	"ERR":     3,
	"WARNING": 4,
	"NOTICE":  5,
	"INFO":    6,
	"DEBUG":   7,
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Severity == "" {
		p.config.Severity = "INFO"
	}
	p.config.Severity = strings.ToUpper(p.config.Severity)
	if len(p.config.Tags) == 0 {
		p.config.Tags = []string{"apisix"}
	}
	if p.config.Host == "" {
		p.config.Host = "logs-01.loggly.com"
	}
	if p.config.Port == 0 {
		p.config.Port = 514
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 5000
	}

	p.LogFormat = p.config.LogFormat

	p.Consume()

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return p.BaseLoggerPlugin.Handler(next)
}

func (p *Plugin) Send(log map[string]any) {
	message := p.buildMessage(log)
	conn, err := net.DialTimeout(
		"udp",
		fmt.Sprintf("%s:%d", p.config.Host, p.config.Port),
		time.Duration(p.config.Timeout)*time.Millisecond,
	)
	if err != nil {
		logger.Errorf("failed to connect to Loggly UDP endpoint %s:%d: %s", p.config.Host, p.config.Port, err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		logger.Errorf("failed to send loggly message: %s", err)
	}
}

func (p *Plugin) buildMessage(log map[string]any) string {
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "-"
	}

	return strings.Join([]string{
		fmt.Sprintf("<%d>1", 8+messageSeverity(p.config.Severity, p.config.SeverityMap, log)),
		time.Now().UTC().Format(time.RFC3339Nano),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(),
		string(payload),
	}, " ")
}

func messageSeverity(defaultSeverity string, severityMap map[string]string, log map[string]any) int {
	if status, ok := log["status"]; ok {
		key := fmt.Sprint(status)
		if severity, ok := severityMap[key]; ok {
			return severityCode(severity)
		}
	}
	return severityCode(defaultSeverity)
}

func severityCode(severity string) int {
	if code, ok := severityValues[strings.ToUpper(severity)]; ok {
		return code
	}
	return severityValues["INFO"]
}

func (p *Plugin) structuredData() string {
	tags := make([]string, 0, len(p.config.Tags))
	for _, tag := range p.config.Tags {
		tags = append(tags, fmt.Sprintf(`tag="%s"`, tag))
	}
	if len(tags) == 0 {
		return fmt.Sprintf("[%s@41058]", p.config.CustomerToken)
	}
	return fmt.Sprintf("[%s@41058 %s]", p.config.CustomerToken, strings.Join(tags, " "))
}
