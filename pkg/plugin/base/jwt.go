package base

import (
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
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
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(raw, jwt.MapClaims{})
	if err != nil {
		return JWTToken{}, err
	}
	payload, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return JWTToken{}, fmt.Errorf("unexpected JWT claims type")
	}
	parts := strings.Split(raw, ".")
	return JWTToken{
		Header:    token.Header,
		Payload:   payload,
		Signing:   parts[0] + "." + parts[1],
		Signature: token.Signature,
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
