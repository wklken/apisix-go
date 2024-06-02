package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func adminRouter() http.Handler {
	r := chi.NewRouter()
	// FIXME: set the admin key here, and `allow_admin`
	r.Use(AdminKeyMiddleware("admin-key"))

	// r.Use(AdminOnly)
	// r.Get("/", adminIndex)
	// r.Get("/accounts", adminListAccounts)

	// FIXME: read the source code, how to implement: service/route and all other routes

	r.Route("/upstreams", func(r chi.Router) {
		// r.With(paginate).Get("/", listArticles)
		r.Get("/", listUpstreams)
		r.Post("/", createUpstream)

		// Subrouters:
		r.Route("/{upstreamID}", func(r chi.Router) {
			// r.Use(ArticleCtx)
			r.Get("/", getUpstream)
			r.Put("/", updateUpstream)
			r.Delete("/", deleteUpstream)
			r.Patch("/", patchUpstream)
		})
	})

	return r
}
