package admin

// a chi middleware to check the header X-API-KEY is equals to the admin key
import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

func constantTimeEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func AdminKeyMiddleware(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if the X-API-KEY header is present and equals to the admin key
			apiKey := r.Header.Get("X-API-KEY")
			if apiKey == "" {
				http.Error(w, "Unauthorized, X-API-KEY absent", http.StatusUnauthorized)
				return
			}

			if !constantTimeEqual(apiKey, adminKey) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Call the next handler
			next.ServeHTTP(w, r)
		})
	}
}
