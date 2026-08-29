package chaitin_waf

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	effective effectiveConfig

	picker nodePicker
	match  []*pluginexpr.Expression
}

const (
	priority = 2700
	name     = "chaitin-waf"

	HeaderChaitinWAF       = "X-APISIX-CHAITIN-WAF"
	HeaderChaitinWAFError  = "X-APISIX-CHAITIN-WAF-ERROR"
	HeaderChaitinWAFTime   = "X-APISIX-CHAITIN-WAF-TIME"
	HeaderChaitinWAFStatus = "X-APISIX-CHAITIN-WAF-STATUS"
	HeaderChaitinWAFAction = "X-APISIX-CHAITIN-WAF-ACTION"
	HeaderChaitinWAFServer = "X-APISIX-CHAITIN-WAF-SERVER"
)

const schema = `
{
  "type": "object",
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["off", "monitor", "block"]
    },
    "match": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "vars": {
            "type": "array"
          }
        }
      }
    },
    "append_waf_resp_header": {
      "type": "boolean",
      "default": true
    },
    "append_waf_debug_header": {
      "type": "boolean",
      "default": false
    },
    "config": {
      "type": "object",
      "properties": {
        "connect_timeout": {"type": "integer"},
        "send_timeout": {"type": "integer"},
        "read_timeout": {"type": "integer"},
        "req_body_size": {"type": "integer"},
        "keepalive_size": {"type": "integer"},
        "keepalive_timeout": {"type": "integer"},
        "real_client_ip": {"type": "boolean"}
      }
    }
  }
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "mode": {"type": "string", "enum": ["off", "monitor", "block"]},
    "nodes": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "host": {"type": "string"},
          "port": {"type": "integer", "minimum": 1, "default": 80}
        },
        "required": ["host"]
      }
    },
    "config": {"type": "object"}
  },
  "required": ["nodes"]
}
`

type Config struct {
	Mode                 string      `json:"mode,omitempty"`
	Match                []MatchRule `json:"match,omitempty"`
	AppendWAFRespHeader  *bool       `json:"append_waf_resp_header,omitempty"`
	AppendWAFDebugHeader *bool       `json:"append_waf_debug_header,omitempty"`
	Config               WAFConfig   `json:"config"`

	Nodes []Node `json:"nodes,omitempty"`
}

type Metadata struct {
	Mode   string    `json:"mode,omitempty"`
	Nodes  []Node    `json:"nodes"`
	Config WAFConfig `json:"config"`
}

type MatchRule struct {
	Vars any `json:"vars,omitempty"`
}

type Node struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

type WAFConfig struct {
	ConnectTimeout   int   `json:"connect_timeout,omitempty"`
	SendTimeout      int   `json:"send_timeout,omitempty"`
	ReadTimeout      int   `json:"read_timeout,omitempty"`
	ReqBodySize      int   `json:"req_body_size,omitempty"`
	KeepaliveSize    int   `json:"keepalive_size,omitempty"`
	KeepaliveTimeout int   `json:"keepalive_timeout,omitempty"`
	RealClientIP     *bool `json:"real_client_ip,omitempty"`
}

type wafDecision struct {
	Status          int         `json:"status"`
	EventID         string      `json:"event_id,omitempty"`
	Action          string      `json:"action,omitempty"`
	ResponseHeaders http.Header `json:"-"`
}

type effectiveConfig struct {
	Mode   string
	Nodes  []Node
	Config WAFConfig
}

const unhealthyNodeCooldown = 5 * time.Minute

const (
	t1kTagHead        byte = 0x01
	t1kTagBody        byte = 0x02
	t1kTagExtra       byte = 0x03
	t1kTagVersion     byte = 0x20
	t1kTagExtraHeader byte = 0x23
	t1kTagExtraBody   byte = 0x24
	t1kTagMetadata    byte = 0x25
	t1kMaskFirst      byte = 0x40
	t1kMaskLast       byte = 0x80

	t1kHeaderSize           = 5
	t1kMaxResponseFrameSize = 1 << 20
	t1kMaxResponseSize      = 2 << 20
	t1kMaxResponseFrames    = 8
	t1kProtocolVersion      = "Proto:2\n"
)

type nodePicker struct {
	mu          sync.Mutex
	signature   string
	next        int
	unhealthyTo map[string]time.Time
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	routeConfig := p.config
	p.applyDefaults()
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("chaitin-waf metadata decode failed: %w", err)
	}
	p.effective = effectiveConfig{
		Mode:   "monitor",
		Nodes:  append([]Node(nil), metadata.Nodes...),
		Config: applyWAFConfigDefaults(metadata.Config),
	}
	if metadata.Mode != "" {
		p.effective.Mode = metadata.Mode
	}
	if routeConfig.Mode != "" {
		p.effective.Mode = routeConfig.Mode
	}
	if len(routeConfig.Nodes) > 0 {
		p.effective.Nodes = append([]Node(nil), routeConfig.Nodes...)
	}
	p.effective.Config = mergeWAFConfig(p.effective.Config, routeConfig.Config)
	p.match = p.match[:0]
	for index, rule := range p.config.Match {
		expression, err := pluginexpr.Compile(normalizeMatchVars(rule.Vars))
		if err != nil {
			return fmt.Errorf("chaitin-waf match %d vars validation failed: %w", index, err)
		}
		p.match = append(p.match, expression)
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		code, body, headers, responseHeaders := p.doAccess(r)
		for key, values := range responseHeaders {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if !*p.config.AppendWAFDebugHeader {
			delete(headers, HeaderChaitinWAFError)
			delete(headers, HeaderChaitinWAFServer)
		}
		if *p.config.AppendWAFRespHeader {
			for key, value := range headers {
				w.Header().Set(key, value)
			}
		}
		if code != 0 {
			w.WriteHeader(code)
			if body != "" {
				_, _ = w.Write([]byte(body))
			}
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Stop() {
	// T1K connections are request-scoped and closed by askWAF. Keep the
	// lifecycle hook so generation retirement remains harmless while a request
	// that already owns its plugin instance is still in flight.
}

func (p *Plugin) applyDefaults() {
	if p.config.AppendWAFRespHeader == nil {
		b := true
		p.config.AppendWAFRespHeader = &b
	}
	if p.config.AppendWAFDebugHeader == nil {
		b := false
		p.config.AppendWAFDebugHeader = &b
	}
	if p.config.Mode == "" {
		p.config.Mode = "monitor"
	}
	p.config.Config = applyWAFConfigDefaults(p.config.Config)
}

func applyWAFConfigDefaults(cfg WAFConfig) WAFConfig {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 1000
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 1000
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 1000
	}
	if cfg.ReqBodySize == 0 {
		cfg.ReqBodySize = 1024
	}
	if cfg.KeepaliveSize == 0 {
		cfg.KeepaliveSize = 256
	}
	if cfg.KeepaliveTimeout == 0 {
		cfg.KeepaliveTimeout = 60000
	}
	if cfg.RealClientIP == nil {
		b := true
		cfg.RealClientIP = &b
	}
	return cfg
}

func (p *Plugin) doAccess(r *http.Request) (int, string, map[string]string, http.Header) {
	headers := map[string]string{}
	effective := p.effective
	if len(effective.Nodes) == 0 {
		headers[HeaderChaitinWAF] = "err"
		headers[HeaderChaitinWAFError] = "missing metadata"
		return http.StatusInternalServerError, "", headers, nil
	}

	node, ok := p.picker.pick(effective.Nodes)
	if !ok {
		headers[HeaderChaitinWAF] = "unhealthy"
		headers[HeaderChaitinWAFError] = "no healthy nodes"
		return http.StatusInternalServerError, "", headers, nil
	}
	headers[HeaderChaitinWAFServer] = node.hostPort()

	if effective.Mode == "off" {
		headers[HeaderChaitinWAF] = "off"
		return 0, "", headers, nil
	}
	if !p.matches(r) {
		headers[HeaderChaitinWAF] = "no"
		return 0, "", headers, nil
	}

	decision, elapsed, err := p.askWAF(r, node, effective.Config)
	headers[HeaderChaitinWAFTime] = fmt.Sprintf("%.0f", elapsed.Seconds()*1000)
	if err != nil {
		p.picker.markFailure(node)
		headers[HeaderChaitinWAF] = "waf-err"
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			headers[HeaderChaitinWAF] = "timeout"
		}
		headers[HeaderChaitinWAFError] = err.Error()
		if effective.Mode == "monitor" {
			return 0, "", headers, nil
		}
		return http.StatusInternalServerError, "", headers, nil
	}
	p.picker.markSuccess(node)

	headers[HeaderChaitinWAF] = "yes"
	headers[HeaderChaitinWAFAction] = "pass"
	if decision.Status == 0 {
		decision.Status = http.StatusOK
	}
	headers[HeaderChaitinWAFStatus] = strconv.Itoa(decision.Status)

	if decision.Status != http.StatusOK {
		headers[HeaderChaitinWAFAction] = "reject"
		if effective.Mode == "monitor" {
			return 0, "", headers, nil
		}
		if _, ok := util.TerminalStatus(decision.Status); !ok {
			headers[HeaderChaitinWAF] = "waf-err"
			return http.StatusInternalServerError, "", headers, nil
		}
		return decision.Status,
			fmt.Sprintf(
				`{"code": %d, "success":false, "message": "blocked by Chaitin SafeLine Web Application Firewall", "event_id": "%s"}`+"\n",
				decision.Status,
				decision.EventID,
			),
			headers,
			decision.ResponseHeaders
	}

	if effective.Mode == "block" {
		return 0, "", headers, decision.ResponseHeaders
	}
	return 0, "", headers, nil
}

func mergeWAFConfig(baseConfig, override WAFConfig) WAFConfig {
	if override.ConnectTimeout != 0 {
		baseConfig.ConnectTimeout = override.ConnectTimeout
	}
	if override.SendTimeout != 0 {
		baseConfig.SendTimeout = override.SendTimeout
	}
	if override.ReadTimeout != 0 {
		baseConfig.ReadTimeout = override.ReadTimeout
	}
	if override.ReqBodySize != 0 {
		baseConfig.ReqBodySize = override.ReqBodySize
	}
	if override.KeepaliveSize != 0 {
		baseConfig.KeepaliveSize = override.KeepaliveSize
	}
	if override.KeepaliveTimeout != 0 {
		baseConfig.KeepaliveTimeout = override.KeepaliveTimeout
	}
	if override.RealClientIP != nil {
		baseConfig.RealClientIP = override.RealClientIP
	}
	return baseConfig
}

func (p *Plugin) askWAF(r *http.Request, node Node, cfg WAFConfig) (wafDecision, time.Duration, error) {
	body, err := apisixctx.ReadRequestBody(r)
	if err != nil {
		return wafDecision{}, 0, err
	}
	inspectionBody := body
	limit := cfg.ReqBodySize * 1024
	if limit <= 0 {
		inspectionBody = inspectionBody[:0]
	} else if len(inspectionBody) > limit {
		inspectionBody = inspectionBody[:limit]
	}

	header, err := buildT1KRequestHeader(r)
	if err != nil {
		return wafDecision{}, 0, err
	}
	extra, err := buildT1KExtra(r, *cfg.RealClientIP)
	if err != nil {
		return wafDecision{}, 0, err
	}

	start := time.Now()
	connection, err := (&net.Dialer{
		Timeout: time.Duration(cfg.ConnectTimeout) * time.Millisecond,
	}).DialContext(r.Context(), "tcp", node.hostPort())
	elapsed := time.Since(start)
	if err != nil {
		return wafDecision{}, elapsed, err
	}
	defer func() { _ = connection.Close() }()

	if err := setT1KDeadline(connection, r, cfg.SendTimeout, true); err != nil {
		return wafDecision{}, elapsed, err
	}
	if err := writeT1KFrame(connection, t1kTagHead|t1kMaskFirst, header); err != nil {
		return wafDecision{}, time.Since(start), fmt.Errorf("send T1K HEAD: %w", err)
	}
	if requestHasBody(r) {
		if err := writeT1KFrame(connection, t1kTagBody, inspectionBody); err != nil {
			return wafDecision{}, time.Since(start), fmt.Errorf("send T1K BODY: %w", err)
		}
	}
	if err := writeT1KFrame(connection, t1kTagVersion, []byte(t1kProtocolVersion)); err != nil {
		return wafDecision{}, time.Since(start), fmt.Errorf("send T1K VERSION: %w", err)
	}
	if err := writeT1KFrame(connection, t1kTagExtra|t1kMaskLast, extra); err != nil {
		return wafDecision{}, time.Since(start), fmt.Errorf("send T1K EXTRA: %w", err)
	}

	if err := setT1KDeadline(connection, r, cfg.ReadTimeout, false); err != nil {
		return wafDecision{}, time.Since(start), err
	}
	decision, err := readT1KDecision(connection)
	return decision, time.Since(start), err
}

func buildT1KRequestHeader(r *http.Request) ([]byte, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("build T1K HEAD: request is nil")
	}
	if strings.ContainsAny(r.Method, "\r\n") || strings.ContainsAny(r.URL.RequestURI(), "\r\n") ||
		strings.ContainsAny(r.Host, "\r\n") {
		return nil, fmt.Errorf("build T1K HEAD: invalid request line")
	}

	var buffer bytes.Buffer
	version := fmt.Sprintf("HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor)
	if r.ProtoMajor == 0 {
		version = "HTTP/1.1"
	}
	fmt.Fprintf(&buffer, "%s %s %s\r\n", r.Method, r.URL.RequestURI(), version)
	fmt.Fprintf(&buffer, "Host: %s\r\n", r.Host)
	if r.ContentLength > 0 && r.Header.Get("Content-Length") == "" {
		fmt.Fprintf(&buffer, "Content-Length: %d\r\n", r.ContentLength)
	}

	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !validT1KHeaderName(key) {
			return nil, fmt.Errorf("build T1K HEAD: invalid header name %q", key)
		}
		for _, value := range r.Header.Values(key) {
			if strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("build T1K HEAD: invalid value for header %q", key)
			}
			fmt.Fprintf(&buffer, "%s: %s\r\n", key, value)
		}
	}
	buffer.WriteString("\r\n")
	if buffer.Len() > t1kMaxResponseFrameSize {
		return nil, fmt.Errorf("build T1K HEAD: %d bytes exceeds %d-byte limit", buffer.Len(), t1kMaxResponseFrameSize)
	}
	return buffer.Bytes(), nil
}

func buildT1KExtra(r *http.Request, realClientIP bool) ([]byte, error) {
	remoteAddr := clientIP(r, realClientIP)
	_, remotePort, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, fmt.Errorf("build T1K EXTRA: invalid remote address: %w", err)
	}
	localAddr, localPort := "", ""
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		localAddr, localPort, err = net.SplitHostPort(local.String())
	}
	if err != nil {
		return nil, fmt.Errorf("build T1K EXTRA: invalid local address: %w", err)
	}
	if localAddr == "" || localPort == "" {
		// net/http always installs LocalAddrContextKey for accepted server
		// requests. A deterministic unspecified address keeps internal/test
		// requests protocol-valid without misrepresenting the remote client.
		localAddr, localPort = "0.0.0.0", "0"
	}

	uuid, err := newT1KUUID()
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	serverName := r.Host
	if host, _, splitErr := net.SplitHostPort(r.Host); splitErr == nil {
		serverName = host
	}
	started := time.Now()
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil && !lifecycle.StartedAt().IsZero() {
		started = lifecycle.StartedAt()
	}
	proxyName, err := os.Hostname()
	if err != nil || proxyName == "" {
		return nil, fmt.Errorf("build T1K EXTRA: resolve proxy name: %w", err)
	}

	extra := fmt.Sprintf(
		"UUID:%s\nRemoteAddr:%s\nRemotePort:%s\nLocalAddr:%s\nLocalPort:%s\n"+
			"Scheme:%s\nServerName:%s\nProxyName:%s\nReqBeginTime:%d\nHasRspIfOK:n\nHasRspIfBlock:n\n",
		uuid, remoteAddr, remotePort, localAddr, localPort, scheme, serverName, proxyName,
		started.UnixMicro(),
	)
	return []byte(extra), nil
}

func newT1KUUID() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("build T1K EXTRA UUID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16],
	), nil
}

func requestHasBody(r *http.Request) bool {
	return r.Body != nil && r.Body != http.NoBody || r.ContentLength != 0 || len(r.TransferEncoding) > 0
}

func setT1KDeadline(connection net.Conn, r *http.Request, timeoutMS int, write bool) error {
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	if contextDeadline, ok := r.Context().Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if write {
		return connection.SetWriteDeadline(deadline)
	}
	return connection.SetReadDeadline(deadline)
}

func writeT1KFrame(writer io.Writer, tag byte, payload []byte) error {
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return fmt.Errorf("T1K frame payload is too large: %d", len(payload))
	}
	var header [t1kHeaderSize]byte
	header[0] = tag
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeT1KAll(writer, header[:]); err != nil {
		return err
	}
	return writeT1KAll(writer, payload)
}

func writeT1KAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func readT1KDecision(reader io.Reader) (wafDecision, error) {
	decision := wafDecision{ResponseHeaders: make(http.Header)}
	lastRank := -1
	totalSize := 0
	finished := false
	seen := make(map[byte]struct{})

	for frameIndex := range t1kMaxResponseFrames {
		var header [t1kHeaderSize]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return wafDecision{}, fmt.Errorf("read T1K response header: %w", err)
		}
		rawTag := header[0]
		if frameIndex == 0 {
			if rawTag&t1kMaskFirst == 0 {
				return wafDecision{}, fmt.Errorf("T1K response first frame lacks FIRST mask")
			}
		} else if rawTag&t1kMaskFirst != 0 {
			return wafDecision{}, fmt.Errorf("T1K response frame %d unexpectedly has FIRST mask", frameIndex)
		}

		tag := rawTag &^ (t1kMaskFirst | t1kMaskLast)
		rank, ok := t1kResponseTagRank(tag)
		if !ok {
			return wafDecision{}, fmt.Errorf("T1K response has unknown tag 0x%02x", tag)
		}
		if frameIndex == 0 && tag != t1kTagHead {
			return wafDecision{}, fmt.Errorf("T1K response first tag is 0x%02x, want HEAD", tag)
		}
		if _, duplicate := seen[tag]; duplicate {
			return wafDecision{}, fmt.Errorf("T1K response repeats tag 0x%02x", tag)
		}
		if rank <= lastRank {
			return wafDecision{}, fmt.Errorf("T1K response tag 0x%02x is out of order", tag)
		}
		seen[tag] = struct{}{}
		lastRank = rank

		length := binary.LittleEndian.Uint32(header[1:])
		if length > t1kMaxResponseFrameSize {
			return wafDecision{}, fmt.Errorf(
				"T1K response frame length %d exceeds %d-byte limit",
				length,
				t1kMaxResponseFrameSize,
			)
		}
		totalSize += int(length)
		if totalSize > t1kMaxResponseSize {
			return wafDecision{}, fmt.Errorf("T1K response exceeds %d-byte total limit", t1kMaxResponseSize)
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return wafDecision{}, fmt.Errorf("read T1K response tag 0x%02x payload: %w", tag, err)
		}

		switch tag {
		case t1kTagHead:
			decision.Action = string(payload)
			if decision.Action != "." && decision.Action != "?" {
				return wafDecision{}, fmt.Errorf("T1K response has unknown action %q", decision.Action)
			}
		case t1kTagBody:
			status, err := strconv.Atoi(string(payload))
			if err != nil || status < 100 || status > 599 {
				return wafDecision{}, fmt.Errorf("T1K response has invalid status %q", payload)
			}
			decision.Status = status
		case t1kTagMetadata:
			// SafeLine metadata is diagnostic only. It is an allowed v2 frame,
			// but authorization is derived from the action/status/event frames.
		case t1kTagExtraHeader:
			parsed, err := parseT1KResponseHeaders(payload)
			if err != nil {
				return wafDecision{}, err
			}
			decision.ResponseHeaders = parsed
		case t1kTagExtraBody:
			decision.EventID = parseT1KEventID(string(payload))
			if decision.EventID == "" {
				return wafDecision{}, fmt.Errorf("T1K response EXTRA_BODY lacks a valid event_id")
			}
		}

		if rawTag&t1kMaskLast != 0 {
			finished = true
			break
		}
	}
	if !finished {
		return wafDecision{}, fmt.Errorf("T1K response did not terminate within %d frames", t1kMaxResponseFrames)
	}
	if decision.Action == "?" {
		if decision.Status == 0 || decision.Status == http.StatusOK || decision.EventID == "" {
			return wafDecision{}, fmt.Errorf("T1K blocked response is missing status or event_id")
		}
	} else if decision.Status != 0 && decision.Status != http.StatusOK {
		return wafDecision{}, fmt.Errorf("T1K pass response has non-200 status %d", decision.Status)
	}
	return decision, nil
}

func t1kResponseTagRank(tag byte) (int, bool) {
	switch tag {
	case t1kTagHead:
		return 0, true
	case t1kTagBody:
		return 1, true
	case t1kTagMetadata:
		return 2, true
	case t1kTagExtraHeader:
		return 3, true
	case t1kTagExtraBody:
		return 4, true
	default:
		return 0, false
	}
}

func parseT1KResponseHeaders(payload []byte) (http.Header, error) {
	headers := make(http.Header)
	for line := range strings.SplitSeq(string(payload), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !validT1KHeaderName(key) || strings.ContainsAny(value, "\r\x00") {
			return nil, fmt.Errorf("T1K response has malformed header line %q", line)
		}
		headers.Add(key, strings.TrimSpace(value))
	}
	return headers, nil
}

func validT1KHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := range len(name) {
		character := name[index]
		if !isT1KHeaderNameCharacter(character) {
			return false
		}
	}
	return true
}

func parseT1KEventID(body string) string {
	const prefix = "<!-- event_id: "
	const suffix = " -->"
	if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, suffix) {
		return ""
	}
	value := strings.TrimSuffix(strings.TrimPrefix(body, prefix), suffix)
	if before, _, found := strings.Cut(value, " TYPE: "); found {
		value = before
	}
	if value == "" {
		return ""
	}
	for _, character := range value {
		if !isT1KAlphaNumeric(character) {
			return ""
		}
	}
	return value
}

func isT1KHeaderNameCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character))
}

func isT1KAlphaNumeric(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func (p *Plugin) matches(r *http.Request) bool {
	if len(p.match) == 0 {
		return true
	}
	for _, expression := range p.match {
		if expression.Eval(func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}) {
			return true
		}
	}
	return false
}

func normalizeMatchVars(vars any) []any {
	switch typed := vars.(type) {
	case []any:
		return typed
	case [][]any:
		values := make([]any, len(typed))
		for i, expression := range typed {
			values[i] = expression
		}
		return values
	default:
		return nil
	}
}

func clientIP(r *http.Request, realClientIP bool) string {
	if realClientIP {
		return apisixctx.EffectiveRemoteIP(r)
	}
	return apisixctx.PeerRemoteIP(r)
}

func (n Node) hostPort() string {
	port := n.Port
	if port == 0 {
		port = 80
	}
	return net.JoinHostPort(n.Host, strconv.Itoa(port))
}

func (p *nodePicker) pick(nodes []Node) (Node, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	signature := nodesSignature(nodes)
	if signature != p.signature {
		p.signature = signature
		p.next = 0
		p.unhealthyTo = make(map[string]time.Time)
	}
	if len(nodes) == 0 {
		return Node{}, false
	}
	if p.unhealthyTo == nil {
		p.unhealthyTo = make(map[string]time.Time)
	}
	now := time.Now()
	for offset := range nodes {
		index := (p.next + offset) % len(nodes)
		node := nodes[index]
		key := node.hostPort()
		if until, ok := p.unhealthyTo[key]; ok {
			if until.After(now) {
				continue
			}
			delete(p.unhealthyTo, key)
		}
		p.next = (index + 1) % len(nodes)
		return node, true
	}
	return Node{}, false
}

func (p *nodePicker) markFailure(node Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unhealthyTo == nil {
		p.unhealthyTo = make(map[string]time.Time)
	}
	p.unhealthyTo[node.hostPort()] = time.Now().Add(unhealthyNodeCooldown)
}

func (p *nodePicker) markSuccess(node Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.unhealthyTo, node.hostPort())
}

func nodesSignature(nodes []Node) string {
	var builder strings.Builder
	for _, node := range nodes {
		builder.WriteString(node.hostPort())
		builder.WriteByte('|')
	}
	return builder.String()
}
