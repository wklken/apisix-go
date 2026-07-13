package request_id

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"math/big"
	"math/rand"
	"net/http"
	"slices"
	"time"

	"github.com/gofrs/uuid"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/oxtoacart/bpool"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		requestID := r.Header.Get(p.config.HeaderName)
		if requestID == "" {
			switch p.config.Algorithm {
			case "uuid":
				requestID = uuid.Must(uuid.NewV4()).String()
			case "uuidv7":
				requestID = p.uuidv7ID()
			case "nanoid":
				requestID, _ = gonanoid.New()
			case "ksuid":
				requestID = p.ksuidID()
			case "range_id":
				requestID = p.rangeID(p.config.RangeID.CharSet, p.config.RangeID.Length)
			}
		}

		r.Header.Set(p.config.HeaderName, requestID)

		if *p.config.IncludeInResponse {
			w.Header().Set(p.config.HeaderName, requestID)
		}

		ctx = context.WithValue(ctx, apisixctx.RequestIDKey, requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rangeID(charSet string, length int) string {
	// id := make([]byte, length)
	id := p.bytePool.Get()
	defer p.bytePool.Put(id)

	for i := range length {
		id[i] = charSet[rand.Intn(len(charSet))]
	}

	return util.BytesToString(id)
}

func (p *Plugin) uuidv7ID() string {
	value, err := uuid.NewV7()
	if err != nil {
		return uuid.Must(uuid.NewV4()).String()
	}
	return value.String()
}

const (
	ksuidEpochSeconds = 1400000000
	ksuidAlphabet     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

func (p *Plugin) ksuidID() string {
	value := make([]byte, 20)
	binary.BigEndian.PutUint32(value[:4], uint32(time.Now().Unix()-ksuidEpochSeconds))
	if _, err := crand.Read(value[4:]); err != nil {
		fallback := uuid.Must(uuid.NewV4())
		copy(value[4:], fallback[:])
	}
	return encodeBase62(value)
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
