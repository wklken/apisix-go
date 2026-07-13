package dubbo_proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/dubbo-go-hessian2"
	"github.com/spf13/cast"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

const (
	defaultMultiplexCount        = 32
	maxDubboRetries              = 10
	maxDubboResponsePayload      = 8 * 1024 * 1024
	dubboMagicHigh               = 0xda
	dubboMagicLow                = 0xbb
	dubboHessian2RequestFlag     = 0xc2
	dubboHessian2Serialization   = 2
	dubboResponseOK              = 20
	dubboResponseWithException   = 0
	dubboResponseValue           = 1
	dubboResponseNullValue       = 2
	dubboResponseExceptionAttach = 3
	dubboResponseValueAttach     = 4
	dubboResponseNullAttach      = 5
)

var nextDubboRequestID atomic.Uint64

type targetSlot struct {
	semaphore chan struct{}
}

var targetSlots sync.Map

func loadMultiplexCount() (int, error) {
	if appconfig.GlobalConfig == nil || appconfig.GlobalConfig.PluginAttr == nil {
		return defaultMultiplexCount, nil
	}
	attr := appconfig.GlobalConfig.PluginAttr[name]
	if attr == nil {
		return defaultMultiplexCount, nil
	}
	raw, ok := attr["upstream_multiplex_count"]
	if !ok {
		return defaultMultiplexCount, nil
	}
	count := cast.ToInt(raw)
	if count < 1 {
		return 0, fmt.Errorf("dubbo-proxy upstream_multiplex_count must be at least 1")
	}
	return count, nil
}

func acquireTargetSlot(ctx context.Context, target string, limit int) (bool, func()) {
	if limit < 1 {
		limit = defaultMultiplexCount
	}
	key := target + "\x00" + strconv.Itoa(limit)
	value, _ := targetSlots.LoadOrStore(key, &targetSlot{semaphore: make(chan struct{}, limit)})
	slot := value.(*targetSlot)
	select {
	case slot.semaphore <- struct{}{}:
		return true, func() { <-slot.semaphore }
	case <-ctx.Done():
		return false, func() {}
	}
}

func (p *Plugin) ServeDubbo(w http.ResponseWriter, r *http.Request, target string) {
	ServeDubbo(w, r, target, p.config)
}

func ServeDubbo(w http.ResponseWriter, r *http.Request, target string, cfg Config) {
	frame, err := buildDubboRequest(r, cfg)
	if err != nil {
		writeDubboError(w, http.StatusBadRequest, "failed to build Dubbo request: "+err.Error())
		return
	}
	result := serveDubboAttempt(r, target, cfg, frame)
	reportDubboOutcome(r, result)
	writeDubboAttemptResult(w, r, result)
}

// ServeDubboWithRetries retries only failures that happen before any request
// bytes are written. A Dubbo invocation may be non-idempotent, so a timeout or
// malformed response after a successful write must not issue it again.
func ServeDubboWithRetries(
	w http.ResponseWriter,
	r *http.Request,
	nextTarget func() (string, error),
	cfg Config,
	retries int,
) {
	frame, err := buildDubboRequest(r, cfg)
	if err != nil {
		writeDubboError(w, http.StatusBadRequest, "failed to build Dubbo request: "+err.Error())
		return
	}

	attempts := max(retries+1, 1)
	attempts = min(attempts, maxDubboRetries+1)

	var result dubboAttemptResult
	for attempt := 0; attempt < attempts; attempt++ {
		target, targetErr := nextTarget()
		if targetErr != nil {
			result.err = fmt.Errorf("failed to select upstream target: %w", targetErr)
			break
		}
		result = serveDubboAttempt(r, target, cfg, frame)
		reportDubboOutcome(r, result)
		if result.err == nil || !result.retryable {
			break
		}
		if r.Context().Err() != nil {
			break
		}
	}
	writeDubboAttemptResult(w, r, result)
}

type dubboAttemptResult struct {
	response  map[string]any
	err       error
	retryable bool
}

func reportDubboOutcome(r *http.Request, result dubboAttemptResult) {
	if result.err == nil {
		pxy.ReportHTTPOutcome(r, dubboResponseStatus(result.response))
		return
	}
	if r.Context().Err() != nil {
		return
	}
	var netErr net.Error
	pxy.ReportTCPFailureOutcome(r, errors.Is(result.err, context.DeadlineExceeded) ||
		(errors.As(result.err, &netErr) && netErr.Timeout()))
}

func dubboResponseStatus(response map[string]any) int {
	if value, ok := response["status"]; ok {
		if status, ok := hessianInteger(value); ok && status >= 100 && status <= 599 {
			return int(status)
		}
	}
	return http.StatusOK
}

func serveDubboAttempt(r *http.Request, target string, cfg Config, frame []byte) dubboAttemptResult {
	acquired, release := acquireTargetSlot(r.Context(), target, cfg.MultiplexCount)
	if !acquired {
		return dubboAttemptResult{err: fmt.Errorf("dubbo upstream concurrency limit was canceled")}
	}
	defer release()

	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(r.Context(), "tcp", target)
	if err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to connect to upstream: %w", err),
			retryable: true,
		}
	}
	defer func() { _ = conn.Close() }()
	stopClose := context.AfterFunc(r.Context(), func() { _ = conn.Close() })
	defer stopClose()

	if err := conn.SetWriteDeadline(dubboDeadline(r.Context(), 30*time.Second)); err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to set upstream write deadline: %w", err),
			retryable: true,
		}
	}
	written, err := conn.Write(frame)
	if err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to send Dubbo request: %w", err),
			retryable: written == 0,
		}
	}
	if written != len(frame) {
		return dubboAttemptResult{err: io.ErrShortWrite}
	}
	if err := conn.SetReadDeadline(dubboDeadline(r.Context(), 30*time.Second)); err != nil {
		return dubboAttemptResult{err: fmt.Errorf("failed to set upstream read deadline: %w", err)}
	}

	response, err := readDubboResponse(conn)
	if err != nil {
		return dubboAttemptResult{err: fmt.Errorf("failed to read Dubbo response: %w", err)}
	}
	return dubboAttemptResult{response: response}
}

func writeDubboAttemptResult(w http.ResponseWriter, r *http.Request, result dubboAttemptResult) {
	if result.err != nil {
		writeDubboError(w, dubboErrorStatus(r.Context(), result.err), result.err.Error())
		return
	}
	if err := writeDubboHTTPResponse(w, result.response); err != nil {
		writeDubboError(w, http.StatusBadGateway, "invalid Dubbo response: "+err.Error())
	}
}

func buildDubboRequest(r *http.Request, cfg Config) ([]byte, error) {
	body, err := readDubboRequestBody(r)
	if err != nil {
		return nil, err
	}

	httpContext := make(map[string]any, len(r.Header)+2)
	for name, values := range r.Header {
		key := strings.ToLower(name)
		if len(values) == 1 {
			httpContext[key] = values[0]
			continue
		}
		copyValues := append([]string(nil), values...)
		httpContext[key] = copyValues
	}
	if r.Host != "" {
		if _, exists := httpContext["host"]; !exists {
			httpContext["host"] = r.Host
		}
	}
	httpContext["body"] = body

	encoder := hessian.NewEncoder()
	values := []any{
		"2.0.2",
		cfg.ServiceName,
		cfg.ServiceVersion,
		cfg.Method,
		"Ljava/util/Map;",
		httpContext,
		map[string]string{},
	}
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			return nil, err
		}
	}
	payload := encoder.Buffer()
	frame := make([]byte, 16+len(payload))
	frame[0], frame[1] = dubboMagicHigh, dubboMagicLow
	frame[2] = dubboHessian2RequestFlag
	binary.BigEndian.PutUint64(frame[4:12], nextDubboRequestID.Add(1))
	binary.BigEndian.PutUint32(frame[12:16], uint32(len(payload)))
	copy(frame[16:], payload)
	return frame, nil
}

func readDubboResponse(conn net.Conn) (map[string]any, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	if header[0] != dubboMagicHigh || header[1] != dubboMagicLow {
		return nil, fmt.Errorf("unexpected Dubbo response magic %x%02x", header[0], header[1])
	}
	if header[2]&0x80 != 0 {
		return nil, fmt.Errorf("dubbo upstream returned a request frame")
	}
	if header[2]&0x1f != dubboHessian2Serialization {
		return nil, fmt.Errorf("unsupported Dubbo serialization id %d", header[2]&0x1f)
	}
	if header[3] != dubboResponseOK {
		return nil, fmt.Errorf("dubbo response status %d", header[3])
	}
	payloadLength := binary.BigEndian.Uint32(header[12:16])
	if payloadLength == 0 || payloadLength > maxDubboResponsePayload {
		return nil, fmt.Errorf("dubbo response payload length %d is outside the supported range", payloadLength)
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}

	decoder := hessian.NewDecoder(payload)
	responseType, err := decoder.Decode()
	if err != nil {
		return nil, err
	}
	responseCode, ok := hessianInteger(responseType)
	if !ok {
		return nil, fmt.Errorf("invalid Dubbo response type %T", responseType)
	}
	switch responseCode {
	case dubboResponseWithException, dubboResponseExceptionAttach:
		exception, err := decoder.Decode()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("dubbo provider exception: %v", exception)
	case dubboResponseNullValue, dubboResponseNullAttach:
		return map[string]any{}, nil
	case dubboResponseValue, dubboResponseValueAttach:
		value, err := decoder.Decode()
		if err != nil {
			return nil, err
		}
		return hessianStringMap(value)
	default:
		return nil, fmt.Errorf("unsupported Dubbo response type %d", responseCode)
	}
}

func writeDubboHTTPResponse(w http.ResponseWriter, response map[string]any) error {
	body, err := dubboBodyBytes(response["body"])
	if err != nil {
		return err
	}
	status := http.StatusOK
	if value, ok := response["status"]; ok {
		parsed, ok := hessianInteger(value)
		if !ok || parsed < 100 || parsed > 599 {
			return fmt.Errorf("invalid HTTP status value %v", value)
		}
		status = int(parsed)
	}

	for key, value := range response {
		if key == "status" || key == "body" {
			continue
		}
		w.Header().Set(key, hessianHeaderValue(value))
	}
	w.WriteHeader(status)
	if body != nil {
		_, _ = w.Write(body)
	}
	return nil
}

func dubboBodyBytes(value any) ([]byte, error) {
	switch body := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return append([]byte(nil), body...), nil
	case string:
		return []byte(body), nil
	default:
		return nil, fmt.Errorf("unsupported body type %T", value)
	}
}

func hessianStringMap(value any) (map[string]any, error) {
	result := make(map[string]any)
	switch typed := value.(type) {
	case map[any]any:
		for key, item := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("dubbo response map key %T is not a string", key)
			}
			result[name] = item
		}
	case map[string]any:
		maps.Copy(result, typed)
	default:
		return nil, fmt.Errorf("dubbo response is %T, want a map", value)
	}
	return result, nil
}

func hessianInteger(value any) (int32, bool) {
	switch typed := value.(type) {
	case int:
		return int32(typed), true
	case int8:
		return int32(typed), true
	case int16:
		return int32(typed), true
	case int32:
		return typed, true
	case int64:
		return int32(typed), int64(int32(typed)) == typed
	case uint8:
		return int32(typed), true
	case uint16:
		return int32(typed), true
	case uint32:
		return int32(typed), uint32(int32(typed)) == typed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 32)
		return int32(parsed), err == nil
	default:
		return 0, false
	}
}

func hessianHeaderValue(value any) string {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		return fmt.Sprint(value)
	}
}

func readDubboRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func dubboDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func dubboErrorStatus(ctx context.Context, err error) int {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func writeDubboError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
