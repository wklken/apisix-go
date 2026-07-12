package degraphql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRewritesPOSTBodyToGraphQLRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Query:         "query ($pokemon: PokemonEnum!) { getPokemon(pokemon: $pokemon) { color } }",
		Variables:     []string{"pokemon"},
		OperationName: "GetPokemon",
	})

	req := httptest.NewRequest(http.MethodPost, "/v8", strings.NewReader(`{"pokemon":"pikachu","ignored":"x"}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if payload["query"] != p.config.Query {
			t.Fatalf("query = %v, want configured query", payload["query"])
		}
		if payload["operationName"] != "GetPokemon" {
			t.Fatalf("operationName = %v, want GetPokemon", payload["operationName"])
		}
		variables, ok := payload["variables"].(map[string]any)
		if !ok {
			t.Fatalf("variables = %#v, want object", payload["variables"])
		}
		if variables["pokemon"] != "pikachu" {
			t.Fatalf("pokemon variable = %v, want pikachu", variables["pokemon"])
		}
		if _, ok := variables["ignored"]; ok {
			t.Fatalf("variables included ignored key: %#v", variables)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRewritesGETQueryToGraphQLArgs(t *testing.T) {
	p := newTestPlugin(t, Config{
		Query:     "query ($pokemon: PokemonEnum!) { getPokemon(pokemon: $pokemon) { color } }",
		Variables: []string{"pokemon"},
	})

	req := httptest.NewRequest(http.MethodGet, "/v8?pokemon=eevee&ignored=x", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := r.URL.Query()
		if args.Get("query") != p.config.Query {
			t.Fatalf("query arg = %q, want configured query", args.Get("query"))
		}
		if args.Get("operationName") != "" {
			t.Fatalf("operationName arg = %q, want empty", args.Get("operationName"))
		}
		if args.Get("ignored") != "" {
			t.Fatalf("ignored arg = %q, want removed", args.Get("ignored"))
		}
		var variables map[string]string
		if err := json.Unmarshal([]byte(args.Get("variables")), &variables); err != nil {
			t.Fatalf("decode variables arg: %v", err)
		}
		if variables["pokemon"] != "eevee" {
			t.Fatalf("pokemon variable = %q, want eevee", variables["pokemon"])
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsUnsupportedMethods(t *testing.T) {
	p := newTestPlugin(t, Config{Query: "{ getAllPokemon { key } }"})

	req := httptest.NewRequest(http.MethodPut, "/v8", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandlerRejectsInvalidPOSTVariablesBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Query:     "query ($pokemon: PokemonEnum!) { getPokemon(pokemon: $pokemon) { color } }",
		Variables: []string{"pokemon"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v8", strings.NewReader(`{"pokemon"`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPostInitRejectsInvalidGraphQLQuery(t *testing.T) {
	p := &Plugin{config: Config{Query: "query { viewer { id }"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid GraphQL query rejection")
	}
}

func TestPostInitRequiresOperationNameForMultipleOperations(t *testing.T) {
	p := &Plugin{config: Config{Query: "query First { viewer { id } } query Second { viewer { name } }"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want operation_name requirement")
	}

	p.config.OperationName = "Second"
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() with operation_name error = %v", err)
	}
}
