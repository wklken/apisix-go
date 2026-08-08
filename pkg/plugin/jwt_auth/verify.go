package jwt_auth

import (
	"fmt"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"strings"
	"time"
)

func verifyToken(
	raw string,
	consumer consumerConfig,
	now time.Time,
	leeway time.Duration,
	requiredClaims []string,
) (jwt.MapClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{consumer.Algorithm}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithoutClaimsValidation(),
	)
	claims := jwt.MapClaims{}
	token, err := parser.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return jwtVerificationKey(consumer)
	})
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("failed to verify jwt: %w", err)
	}
	if err := verifyAPISIXTimeClaims(claims, now, leeway, requiredClaims); err != nil {
		return nil, err
	}
	return claims, nil
}

func jwtVerificationKey(consumer consumerConfig) (any, error) {
	if strings.HasPrefix(consumer.Algorithm, "HS") {
		secret, ok := consumer.secret()
		if !ok {
			return nil, fmt.Errorf("invalid secret")
		}
		return secret, nil
	}

	publicKey, ok := consumer.publicKey()
	if !ok {
		return nil, fmt.Errorf("invalid public key")
	}
	return publicKey, nil
}

// verifyAPISIXTimeClaims mirrors the APISIX jwt-auth claim semantics: exp is
// invalid at or before now-leeway and nbf is invalid at or after now+leeway.
// Claims are optional by default; when requiredClaims is configured, a missing
// claim is rejected.
func verifyAPISIXTimeClaims(claims jwt.MapClaims, now time.Time, leeway time.Duration, requiredClaims []string) error {
	check := requiredClaims
	if len(check) == 0 {
		check = []string{"exp", "nbf"}
	}

	nowUnix := now.Unix()
	leewaySeconds := int64(leeway / time.Second)
	for _, claim := range check {
		value, exists := claims[claim]
		if !exists {
			if len(requiredClaims) == 0 {
				continue
			}
			return fmt.Errorf("claim %s is missing", claim)
		}

		ts, ok := base.NumberClaim(value)
		if !ok {
			return fmt.Errorf("claim %s is not a number", claim)
		}

		switch claim {
		case "exp":
			if ts <= nowUnix-leewaySeconds {
				return fmt.Errorf("claim exp expired")
			}
		case "nbf":
			if ts >= nowUnix+leewaySeconds {
				return fmt.Errorf("claim nbf not valid yet")
			}
		}
	}

	return nil
}
