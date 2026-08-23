package ai_rate_limiting

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// BenchmarkAIRateLimit measures the per-request Redis decision path: quota
// check, quota-header snapshot, and response-token charge against a local
// RESP fixture. The fixture counts GET/PTTL/EVAL round trips so the benchmark
// can fail closed if a request ever exceeds the current command budget.
func BenchmarkAIRateLimit(b *testing.B) {
	fixture := newBenchRedisFixture(b)
	p := &Plugin{config: Config{
		Limit:         1 << 30,
		TimeWindow:    60,
		Policy:        "redis",
		RedisHost:     fixture.host(),
		RedisPort:     fixture.port(),
		LimitStrategy: "total_tokens",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	b.Cleanup(p.Stop)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fixture.resetCommands()
		rr := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("response code = %d, want 200", rr.Code)
		}
	}
	b.StopTimer()

	commands := fixture.commands() / int64(b.N)
	if commands > 4 {
		b.Fatalf("redis commands per request = %d, want at most 4", commands)
	}
}

// benchRedisFixture is a minimal RESP server that counts GET/PTTL/EVAL
// commands and emulates the AI rate-limit charge and snapshot scripts.
type benchRedisFixture struct {
	listener net.Listener

	mu       sync.Mutex
	count    atomic.Int64
	values   map[string]string
	integers map[string]int64
	expiries map[string]time.Time
}

func newBenchRedisFixture(b *testing.B) *benchRedisFixture {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen Redis fixture: %v", err)
	}
	fixture := &benchRedisFixture{
		listener: listener,
		values:   make(map[string]string),
		integers: make(map[string]int64),
		expiries: make(map[string]time.Time),
	}
	go fixture.serve()
	b.Cleanup(func() { _ = listener.Close() })
	return fixture
}

func (f *benchRedisFixture) host() string {
	host, _, _ := net.SplitHostPort(f.listener.Addr().String())
	return host
}

func (f *benchRedisFixture) port() int {
	_, port, _ := net.SplitHostPort(f.listener.Addr().String())
	parsed, _ := strconv.Atoi(port)
	return parsed
}

func (f *benchRedisFixture) resetCommands() {
	f.count.Store(0)
}

func (f *benchRedisFixture) commands() int64 {
	return f.count.Load()
}

func (f *benchRedisFixture) serve() {
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.serveConnection(connection)
	}
}

func (f *benchRedisFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for {
		command, err := readBenchRESPCommand(reader)
		if err != nil {
			return
		}
		if err := f.writeResponse(connection, command); err != nil {
			return
		}
	}
}

func (f *benchRedisFixture) writeResponse(writer io.Writer, command []string) error {
	if len(command) == 0 {
		return writeBenchRESPError(writer, "empty command")
	}
	switch strings.ToUpper(command[0]) {
	case "PING":
		return writeBenchSimpleRESP(writer, "PONG")
	case "HELLO":
		return writeBenchRESPError(writer, "unknown command 'HELLO'")
	case "AUTH", "SELECT", "CLIENT":
		return writeBenchSimpleRESP(writer, "OK")
	case "GET":
		f.count.Add(1)
		f.mu.Lock()
		value, ok := f.values[command[1]]
		if integer, integerOK := f.integers[command[1]]; integerOK {
			value = strconv.FormatInt(integer, 10)
			ok = true
		}
		f.mu.Unlock()
		if !ok {
			return writeBenchRESPNull(writer)
		}
		return writeBenchRESPBulk(writer, value)
	case "PTTL":
		f.count.Add(1)
		f.mu.Lock()
		expiry, ok := f.expiries[command[1]]
		f.mu.Unlock()
		if !ok {
			return writeBenchRESPInteger(writer, -2)
		}
		return writeBenchRESPInteger(writer, max(time.Until(expiry).Milliseconds(), 1))
	case "EVAL", "EVALSHA":
		f.count.Add(1)
		return f.writeEvalResponse(writer, command)
	default:
		return writeBenchRESPError(writer, "unsupported command "+command[0])
	}
}

func (f *benchRedisFixture) writeEvalResponse(writer io.Writer, command []string) error {
	script := command[1]
	if len(command) < 4 {
		return writeBenchRESPError(writer, "wrong number of arguments for EVAL")
	}
	key := command[3]
	if strings.Contains(script, `redis.call("INCRBY"`) {
		if len(command) < 6 {
			return writeBenchRESPError(writer, "wrong number of arguments for AI rate script")
		}
		tokens, err := strconv.ParseInt(command[4], 10, 64)
		if err != nil {
			return writeBenchRESPError(writer, "AI rate tokens are not an integer")
		}
		ttlMilliseconds, err := strconv.ParseInt(command[5], 10, 64)
		if err != nil || ttlMilliseconds <= 0 {
			return writeBenchRESPError(writer, "AI rate TTL is not a positive integer")
		}
		f.mu.Lock()
		f.integers[key] += tokens
		current := f.integers[key]
		expiry, hasExpiry := f.expiries[key]
		if !hasExpiry || !time.Now().Before(expiry) {
			expiry = time.Now().Add(time.Duration(ttlMilliseconds) * time.Millisecond)
			f.expiries[key] = expiry
		}
		ttl := max(time.Until(expiry).Milliseconds(), 1)
		f.mu.Unlock()
		return writeBenchRESPArray(writer, []string{strconv.FormatInt(current, 10), strconv.FormatInt(ttl, 10)})
	}
	// snapshot script: {value, ttl} with GET+PTTL
	f.mu.Lock()
	count := f.integers[key]
	if value, ok := f.values[key]; ok {
		count, _ = strconv.ParseInt(value, 10, 64)
	}
	expiry, hasExpiry := f.expiries[key]
	f.mu.Unlock()
	ttl := int64(0)
	if hasExpiry {
		ttl = max(time.Until(expiry).Milliseconds(), 0)
	}
	return writeBenchRESPIntegerArray(writer, count, ttl)
}

func readBenchRESPCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("invalid RESP command header %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || count < 0 || count > 1024 {
		return nil, fmt.Errorf("invalid RESP command count %q", line)
	}
	command := make([]string, 0, count)
	for range count {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) < 3 || line[0] != '$' {
			return nil, fmt.Errorf("invalid RESP bulk header %q", line)
		}
		length, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || length < 0 || length > 16<<20 {
			return nil, fmt.Errorf("invalid RESP bulk length %q", line)
		}
		value := make([]byte, length+2)
		if _, err := io.ReadFull(reader, value); err != nil {
			return nil, err
		}
		command = append(command, string(value[:length]))
	}
	return command, nil
}

func writeBenchSimpleRESP(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, "+"+value+"\r\n")
	return err
}

func writeBenchRESPError(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, "-ERR "+value+"\r\n")
	return err
}

func writeBenchRESPNull(writer io.Writer) error {
	_, err := io.WriteString(writer, "$-1\r\n")
	return err
}

func writeBenchRESPBulk(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, "$"+strconv.Itoa(len(value))+"\r\n"+value+"\r\n")
	return err
}

func writeBenchRESPInteger(writer io.Writer, value int64) error {
	_, err := io.WriteString(writer, ":"+strconv.FormatInt(value, 10)+"\r\n")
	return err
}

func writeBenchRESPArray(writer io.Writer, values []string) error {
	var builder strings.Builder
	builder.WriteString("*")
	builder.WriteString(strconv.Itoa(len(values)))
	builder.WriteString("\r\n")
	for _, value := range values {
		builder.WriteString("$")
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteString("\r\n")
		builder.WriteString(value)
		builder.WriteString("\r\n")
	}
	_, err := io.WriteString(writer, builder.String())
	return err
}

func writeBenchRESPIntegerArray(writer io.Writer, values ...int64) error {
	var builder strings.Builder
	builder.WriteString("*")
	builder.WriteString(strconv.Itoa(len(values)))
	builder.WriteString("\r\n")
	for _, value := range values {
		builder.WriteString(":")
		builder.WriteString(strconv.FormatInt(value, 10))
		builder.WriteString("\r\n")
	}
	_, err := io.WriteString(writer, builder.String())
	return err
}
