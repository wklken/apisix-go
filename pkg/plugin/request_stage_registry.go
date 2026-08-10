package plugin

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// RequestStageSpec describes the audited request-stage owner for one exact
// plugin factory key. AdaptLegacyHandler is set only for handlers audited for
// request-stage adaptation with no post-next work, response-writer wrapper,
// flush/hijack, logging, or deferred cleanup behavior.
type RequestStageSpec struct {
	Stage                 RequestStage
	AuthenticatesConsumer bool
	ConsumerConfigOnly    bool
	AdaptLegacyHandler    bool
}

// requestStageRegistry is intentionally exact. A factory alias or an
// implementation name is not implicitly normalized here.
var requestStageRegistry = map[string]RequestStageSpec{
	"request-context":     {Stage: RequestStageRewrite},
	"request-id":          {Stage: RequestStageRewrite},
	"real-ip":             {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"proxy-rewrite":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"proxy-control":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"proxy-mirror":        {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"traffic-label":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"traffic-split":       {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"ai-prompt-decorator": {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"ai-prompt-template":  {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"ai-rag":              {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"ai-request-rewrite":  {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"data-mask":           {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"degraphql":           {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"example-plugin":      {Stage: RequestStageRewrite, AdaptLegacyHandler: true},
	"jwe-decrypt":         {Stage: RequestStageRewrite, AdaptLegacyHandler: true},

	"limit-conn": {Stage: RequestStageAccess},

	"basic-auth": {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"hmac-auth":  {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"jwt-auth":   {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"key-auth":   {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"ldap-auth":  {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"multi-auth": {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},
	"wolf-rbac":  {Stage: RequestStageAccess, AuthenticatesConsumer: true, ConsumerConfigOnly: true},

	"attach-consumer-label": {Stage: RequestStageConsumerRewrite, AdaptLegacyHandler: true},

	"acl":                       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"ai-aws-content-moderation": {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"ai-prompt-guard":           {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"authz-casbin":              {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"authz-casdoor":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"authz-keycloak":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"cas-auth":                  {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"chaitin-waf":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"client-control":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"consumer-restriction":      {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"csrf":                      {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"dingtalk-auth":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"feishu-auth":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"forward-auth":              {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"graphql-limit-count":       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"ip-restriction":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"limit-count":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"limit-req":                 {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"oas-validator":             {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"opa":                       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"openid-connect":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"referer-restriction":       {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"request-validation":        {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"saml-auth":                 {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"ua-restriction":            {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"uri-blocker":               {Stage: RequestStageAccess, AdaptLegacyHandler: true},
	"workflow":                  {Stage: RequestStageAccess, AdaptLegacyHandler: true},
}

// RequestStageFor resolves only the exact factory/config name.
func RequestStageFor(name string) (RequestStageSpec, bool) {
	spec, ok := requestStageRegistry[name]
	return spec, ok
}

// rewriteOnlyAdapter turns an audited legacy Handler into one request-phase
// operation. The sentinel captures the one replacement request without ever
// invoking the downstream executor itself.
type rewriteOnlyAdapter struct {
	factoryName string
	plugin      Plugin
	provenance  ResourceProvenance
}

type rewriteAdapterDiagnosticRecorder func(string)

type rewriteAdapterDiagnosticKey struct{}

const maxRewriteAdapterDiagnosticBytes = 4 * 1024

func withRewriteAdapterDiagnosticRecorder(
	r *http.Request,
	recorder rewriteAdapterDiagnosticRecorder,
) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), rewriteAdapterDiagnosticKey{}, recorder))
}

func recordRewriteAdapterDiagnostic(r *http.Request, message string) {
	// Keep provenance in the bounded internal log; never expose it in the
	// client-facing fail-closed response.
	if len(message) > maxRewriteAdapterDiagnosticBytes {
		message = message[:maxRewriteAdapterDiagnosticBytes]
	}
	logger.Errorf("%s", message)
	if recorder, ok := r.Context().Value(rewriteAdapterDiagnosticKey{}).(rewriteAdapterDiagnosticRecorder); ok {
		recorder(message)
	}
}

func newRewriteOnlyAdapter(
	factoryName string,
	plugin Plugin,
	provenance ResourceProvenance,
) base.RequestPhasePlugin {
	return rewriteOnlyAdapter{factoryName: factoryName, plugin: plugin, provenance: provenance}
}

type unregisteredRewriteAdapter struct {
	factoryName string
	provenance  ResourceProvenance
}

func newUnregisteredRewriteAdapter(factoryName string, provenance ResourceProvenance) base.RequestPhasePlugin {
	return unregisteredRewriteAdapter{factoryName: factoryName, provenance: provenance}
}

func (a unregisteredRewriteAdapter) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	diagnostic := fmt.Sprintf(
		"request-stage adapter rejected unregistered rewrite binding (factory=%q resource=%s/%s)",
		a.factoryName,
		a.provenance.Kind,
		a.provenance.ID,
	)
	recordRewriteAdapterDiagnostic(r, diagnostic)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	return base.StopRequest(r)
}

func (a rewriteOnlyAdapter) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	var mu sync.Mutex
	var calls int
	var replacement *http.Request
	sentinel := http.HandlerFunc(func(_ http.ResponseWriter, nextRequest *http.Request) {
		mu.Lock()
		calls++
		if calls == 1 {
			replacement = nextRequest
		}
		mu.Unlock()
	})

	handler := a.plugin.Handler(sentinel)
	if handler == nil {
		diagnostic := a.diagnostic("legacy handler returned nil")
		recordRewriteAdapterDiagnostic(r, diagnostic)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return base.StopRequest(r)
	}
	handler.ServeHTTP(w, r)

	mu.Lock()
	callCount := calls
	nextRequest := replacement
	mu.Unlock()
	if callCount > 1 {
		diagnostic := a.diagnostic("legacy handler called next more than once")
		recordRewriteAdapterDiagnostic(r, diagnostic)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return base.StopRequest(r)
	}
	if callCount == 0 {
		return base.StopRequest(r)
	}
	return base.ContinueRequest(nextRequest)
}

func (a rewriteOnlyAdapter) diagnostic(reason string) string {
	return fmt.Sprintf(
		"request-stage adapter rejected plugin: %s (factory=%q plugin=%q resource=%s/%s)",
		reason,
		a.factoryName,
		a.plugin.GetName(),
		a.provenance.Kind,
		a.provenance.ID,
	)
}
