package hmac_auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strings"
)

func retrieveSignatureParams(r *http.Request) (signatureParams, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return signatureParams{}, errors.New("missing Authorization header")
	}
	if !strings.HasPrefix(auth, "Signature") {
		return signatureParams{}, errors.New("authorization header does not start with 'Signature'")
	}

	fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(auth, "Signature")), ",")
	params := signatureParams{
		Date:       r.Header.Get("Date"),
		BodyDigest: r.Header.Get("Digest"),
	}
	for _, field := range fields {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "keyId":
			params.KeyID = value
		case "algorithm":
			params.Algorithm = value
		case "headers":
			params.Headers = strings.Fields(value)
		case "signature":
			params.Signature = value
		}
	}

	return params, nil
}

func validateSignature(r *http.Request, secretKey string, params signatureParams) error {
	requestSignature, err := base64.StdEncoding.DecodeString(params.Signature)
	if err != nil {
		return errInvalidSignature
	}

	generatedSignature, err := generateSignature(r, secretKey, params)
	if err != nil {
		return errInvalidSignature
	}
	if subtle.ConstantTimeCompare(requestSignature, generatedSignature) != 1 {
		return errInvalidSignature
	}
	return nil
}

func generateSignature(r *http.Request, secretKey string, params signatureParams) ([]byte, error) {
	var signingString strings.Builder
	signingString.WriteString(params.KeyID + "\n")
	for _, header := range params.Headers {
		if header == "@request-target" {
			signingString.WriteString(r.Method + " " + requestURI(r) + "\n")
			continue
		}
		if value := r.Header.Get(header); value != "" {
			signingString.WriteString(header + ": " + value + "\n")
		}
	}

	hashFunc, err := hashForAlgorithm(params.Algorithm)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(hashFunc, []byte(secretKey))
	mac.Write([]byte(signingString.String()))
	return mac.Sum(nil), nil
}

func hashForAlgorithm(algorithm string) (func() hash.Hash, error) {
	switch algorithm {
	case "hmac-sha1":
		return sha1.New, nil
	case "hmac-sha256":
		return sha256.New, nil
	case "hmac-sha512":
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("unsupported algorithm %q", algorithm)
	}
}

func requestURI(r *http.Request) string {
	if r.URL == nil || r.URL.RequestURI() == "" {
		return "/"
	}
	return r.URL.RequestURI()
}

var errBodyTooLarge = errors.New("request body too large")
