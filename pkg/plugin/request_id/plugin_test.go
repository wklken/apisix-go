package request_id

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

func TestKSUIDIDReturnsErrorForFailingReader(t *testing.T) {
	p := &Plugin{}
	id, err := p.ksuidID(failingReader{})
	if err == nil {
		t.Fatal("ksuidID() error = nil, want failure")
	}
	if id != "" {
		t.Fatalf("ksuidID() = %q, want empty on failure", id)
	}
}

func TestUUIDV4IDReturnsErrorForFailingReader(t *testing.T) {
	p := &Plugin{}
	id, err := p.uuidV4ID(failingReader{})
	if err == nil {
		t.Fatal("uuidV4ID() error = nil, want failure")
	}
	if id != "" {
		t.Fatalf("uuidV4ID() = %q, want empty on failure", id)
	}
}

func TestSchemaAcceptsUUIDv7AndKSUIDAlgorithms(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, algorithm := range []string{"uuidv7", "ksuid"} {
		if err := util.Validate(map[string]any{"algorithm": algorithm}, p.GetSchema()); err != nil {
			t.Fatalf("%s algorithm should validate: %v", algorithm, err)
		}
	}
}

func TestUUIDv7GeneratesVersionSevenRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	requestID := generatedRequestID(t, p)
	if len(requestID) != 36 || requestID[14] != '7' {
		t.Fatalf("request id = %q, want UUIDv7 format", requestID)
	}
}

func TestUUIDv7IsLexicographicallyMonotoneWithinMillisecond(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	p.uuidv7Now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	p.uuidv7Rand = bytes.NewReader(make([]byte, 64))
	p.uuidv7Sequence = 0xffe
	p.uuidv7LastMS = 1_700_000_000_000

	previous := p.uuidv7ID()
	for range 20 {
		current := p.uuidv7ID()
		if current <= previous {
			t.Fatalf("UUIDv7 is not monotone: previous=%q current=%q", previous, current)
		}
		previous = current
	}
}

func TestUUIDv7IsUniqueAcrossConcurrentCalls(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	const count = 180
	values := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			values <- p.uuidv7ID()
		})
	}
	wg.Wait()
	close(values)

	seen := make(map[string]struct{}, count)
	for value := range values {
		if _, ok := seen[value]; ok {
			t.Fatalf("duplicate UUIDv7 %q", value)
		}
		seen[value] = struct{}{}
	}
}

func TestUUIDv7KeepsOrderingWhenClockMovesBackwards(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	times := []time.Time{
		time.UnixMilli(1_700_000_000_100),
		time.UnixMilli(1_700_000_000_099),
	}
	p.uuidv7Now = func() time.Time {
		current := times[0]
		times = times[1:]
		return current
	}
	p.uuidv7Rand = bytes.NewReader(make([]byte, 64))

	first := p.uuidv7ID()
	second := p.uuidv7ID()
	if second <= first {
		t.Fatalf("UUIDv7 after clock rollback = %q, want greater than %q", second, first)
	}
}

func TestUUIDv7RefreshesTimeAfterSequenceOverflow(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	const milliseconds = int64(1_700_000_000_100)
	calls := 0
	p.uuidv7Now = func() time.Time {
		calls++
		if calls == 1 {
			return time.UnixMilli(milliseconds)
		}
		return time.UnixMilli(milliseconds + 1)
	}
	p.uuidv7Rand = bytes.NewReader(make([]byte, 64))
	p.uuidv7LastMS = milliseconds
	p.uuidv7Sequence = 0x3ffff

	requestID := p.uuidv7ID()
	timestamp := strings.ReplaceAll(requestID[:13], "-", "")
	if want := fmt.Sprintf("%012x", milliseconds+1); timestamp != want {
		t.Fatalf("UUIDv7 timestamp = %q, want refreshed timestamp %q", timestamp, want)
	}
	if calls < 2 {
		t.Fatalf("clock calls = %d, want overflow refresh", calls)
	}
}

func TestKSUIDGeneratesSortableBase62RequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "ksuid"})
	requestID := generatedRequestID(t, p)
	if len(requestID) != 27 {
		t.Fatalf("request id = %q, want 27-character KSUID", requestID)
	}
	if strings.Trim(requestID, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") != "" {
		t.Fatalf("request id = %q, want base62 characters", requestID)
	}
}

func TestHandlerPreservesIncomingRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuid"})
	req := httptest.NewRequest(http.MethodGet, "/request-id", nil)
	req.Header.Set("X-Request-Id", "client-provided")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Request-Id"); got != "client-provided" {
			t.Fatalf("request id = %q, want client-provided", got)
		}
		if got := r.Context().Value(apisixctx.RequestIDKey); got != "client-provided" {
			t.Fatalf("context request_id = %#v, want client-provided", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-Id"); got != "client-provided" {
		t.Fatalf("response request id = %q, want client-provided", got)
	}
}

func TestHandlerCanOmitResponseHeader(t *testing.T) {
	include := false
	p := newTestPlugin(t, Config{IncludeInResponse: &include})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Request-Id"); got == "" {
			t.Fatal("upstream request is missing X-Request-Id")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/request-id", nil))

	if got := rr.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("response request id = %q, want empty", got)
	}
}

func generatedRequestID(t *testing.T, p *Plugin) string {
	t.Helper()
	var requestID string
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/request-id", nil))
	if requestID == "" {
		t.Fatal("request id is empty")
	}
	return requestID
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}

func TestKSUIDHasFixedLengthAndAlphabet(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "ksuid"})
	id, err := p.ksuidID(rand.Reader)
	if err != nil {
		t.Fatalf("ksuidID() error = %v", err)
	}
	if len(id) != 27 {
		t.Fatalf("ksuid length = %d, want 27", len(id))
	}
	for _, ch := range id {
		if !strings.ContainsRune(ksuidAlphabet, ch) {
			t.Fatalf("ksuid contains out-of-alphabet char %q", ch)
		}
	}
}

func TestKSUIDCarriesRecentTimestampPrefix(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "ksuid"})
	before := time.Now().Unix()
	id, err := p.ksuidID(rand.Reader)
	if err != nil {
		t.Fatalf("ksuidID() error = %v", err)
	}
	// decode base62 back to bytes
	value := new(big.Int)
	base := big.NewInt(62)
	for _, ch := range id {
		value.Mul(value, base)
		value.Add(value, big.NewInt(int64(strings.IndexRune(ksuidAlphabet, ch))))
	}
	raw := value.Bytes()
	if len(raw) < 4 {
		t.Fatalf("decoded ksuid too short: %x", raw)
	}
	offset := binary.BigEndian.Uint32(raw[:4])
	epoch := int64(offset) + ksuidEpochSeconds
	if epoch < before-5 || epoch > time.Now().Unix()+5 {
		t.Fatalf("ksuid timestamp = %d, want within the current window", epoch)
	}
}

func TestKSUIDIsUniqueAcrossCalls(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "ksuid"})
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id, err := p.ksuidID(rand.Reader)
		if err != nil {
			t.Fatalf("ksuidID() error = %v", err)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate ksuid %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestKSUIDEncodingIsDeterministic(t *testing.T) {
	input := []byte{
		0x00,
		0x01,
		0x02,
		0x03,
		0x04,
		0x05,
		0x06,
		0x07,
		0x08,
		0x09,
		0x0a,
		0x0b,
		0x0c,
		0x0d,
		0x0e,
		0x0f,
		0x10,
		0x11,
		0x12,
		0x13,
	}
	first := encodeBase62(input)
	second := encodeBase62(input)
	if first != second {
		t.Fatalf("encodeBase62 not deterministic: %q vs %q", first, second)
	}
	if len(first) != 27 {
		t.Fatalf("encodeBase62 length = %d, want 27", len(first))
	}
}

func TestRangeIDDoesNotAliasReusedPoolBuffer(t *testing.T) {
	p := newTestPlugin(t, Config{
		Algorithm: "range_id",
		RangeID:   RangeID{Length: 16, CharSet: "AAAAAB"},
	})
	first := p.rangeID("AAAAAB", 16)
	want := strings.Clone(first)
	for range 10_000 {
		_ = p.rangeID("ZZZZZY", 16)
	}
	if first != want {
		t.Fatalf("first range id changed after pool reuse: got %q, want %q", first, want)
	}
}

func TestRangeIDUsesCustomAlphabetAndLength(t *testing.T) {
	p := newTestPlugin(t, Config{
		Algorithm: "range_id",
		RangeID:   RangeID{Length: 8, CharSet: "ABC"},
	})
	id := p.rangeID("ABC", 8)
	if len(id) != 8 {
		t.Fatalf("range id length = %d, want 8", len(id))
	}
	for _, ch := range id {
		if !strings.ContainsRune("ABC", ch) {
			t.Fatalf("range id contains out-of-alphabet char %q", ch)
		}
	}
}

func TestRangeIDDefaultsAreAppliedBeforeGeneration(t *testing.T) {
	// The schema requires char_set minLength 6 and length minimum 6, and
	// PostInit defaults empty values, so the handler can never observe an
	// invalid range_id configuration.
	p := newTestPlugin(t, Config{
		Algorithm: "range_id",
		RangeID:   RangeID{},
	})
	if p.config.RangeID.Length != 16 {
		t.Fatalf("default range_id length = %d, want 16", p.config.RangeID.Length)
	}
	if len(p.config.RangeID.CharSet) < 6 {
		t.Fatalf("default range_id char_set = %q, want at least 6 chars", p.config.RangeID.CharSet)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := request.Header.Get(p.config.HeaderName); len(got) != 16 {
		t.Fatalf("generated range id length = %d, want 16", len(got))
	}
}
