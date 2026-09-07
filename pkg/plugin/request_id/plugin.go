package request_id

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/gofrs/uuid"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/oxtoacart/bpool"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const (
	// version = "0.1"
	priority = 12015
	name     = "request-id"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "header_name": {
		"type": "string",
		"default": "X-Request-Id"
	  },
	  "include_in_response": {
		"type": "boolean",
		"default": true
	  },
	  "algorithm": {
		"type": "string",
		"enum": ["uuid", "uuidv7", "nanoid", "ksuid", "range_id"],
		"default": "uuid"
	  },
	  "range_id": {
		"type": "object",
		"properties": {
		  "length": {
			"type": "integer",
			"minimum": 6,
			"default": 16
		  },
		  "char_set": {
			"type": "string",
			"minLength": 6,
			"default": "abcdefghijklmnopqrstuvwxyzABCDEFGHIGKLMNOPQRSTUVWXYZ0123456789"
		  }
		},
		"default": {}
	  }
	}
  }
`

type Plugin struct {
	base.BasePlugin
	config Config

	bytePool *bpool.BytePool

	uuidv7Mu       sync.Mutex
	uuidv7LastMS   int64
	uuidv7Sequence uint32
	uuidv7Random   [7]byte
	uuidv7Now      func() time.Time
	uuidv7Rand     io.Reader
}

type Config struct {
	HeaderName        string  `json:"header_name"`
	IncludeInResponse *bool   `json:"include_in_response"`
	Algorithm         string  `json:"algorithm"`
	RangeID           RangeID `json:"range_id"`
}

type RangeID struct {
	Length  int    `json:"length"`
	CharSet string `json:"char_set"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.uuidv7Now == nil {
		p.uuidv7Now = time.Now
	}
	if p.uuidv7Rand == nil {
		p.uuidv7Rand = crand.Reader
	}
	if p.config.HeaderName == "" {
		p.config.HeaderName = "X-Request-Id"
	}
	// how to know the include_in_response is set to false or not been set?
	if p.config.IncludeInResponse == nil {
		defaultValue := true
		p.config.IncludeInResponse = &defaultValue
	}

	if p.config.Algorithm == "" {
		p.config.Algorithm = "uuid"
	}

	if p.config.Algorithm == "range_id" {
		if p.config.RangeID.Length == 0 {
			p.config.RangeID.Length = 16
		}
		if p.config.RangeID.CharSet == "" {
			p.config.RangeID.CharSet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIGKLMNOPQRSTUVWXYZ0123456789"
		}

		p.bytePool = bpool.NewBytePool(10000, p.config.RangeID.Length)
	}

	return nil
}

// Handler preserves header-filter timing for callers outside the production executor.
func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentRequest := r
		committed := false
		apply := func() {
			if committed {
				return
			}
			committed = true
			_ = p.RunStreamingHeaderFilter(currentRequest, &base.StreamingResponseState{Header: w.Header()})
		}
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					if code >= 200 || code == 101 {
						apply()
					}
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) { apply(); return next(b) }
			},
			WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
				return func(s string) (int, error) { apply(); return next(s) }
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(reader io.Reader) (int64, error) { apply(); return next(reader) }
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc { return func() { apply(); next() } },
			FlushError: func(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
				return func() error { apply(); return next() }
			},
		})
		base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			currentRequest = request
			next.ServeHTTP(w, request)
		})).ServeHTTP(wrapped, r)
		apply()
	})
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	r = apisixctx.WithRequestVars(r)
	requestID := r.Header.Get(p.config.HeaderName)
	if requestID == "" {
		var err error
		switch p.config.Algorithm {
		case "uuid":
			requestID, err = p.uuidV4ID(crand.Reader)
		case "uuidv7":
			requestID = p.uuidv7ID()
		case "nanoid":
			requestID, _ = gonanoid.New()
		case "ksuid":
			requestID, err = p.ksuidID(crand.Reader)
		case "range_id":
			requestID = p.rangeID(p.config.RangeID.CharSet, p.config.RangeID.Length)
		}
		if err != nil {
			http.Error(w, "failed to generate request id", http.StatusInternalServerError)
			return base.StopRequest(r)
		}
	}

	r.Header.Set(p.config.HeaderName, requestID)
	apisixctx.RegisterApisixVar(r, "$apisix_request_id", requestID)
	apisixctx.RegisterRequestVar(r, "$apisix_request_id", requestID)

	requestContext := context.WithValue(r.Context(), responseIDKey{p.config.HeaderName}, requestID)
	requestContext = context.WithValue(requestContext, apisixctx.RequestIDKey, requestID)
	return base.ContinueRequest(r.WithContext(requestContext))
}

type responseIDKey struct{ header string }

func (p *Plugin) RunStreamingHeaderFilter(r *http.Request, state *base.StreamingResponseState) error {
	if state == nil || !*p.config.IncludeInResponse || state.Header.Get(p.config.HeaderName) != "" {
		return nil
	}
	requestID, _ := r.Context().Value(responseIDKey{p.config.HeaderName}).(string)
	if requestID == "" {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	state.Header.Set(p.config.HeaderName, requestID)
	return nil
}

func (p *Plugin) rangeID(charSet string, length int) string {
	// id := make([]byte, length)
	id := p.bytePool.Get()
	defer p.bytePool.Put(id)

	for i := range length {
		id[i] = charSet[rand.Intn(len(charSet))]
	}

	return string(id)
}

// uuidv7ID generates a UUIDv7 with strict per-instance monotonic ordering.
// gofrs/uuid.NewV7 is intentionally not used: it does not expose the 22-bit
// monotonic sequence with the clock-rollback and overflow handling this
// plugin relies on for lexicographically ordered request IDs.
func (p *Plugin) uuidv7ID() string {
	p.uuidv7Mu.Lock()
	defer p.uuidv7Mu.Unlock()

	milliseconds := p.uuidv7Now().UnixMilli()
	if milliseconds > p.uuidv7LastMS {
		p.resetUUIDv7(milliseconds)
	} else {
		if milliseconds < p.uuidv7LastMS {
			milliseconds = p.uuidv7LastMS
		}
		p.uuidv7Sequence++
		if p.uuidv7Sequence > 0x3ffff {
			for milliseconds <= p.uuidv7LastMS {
				time.Sleep(time.Millisecond)
				milliseconds = p.uuidv7Now().UnixMilli()
			}
			p.resetUUIDv7(milliseconds)
		}
	}

	var value uuid.UUID
	value[0] = byte(milliseconds >> 40)
	value[1] = byte(milliseconds >> 32)
	value[2] = byte(milliseconds >> 24)
	value[3] = byte(milliseconds >> 16)
	value[4] = byte(milliseconds >> 8)
	value[5] = byte(milliseconds)
	binary.BigEndian.PutUint16(value[6:8], 0x7000|uint16(p.uuidv7Sequence>>6))
	value[8] = 0x80 | byte(p.uuidv7Sequence&0x3f)
	copy(value[9:], p.uuidv7Random[:])
	return value.String()
}

func (p *Plugin) resetUUIDv7(milliseconds int64) {
	p.uuidv7LastMS = milliseconds
	p.uuidv7Sequence = 0
	if _, err := io.ReadFull(p.uuidv7Rand, p.uuidv7Random[:]); err != nil {
		fallback, fallbackErr := uuid.NewV4()
		if fallbackErr == nil {
			copy(p.uuidv7Random[:], fallback[:7])
		}
	}
}

const (
	ksuidEpochSeconds = 1400000000
	ksuidAlphabet     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

func (p *Plugin) uuidV4ID(reader io.Reader) (string, error) {
	id, err := uuid.NewGenWithOptions(uuid.WithRandomReader(reader)).NewV4()
	if err != nil {
		return "", fmt.Errorf("generate uuid request id: %w", err)
	}
	return id.String(), nil
}

func (p *Plugin) ksuidID(reader io.Reader) (string, error) {
	value := make([]byte, 20)
	binary.BigEndian.PutUint32(value[:4], uint32(time.Now().Unix()-ksuidEpochSeconds))
	if _, err := io.ReadFull(reader, value[4:]); err != nil {
		return "", fmt.Errorf("generate ksuid request id: %w", err)
	}
	return encodeBase62(value), nil
}

func encodeBase62(value []byte) string {
	remaining := new(big.Int).SetBytes(value)
	base := big.NewInt(int64(len(ksuidAlphabet)))
	encoded := make([]byte, 27)
	for index := range slices.Backward(encoded) {
		digit := new(big.Int)
		remaining.QuoRem(remaining, base, digit)
		encoded[index] = ksuidAlphabet[digit.Int64()]
	}
	return string(encoded)
}
