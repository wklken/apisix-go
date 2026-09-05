// Package compression coordinates content-coding selection for response
// compression plugins.  Plugins register their route-owned offers before the
// downstream handler writes the final response; the shared state then makes a
// single, order-independent decision.
package compression

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type Coding string

const (
	Identity Coding = "identity"
	Gzip     Coding = "gzip"
	Deflate  Coding = "deflate"
	Brotli   Coding = "br"
)

type ResponseMeta struct {
	Method string
	Status int
	Header http.Header
}

type Offer struct {
	Coding   Coding
	Rank     int
	Vary     bool
	Eligible func(ResponseMeta) bool
}

type Decision struct {
	Coding          Coding
	Vary            bool
	IdentityAllowed bool
}

type stateContextKey struct{}

// State is the per-request offer set and frozen negotiation result.  The
// mutex covers both registration and decision so a late registration cannot
// race with, or mutate, a result that has already been selected.
type State struct {
	mu              sync.Mutex
	offers          []Offer
	acceptEncodings []string
	frozen          bool
	decision        Decision
}

// Register adds offers to the request's shared negotiation state.  The
// returned request carries the state in its context and must be passed to the
// downstream handler.  Once Decide has frozen the state, later registrations
// are deliberately ignored so repeated middleware callbacks remain idempotent.
func Register(r *http.Request, offers ...Offer) (*http.Request, *State) {
	if r == nil {
		return nil, nil
	}
	state, _ := r.Context().Value(stateContextKey{}).(*State)
	if state == nil {
		state = &State{acceptEncodings: requestHeaderValues(r.Header, "Accept-Encoding")}
		r = r.WithContext(contextWithState(r, state))
	}
	state.mu.Lock()
	if !state.frozen {
		state.offers = append(state.offers, offers...)
	}
	state.mu.Unlock()
	return r, state
}

// contextWithState is kept as a small helper to make it explicit that the
// request is copied only when the first plugin creates the state.
func contextWithState(r *http.Request, state *State) context.Context {
	return context.WithValue(r.Context(), stateContextKey{}, state)
}

// Decide freezes the complete offer set and returns the same result for every
// subsequent call.  Eligibility is evaluated against the final response
// metadata supplied by the caller.
func (s *State) Decide(meta ResponseMeta) Decision {
	if s == nil {
		return Decision{Coding: Identity, IdentityAllowed: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return s.decision
	}
	s.decision = decide(s.acceptEncodings, s.offers, meta)
	s.frozen = true
	return s.decision
}

type preference struct {
	explicit bool
	valid    bool
	q        float64
}

type preferences struct {
	byCoding map[Coding]preference
	wildcard preference
}

func decide(headerValues []string, offers []Offer, meta ResponseMeta) Decision {
	// Final bodyless/status-transition responses do not participate in content
	// negotiation. A 304 may still carry an explicitly configured Vary contract,
	// but it must retain the upstream representation metadata unchanged.
	switch meta.Status {
	case http.StatusSwitchingProtocols, http.StatusNoContent:
		return Decision{Coding: Identity, IdentityAllowed: true}
	}

	prefs := parsePreferences(headerValues)
	identityAllowed, identityQuality, identityExplicit := identityPreference(prefs)
	if coding := responseContentCoding(meta.Header); coding != "" {
		// APISIX compression plugins return before applying their optional Vary
		// policy when the upstream response is already encoded.
		return Decision{Coding: coding, IdentityAllowed: identityAllowed}
	}
	eligibleOffers := make([]Offer, 0, len(offers))
	vary := false
	for _, offer := range offers {
		if offer.Coding == Identity {
			continue
		}
		eligible := offer.Eligible == nil || offer.Eligible(meta)
		if eligible {
			eligibleOffers = append(eligibleOffers, offer)
			if q, available := codingQuality(prefs, offer.Coding); offer.Vary && available && q > 0 {
				vary = true
			}
		}
	}
	if meta.Status == http.StatusNotModified {
		return Decision{Coding: Identity, Vary: vary, IdentityAllowed: true}
	}

	best := candidate{}
	for _, offer := range eligibleOffers {
		q, available := codingQuality(prefs, offer.Coding)
		if !available || q <= 0 {
			continue
		}
		candidate := candidate{coding: offer.Coding, q: q, rank: offer.Rank, set: true}
		if !best.set || betterCandidate(candidate, best) {
			best = candidate
		}
	}

	if identityExplicit && identityAllowed && (!best.set || identityQuality > best.q ||
		(identityQuality == best.q && identityRank() > best.rank)) {
		return Decision{Coding: Identity, Vary: vary, IdentityAllowed: true}
	}
	if best.set {
		return Decision{Coding: best.coding, Vary: vary, IdentityAllowed: identityAllowed}
	}
	// APISIX compression filters pass through the upstream response when no
	// configured coding is selected, even when identity has q=0.
	return Decision{Coding: Identity, Vary: vary, IdentityAllowed: identityAllowed}
}

func responseContentCoding(header http.Header) Coding {
	for actual, values := range header {
		if !strings.EqualFold(actual, "Content-Encoding") || len(values) == 0 {
			continue
		}
		if coding := strings.ToLower(strings.TrimSpace(values[0])); coding != "" {
			return Coding(coding)
		}
	}
	return ""
}

type candidate struct {
	coding Coding
	q      float64
	rank   int
	set    bool
}

func betterCandidate(next, current candidate) bool {
	if !current.set {
		return true
	}
	if next.q != current.q {
		return next.q > current.q
	}
	if next.rank != current.rank {
		return next.rank > current.rank
	}
	return canonicalRank(next.coding) > canonicalRank(current.coding)
}

func identityRank() int { return 0 }

func canonicalRank(coding Coding) int {
	switch coding {
	case Brotli:
		return 3
	case Gzip:
		return 2
	case Deflate:
		return 1
	default:
		return 0
	}
}

func codingQuality(prefs preferences, coding Coding) (float64, bool) {
	if pref, ok := prefs.byCoding[coding]; ok && pref.explicit {
		return pref.q, pref.valid
	}
	if prefs.wildcard.explicit {
		return prefs.wildcard.q, prefs.wildcard.valid
	}
	return 0, false
}

func identityPreference(prefs preferences) (allowed bool, q float64, explicit bool) {
	if pref, ok := prefs.byCoding[Identity]; ok && pref.explicit {
		return pref.valid && pref.q > 0, pref.q, true
	}
	if prefs.wildcard.explicit && prefs.wildcard.valid && prefs.wildcard.q == 0 {
		return false, 0, false
	}
	return true, 0, false
}

func parsePreferences(values []string) preferences {
	prefs := preferences{byCoding: make(map[Coding]preference)}
	for _, value := range values {
		for member := range strings.SplitSeq(value, ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			codingText, params, _ := strings.Cut(member, ";")
			coding := Coding(strings.ToLower(strings.TrimSpace(codingText)))
			if coding == "" {
				continue
			}
			quality, valid := parseMemberQuality(params)
			switch coding {
			case Identity, Gzip, Deflate, Brotli:
				mergePreference(prefs.byCoding, coding, preference{explicit: true, valid: valid, q: quality})
			case Coding("*"):
				mergeWildcard(&prefs.wildcard, preference{explicit: true, valid: valid, q: quality})
			}
		}
	}
	return prefs
}

func mergePreference(values map[Coding]preference, coding Coding, next preference) {
	current, exists := values[coding]
	if !exists {
		values[coding] = next
		return
	}
	// An invalid duplicate is unavailable but must not erase a valid one.
	if current.valid && next.valid {
		if next.q > current.q {
			values[coding] = next
		}
		return
	}
	if !current.valid && next.valid {
		values[coding] = next
	}
}

func mergeWildcard(current *preference, next preference) {
	if !current.explicit || (!current.valid && next.valid) ||
		(current.valid && next.valid && next.q > current.q) {
		*current = next
	}
}

func parseMemberQuality(params string) (float64, bool) {
	quality := 1.0
	found := false
	for param := range strings.SplitSeq(params, ";") {
		param = strings.TrimSpace(param)
		if param == "" {
			continue
		}
		key, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		if found {
			return 0, false
		}
		found = true
		var valid bool
		quality, valid = parseQuality(strings.TrimSpace(value))
		if !valid {
			return 0, false
		}
	}
	return quality, true
}

func parseQuality(value string) (float64, bool) {
	if value == "" || strings.ContainsAny(value, "+-eE") {
		return 0, false
	}
	integer, fraction, hasDot := strings.Cut(value, ".")
	if !hasDot {
		integer = value
		fraction = ""
	} else {
		if strings.Contains(fraction, ".") {
			return 0, false
		}
		if len(fraction) > 3 {
			return 0, false
		}
	}
	if integer != "0" && integer != "1" {
		return 0, false
	}
	for _, r := range fraction {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	if integer == "1" {
		for _, r := range fraction {
			if r != '0' {
				return 0, false
			}
		}
		return 1, true
	}
	if fraction == "" {
		return 0, true
	}
	parsed, err := strconv.Atoi(fraction)
	if err != nil {
		return 0, false
	}
	quality := float64(parsed)
	for range fraction {
		quality /= 10
	}
	return quality, !math.IsNaN(quality) && !math.IsInf(quality, 0)
}

func requestHeaderValues(header http.Header, name string) []string {
	var values []string
	for actual, entries := range header {
		if strings.EqualFold(actual, name) {
			values = append(values, entries...)
		}
	}
	return values
}
