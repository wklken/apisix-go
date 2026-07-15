package pluginintegration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type redisFixture struct {
	kind      string
	listener  net.Listener
	expect    []NetworkAssertion
	received  chan []byte
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	sequence  sync.Mutex
	next      int
	stateMu   sync.Mutex
	values    map[string]string
	integers  map[string]int64
	hashes    map[string]map[string]string
	wg        sync.WaitGroup
}

func startRedisFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen Redis fixture: %w", err)
	}
	fixture := &redisFixture{
		kind:     spec.Kind,
		listener: listener,
		expect:   spec.NetworkExpect,
		received: make(chan []byte, len(spec.NetworkExpect)+1),
		errors:   make(chan error, len(spec.NetworkExpect)+1),
		done:     make(chan struct{}),
		values:   make(map[string]string),
		integers: make(map[string]int64),
		hashes:   make(map[string]map[string]string),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *redisFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.errors <- fmt.Errorf("accept Redis fixture connection: %w", err)
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.serveConnection(connection)
		}()
	}
}

func (f *redisFixture) serveConnection(connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			if err != io.EOF {
				f.errors <- fmt.Errorf("read Redis command: %w", err)
			}
			return
		}
		payload := []byte(strings.Join(command, " "))
		f.received <- payload
		if err := f.writeResponse(connection, command); err != nil {
			f.errors <- fmt.Errorf("write Redis response: %w", err)
			return
		}
	}
}

func (f *redisFixture) writeResponse(writer io.Writer, command []string) error {
	if len(command) == 0 {
		return writeRESPError(writer, "empty command")
	}
	switch strings.ToUpper(command[0]) {
	case "PING":
		return writeSimpleRESP(writer, "PONG")
	case "AUTH", "SELECT", "HELLO", "CLIENT", "READONLY", "READ_ONLY", "ASKING":
		return writeSimpleRESP(writer, "OK")
	case "QUIT":
		return writeSimpleRESP(writer, "OK")
	case "GET":
		if len(command) < 2 {
			return writeRESPError(writer, "wrong number of arguments for GET")
		}
		f.stateMu.Lock()
		value, ok := f.values[command[1]]
		f.stateMu.Unlock()
		if !ok {
			return writeRESPNull(writer)
		}
		return writeRESPBulk(writer, value)
	case "SET":
		if len(command) < 3 {
			return writeRESPError(writer, "wrong number of arguments for SET")
		}
		f.stateMu.Lock()
		for _, option := range command[3:] {
			if strings.EqualFold(option, "NX") {
				if _, exists := f.values[command[1]]; exists {
					f.stateMu.Unlock()
					return writeRESPNull(writer)
				}
			}
		}
		f.values[command[1]] = command[2]
		f.stateMu.Unlock()
		return writeSimpleRESP(writer, "OK")
	case "HSET":
		if len(command) < 4 || len(command[2:])%2 != 0 {
			return writeRESPError(writer, "wrong number of arguments for HSET")
		}
		f.stateMu.Lock()
		if f.hashes[command[1]] == nil {
			f.hashes[command[1]] = make(map[string]string)
		}
		added := int64(0)
		for i := 2; i < len(command); i += 2 {
			if _, exists := f.hashes[command[1]][command[i]]; !exists {
				added++
			}
			f.hashes[command[1]][command[i]] = command[i+1]
		}
		f.stateMu.Unlock()
		return writeRESPInteger(writer, added)
	case "HGET":
		if len(command) < 3 {
			return writeRESPError(writer, "wrong number of arguments for HGET")
		}
		f.stateMu.Lock()
		value, ok := f.hashes[command[1]][command[2]]
		f.stateMu.Unlock()
		if !ok {
			return writeRESPNull(writer)
		}
		return writeRESPBulk(writer, value)
	case "INCR", "INCRBY", "DECR", "DECRBY":
		return f.writeIntegerMutation(writer, command)
	case "DEL", "UNLINK":
		removed := int64(0)
		f.stateMu.Lock()
		for _, key := range command[1:] {
			if _, ok := f.values[key]; ok {
				delete(f.values, key)
				removed++
			}
			if _, ok := f.integers[key]; ok {
				delete(f.integers, key)
				removed++
			}
		}
		f.stateMu.Unlock()
		return writeRESPInteger(writer, removed)
	case "EXISTS":
		count := int64(0)
		f.stateMu.Lock()
		for _, key := range command[1:] {
			if _, ok := f.values[key]; ok {
				count++
			}
			if _, ok := f.integers[key]; ok {
				count++
			}
		}
		f.stateMu.Unlock()
		return writeRESPInteger(writer, count)
	case "EXPIRE", "PEXPIRE", "PERSIST":
		return writeRESPInteger(writer, 1)
	case "TTL", "PTTL":
		return writeRESPInteger(writer, -1)
	case "SCRIPT":
		if len(command) > 1 && strings.EqualFold(command[1], "LOAD") {
			return writeRESPBulk(writer, "fixture-script")
		}
		return writeSimpleRESP(writer, "OK")
	case "EVAL", "EVALSHA":
		return f.writeEvalResponse(writer, command)
	case "CLUSTER":
		return f.writeClusterResponse(writer, command)
	case "SENTINEL":
		return f.writeSentinelResponse(writer, command)
	default:
		return writeRESPError(writer, "unsupported command "+command[0])
	}
}

func (f *redisFixture) writeIntegerMutation(writer io.Writer, command []string) error {
	if len(command) < 2 {
		return writeRESPError(writer, "wrong number of arguments")
	}
	delta := int64(1)
	if len(command) > 2 {
		parsed, err := strconv.ParseInt(command[2], 10, 64)
		if err != nil {
			return writeRESPError(writer, "value is not an integer")
		}
		delta = parsed
	}
	if strings.EqualFold(command[0], "DECR") || strings.EqualFold(command[0], "DECRBY") {
		delta = -delta
	}
	f.stateMu.Lock()
	if current, ok := f.values[command[1]]; ok {
		if parsed, err := strconv.ParseInt(current, 10, 64); err == nil {
			f.integers[command[1]] = parsed
		}
		delete(f.values, command[1])
	}
	f.integers[command[1]] += delta
	value := f.integers[command[1]]
	f.stateMu.Unlock()
	return writeRESPInteger(writer, value)
}

func (f *redisFixture) writeEvalResponse(writer io.Writer, command []string) error {
	script := ""
	if len(command) > 1 {
		script = command[1]
	}
	if strings.Contains(script, "redis.call(\"INCR\"") && strings.Contains(script, "redis.call(\"DECR\"") {
		return writeRESPArray(writer, []string{"1", "0"})
	}
	return writeRESPInteger(writer, 1)
}

func (f *redisFixture) writeClusterResponse(writer io.Writer, command []string) error {
	if len(command) > 1 && strings.EqualFold(command[1], "SLOTS") {
		return writeRESPRaw(writer, "*1\r\n*3\r\n:0\r\n:16383\r\n*2\r\n$9\r\n127.0.0.1\r\n:"+f.port()+"\r\n")
	}
	return writeSimpleRESP(writer, "OK")
}

func (f *redisFixture) writeSentinelResponse(writer io.Writer, command []string) error {
	if len(command) > 1 && strings.EqualFold(command[1], "GET-MASTER-ADDR-BY-NAME") {
		return writeRESPArray(writer, []string{"127.0.0.1", f.port()})
	}
	return writeSimpleRESP(writer, "OK")
}

func (f *redisFixture) address() string { return f.listener.Addr().String() }

func (f *redisFixture) host() string {
	host, _, _ := net.SplitHostPort(f.address())
	return host
}

func (f *redisFixture) port() string {
	_, port, _ := net.SplitHostPort(f.address())
	return port
}

func (f *redisFixture) url() string { return "redis://" + f.address() }

func (f *redisFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
	})
}

func (f *redisFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for i, expected := range spec.NetworkExpect {
		select {
		case payload := <-f.received:
			if err := matchNetworkAssertion(expected, payload); err != nil {
				t.Errorf("fixture %s command %d: %v", spec.Name, i+1, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("fixture %s did not receive expected command %d", spec.Name, i+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	if extra := len(f.received); extra > 0 {
		t.Errorf("fixture %s received %d unexpected extra commands", spec.Name, extra)
	}
}

func TestRedisFixtureServesRESP(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis",
		Kind: "redis",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: stringPointer("PING")},
		}},
		NetworkRespond: []NetworkResponse{{Payload: "ignored"}},
	}
	fixture, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	defer fixture.close()
	connection, err := net.Dial("tcp", fixture.address())
	if err != nil {
		t.Fatalf("dial Redis fixture: %v", err)
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, "*1\r\n$4\r\nPING\r\n"); err != nil {
		t.Fatalf("write Redis command: %v", err)
	}
	response := make([]byte, len("+PONG\r\n"))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("read Redis response: %v", err)
	}
	if string(response) != "+PONG\r\n" {
		t.Fatalf("Redis response = %q, want +PONG", response)
	}
	fixture.assert(t, spec)
}

func TestRedisFixtureSupportsStatefulCommands(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-state",
		Kind: "redis",
		NetworkExpect: []NetworkAssertion{
			{Payload: &Matcher{Equals: stringPointer("SET quota 1 NX EX 60")}},
			{Payload: &Matcher{Equals: stringPointer("INCR quota")}},
			{Payload: &Matcher{Equals: stringPointer("HSET hash field 1")}},
			{Payload: &Matcher{Equals: stringPointer("HGET hash field")}},
		},
		NetworkRespond: make([]NetworkResponse, 4),
	}
	fixture, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	defer fixture.close()
	connection, err := net.Dial("tcp", fixture.address())
	if err != nil {
		t.Fatalf("dial Redis fixture: %v", err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	commands := [][]string{{"SET", "quota", "1", "NX", "EX", "60"}, {"INCR", "quota"}, {"HSET", "hash", "field", "1"}, {"HGET", "hash", "field"}}
	wantResponses := []string{"+OK\r\n", ":2\r\n", ":1\r\n", "$1\r\n1\r\n"}
	for i, command := range commands {
		if err := writeRESPCommand(connection, command); err != nil {
			t.Fatalf("write Redis command %d: %v", i+1, err)
		}
		response, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read Redis response %d: %v", i+1, err)
		}
		if strings.HasPrefix(wantResponses[i], "$") {
			body, err := io.ReadAll(io.LimitReader(reader, 3))
			if err != nil {
				t.Fatalf("read Redis bulk response %d: %v", i+1, err)
			}
			response += string(body)
		}
		if response != wantResponses[i] {
			t.Fatalf("Redis response %d = %q, want %q", i+1, response, wantResponses[i])
		}
	}
	fixture.assert(t, spec)
}

func writeRESPCommand(writer io.Writer, command []string) error {
	if err := writeRESPRaw(writer, "*"+strconv.Itoa(len(command))+"\r\n"); err != nil {
		return err
	}
	for _, value := range command {
		if err := writeRESPRaw(writer, "$"+strconv.Itoa(len(value))+"\r\n"+value+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
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
	for i := 0; i < count; i++ {
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

func writeSimpleRESP(writer io.Writer, value string) error {
	return writeRESPRaw(writer, "+"+value+"\r\n")
}

func writeRESPError(writer io.Writer, value string) error {
	return writeRESPRaw(writer, "-ERR "+value+"\r\n")
}

func writeRESPNull(writer io.Writer) error { return writeRESPRaw(writer, "$-1\r\n") }

func writeRESPBulk(writer io.Writer, value string) error {
	return writeRESPRaw(writer, "$"+strconv.Itoa(len(value))+"\r\n"+value+"\r\n")
}

func writeRESPInteger(writer io.Writer, value int64) error {
	return writeRESPRaw(writer, ":"+strconv.FormatInt(value, 10)+"\r\n")
}

func writeRESPArray(writer io.Writer, values []string) error {
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
	return writeRESPRaw(writer, builder.String())
}

func writeRESPRaw(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, value)
	return err
}
