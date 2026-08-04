package base

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

// JWTToken is the parsed representation of an unverified JWT.
type JWTToken struct {
	Header    map[string]any
	Payload   map[string]any
	Signing   string
	Signature []byte
}

// ParseJWT splits and decodes a three-part JWT without verifying it.
func ParseJWT(raw string) (JWTToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return JWTToken{}, fmt.Errorf("token must have three parts")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return JWTToken{}, err
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JWTToken{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return JWTToken{}, err
	}

	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return JWTToken{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return JWTToken{}, err
	}

	return JWTToken{
		Header:    header,
		Payload:   payload,
		Signing:   parts[0] + "." + parts[1],
		Signature: signature,
	}, nil
}

// NumberClaim converts a JSON-decoded numeric claim to an int64.
func NumberClaim(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}
