package pluginintegration

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	limiterredis "github.com/ulule/limiter/v3/drivers/store/redis"
)

type redisFixture struct {
	kind             string
	spec             FixtureSpec
	listener         net.Listener
	caPath           string
	expect           []NetworkAssertion
	received         chan []byte
	errors           chan error
	done             chan struct{}
	closeOnce        sync.Once
	stateMu          sync.Mutex
	values           map[string]string
	integers         map[string]int64
	hashes           map[string]map[string]string
	limitConnMembers map[string]map[string]int64
	expiries         map[string]time.Time
	expirySet        map[string]int
	auth             []RedisAuthAssertion
	scripts          map[string]string
	scriptSeq        int
	wg               sync.WaitGroup
}

func startRedisFixture(spec FixtureSpec) (namedFixture, error) {
	var listener net.Listener
	var caPath string
	if spec.Redis != nil && spec.Redis.TLS {
		certPEM, keyPEM, err := generateRedisFixtureCertificate()
		if err != nil {
			return nil, err
		}
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load Redis fixture certificate: %w", err)
		}
		caFile, err := os.CreateTemp("", "apisix-go-redis-ca-*.pem")
		if err != nil {
			return nil, fmt.Errorf("create Redis fixture CA file: %w", err)
		}
		caPath = caFile.Name()
		if _, err = caFile.Write(certPEM); err != nil {
			_ = caFile.Close()
			_ = os.Remove(caPath)
			return nil, fmt.Errorf("write Redis fixture CA file: %w", err)
		}
		if err = caFile.Close(); err != nil {
			_ = os.Remove(caPath)
			return nil, fmt.Errorf("close Redis fixture CA file: %w", err)
		}
		listener, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		})
		if err != nil {
			_ = os.Remove(caPath)
			return nil, fmt.Errorf("listen TLS Redis fixture: %w", err)
		}
	} else {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen Redis fixture: %w", err)
		}
	}
	receivedCapacity := len(spec.NetworkExpect) + 1
	if spec.Redis != nil && spec.Redis.AllowUnassertedCommands {
		receivedCapacity = 128
	}
	if len(spec.NetworkExpect) == 1 && spec.NetworkExpect[0].Payload != nil &&
		spec.NetworkExpect[0].Payload.Matches != nil && *spec.NetworkExpect[0].Payload.Matches == ".*" {
		receivedCapacity = 128
	}
	fixture := &redisFixture{
		kind:             spec.Kind,
		spec:             spec,
		listener:         listener,
		caPath:           caPath,
		expect:           spec.NetworkExpect,
		received:         make(chan []byte, receivedCapacity),
		errors:           make(chan error, len(spec.NetworkExpect)+1),
		done:             make(chan struct{}),
		values:           make(map[string]string),
		integers:         make(map[string]int64),
		hashes:           make(map[string]map[string]string),
		limitConnMembers: make(map[string]map[string]int64),
		expiries:         make(map[string]time.Time),
		expirySet:        make(map[string]int),
		scripts:          make(map[string]string),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func generateRedisFixtureCertificate() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate Redis fixture TLS key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create Redis fixture TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Redis fixture TLS key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
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
			f.reportError(fmt.Errorf("accept Redis fixture connection: %w", err))
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
				f.reportError(fmt.Errorf("read Redis command: %w", err))
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
			f.reportError(fmt.Errorf("write Redis response: %w", err))
			return
		}
	}
}

func (f *redisFixture) reportError(err error) {
	select {
	case f.errors <- err:
	case <-f.done:
	default:
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
	if commandAuthenticates(command) && len(f.spec.NetworkRespond) > 0 {
		response := f.spec.NetworkRespond[0]
		if response.Payload != "" {
			return writeRESPRaw(writer, response.Payload)
		}
	}
	switch strings.ToUpper(command[0]) {
	case "PING":
		return writeSimpleRESP(writer, "PONG")
	case "HELLO":
		return writeRESPError(writer, "unknown command 'HELLO'")
	case "AUTH", "CLIENT", "READONLY", "READ_ONLY", "ASKING":
		return writeSimpleRESP(writer, "OK")
	case "SELECT":
		if len(command) < 2 {
			return writeRESPError(writer, "wrong number of arguments for SELECT")
		}
		database, err := strconv.Atoi(command[1])
		if err != nil || database < 0 || database > 15 {
			return writeRESPError(writer, "DB index is out of range")
		}
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
			delete(f.limitConnMembers, key)
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
				return f.writeScriptLoad(writer, command)
			case strings.EqualFold(command[1], "EXISTS"):
				return writeRESPRaw(writer, "*1\r\n:0\r\n")
			}
		}
		return writeSimpleRESP(writer, "OK")
	case "EVAL", "EVALSHA":
		return f.writeEvalResponse(writer, command)
	case "CLUSTER":
		return f.writeClusterResponse(writer, command)
	default:
		return writeRESPError(writer, "unsupported command "+command[0])
	}
}

func (f *redisFixture) writeScriptLoad(writer io.Writer, command []string) error {
	if len(command) < 3 {
		return writeRESPError(writer, "wrong number of arguments for SCRIPT LOAD")
	}
	f.stateMu.Lock()
	f.scriptSeq++
	sha := fmt.Sprintf("fixture-script-%d", f.scriptSeq)
	f.scripts[sha] = command[2]
	f.stateMu.Unlock()
	return writeRESPBulk(writer, sha)
}

func commandAuthenticates(command []string) bool {
	if len(command) == 0 {
		return false
	}
	if strings.EqualFold(command[0], "AUTH") {
		return true
	}
	if !strings.EqualFold(command[0], "HELLO") {
		return false
	}
	for _, argument := range command[1:] {
		if strings.EqualFold(argument, "AUTH") {
			return true
		}
	}
	return false
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
	if strings.EqualFold(command[0], "EVALSHA") {
		f.stateMu.Lock()
		script = f.scripts[script]
		f.stateMu.Unlock()
	}
	if strings.Contains(script, `redis.call("ZREMRANGEBYSCORE"`) &&
		strings.Contains(script, `redis.call("ZADD"`) &&
		strings.Contains(script, `redis.call("ZCARD"`) {
		return f.writeLimitConnIncoming(writer, command)
	}
	if strings.Contains(script, `redis.call("ZREM"`) &&
		strings.Contains(script, `redis.call("ZCARD"`) &&
		strings.Contains(script, `redis.call("DEL"`) {
		return f.writeLimitConnLeaving(writer, command)
	}
	if strings.Contains(script, "apisix-go sliding-window check-and-increment") {
		return f.writeSlidingWindowCheckAndIncrement(writer, command)
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
	normalizedScript := strings.ReplaceAll(strings.ToLower(script), "'", `"`)
	if strings.Contains(normalizedScript, `redis.call("hmget"`) &&
		strings.Contains(normalizedScript, `redis.call("hmset"`) &&
		strings.Contains(normalizedScript, `redis.call("pexpire"`) {
		return f.writeLimitReqIncoming(writer, command)
	}
	if strings.Contains(normalizedScript, `redis.call("incrby"`) &&
		strings.Contains(normalizedScript, `redis.call("ttl"`) &&
		strings.Contains(normalizedScript, `redis.call("expire"`) {
		return f.writeGraphQLLimitCountIncoming(writer, command)
	}
	if strings.Contains(normalizedScript, `redis.call("pttl"`) &&
		strings.Contains(normalizedScript, `redis.call("set"`) &&
		strings.Contains(normalizedScript, `redis.call("incrby"`) {
		return f.writeSlidingWindowIncrement(writer, command)
	}
	if strings.Contains(normalizedScript, `redis.call("get"`) &&
		strings.Contains(normalizedScript, `redis.call("pttl"`) &&
		!strings.Contains(normalizedScript, `redis.call("incrby"`) {
		return f.writeUluleLimiterPeek(writer, command)
	}
	if strings.Contains(normalizedScript, `redis.call("incrby"`) &&
		strings.Contains(normalizedScript, `redis.call("pttl"`) &&
		strings.Contains(normalizedScript, `redis.call("pexpire"`) {
		return f.writeUluleLimiterIncrement(writer, command)
	}
	return writeRESPInteger(writer, 1)
}

func (f *redisFixture) writeLimitConnIncoming(writer io.Writer, command []string) error {
	if len(command) < 10 {
		return writeRESPError(writer, "wrong number of arguments for limit-conn incoming")
	}
	conn, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil || conn <= 0 {
		return writeRESPError(writer, "limit-conn conn is not a positive integer")
	}
	burst, err := strconv.ParseInt(command[5], 10, 64)
	if err != nil || burst < 0 {
		return writeRESPError(writer, "limit-conn burst is not a non-negative integer")
	}
	defaultDelay, err := strconv.ParseFloat(command[6], 64)
	if err != nil || defaultDelay <= 0 {
		return writeRESPError(writer, "limit-conn default delay is not positive")
	}
	ttlMilliseconds, err := strconv.ParseInt(command[7], 10, 64)
	if err != nil || ttlMilliseconds <= 0 {
		return writeRESPError(writer, "limit-conn TTL is not a positive integer")
	}
	nowMilliseconds, err := strconv.ParseInt(command[8], 10, 64)
	if err != nil || nowMilliseconds <= 0 {
		return writeRESPError(writer, "limit-conn now is not a positive integer")
	}
	member := command[9]
	if member == "" {
		return writeRESPError(writer, "limit-conn member is empty")
	}

	key := command[3]
	f.stateMu.Lock()
	members := f.limitConnMembers[key]
	if members == nil {
		members = make(map[string]int64)
	}
	for existingMember, deadline := range members {
		if deadline <= nowMilliseconds {
			delete(members, existingMember)
		}
	}
	current := int64(len(members))
	if current >= conn+burst {
		f.syncLimitConnMembers(key, members)
		f.stateMu.Unlock()
		return writeRESPIntegerArray(writer, 0, 0)
	}
	if _, exists := members[member]; exists {
		f.syncLimitConnMembers(key, members)
		f.stateMu.Unlock()
		return writeRESPIntegerArray(writer, 0, 0)
	}
	members[member] = nowMilliseconds + ttlMilliseconds
	current++
	f.syncLimitConnMembers(key, members)
	f.expiries[key] = time.UnixMilli(nowMilliseconds + ttlMilliseconds)
	f.expirySet[key]++
	f.stateMu.Unlock()

	delayMilliseconds := int64(0)
	if current > conn {
		delayMilliseconds = int64(math.Floor(float64((current-1)/conn) * defaultDelay * 1000))
	}
	return writeRESPIntegerArray(writer, 1, delayMilliseconds)
}

func (f *redisFixture) writeLimitConnLeaving(writer io.Writer, command []string) error {
	if len(command) < 5 {
		return writeRESPError(writer, "wrong number of arguments for limit-conn leaving")
	}
	key := command[3]
	member := command[4]
	f.stateMu.Lock()
	members := f.limitConnMembers[key]
	removed := int64(0)
	if _, exists := members[member]; exists {
		delete(members, member)
		removed = 1
	}
	f.syncLimitConnMembers(key, members)
	f.stateMu.Unlock()
	return writeRESPInteger(writer, removed)
}

func (f *redisFixture) syncLimitConnMembers(key string, members map[string]int64) {
	if len(members) == 0 {
		delete(f.limitConnMembers, key)
		delete(f.integers, key)
		delete(f.values, key)
		delete(f.expiries, key)
		return
	}
	f.limitConnMembers[key] = members
	f.integers[key] = int64(len(members))
	delete(f.values, key)
}

func (f *redisFixture) writeUluleLimiterPeek(writer io.Writer, command []string) error {
	if len(command) < 4 {
		return writeRESPError(writer, "wrong number of arguments for limiter peek")
	}
	key := command[3]
	f.stateMu.Lock()
	count := f.integers[key]
	if value, ok := f.values[key]; ok {
		count, _ = strconv.ParseInt(value, 10, 64)
	}
	expiry, hasExpiry := f.expiries[key]
	f.stateMu.Unlock()

	ttl := int64(0)
	if hasExpiry {
		ttl = max(time.Until(expiry).Milliseconds(), 0)
	}
	return writeRESPIntegerArray(writer, count, ttl)
}

func (f *redisFixture) writeSlidingWindowCheckAndIncrement(writer io.Writer, command []string) error {
	if len(command) < 10 {
		return writeRESPError(writer, "wrong number of arguments for sliding-window check-and-increment")
	}
	cost, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil {
		return writeRESPError(writer, "sliding-window cost is not an integer")
	}
	limit, err := strconv.ParseInt(command[5], 10, 64)
	if err != nil {
		return writeRESPError(writer, "sliding-window limit is not an integer")
	}
	window, err := strconv.ParseFloat(command[6], 64)
	if err != nil || window <= 0 {
		return writeRESPError(writer, "sliding-window size is not positive")
	}
	remaining, err := strconv.ParseFloat(command[7], 64)
	if err != nil || remaining < 0 {
		return writeRESPError(writer, "sliding-window remaining time is invalid")
	}
	expiry, err := strconv.ParseInt(command[8], 10, 64)
	if err != nil || expiry <= 0 {
		return writeRESPError(writer, "sliding-window expiry is not positive")
	}
	last, err := strconv.ParseInt(command[9], 10, 64)
	if err != nil {
		return writeRESPError(writer, "sliding-window previous count is not an integer")
	}
	last = min(last, limit)

	key := command[3]
	now := time.Now()
	f.stateMu.Lock()
	current := f.integers[key]
	if current == 0 {
		if stringValue, ok := f.values[key]; ok {
			current, _ = strconv.ParseInt(stringValue, 10, 64)
		}
	}
	if deadline, ok := f.expiries[key]; ok && !now.Before(deadline) {
		current = 0
		delete(f.values, key)
		delete(f.integers, key)
		delete(f.expiries, key)
	}
	estimated := float64(last)/window*remaining + float64(current)
	if estimated+float64(cost) > float64(limit) {
		f.stateMu.Unlock()
		return writeRESPIntegerArray(writer, 0, current, last)
	}

	current += cost
	f.integers[key] = current
	delete(f.values, key)
	if _, ok := f.expiries[key]; !ok {
		f.expiries[key] = now.Add(time.Duration(expiry) * time.Second)
		f.expirySet[key]++
	}
	f.stateMu.Unlock()
	return writeRESPIntegerArray(writer, 1, current, last)
}

func (f *redisFixture) writeSlidingWindowIncrement(writer io.Writer, command []string) error {
	if len(command) < 6 {
		return writeRESPError(writer, "wrong number of arguments for sliding-window increment")
	}
	delta, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil {
		return writeRESPError(writer, "sliding-window increment is not an integer")
	}
	expirySeconds, err := strconv.ParseInt(command[5], 10, 64)
	if err != nil || expirySeconds <= 0 {
		return writeRESPError(writer, "sliding-window expiry is not positive")
	}

	key := command[3]
	now := time.Now()
	f.stateMu.Lock()
	expiry, hasExpiry := f.expiries[key]
	if !hasExpiry || !now.Before(expiry) {
		f.integers[key] = delta
		delete(f.values, key)
		f.expiries[key] = now.Add(time.Duration(expirySeconds) * time.Second)
		f.expirySet[key]++
	} else {
		f.integers[key] += delta
	}
	current := f.integers[key]
	f.stateMu.Unlock()
	return writeRESPInteger(writer, current)
}

func (f *redisFixture) writeLimitReqIncoming(writer io.Writer, command []string) error {
	if len(command) < 8 {
		return writeRESPError(writer, "wrong number of arguments for limit-req incoming")
	}
	nowMilliseconds, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil {
		return writeRESPError(writer, "limit-req time is not an integer")
	}
	rate, err := strconv.ParseFloat(command[5], 64)
	if err != nil || rate <= 0 {
		return writeRESPError(writer, "limit-req rate is not positive")
	}
	burst, err := strconv.ParseFloat(command[6], 64)
	if err != nil || burst < 0 {
		return writeRESPError(writer, "limit-req burst is negative")
	}
	ttlMilliseconds, err := strconv.ParseInt(command[7], 10, 64)
	if err != nil || ttlMilliseconds <= 0 {
		return writeRESPError(writer, "limit-req TTL is not a positive integer")
	}

	key := command[3]
	wallNow := time.Now()
	f.stateMu.Lock()
	if expiry, ok := f.expiries[key]; ok && !wallNow.Before(expiry) {
		delete(f.hashes, key)
	}
	fields := f.hashes[key]
	if fields == nil {
		fields = make(map[string]string)
		f.hashes[key] = fields
	}
	excess, _ := strconv.ParseFloat(fields["excess"], 64)
	last, err := strconv.ParseInt(fields["last"], 10, 64)
	if err != nil {
		last = nowMilliseconds
	}
	elapsed := math.Max(0, float64(nowMilliseconds-last)/1000)
	excess = math.Max(0, excess-elapsed*rate) + 1
	allowed := int64(1)
	if maxExcess := burst + 1; excess > maxExcess {
		excess = maxExcess
		allowed = 0
	}
	fields["excess"] = strconv.FormatFloat(excess, 'f', -1, 64)
	fields["last"] = strconv.FormatInt(nowMilliseconds, 10)
	f.expiries[key] = wallNow.Add(time.Duration(ttlMilliseconds) * time.Millisecond)
	f.expirySet[key]++
	f.stateMu.Unlock()

	delayMilliseconds := int64(0)
	if allowed == 1 {
		delayMilliseconds = int64(math.Floor(math.Max(0, (excess-1)/rate) * 1000))
	}
	return writeRESPIntegerArray(writer, allowed, delayMilliseconds)
}

func (f *redisFixture) writeGraphQLLimitCountIncoming(writer io.Writer, command []string) error {
	if len(command) < 7 {
		return writeRESPError(writer, "wrong number of arguments for GraphQL limit-count")
	}
	cost, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil {
		return writeRESPError(writer, "GraphQL limit-count cost is not an integer")
	}
	limit, err := strconv.ParseInt(command[5], 10, 64)
	if err != nil || limit <= 0 {
		return writeRESPError(writer, "GraphQL limit-count limit is not positive")
	}
	windowSeconds, err := strconv.ParseInt(command[6], 10, 64)
	if err != nil || windowSeconds <= 0 {
		return writeRESPError(writer, "GraphQL limit-count window is not positive")
	}

	key := command[3]
	now := time.Now()
	f.stateMu.Lock()
	expiry, hasExpiry := f.expiries[key]
	if hasExpiry && !now.Before(expiry) {
		delete(f.values, key)
		delete(f.integers, key)
		delete(f.expiries, key)
		hasExpiry = false
	}
	if value, ok := f.values[key]; ok {
		f.integers[key], _ = strconv.ParseInt(value, 10, 64)
		delete(f.values, key)
	}
	f.integers[key] += cost
	current := f.integers[key]
	if !hasExpiry {
		expiry = now.Add(time.Duration(windowSeconds) * time.Second)
		f.expiries[key] = expiry
		f.expirySet[key]++
	}
	reset := max(int64(math.Ceil(time.Until(expiry).Seconds())), 1)
	f.stateMu.Unlock()

	remaining := max(limit-current, 0)
	allowed := int64(1)
	if current > limit {
		allowed = 0
	}
	return writeRESPIntegerArray(writer, allowed, remaining, reset)
}

func (f *redisFixture) writeUluleLimiterIncrement(writer io.Writer, command []string) error {
	if len(command) < 6 {
		return writeRESPError(writer, "wrong number of arguments for limiter increment")
	}
	increment, err := strconv.ParseInt(command[4], 10, 64)
	if err != nil {
		return writeRESPError(writer, "limiter increment is not an integer")
	}
	ttlMilliseconds, err := strconv.ParseInt(command[5], 10, 64)
	if err != nil || ttlMilliseconds <= 0 {
		return writeRESPError(writer, "limiter TTL is not a positive integer")
	}
	key := command[3]
	f.stateMu.Lock()
	f.integers[key] += increment
	current := f.integers[key]
	expiry, hasExpiry := f.expiries[key]
	if !hasExpiry || !time.Now().Before(expiry) {
		expiry = time.Now().Add(time.Duration(ttlMilliseconds) * time.Millisecond)
		f.expiries[key] = expiry
		f.expirySet[key]++
	}
	ttl := max(time.Until(expiry).Milliseconds(), 1)
	if current == increment {
		ttl = ttlMilliseconds
	}
	f.stateMu.Unlock()
	return writeRESPIntegerArray(writer, current, ttl)
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
			"*1\r\n*3\r\n:0\r\n:16383\r\n*3\r\n$9\r\n127.0.0.1\r\n:"+f.port()+"\r\n$7\r\nfixture\r\n",
		)
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

func (f *redisFixture) caFile() string { return f.caPath }

func (f *redisFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
		if f.caPath != "" {
			_ = os.Remove(f.caPath)
		}
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
	if spec.Redis == nil ||
		(spec.Redis.Values == nil && spec.Redis.ValueMatches == nil && spec.Redis.Hashes == nil) {
		return
	}

	f.stateMu.Lock()
	actual := make(map[string]string, len(f.values)+len(f.integers))
	maps.Copy(actual, f.values)
	for key, value := range f.integers {
		actual[key] = strconv.FormatInt(value, 10)
	}
	actualHashes := make(map[string]map[string]string, len(f.hashes))
	for key, fields := range f.hashes {
		actualHashes[key] = maps.Clone(fields)
	}
	f.stateMu.Unlock()

	for _, problem := range redisValueProblems(actual, spec.Redis.Values, spec.Redis.ValueMatches) {
		t.Errorf("fixture %s Redis state: %s", spec.Name, problem)
	}
	assertRedisHashes(t, spec.Name, actualHashes, spec.Redis.Hashes)
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

func assertRedisHashes(
	t *testing.T,
	fixtureName string,
	actual map[string]map[string]string,
	expected map[string]map[string]Matcher,
) {
	t.Helper()
	if expected == nil {
		return
	}
	if len(actual) != len(expected) {
		t.Errorf("fixture %s Redis hashes = %d, want %d: %#v", fixtureName, len(actual), len(expected), actual)
	}
	for key, expectedFields := range expected {
		actualFields, ok := actual[key]
		if !ok {
			t.Errorf("fixture %s Redis hash %q is absent", fixtureName, key)
			continue
		}
		if len(actualFields) != len(expectedFields) {
			t.Errorf(
				"fixture %s Redis hash %q fields = %d, want %d: %#v",
				fixtureName,
				key,
				len(actualFields),
				len(expectedFields),
				actualFields,
			)
		}
		for field, matcher := range expectedFields {
			value, ok := actualFields[field]
			if !ok {
				t.Errorf("fixture %s Redis hash %q field %q is absent", fixtureName, key, field)
				continue
			}
			if err := matcher.match(value, true); err != nil {
				t.Errorf("fixture %s Redis hash %q field %q: %v", fixtureName, key, field, err)
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

func redisValueProblems(
	actual map[string]string,
	exact map[string]string,
	matches map[string]string,
) []string {
	problems := make([]string, 0)
	consumed := make(map[string]string, len(exact)+len(matches))

	exactKeys := make([]string, 0, len(exact))
	for key := range exact {
		exactKeys = append(exactKeys, key)
	}
	sort.Strings(exactKeys)
	for _, key := range exactKeys {
		expected := exact[key]
		value, ok := actual[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("key %q is absent, want %q", key, expected))
			continue
		}
		consumed[key] = "exact key " + key
		if value != expected {
			problems = append(
				problems,
				fmt.Sprintf("key %q = %q, want %q", key, value, expected),
			)
		}
	}

	patterns := make([]string, 0, len(matches))
	for pattern := range matches {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			problems = append(problems, fmt.Sprintf("pattern %q is invalid: %v", pattern, err))
			continue
		}
		keys := make([]string, 0, 1)
		for key := range actual {
			if expression.MatchString(key) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		if len(keys) != 1 {
			problems = append(
				problems,
				fmt.Sprintf("pattern %q matched %d keys, want exactly 1: %v", pattern, len(keys), keys),
			)
			continue
		}
		key := keys[0]
		if owner, ok := consumed[key]; ok {
			problems = append(
				problems,
				fmt.Sprintf("pattern %q matched key %q already claimed by %s", pattern, key, owner),
			)
			continue
		}
		consumed[key] = "pattern " + pattern
		if actual[key] != matches[pattern] {
			problems = append(
				problems,
				fmt.Sprintf("key %q = %q, want %q", key, actual[key], matches[pattern]),
			)
		}
	}

	actualKeys := make([]string, 0, len(actual))
	for key := range actual {
		actualKeys = append(actualKeys, key)
	}
	sort.Strings(actualKeys)
	for _, key := range actualKeys {
		if _, ok := consumed[key]; !ok {
			problems = append(problems, fmt.Sprintf("unexpected key %q = %q", key, actual[key]))
		}
	}
	return problems
}

func TestRedisValueProblemsRequiresOneKeyPerMatcherAndExactValue(t *testing.T) {
	for _, test := range []struct {
		name    string
		actual  map[string]string
		matches map[string]string
		want    string
	}{
		{
			name:    "unique key and exact value",
			actual:  map[string]string{"plugin-limit-count:route:one:123": "2"},
			matches: map[string]string{`^plugin-limit-count:route:one:\d+$`: "2"},
		},
		{
			name:    "exact value mismatch",
			actual:  map[string]string{"plugin-limit-count:route:one:123": "1"},
			matches: map[string]string{`^plugin-limit-count:route:one:\d+$`: "2"},
			want:    `= "1", want "2"`,
		},
		{
			name:    "no matching key",
			actual:  map[string]string{"another-key": "2"},
			matches: map[string]string{`^plugin-limit-count:`: "2"},
			want:    "matched 0 keys, want exactly 1",
		},
		{
			name: "multiple matching keys",
			actual: map[string]string{
				"plugin-limit-count:route:one:123": "2",
				"plugin-limit-count:route:one:124": "2",
			},
			matches: map[string]string{`^plugin-limit-count:route:one:\d+$`: "2"},
			want:    "matched 2 keys, want exactly 1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			problems := redisValueProblems(test.actual, nil, test.matches)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("redisValueProblems() = %v, want no problems", problems)
				}
				return
			}
			if got := strings.Join(problems, "\n"); !strings.Contains(got, test.want) {
				t.Fatalf("redisValueProblems() = %q, want substring %q", got, test.want)
			}
		})
	}
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

func TestRedisFixtureCanRejectAuthentication(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-auth-failure",
		Kind: "redis",
		NetworkRespond: []NetworkResponse{{
			Payload: "-WRONGPASS invalid username-password pair\r\n",
		}},
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Auth: []RedisAuthAssertion{{
				Username: "alice",
				Password: "wrong",
			}},
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
	if _, err := io.WriteString(connection, "*3\r\n$4\r\nAUTH\r\n$5\r\nalice\r\n$5\r\nwrong\r\n"); err != nil {
		t.Fatalf("write Redis AUTH: %v", err)
	}
	response, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatalf("read Redis AUTH response: %v", err)
	}
	if response != "-WRONGPASS invalid username-password pair\r\n" {
		t.Fatalf("Redis AUTH response = %q, want WRONGPASS", response)
	}

	fixture.assert(t, spec)
}

func TestRedisFixtureEmulatesUluleLimiterIncrementScript(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-ulule-limiter",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{"limit-count:route:route-1:client": "2"},
			TTLSecondsBetween: map[string]IntRange{
				"limit-count:route:route-1:client": {Min: 59, Max: 60},
			},
			ExpiryInitializations: map[string]int{"limit-count:route:route-1:client": 1},
		},
	}
	fixture, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	defer fixture.close()

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	defer func() { _ = client.Close() }()
	script := `
local key = KEYS[1]
local count = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local ret = redis.call("incrby", key, ARGV[1])
if ret == count then
	if ttl > 0 then
		redis.call("pexpire", key, ARGV[2])
	end
	return {ret, ttl}
end
ttl = redis.call("pttl", key)
return {ret, ttl}
`
	sha, err := client.ScriptLoad(context.Background(), script).Result()
	if err != nil {
		t.Fatalf("load ulule limiter script: %v", err)
	}
	key := "limit-count:route:route-1:client"
	first, err := client.EvalSha(context.Background(), sha, []string{key}, 1, 60000).Result()
	if err != nil {
		t.Fatalf("first limiter increment: %v", err)
	}
	if got := fmt.Sprint(first); got != "[1 60000]" {
		t.Fatalf("first limiter result = %s, want [1 60000]", got)
	}
	second, err := client.EvalSha(context.Background(), sha, []string{key}, 1, 60000).Result()
	if err != nil {
		t.Fatalf("second limiter increment: %v", err)
	}
	fields, ok := second.([]any)
	if !ok || len(fields) != 2 || fields[0] != int64(2) {
		t.Fatalf("second limiter result = %#v, want count 2 and TTL", second)
	}
	ttl, ok := fields[1].(int64)
	if !ok || ttl < 59000 || ttl > 60000 {
		t.Fatalf("second limiter TTL = %#v, want 59000..60000", fields[1])
	}

	fixture.assert(t, spec)
}

func TestRedisFixtureEmulatesUluleLimiterPeekScript(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-ulule-peek",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			IgnoreNegotiation:       true,
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	store, err := limiterredis.NewStoreWithOptions(client, limiter.StoreOptions{Prefix: "limit-count"})
	if err != nil {
		t.Fatalf("create limiter Redis store: %v", err)
	}
	lim := limiter.New(store, limiter.Rate{Period: time.Minute, Limit: 7})

	quota, err := lim.Peek(context.Background(), "route:route-1:client")
	if err != nil {
		t.Fatalf("Peek() error = %v", err)
	}
	if quota.Limit != 7 || quota.Remaining != 7 || quota.Reached {
		t.Fatalf("Peek() quota = %#v, want unused 7-request quota", quota)
	}
}

func TestRedisFixtureEmulatesSlidingWindowAtomicCheckAndIncrement(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-sliding-window",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			IgnoreNegotiation:       true,
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	script := `
-- apisix-go sliding-window check-and-increment
local current = redis.call('get', KEYS[1])
return {1, current, 0}
`
	call := func() []any {
		t.Helper()
		result, err := client.Eval(
			context.Background(),
			script,
			[]string{"plugin-limit-count:key.1.counter"},
			1,
			2,
			5,
			3,
			10,
			0,
		).Slice()
		if err != nil {
			t.Fatalf("evaluate sliding-window script: %v", err)
		}
		return result
	}

	if got := call(); !equalRedisIntegers(got, 1, 1, 0) {
		t.Fatalf("first result = %#v, want [1 1 0]", got)
	}
	if got := call(); !equalRedisIntegers(got, 1, 2, 0) {
		t.Fatalf("second result = %#v, want [1 2 0]", got)
	}
	if got := call(); !equalRedisIntegers(got, 0, 2, 0) {
		t.Fatalf("third result = %#v, want [0 2 0]", got)
	}
	stored, err := client.Get(context.Background(), "plugin-limit-count:key.1.counter").Int64()
	if err != nil {
		t.Fatalf("read sliding-window counter: %v", err)
	}
	if stored != 2 {
		t.Fatalf("stored sliding-window counter = %d, want 2", stored)
	}
}

func TestRedisFixtureRejectsSlidingWindowCostOverflowWithoutIncrement(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-sliding-window-cost",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			IgnoreNegotiation:       true,
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	script := `
-- apisix-go sliding-window check-and-increment
local current = redis.call('get', KEYS[1])
return {1, current, 0}
`
	call := func(cost int64) []any {
		t.Helper()
		result, err := client.Eval(
			context.Background(),
			script,
			[]string{"plugin-limit-count:cost.1.counter"},
			cost,
			2,
			5,
			3,
			10,
			0,
		).Slice()
		if err != nil {
			t.Fatalf("evaluate sliding-window script: %v", err)
		}
		return result
	}

	if got := call(1); !equalRedisIntegers(got, 1, 1, 0) {
		t.Fatalf("cost-one result = %#v, want [1 1 0]", got)
	}
	if got := call(2); !equalRedisIntegers(got, 0, 1, 0) {
		t.Fatalf("cost-two overflow result = %#v, want [0 1 0]", got)
	}
	stored, err := client.Get(context.Background(), "plugin-limit-count:cost.1.counter").Int64()
	if err != nil {
		t.Fatalf("read sliding-window counter: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored sliding-window counter = %d, want rejected cost not incremented", stored)
	}
}

func TestRedisFixtureEmulatesSlidingWindowIncrementScript(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-sliding-window-increment",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values: map[string]string{
				"plugin-limit-count:route:delayed:user.1.counter": "5",
			},
			TTLSecondsBetween: map[string]IntRange{
				"plugin-limit-count:route:delayed:user.1.counter": {Min: 119, Max: 120},
			},
			ExpiryInitializations: map[string]int{
				"plugin-limit-count:route:delayed:user.1.counter": 1,
			},
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	script := `
local ttl = redis.call('pttl', KEYS[1])
if ttl < 0 then
    redis.call('set', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return tonumber(ARGV[1])
end
return redis.call('incrby', KEYS[1], ARGV[1])
`
	key := "plugin-limit-count:route:delayed:user.1.counter"
	first, err := client.Eval(context.Background(), script, []string{key}, 2, 120).Int64()
	if err != nil {
		t.Fatalf("first sliding increment: %v", err)
	}
	if first != 2 {
		t.Fatalf("first sliding increment = %d, want 2", first)
	}
	second, err := client.Eval(context.Background(), script, []string{key}, 3, 120).Int64()
	if err != nil {
		t.Fatalf("second sliding increment: %v", err)
	}
	if second != 5 {
		t.Fatalf("second sliding increment = %d, want 5", second)
	}

	fixture.assert(t, spec)
}

func TestRedisFixtureEmulatesLimitConnScripts(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-limit-conn",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values: map[string]string{
				"plugin-limit-conn:route:route-1:client": "2",
			},
			TTLSecondsBetween: map[string]IntRange{
				"plugin-limit-conn:route:route-1:client": {Min: 4, Max: 5},
			},
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	incomingScript := `
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[5])
local current = redis.call("ZCARD", KEYS[1])
if current >= tonumber(ARGV[1]) + tonumber(ARGV[2]) then
  return {0, 0}
end
redis.call("ZADD", KEYS[1], "NX", tonumber(ARGV[5]) + tonumber(ARGV[4]), ARGV[6])
redis.call("PEXPIRE", KEYS[1], ARGV[4])
return {1, current == 0 and 0 or 250}
`
	leavingScript := `
local removed = redis.call("ZREM", KEYS[1], ARGV[1])
if redis.call("ZCARD", KEYS[1]) == 0 then
  redis.call("DEL", KEYS[1])
end
return removed
`
	key := "plugin-limit-conn:route:route-1:client"
	baseNow := time.Now().UnixMilli()
	incoming := func(member string, now int64) []any {
		t.Helper()
		result, err := client.Eval(
			context.Background(),
			incomingScript,
			[]string{key},
			1,
			1,
			0.25,
			5000,
			now,
			member,
		).Slice()
		if err != nil {
			t.Fatalf("evaluate limit-conn incoming: %v", err)
		}
		return result
	}

	if got := incoming("member-1", baseNow); !equalRedisIntegers(got, 1, 0) {
		t.Fatalf("first incoming = %#v, want [1 0]", got)
	}
	if got := incoming("member-1", baseNow); !equalRedisIntegers(got, 0, 0) {
		t.Fatalf("duplicate-member incoming = %#v, want [0 0]", got)
	}
	if got := incoming("member-2", baseNow); !equalRedisIntegers(got, 1, 250) {
		t.Fatalf("burst incoming = %#v, want [1 250]", got)
	}
	if got := incoming("member-3", baseNow); !equalRedisIntegers(got, 0, 0) {
		t.Fatalf("rejected incoming = %#v, want [0 0]", got)
	}
	fixture.assert(t, spec)

	missingLeaving := client.Eval(context.Background(), leavingScript, []string{key}, "missing-member")
	if got, err := missingLeaving.Int64(); err != nil || got != 0 {
		t.Fatalf("missing-member leaving = %d, %v; want 0", got, err)
	}
	fixture.assert(t, spec)

	firstLeaving := client.Eval(context.Background(), leavingScript, []string{key}, "member-1")
	if got, err := firstLeaving.Int64(); err != nil || got != 1 {
		t.Fatalf("first leaving = %d, %v; want 1", got, err)
	}
	secondLeaving := client.Eval(context.Background(), leavingScript, []string{key}, "member-2")
	if got, err := secondLeaving.Int64(); err != nil || got != 1 {
		t.Fatalf("second leaving = %d, %v; want 1", got, err)
	}
	fixture.assert(t, FixtureSpec{
		Name: "redis-limit-conn",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{},
		},
	})

	if got := incoming("expired-member", baseNow); !equalRedisIntegers(got, 1, 0) {
		t.Fatalf("expiring incoming = %#v, want [1 0]", got)
	}
	if got := incoming("fresh-member", baseNow+6000); !equalRedisIntegers(got, 1, 0) {
		t.Fatalf("post-expiry incoming = %#v, want [1 0]", got)
	}
}

func TestRedisFixtureEmulatesLimitReqScript(t *testing.T) {
	key := "plugin-limit-req:route:route-1:client"
	excess := "2"
	last := "2000"
	spec := FixtureSpec{
		Name: "redis-limit-req",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Hashes: map[string]map[string]Matcher{
				key: {
					"excess": {Equals: &excess},
					"last":   {Equals: &last},
				},
			},
			TTLSecondsBetween: map[string]IntRange{
				key: {Min: 1, Max: 2},
			},
			ExpiryInitializations: map[string]int{key: 4},
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	script := `
local state = redis.call("HMGET", KEYS[1], "excess", "last")
local excess = tonumber(state[1]) or 0
local last = tonumber(state[2]) or tonumber(ARGV[1])
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local elapsed = math.max(0, (now - last) / 1000)
excess = math.max(0, excess - elapsed * rate) + 1
local max_excess = burst + 1
local allowed = 1
if excess > max_excess then
  excess = max_excess
  allowed = 0
end

redis.call("HMSET", KEYS[1], "excess", excess, "last", now)
redis.call("PEXPIRE", KEYS[1], ttl)

local delay = 0
if allowed == 1 then
  delay = math.max(0, (excess - 1) / rate)
end

return {allowed, math.floor(delay * 1000)}
`
	incoming := func(now int64) []any {
		t.Helper()
		result, err := client.Eval(
			context.Background(),
			script,
			[]string{key},
			now,
			1,
			1,
			2000,
		).Slice()
		if err != nil {
			t.Fatalf("evaluate limit-req incoming: %v", err)
		}
		return result
	}

	if got := incoming(1001); !equalRedisIntegers(got, 1, 0) {
		t.Fatalf("first incoming = %#v, want [1 0]", got)
	}
	if got := incoming(1000); !equalRedisIntegers(got, 1, 1000) {
		t.Fatalf("out-of-order burst incoming = %#v, want [1 1000]", got)
	}
	if got := incoming(1000); !equalRedisIntegers(got, 0, 0) {
		t.Fatalf("rejected incoming = %#v, want [0 0]", got)
	}
	if got := incoming(2000); !equalRedisIntegers(got, 1, 1000) {
		t.Fatalf("leaked-token incoming = %#v, want [1 1000]", got)
	}

	fixture.assert(t, spec)
}

func TestRedisFixtureEmulatesGraphQLLimitCountScript(t *testing.T) {
	key := "plugin-graphql-limit-count:route:route-1:client"
	spec := FixtureSpec{
		Name: "redis-graphql-limit-count",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			Values:                  map[string]string{key: "6"},
			TTLSecondsBetween: map[string]IntRange{
				key: {Min: 59, Max: 60},
			},
			ExpiryInitializations: map[string]int{key: 1},
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	script := `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
local ttl = redis.call("TTL", KEYS[1])
if ttl < 0 then
  redis.call("EXPIRE", KEYS[1], ARGV[3])
  ttl = tonumber(ARGV[3])
end

local limit = tonumber(ARGV[2])
local remaining = limit - current
if remaining < 0 then
  remaining = 0
end

local allowed = 1
if current > limit then
  allowed = 0
end

return {allowed, remaining, ttl}
`
	first, err := client.Eval(context.Background(), script, []string{key}, 4, 5, 60).Slice()
	if err != nil {
		t.Fatalf("first GraphQL limit-count increment: %v", err)
	}
	if !equalRedisIntegers(first, 1, 1, 60) {
		t.Fatalf("first GraphQL limit-count result = %#v, want [1 1 60]", first)
	}
	second, err := client.Eval(context.Background(), script, []string{key}, 2, 5, 60).Slice()
	if err != nil {
		t.Fatalf("second GraphQL limit-count increment: %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("second GraphQL limit-count result = %#v, want three elements", second)
	}
	if allowed, ok := second[0].(int64); !ok || allowed != 0 {
		t.Fatalf("second allowed = %#v, want 0", second[0])
	}
	if remaining, ok := second[1].(int64); !ok || remaining != 0 {
		t.Fatalf("second remaining = %#v, want 0", second[1])
	}
	reset, ok := second[2].(int64)
	if !ok || reset < 59 || reset > 60 {
		t.Fatalf("second reset = %#v, want 59..60", second[2])
	}

	fixture.assert(t, spec)
}

func TestRedisFixtureRejectsOutOfRangeDatabaseSelection(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-database-selection",
		Kind: "redis",
		Redis: &RedisFixtureAssertion{
			AllowUnassertedCommands: true,
			IgnoreNegotiation:       true,
		},
	}
	started, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis fixture: %v", err)
	}
	fixture := started.(*redisFixture)
	t.Cleanup(fixture.close)

	client := redis.NewClient(&redis.Options{Addr: fixture.address()})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Do(context.Background(), "SELECT", "999999").Err(); err == nil ||
		!strings.Contains(err.Error(), "DB index is out of range") {
		t.Fatalf("out-of-range SELECT error = %v, want DB index is out of range", err)
	}
	if err := client.Do(context.Background(), "SELECT", "15").Err(); err != nil {
		t.Fatalf("highest supported SELECT failed: %v", err)
	}
	if err := client.Do(context.Background(), "SELECT", "16").Err(); err == nil ||
		!strings.Contains(err.Error(), "DB index is out of range") {
		t.Fatalf("upper-bound SELECT error = %v, want DB index is out of range", err)
	}
}

func equalRedisIntegers(values []any, expected ...int64) bool {
	if len(values) != len(expected) {
		return false
	}
	for i, value := range values {
		integer, ok := value.(int64)
		if !ok || integer != expected[i] {
			return false
		}
	}
	return true
}

func TestRedisClusterFixtureSupportsUluleLimiter(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-cluster-ulule-limiter",
		Kind: "redis-cluster",
		NetworkExpect: []NetworkAssertion{{
			Payload: &Matcher{Matches: new(".*")},
		}},
		NetworkRespond: []NetworkResponse{{Payload: "ignored"}},
	}
	fixture, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start Redis cluster fixture: %v", err)
	}
	defer fixture.close()

	client := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{fixture.address()}})
	defer func() { _ = client.Close() }()
	store, err := limiterredis.NewStoreWithOptions(client, limiter.StoreOptions{Prefix: "limit-count"})
	if err != nil {
		fixture.assert(t, spec)
		t.Fatalf("create limiter Redis cluster store: %v", err)
	}
	lim := limiter.New(store, limiter.Rate{Period: time.Minute, Limit: 2})
	result, err := lim.Get(context.Background(), "route:route-1:client")
	if err != nil {
		t.Fatalf("cluster limiter Get() error = %v", err)
	}
	if result.Remaining != 1 {
		t.Fatalf("cluster limiter remaining = %d, want 1", result.Remaining)
	}

	fixture.assert(t, spec)
}

func TestRedisClusterTLSFixtureExposesCAAndPreservesStatefulEval(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-cluster-tls-limit-conn",
		Kind: "redis-cluster",
		Redis: &RedisFixtureAssertion{
			TLS:                     true,
			AllowUnassertedCommands: true,
		},
	}
	named, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start TLS Redis cluster fixture: %v", err)
	}
	defer named.close()

	trusted, ok := named.(interface{ caFile() string })
	if !ok || trusted.caFile() == "" {
		t.Fatal("TLS Redis cluster fixture does not expose a CA file")
	}

	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: []string{named.address()},
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	defer func() { _ = client.Close() }()
	incomingScript := `
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[5])
local current = redis.call("ZCARD", KEYS[1])
if current >= tonumber(ARGV[1]) + tonumber(ARGV[2]) then
  return {0, 0}
end
redis.call("ZADD", KEYS[1], "NX", tonumber(ARGV[5]) + tonumber(ARGV[4]), ARGV[6])
redis.call("PEXPIRE", KEYS[1], ARGV[4])
return {1, 0}
`
	key := "route:route-1:client"
	evalIncoming := func(member string) []any {
		t.Helper()
		result, evalErr := client.Eval(
			context.Background(),
			incomingScript,
			[]string{key},
			1,
			0,
			0.1,
			3600_000,
			time.Now().UnixMilli(),
			member,
		).Slice()
		if evalErr != nil {
			t.Fatalf("TLS cluster limit-conn incoming Eval() error = %v", evalErr)
		}
		return result
	}
	if result := evalIncoming("member-1"); !equalRedisIntegers(result, 1, 0) {
		t.Fatalf("first TLS cluster limit-conn result = %#v, want [1 0]", result)
	}
	if result := evalIncoming("member-2"); !equalRedisIntegers(result, 0, 0) {
		t.Fatalf("second TLS cluster limit-conn result = %#v, want [0 0]", result)
	}

	leavingScript := `
local removed = redis.call("ZREM", KEYS[1], ARGV[1])
if redis.call("ZCARD", KEYS[1]) == 0 then
  redis.call("DEL", KEYS[1])
end
return removed
`
	removed, err := client.Eval(
		context.Background(),
		leavingScript,
		[]string{key},
		"member-1",
	).Int64()
	if err != nil || removed != 1 {
		t.Fatalf("TLS cluster limit-conn leaving Eval() = %d, %v; want 1", removed, err)
	}
	if result := evalIncoming("member-3"); !equalRedisIntegers(result, 1, 0) {
		t.Fatalf("post-release TLS cluster limit-conn result = %#v, want [1 0]", result)
	}
}

func TestRedisClusterTLSFixtureSupportsTrustedAndWrongCA(t *testing.T) {
	spec := FixtureSpec{
		Name: "redis-cluster-tls-ca",
		Kind: "redis-cluster",
		Redis: &RedisFixtureAssertion{
			TLS:                     true,
			AllowUnassertedCommands: true,
		},
	}
	named, err := startRedisFixture(spec)
	if err != nil {
		t.Fatalf("start TLS Redis cluster fixture: %v", err)
	}
	defer named.close()

	trusted := named.(interface{ caFile() string })
	caPEM, err := os.ReadFile(trusted.caFile())
	if err != nil {
		t.Fatalf("read Redis fixture CA: %v", err)
	}
	trustedRoots := x509.NewCertPool()
	if !trustedRoots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append Redis fixture CA")
	}
	trustedClient := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:       []string{named.address()},
		DialTimeout: time.Second,
		ReadTimeout: time.Second,
		TLSConfig: &tls.Config{
			RootCAs:    trustedRoots,
			ServerName: named.host(),
			MinVersion: tls.VersionTLS12,
		},
	})
	if err := trustedClient.Ping(context.Background()).Err(); err != nil {
		_ = trustedClient.Close()
		t.Fatalf("trusted Redis cluster Ping() error = %v", err)
	}
	_ = trustedClient.Close()

	wrongCert, _, err := generateRedisFixtureCertificate()
	if err != nil {
		t.Fatalf("generate wrong Redis fixture CA: %v", err)
	}
	wrongRoots := x509.NewCertPool()
	if !wrongRoots.AppendCertsFromPEM(wrongCert) {
		t.Fatal("append wrong Redis fixture CA")
	}
	wrongClient := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:       []string{named.address()},
		DialTimeout: 250 * time.Millisecond,
		ReadTimeout: 250 * time.Millisecond,
		MaxRetries:  0,
		TLSConfig: &tls.Config{
			RootCAs:    wrongRoots,
			ServerName: named.host(),
			MinVersion: tls.VersionTLS12,
		},
	})
	defer func() { _ = wrongClient.Close() }()
	if err := wrongClient.Ping(context.Background()).Err(); err == nil {
		t.Fatal("Redis cluster Ping() with wrong CA error = nil")
	}
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

func writeRESPIntegerArray(writer io.Writer, values ...int64) error {
	var builder strings.Builder
	builder.WriteString("*")
	builder.WriteString(strconv.Itoa(len(values)))
	builder.WriteString("\r\n")
	for _, value := range values {
		builder.WriteString(":")
		builder.WriteString(strconv.FormatInt(value, 10))
		builder.WriteString("\r\n")
	}
	return writeRESPRaw(writer, builder.String())
}

func writeRESPRaw(writer io.Writer, value string) error {
	_, err := io.WriteString(writer, value)
	return err
}
