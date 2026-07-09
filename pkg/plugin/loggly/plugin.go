package loggly

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
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
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "include_req_body": {
      "type": "boolean",
      "default": false
    },
    "include_req_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "include_resp_body": {
      "type": "boolean",
      "default": false
    },
    "include_resp_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "max_req_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "max_resp_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
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
    },
    "protocol": {
      "type": "string",
      "default": "syslog",
      "enum": ["syslog", "http", "https"]
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
	SSLVerify     *bool             `json:"ssl_verify,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Host          string            `json:"host,omitempty"`
	Port          int               `json:"port,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`
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
	if p.config.Protocol == "" {
		p.config.Protocol = "syslog"
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}

	p.LogFormat = p.config.LogFormat

	p.Consume()

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if !p.config.IncludeReqBody && !p.config.IncludeRespBody {
		return p.BaseLoggerPlugin.Handler(next)
	}

	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody && exprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *logglyResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &logglyResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.status
		}

		logFields := apisixlog.GetFields(r, p.LogFormat)
		if requestBody != "" {
			nestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.body.Len() > 0 && exprMatched(r, p.config.IncludeRespBodyExpr, status) {
			nestedLogMap(logFields, "response")["body"] = recorder.body.String()
		}

		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

type logglyResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *logglyResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *logglyResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *logglyResponseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
}

func readAndRestoreRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return string(body), nil
}

func nestedLogMap(fields map[string]any, key string) map[string]any {
	if value, ok := fields[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	fields[key] = value
	return value
}

func exprMatched(r *http.Request, exprs [][]any, status int) bool {
	if len(exprs) == 0 {
		return true
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range exprs {
		if len(condition) == 1 {
			if op, ok := condition[0].(string); ok {
				switch strings.ToUpper(op) {
				case "AND", "OR":
					pendingOp = strings.ToUpper(op)
				default:
					return false
				}
				continue
			}
		}

		matched := matchCondition(r, condition, status)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}

		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func matchCondition(r *http.Request, condition []any, status int) bool {
	if len(condition) != 3 {
		return false
	}

	left := fmt.Sprint(condition[0])
	op := fmt.Sprint(condition[1])
	right := fmt.Sprint(condition[2])
	actual := requestVar(r, left, status)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

func requestVar(r *http.Request, name string, status int) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code":
		if status > 0 {
			return strconv.Itoa(status)
		}
		return fmt.Sprint(apisixctx.GetRequestVar(r, "$status"))
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}

func (p *Plugin) Send(log map[string]any) {
	if p.config.Protocol == "http" || p.config.Protocol == "https" {
		p.sendHTTPBulk(log)
		return
	}

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

func (p *Plugin) sendHTTPBulk(log map[string]any) {
	payload, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal loggly message: %s", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, p.bulkEndpoint(), bytes.NewReader(payload))
	if err != nil {
		logger.Errorf("failed to build Loggly bulk request: %s", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LOGGLY-TAG", strings.Join(p.config.Tags, ","))

	client := &http.Client{
		Timeout: time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !*p.config.SSLVerify},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf("failed to send loggly bulk message: %s", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		logger.Errorf("failed to send loggly bulk message: status %d", resp.StatusCode)
	}
}

func (p *Plugin) bulkEndpoint() string {
	host := strings.TrimRight(p.config.Host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = p.config.Protocol + "://" + host
	}
	return host + "/bulk/" + p.config.CustomerToken + "/tag/bulk"
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
