package compression

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func eligibleOffer(coding Coding, rank int) Offer {
	return Offer{
		Coding: coding,
		Rank:   rank,
		Vary:   true,
		Eligible: func(ResponseMeta) bool {
			return true
		},
	}
}

func TestNegotiationRespectsOfferVaryPolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		header   http.Header
		wantVary bool
	}{
		{name: "compressed response", status: http.StatusOK},
		{name: "not modified", status: http.StatusNotModified},
		{
			name:   "preencoded response",
			status: http.StatusOK,
			header: http.Header{"Content-Encoding": []string{"gzip"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "br")
			_, state := Register(req, Offer{
				Coding: Brotli,
				Rank:   996,
				Eligible: func(ResponseMeta) bool {
					return true
				},
			})
			decision := state.Decide(ResponseMeta{
				Method: http.MethodGet,
				Status: tt.status,
				Header: tt.header,
			})
			if decision.Vary != tt.wantVary {
				t.Fatalf("Vary = %t, want %t (%#v)", decision.Vary, tt.wantVary, decision)
			}
		})
	}
}

func allOffers() []Offer {
	return []Offer{
		eligibleOffer(Brotli, 996),
		eligibleOffer(Gzip, 995),
		eligibleOffer(Deflate, 994),
	}
}

func TestNegotiationMatrix(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding []string
		offers         []Offer
		wantCoding     Coding
		wantNA         bool
		wantVary       bool
	}{
		{
			name:           "missing selects identity without vary",
			acceptEncoding: nil,
			offers:         allOffers(),
			wantCoding:     Identity,
		},
		{
			name:           "empty selects identity",
			acceptEncoding: []string{""},
			offers:         allOffers(),
			wantCoding:     Identity,
		},
		{
			name:           "repeated fields and duplicate coding use highest valid q",
			acceptEncoding: []string{"gzip;q=0.2", "GZIP;Q=0.8, deflate;q=0.1"},
			offers:         allOffers(),
			wantCoding:     Gzip,
			wantVary:       true,
		},
		{
			name:           "wildcard does not override explicit zero",
			acceptEncoding: []string{"GZIP;q=0, *;q=0.9"},
			offers:         allOffers(),
			wantCoding:     Brotli,
			wantVary:       true,
		},
		{
			name:           "invalid explicit q remains unavailable",
			acceptEncoding: []string{"gzip;q=1.0000, *;q=0"},
			offers:         allOffers(),
			wantNA:         true,
		},
		{
			name:           "invalid duplicate does not erase valid q",
			acceptEncoding: []string{"gzip;q=bogus, gzip;q=0.5, deflate;q=0"},
			offers:         allOffers(),
			wantCoding:     Gzip,
			wantVary:       true,
		},
		{
			name:           "explicit identity participates",
			acceptEncoding: []string{"gzip;q=0.5, identity;q=1"},
			offers:         allOffers(),
			wantCoding:     Identity,
			wantVary:       true,
		},
		{
			name:           "individual offer set",
			acceptEncoding: []string{"br;q=1"},
			offers:         []Offer{eligibleOffer(Gzip, 995)},
			wantCoding:     Identity,
		},
		{
			name:           "tie uses server rank",
			acceptEncoding: []string{"gzip, deflate, br"},
			offers:         allOffers(),
			wantCoding:     Brotli,
			wantVary:       true,
		},
		{
			name:           "all representations excluded",
			acceptEncoding: []string{"gzip;q=0, deflate;q=0, br;q=0, identity;q=0"},
			offers:         allOffers(),
			wantNA:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, value := range tt.acceptEncoding {
				req.Header.Add("Accept-Encoding", value)
			}
			_, state := Register(req, tt.offers...)
			decision := state.Decide(ResponseMeta{
				Method: http.MethodGet,
				Status: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/plain"}},
			})
			if decision.NotAcceptable != tt.wantNA {
				t.Fatalf("NotAcceptable = %t, want %t (%#v)", decision.NotAcceptable, tt.wantNA, decision)
			}
			if !tt.wantNA && decision.Coding != tt.wantCoding {
				t.Fatalf("Coding = %q, want %q (%#v)", decision.Coding, tt.wantCoding, decision)
			}
			if decision.Vary != tt.wantVary {
				t.Fatalf("Vary = %t, want %t (%#v)", decision.Vary, tt.wantVary, decision)
			}
		})
	}
}

func TestNegotiationIndependentOfRegistrationOrder(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0.8, br;q=0.8, deflate;q=0.8")
	_, first := Register(req, eligibleOffer(Gzip, 995), eligibleOffer(Brotli, 996), eligibleOffer(Deflate, 994))
	_, second := Register(req, eligibleOffer(Deflate, 994), eligibleOffer(Brotli, 996), eligibleOffer(Gzip, 995))
	meta := ResponseMeta{Method: http.MethodGet, Status: http.StatusOK, Header: make(http.Header)}
	if got := first.Decide(meta); got.Coding != Brotli {
		t.Fatalf("first order selected %q, want br", got.Coding)
	}
	if got := second.Decide(meta); got.Coding != Brotli {
		t.Fatalf("second order selected %q, want br", got.Coding)
	}
}

func TestDecisionIsIdempotentAndConcurrencySafe(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, state := Register(req, eligibleOffer(Gzip, 995))
	meta := ResponseMeta{Method: http.MethodGet, Status: http.StatusOK, Header: make(http.Header)}
	want := state.Decide(meta)
	results := make(chan Decision, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			results <- state.Decide(meta)
		})
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got != want {
			t.Fatalf("concurrent decision %#v differs from %#v", got, want)
		}
	}
}

func TestNegotiationBodylessStatusesDoNotParticipate(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantVary bool
	}{
		{name: "switching protocols", status: http.StatusSwitchingProtocols},
		{name: "no content", status: http.StatusNoContent},
		{name: "not modified", status: http.StatusNotModified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "*;q=0")
			_, state := Register(req, Offer{
				Coding: Brotli,
				Rank:   996,
				Eligible: func(ResponseMeta) bool {
					return true
				},
			})
			decision := state.Decide(ResponseMeta{Method: http.MethodGet, Status: tt.status, Header: make(http.Header)})
			if decision.Coding != Identity || decision.NotAcceptable || decision.Vary != tt.wantVary {
				t.Fatalf("decision = %#v, want identity/notAcceptable=false/vary=%t", decision, tt.wantVary)
			}
		})
	}
}

func TestNegotiationPreservesAcceptedExistingCoding(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br, identity;q=0")
	_, state := Register(req, Offer{
		Coding: Brotli,
		Rank:   996,
		Eligible: func(ResponseMeta) bool {
			return false
		},
	})
	decision := state.Decide(ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Encoding": []string{"br"}},
	})
	if decision.Coding != Brotli || decision.NotAcceptable || decision.Vary {
		t.Fatalf("decision = %#v, want br pass-through without plugin-added Vary", decision)
	}
}

func TestNegotiationReportsIdentityFallbackAvailability(t *testing.T) {
	for _, tt := range []struct {
		name           string
		acceptEncoding string
		want           bool
	}{
		{name: "implicit identity", acceptEncoding: "br", want: true},
		{name: "identity forbidden", acceptEncoding: "br, identity;q=0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			_, state := Register(req, eligibleOffer(Brotli, 996))
			decision := state.Decide(ResponseMeta{Method: http.MethodGet, Status: http.StatusOK})
			if decision.Coding != Brotli || decision.IdentityAllowed != tt.want {
				t.Fatalf("decision = %#v, want coding br identityAllowed=%t", decision, tt.want)
			}
		})
	}
}
