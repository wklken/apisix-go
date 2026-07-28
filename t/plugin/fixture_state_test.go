package pluginintegration

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type redisFixture struct {
	kind      string
	spec      FixtureSpec
	listener  net.Listener
	expect    []NetworkAssertion
	received  chan []byte
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	stateMu   sync.Mutex
	values    map[string]string
	integers  map[string]int64
	hashes    map[string]map[string]string
	expiries  map[string]time.Time
	expirySet map[string]int
	auth      []RedisAuthAssertion
	wg        sync.WaitGroup
}

func startRedisFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen Redis fixture: %w", err)
	}
	receivedCapacity := len(spec.NetworkExpect) + 1
	if spec.Redis != nil && spec.Redis.AllowUnassertedCommands {
		receivedCapacity = 128
	}
	fixture := &redisFixture{
		kind:      spec.Kind,
		spec:      spec,
		listener:  listener,
		expect:    spec.NetworkExpect,
		received:  make(chan []byte, receivedCapacity),
		errors:    make(chan error, len(spec.NetworkExpect)+1),
		done:      make(chan struct{}),
		values:    make(map[string]string),
		integers:  make(map[string]int64),
		hashes:    make(map[string]map[string]string),
		expiries:  make(map[string]time.Time),
		expirySet: make(map[string]int),
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
		f.wg.Go(func() {
			f.serveConnection(connection)
		})
	}
}

func (f *redisFixture) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			if err != io.EOF {
				f.errors <- fmt.Errorf("read Redis command: %w", err)
			}
			return
		}
		f.recordAuthentication(command)
		allowUnasserted := f.spec.Redis != nil && f.spec.Redis.AllowUnassertedCommands
		if !f.ignoreNegotiation(command) && !allowUnasserted {
			payload := []byte(strings.Join(command, " "))
			f.received <- payload
		}
		if err := f.writeResponse(connection, command); err != nil {
			f.errors <- fmt.Errorf("write Redis response: %w", err)
			return
		}
	}
}

func (f *redisFixture) recordAuthentication(command []string) {
	var credential RedisAuthAssertion
	switch {
	case len(command) == 2 && strings.EqualFold(command[0], "AUTH"):
		credential.Password = command[1]
	case len(command) == 3 && strings.EqualFold(command[0], "AUTH"):
		credential.Username = command[1]
		credential.Password = command[2]
	case len(command) >= 5 && strings.EqualFold(command[0], "HELLO"):
		for i := 2; i+2 < len(command); i++ {
			if strings.EqualFold(command[i], "AUTH") {
				credential.Username = command[i+1]
				credential.Password = command[i+2]
				break
			}
		}
	}
	if credential.Password == "" {
		return
	}
	f.stateMu.Lock()
	f.auth = append(f.auth, credential)
	f.stateMu.Unlock()
}

func (f *redisFixture) ignoreNegotiation(command []string) bool {
	if len(command) == 0 || f.spec.Redis == nil || !f.spec.Redis.IgnoreNegotiation {
		return false
	}
	switch strings.ToUpper(command[0]) {
	case "HELLO", "AUTH", "SELECT", "CLIENT":
		return true
	default:
		return false
	}
}

func (f *redisFixture) writeResponse(writer io.Writer, command []string) error {
	if len(command) == 0 {
		return writeRESPError(writer, "empty command")
	}
	switch strings.ToUpper(command[0]) {
	case "PING":
		return writeSimpleRESP(writer, "PONG")
	case "HELLO":
		return writeRESPError(writer, "unknown command 'HELLO'")
	case "AUTH", "SELECT", "CLIENT", "READONLY", "READ_ONLY", "ASKING":
		return writeSimpleRESP(writer, "OK")
	case "QUIT":
		return writeSimpleRESP(writer, "OK")
	case "GET":
		if len(command) < 2 {
			return writeRESPError(writer, "wrong number of arguments for GET")
		}
		f.stateMu.Lock()
		value, ok := f.values[command[1]]
		if integer, integerOK := f.integers[command[1]]; integerOK {
			value = strconv.FormatInt(integer, 10)
			ok = true
		}
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
			delete(f.expiries, key)
			delete(f.expirySet, key)
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
	case "EXPIRE", "PEXPIRE":
		return f.writeExpiry(writer, command)
	case "PERSIST":
		if len(command) < 2 {
			return writeRESPError(writer, "wrong number of arguments for PERSIST")
		}
		f.stateMu.Lock()
		_, existed := f.expiries[command[1]]
		delete(f.expiries, command[1])
		f.stateMu.Unlock()
		if existed {
			return writeRESPInteger(writer, 1)
		}
		return writeRESPInteger(writer, 0)
	case "TTL", "PTTL":
		return f.writeTTL(writer, command)
	case "SCRIPT":
		if len(command) > 1 {
			switch {
			case strings.EqualFold(command[1], "LOAD"):
				return writeRESPBulk(writer, "fixture-script")
			case strings.EqualFold(command[1], "EXISTS"):
				return writeRESPRaw(writer, "*1\r\n:0\r\n")
			}
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
	if strings.Contains(script, "redis.call(\"INCR\"") &&
		strings.Contains(script, "redis.call(\"DECR\"") {
		return writeRESPArray(writer, []string{"1", "0"})
	}
	if strings.Contains(script, `redis.call("INCRBY"`) &&
		strings.Contains(script, `redis.call("PTTL"`) &&
		strings.Contains(script, `redis.call("PEXPIRE"`) {
		if len(command) < 6 {
			return writeRESPError(writer, "wrong number of arguments for AI rate script")
		}
		tokens, err := strconv.ParseInt(command[4], 10, 64)
		if err != nil {
			return writeRESPError(writer, "AI rate tokens are not an integer")
		}
		ttlMilliseconds, err := strconv.ParseInt(command[5], 10, 64)
		if err != nil || ttlMilliseconds <= 0 {
			return writeRESPError(writer, "AI rate TTL is not a positive integer")
		}
		key := command[3]
		f.stateMu.Lock()
		f.integers[key] += tokens
		current := f.integers[key]
		expiry, hasExpiry := f.expiries[key]
		if !hasExpiry || !time.Now().Before(expiry) {
			expiry = time.Now().Add(time.Duration(ttlMilliseconds) * time.Millisecond)
			f.expiries[key] = expiry
			f.expirySet[key]++
		}
		ttl := max(time.Until(expiry).Milliseconds(), 1)
		f.stateMu.Unlock()
		return writeRESPArray(writer, []string{strconv.FormatInt(current, 10), strconv.FormatInt(ttl, 10)})
	}
	return writeRESPInteger(writer, 1)
}

func (f *redisFixture) writeExpiry(writer io.Writer, command []string) error {
	if len(command) < 3 {
		return writeRESPError(writer, "wrong number of arguments for expiry")
	}
	value, err := strconv.ParseInt(command[2], 10, 64)
	if err != nil || value <= 0 {
		return writeRESPError(writer, "expiry is not a positive integer")
	}
	duration := time.Duration(value) * time.Second
	if strings.EqualFold(command[0], "PEXPIRE") {
		duration = time.Duration(value) * time.Millisecond
	}
	f.stateMu.Lock()
	_, stringExists := f.values[command[1]]
	_, integerExists := f.integers[command[1]]
	if stringExists || integerExists {
		f.expiries[command[1]] = time.Now().Add(duration)
		f.expirySet[command[1]]++
	}
	f.stateMu.Unlock()
	if stringExists || integerExists {
		return writeRESPInteger(writer, 1)
	}
	return writeRESPInteger(writer, 0)
}

func (f *redisFixture) writeTTL(writer io.Writer, command []string) error {
	if len(command) < 2 {
		return writeRESPError(writer, "wrong number of arguments for TTL")
	}
	f.stateMu.Lock()
	expiry, ok := f.expiries[command[1]]
	f.stateMu.Unlock()
	if !ok {
		return writeRESPInteger(writer, -1)
	}
	ttl := time.Until(expiry)
	if ttl <= 0 {
		return writeRESPInteger(writer, -2)
	}
	if strings.EqualFold(command[0], "PTTL") {
		return writeRESPInteger(writer, max(ttl.Milliseconds(), 1))
	}
	return writeRESPInteger(writer, max(int64(math.Ceil(ttl.Seconds())), 1))
}

func (f *redisFixture) writeClusterResponse(writer io.Writer, command []string) error {
	if len(command) > 1 && strings.EqualFold(command[1], "SLOTS") {
		return writeRESPRaw(
			writer,
			"*1\r\n*3\r\n:0\r\n:16383\r\n*2\r\n$9\r\n127.0.0.1\r\n:"+f.port()+"\r\n",
		)
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
	allowExtra := spec.Redis != nil && spec.Redis.AllowUnassertedCommands
	allowExtra = allowExtra || len(spec.NetworkExpect) == 1 && spec.NetworkExpect[0].Payload != nil &&
		spec.NetworkExpect[0].Payload.Matches != nil && *spec.NetworkExpect[0].Payload.Matches == ".*"
	if !allowExtra {
		if extra := len(f.received); extra > 0 {
			t.Errorf("fixture %s received %d unexpected extra commands", spec.Name, extra)
		}
	}
	f.assertState(t, spec)
}

func (f *redisFixture) assertState(t *testing.T, spec FixtureSpec) {
	t.Helper()
	if spec.Redis == nil || spec.Redis.Values == nil {
		return
	}

	f.stateMu.Lock()
	actual := make(map[string]string, len(f.values)+len(f.integers))
	maps.Copy(actual, f.values)
	for key, value := range f.integers {
		actual[key] = strconv.FormatInt(value, 10)
	}
	f.stateMu.Unlock()

	if len(actual) != len(spec.Redis.Values) {
		t.Errorf(
			"fixture %s Redis state has %d keys, want %d: %#v",
			spec.Name,
			len(actual),
			len(spec.Redis.Values),
			actual,
		)
	}
	for key, expected := range spec.Redis.Values {
		if actual[key] != expected {
			t.Errorf("fixture %s Redis key %q = %q, want %q", spec.Name, key, actual[key], expected)
		}
	}
	for key, expected := range spec.Redis.TTLSeconds {
		actual := f.ttlSeconds(key)
		if actual != expected {
			t.Errorf("fixture %s Redis key %q TTL = %ds, want %ds", spec.Name, key, actual, expected)
		}
	}
	for key, expected := range spec.Redis.TTLSecondsBetween {
		actual := f.ttlSeconds(key)
		if actual < expected.Min || actual > expected.Max {
			t.Errorf(
				"fixture %s Redis key %q TTL = %ds, want between %ds and %ds",
				spec.Name,
				key,
				actual,
				expected.Min,
				expected.Max,
			)
		}
	}
	for key, expected := range spec.Redis.ExpiryInitializations {
		f.stateMu.Lock()
		actual := f.expirySet[key]
		f.stateMu.Unlock()
		if actual != expected {
			t.Errorf(
				"fixture %s Redis key %q expiry initializations = %d, want %d",
				spec.Name,
				key,
				actual,
				expected,
			)
		}
	}
	if len(spec.Redis.Auth) > 0 {
		f.stateMu.Lock()
		auth := append([]RedisAuthAssertion(nil), f.auth...)
		f.stateMu.Unlock()
		if len(auth) == 0 {
			t.Errorf("fixture %s did not receive Redis authentication", spec.Name)
		}
		seen := make([]bool, len(spec.Redis.Auth))
		for i, actual := range auth {
			matched := -1
			for j, expected := range spec.Redis.Auth {
				if redisAuthMatches(actual, expected) {
					matched = j
					break
				}
			}
			if matched < 0 {
				t.Errorf(
					"fixture %s Redis authentication %d = (%q, %q), want one of %#v",
					spec.Name,
					i+1,
					actual.Username,
					actual.Password,
					spec.Redis.Auth,
				)
				continue
			}
			seen[matched] = true
		}
		for i, matched := range seen {
			if !matched {
				t.Errorf(
					"fixture %s did not receive Redis authentication (%q, %q)",
					spec.Name,
					spec.Redis.Auth[i].Username,
					spec.Redis.Auth[i].Password,
				)
			}
		}
	}
}

func (f *redisFixture) ttlSeconds(key string) int {
	f.stateMu.Lock()
	expiry, ok := f.expiries[key]
	f.stateMu.Unlock()
	if !ok {
		return 0
	}
	return max(int(math.Ceil(time.Until(expiry).Seconds())), 0)
}

func redisAuthMatches(actual, expected RedisAuthAssertion) bool {
	if actual.Password != expected.Password {
		return false
	}
	if actual.Username == expected.Username {
		return true
	}
	return expected.Username == "" && actual.Username == "default"
}

func TestRedisFixtureServesRESP(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis",
		Kind: "redis",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: new("PING")},
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
	defer func() { _ = connection.Close() }()
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
			{Payload: &Matcher{Equals: new("SET quota 1 NX EX 60")}},
			{Payload: &Matcher{Equals: new("INCR quota")}},
			{Payload: &Matcher{Equals: new("GET quota")}},
			{Payload: &Matcher{Equals: new("HSET hash field 1")}},
			{Payload: &Matcher{Equals: new("HGET hash field")}},
		},
		NetworkRespond: make([]NetworkResponse, 5),
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
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	commands := [][]string{
		{"SET", "quota", "1", "NX", "EX", "60"},
		{"INCR", "quota"},
		{"GET", "quota"},
		{"HSET", "hash", "field", "1"},
		{"HGET", "hash", "field"},
	}
	wantResponses := []string{"+OK\r\n", ":2\r\n", "$1\r\n2\r\n", ":1\r\n", "$1\r\n1\r\n"}
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

func TestRedisFixtureCanIgnoreNegotiationAndAssertFinalState(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-rate-limit",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			IgnoreNegotiation:       true,
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": "3"},
		},
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
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for _, command := range [][]string{
		{"HELLO", "3"},
		{"CLIENT", "SETINFO", "LIB-NAME", "go-redis(,go1.26.0)"},
		{"INCRBY", "quota", "3"},
		{"EXPIRE", "quota", "60"},
	} {
		if err := writeRESPCommand(connection, command); err != nil {
			t.Fatalf("write Redis command %q: %v", command[0], err)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read Redis response for %q: %v", command[0], err)
		}
	}
	fixture.assert(t, spec)
}

func TestRedisFixtureAllowUnassertedCommandsDoesNotBlockAfter128Commands(t *testing.T) {
	const commands = 256
	spec := FixtureSpec{
		Name: "redis-unasserted-volume",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": strconv.Itoa(commands)},
		},
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
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set fixture deadline: %v", err)
	}
	reader := bufio.NewReader(connection)
	for range commands {
		if err := writeRESPCommand(connection, []string{"INCR", "quota"}); err != nil {
			t.Fatalf("write Redis command: %v", err)
		}
		if response, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read Redis response: %v", err)
		} else if !strings.HasPrefix(response, ":") {
			t.Fatalf("Redis response = %q, want integer", response)
		}
	}
	fixture.assert(t, spec)
}

func TestRedisFixtureStrictModeRetainsUnexpectedCommands(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-strict-extra",
		Kind: "redis",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Equals: new("PING")},
		}},
		NetworkRespond: []NetworkResponse{{}},
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
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for range 2 {
		if err := writeRESPCommand(connection, []string{"PING"}); err != nil {
			t.Fatalf("write Redis command: %v", err)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read Redis response: %v", err)
		}
	}
	if got := len(fixture.(*redisFixture).received); got != 2 {
		t.Fatalf("strict fixture retained %d commands, want both expected and unexpected commands", got)
	}
}

func TestRedisFixtureEmulatesAIRateFixedWindowScriptAndTTL(t *testing.T) {
	const script = `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
local ttl = redis.call("PTTL", KEYS[1])
if ttl < 0 then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  ttl = tonumber(ARGV[2])
end
return {current, ttl}
`
	spec := FixtureSpec{
		Name: "redis-ai-rate-script",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": "6"},
			TTLSeconds:              map[string]int{"quota": 60},
			ExpiryInitializations:   map[string]int{"quota": 1},
		},
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
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for range 2 {
		if err := writeRESPCommand(connection, []string{"EVAL", script, "1", "quota", "3", "60000"}); err != nil {
			t.Fatalf("write EVAL: %v", err)
		}
		for range 5 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatalf("read EVAL response: %v", err)
			}
		}
	}
	fixture.assert(t, spec)
}

func TestRedisFixtureAssertsAuthenticationWhileAllowingBusinessCommands(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-auth",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"quota": "1"},
			Auth:                    []RedisAuthAssertion{{Password: "somepassword"}},
		},
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
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	for _, command := range [][]string{
		{"AUTH", "somepassword"},
		{"INCR", "quota"},
	} {
		if err := writeRESPCommand(connection, command); err != nil {
			t.Fatalf("write %s: %v", command[0], err)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read %s response: %v", command[0], err)
		}
	}
	fixture.assert(t, spec)
}

func TestRedisAuthMatchesPasswordOnlyDefaultUser(t *testing.T) {
	t.Parallel()

	if !redisAuthMatches(
		RedisAuthAssertion{Username: "default", Password: "somepassword"},
		RedisAuthAssertion{Password: "somepassword"},
	) {
		t.Fatal("password-only assertion did not accept Redis implicit default user")
	}
	if redisAuthMatches(
		RedisAuthAssertion{Username: "alice", Password: "somepassword"},
		RedisAuthAssertion{Password: "somepassword"},
	) {
		t.Fatal("password-only assertion accepted an explicit non-default user")
	}
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
