# apisix-go

This is an [apache/apisix](https://github.com/apache/apisix) Data Plane(DP) implemented via Go

This project is still under development and NOT READY FOR PRODUCTION!

## Features

- Small binary size and image size (<100M)
- Easy to deploy and scale
- Better performance with io plugins like `*-logger`
- Easy to extend with Go http middlewares or Go Plugins(develop and test is much easier)

### Supported

- [x] Route
- [x] Service
- [x] Upstream
- [x] Plugin Metadata
- [x] Global Rules
- [x] Plugin Attr
- [x] Consumer
- [x] Plugin Config
- [ ] Consumer Group
- [ ] Script
- [ ] Secret

### Local Go environment

Source `.envrc` before running Go commands (this is local to the checkout and does not require `direnv allow`):

```bash
source .envrc
go test ./...
```

The checkout-local `.cache/` directory contains the Go toolchain download, module/build caches, installed binaries, and temporary build files. It is ignored by Git, so normal tests and builds do not need write access to user-level `/private` or home-directory cache paths.

## Plugins

> still working on it

APISIX 3.17 coverage is tracked in the [parity checklist](docs/apisix-3.17-plugin-parity-checklist.md), the [remaining-plugin gap catalog](docs/apisix-3.17-remaining-plugin-todo.md), and the [execution TODO](docs/apisix-3.17-plugin-parity-execution-todo.md).

### General

> 16/16

- [x] [batch-requests](https://apisix.apache.org/zh/docs/apisix/plugins/batch-requests/) 85%
  - support `/apisix/batch-requests`, global and per-item query/header merging, per-item host overrides, HTTP version validation, default methods, request body forwarding, bounded timeout responses, response aggregation, `X-Real-IP` subrequest injection, `plugin_attr.batch-requests.uri`, plugin metadata `max_body_size` / `max_pipeline_items`, and accepted `ssl_verify` input
  - not support true HTTP pipelining or custom real-ip header names from NGINX config; `ssl_verify` is inapplicable to the local in-process dispatcher because it performs no TLS handshake
- [x] [redirect](https://apisix.apache.org/zh/docs/apisix/plugins/redirect/) 95%
  - support `uri`, `regex_uri`, `http_to_https`, forwarded HTTPS detection, method-specific 301/308 responses, query preservation, robust host/port replacement, `ret_code`, `append_query_string`, `encode_uri`, `plugin_attr.redirect.https_port`, and random `apisix.ssl.listen` fallback
- [x] [echo](https://apisix.apache.org/zh/docs/apisix/plugins/echo/) 95%
  - support `before_body`, `body`, `after_body`, response `headers`, body-modification header cleanup, and official body/header schema boundaries
- [x] [gzip](https://apisix.apache.org/zh/docs/apisix/plugins/gzip/) 98%
  - support `types`, `types = "*"`, `min_length`, `comp_level`, `http_version`, and `vary`
  - not support `buffers`(it's nginx native feature)
- [x] [brotli](https://apisix.apache.org/zh/docs/apisix/plugins/brotli/) 82%
  - support `Accept-Encoding: br` / `*`, `types`, `min_length`, `comp_level`, `lgwin`, `http_version`, `vary`, content-encoding skip, content-length removal, and strong ETag weakening
  - not support NGINX-native streaming compression; the Go Brotli encoder does not expose `mode` or `lgblock` tuning
- [x] [real-ip](https://apisix.apache.org/zh/docs/apisix/plugins/real-ip/) 90%
  - support `arg_*`, `http_*`, `cookie_*`, common built-in request variables, `http_x_forwarded_for`, bare IP and IP:port sources with valid port bounds, `trusted_addresses` with config-time CIDR rejection, `recursive`, and request-context `remote_addr` / `remote_port` updates
  - not support APISIX-Base `set_real_ip`, NGINX variable cache flushing, the full NGINX variable catalog, or exact `ip_def` schema validation
- [x] [server-info](https://apisix.apache.org/zh/docs/apisix/plugins/server-info/) 75%
  - support `/v1/server_info` response shape when `server-info` is enabled in `conf.plugins`, including configured `apisix.id` node IDs, persisted `conf/apisix.uid` fallback IDs, and generated process fallback IDs
  - support periodic reporting to the configured etcd prefix under `data_plane/server_info/{id}`, bounded `plugin_attr.server-info.report_ttl`, lease keepalive, and shutdown cleanup for traditional etcd configuration
  - not support dynamic etcd-version lookup or exact OpenResty shared-dict lifecycle semantics
- [x] [error-page](https://apisix.apache.org/zh/docs/apisix/plugins/error-page/) 70%
  - support official plugin name, priority, empty route schema, metadata-shaped `enable` and `error_404` / `error_500` / `error_502` / `error_503`, custom body/content-type/content-length, and default APISIX-style HTML bodies
  - support response-source provenance markers from the Go route and skip rewriting known upstream error responses; not support exact header/body filter phase timing or metadata schema exposure through the plugin interface
- [x] [exit-transformer](https://apisix.apache.org/zh/docs/apisix/plugins/exit-transformer/) 55%
  - support official plugin name, priority, schema, chained response capture, documented status-remap Lua pattern, normalized JSON error body / `X-Error-Code` header pattern, and skipping known upstream responses via Go response provenance
  - not support arbitrary Lua execution, APISIX `core.response.exit()` callback integration, Lua cache behavior, or general Lua table/header/body mutation
- [x] [attach-consumer-label](https://apisix.apache.org/zh/docs/apisix/plugins/attach-consumer-label/) 80%
  - support official plugin name, priority, schema, and copying configured authenticated consumer labels to request headers before upstream proxying, including JSON serialization for numeric, boolean, and array label values
  - not support independent authentication behavior or APISIX Lua/OpenResty phase fidelity beyond this middleware position
- [x] [serverless-pre-function](https://apisix.apache.org/zh/docs/apisix/plugins/serverless/) 45%
  - support official plugin name, priority, schema, Lua chunks that return functions, sequential execution, `code` / `body` return short-circuiting, `ngx.log`, `ngx.say`, `ngx.req.set_header`, `ngx.header`, `ngx.status`, `ngx.arg`, `cjson`, and selected `apisix.core` helpers
  - not support the full OpenResty/APISIX Lua runtime, shared-dict/lrucache semantics, custom variable registration effects, streaming body chunks, or exact phase lifecycle fidelity
- [x] [serverless-post-function](https://apisix.apache.org/zh/docs/apisix/plugins/serverless/) 45%
  - support official plugin name, priority, schema, Lua chunks that return functions, request-phase execution, response capture for `header_filter` / `body_filter` / `log`, response header/status/body mutation, and the documented JSON body-filter rewrite pattern
  - not support the full OpenResty/APISIX Lua runtime, shared-dict/lrucache semantics, custom variable registration effects, streaming body chunks, or exact phase lifecycle fidelity
- [x] [azure-functions](https://apisix.apache.org/zh/docs/apisix/plugins/azure-functions/) 95%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers and wildcard `:ext` paths to `function_uri`, Azure client/route/metadata authorization precedence, encrypted route API keys and metadata master keys, relaying function status/body/headers, HTTP/2 connection-header filtering, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
- [x] [openfunction](https://apisix.apache.org/zh/docs/apisix/plugins/openfunction/) 94%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers and wildcard `:ext` paths to `function_uri`, Basic authorization from encrypted `authorization.service_token`, relaying function status/body/headers, HTTP/2 connection-header filtering, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
- [x] [openwhisk](https://apisix.apache.org/zh/docs/apisix/plugins/openwhisk/) 93%
  - support official plugin name, priority, schema and name validation, OpenWhisk action endpoint construction with optional package, POST body forwarding, Basic authorization from encrypted `service_token`, default `blocking` / `result` / `timeout` query parameters, JSON result `statusCode` / scalar-or-list `headers` / body values, invalid result fallback to 503, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
- [x] [aws-lambda](https://apisix.apache.org/zh/docs/apisix/plugins/aws-lambda/) 95%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers and wildcard `:ext` paths to `function_uri`, encrypted API-key/IAM credentials, API Gateway `x-api-key` injection without overwriting client headers, IAM SigV4 signing with APISIX-compatible path/query/header canonicalization, relaying function status/body/headers, HTTP/2 connection-header filtering, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
- &#x2612; [ext-plugin-pre-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-pre-req/)      NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-req/)    NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-resp](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-resp/)  NOT SUPPORTED, No need
- &#x2612; [inspect](https://apisix.apache.org/zh/docs/apisix/plugins/inspect/)                            NOT SUPPORTED, lua feature
- &#x2612; [ocsp-stapling](https://apisix.apache.org/zh/docs/apisix/plugins/ocsp-stapling/)                NOT SUPPORTED, nginx feature

### Transformation

> 8/8

- [x] [response-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/response-rewrite/) 97%
  - support `status_code`, validated plain/base64 `body`, legacy and `add` / `set` / `remove` headers with string or numeric values, response/request header variables, nested `lua-resty-expr` logical groups, comparison/regex/list/IP operators, response body `filters`, and gzip/Brotli response decoding before filters
  - support the Go extension `body_secret` for strict APISIX data-encryption resolution before rewriting; ordinary `body` remains compatibility-oriented so plaintext rewrites are not rejected when encryption is enabled; exact OpenResty PCRE semantics and streaming chunk-level body filters remain unsupported
- [x] [proxy-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-rewrite/) 99%
  - support `uri`, `regex_uri`, `use_real_request_uri_unsafe`, `method`, `host`, `scheme`, request header `add` / `set` / `remove`, legacy header set config, bounded header value variable resolution, and `regex_uri` capture resolution in header values
  - support encoded path-segment and raw-query preservation through the Go reverse proxy; not support exact OpenResty URI normalization parity
- [x] [grpc-transcode](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-transcode/) 95%
  - support base64 `.pb` FileDescriptorSet and plain `.proto` proto resources with unchanged-content binding caching, pure-Go imported-source/standard-import resolution, string/integer `proto_id`, GET scalar/repeated/dotted nested-message/scalar-map query and bounded repeated-message query forms (`children.0.field`, `children[0].field`, or repeated JSON-object values), POST JSON mapping including repeated nested messages, Lua-compatible `#` decimal/hex int64 request inputs, gRPC request framing, `grpc-timeout`, JSON response decoding with `enum_as_name`/`enum_as_value`, all three int64 output modes (`int64_as_number`, `int64_as_string`, `int64_as_hexstring`, including nested/list/map fields), JSON-visible proto3 default-value modes, gRPC status to HTTP status mapping, `grpc-status-details-bin` body decoding with optional `status_detail_type`, and in-process gRPC-server integration coverage for unary success/errors
  - explicitly reject client/server-streaming method descriptors; not support any separately defined HTTP-annotation contract, hooks/exact Lua metatable semantics, or streaming response chunk filters
- [x] [grpc-web](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-web/) 88%
  - support CORS preflight and existing-origin preservation, APISIX-style `400` rejection for invalid method/content-type/body, route wildcard-to-gRPC path rewriting, binary/text gRPC-Web request body translation, upstream `application/grpc` content type, response content type restoration, trailers-only status/message preservation, known `http.TrailerPrefix` `grpc-status`/`grpc-message` promotion from the Go reverse proxy, bounded binary/text response streaming with flush and request cancellation, and basic gRPC-Web trailer chunk encoding
  - not support exact OpenResty streaming chunk-level phase filters or arbitrary unknown trailer-field conversion
- [x] [fault-injection](https://apisix.apache.org/zh/docs/apisix/plugins/fault-injection/) 97%
  - support `abort`, `delay`, omitted-vs-explicit `percentage`, empty abort bodies, string/numeric response headers, response body/header variable resolution, fractional delay seconds, config-time `vars` validation, OR across wrapped rules, nested `AND` / `OR` / `!AND` / `!OR`, and comparison/regex/list/IP/negation operators over common NGINX, APISIX, and request variables
  - not support exact OpenResty PCRE semantics, the complete NGINX variable catalog, or exact rewrite-phase timing
- [x] [mocking](https://apisix.apache.org/zh/docs/apisix/plugins/mocking/) 97%
  - support `response_example`, `response_schema` object generation, JSON/plain-text/XML schema bodies, response headers, bounded variable resolution, delay, status, content type, and mock marker header
  - not support APISIX random response value distribution exactly for schema fields without examples
- [x] [degraphql](https://apisix.apache.org/zh/docs/apisix/plugins/degraphql/) 78%
  - support bounded GraphQL syntax/structure validation, multiple-operation `operation_name` enforcement, and GET/POST rewriting to GraphQL `query`, `variables`, and `operationName`
  - not support full GraphQL AST/type validation or exact parser error parity
- [x] [body-transformer](https://apisix.apache.org/zh/docs/apisix/plugins/body-transformer/) 88%
  - support request and response body template substitution for `json`, `xml`, `encoded`, `args`, `plain`, and bounded `multipart`, including nested values, repeated XML element indexes, XML `_attr.<name>` lookup, repeated query/form values with numeric indexes, array indexes and bracket paths, bounded `..` string concatenation and `+` numeric addition, single- and double-quoted string literals, raw `{* expr *}` output, bounded `{% if ... then %}` / `{% elseif ... then %}` / `{% else %}` / `{% end %}` branches, multipart fields/file names, reserved-helper shadowing protection for `_ctx`/`_body`/`_escape_json`/`_escape_xml`/`_multipart`, `_body`, shared APISIX/request `_ctx.var.*` values, `_escape_json()`, `_escape_xml()`, APISIX-compatible Base64 template attempt/fallback behavior, and oversized-request rejection through the existing `client-control.max_body_size` route boundary
  - not support loops/arbitrary Lua and function execution, full XML namespace URI/prefix fidelity, or all multipart helper semantics

### Authentication

> 18/18

- [x] [key-auth](https://apisix.apache.org/zh/docs/apisix/plugins/key-auth/) 75%
  - support encrypted consumer fields, header/query API key lookup, APISIX-style missing/invalid key errors, consumer attachment, `hide_credentials` removal from headers or query strings, and `anonymous_consumer` fallback
- [x] [jwt-auth](https://apisix.apache.org/zh/docs/apisix/plugins/jwt-auth/) 85%
  - support `HS*`, `RS*`, `ES*`, `PS*`, and `EdDSA` signature verification, header/query/cookie token lookup, claim verification for `exp`/`nbf`, `base64_secret`, `hide_credentials`, `store_in_ctx`, and `anonymous_consumer` fallback
  - support encrypted consumer fields through `apisix.data_encryption`
- [x] [jwe-decrypt](https://apisix.apache.org/zh/docs/apisix/plugins/jwe-decrypt/) 90%
  - support compact direct AES-256-GCM JWE parsing, `Bearer` token extraction, `kid` consumer lookup, required 32-byte plain/base64url consumer secrets, and forwarding plaintext to `forward_header`
  - support encrypted consumer fields; alternate JWE algorithms, AAD/header authentication, and anonymous-consumer behavior are not APISIX 3.17 plugin configurations
- [x] [basic-auth](https://apisix.apache.org/zh/docs/apisix/plugins/basic-auth/) 70%
  - support Basic credential extraction, APISIX-style credential whitespace normalization, consumer attachment, password validation, missing/malformed authorization errors, and `hide_credentials`
  - support encrypted consumer fields through `apisix.data_encryption`
- [x] [authz-keycloak](https://apisix.apache.org/zh/docs/apisix/plugins/authz-keycloak/) 85%
  - support explicit `token_endpoint`, discovery, static `permissions`, lazy path resource lookup, UMA decision requests, `http_method_as_scope`, `ENFORCING` access-denied behavior, `access_denied_redirect_uri`, `ssl_verify`, timeout, keepalive settings, password-grant token generation URI, and process-shared discovery/service-account-token caching with `cache_ttl_seconds`, refresh-token reuse, and expiry leeway
  - not support cross-process OpenResty shared-dict fidelity or Lua `http_request_decorator` functions
- [x] [authz-casdoor](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casdoor/) 85%
  - support OAuth authorize redirect, per-`client_id` session cookie, callback state validation, access token exchange against `/api/login/oauth/access_token`, and authenticated session pass-through
  - not support distributed/exact `resty.session` runtime behavior; upstream Casdoor user/access-token forwarding is not an APISIX 3.17 plugin behavior
- [x] [dingtalk-auth](https://apisix.apache.org/docs/apisix/plugins/dingtalk-auth/) 65%
  - support official plugin name, priority, schema, no-code redirect to `redirect_uri`, authorization code extraction from configurable header/query names, DingTalk access token POST, access token caching, DingTalk userinfo POST, signed `dingtalk_session` cookie, `cookie_expires_in`, `secret_fallbacks` verification, `ssl_verify`, timeout, clearing spoofed `X-Userinfo`, Base64 JSON `X-Userinfo` forwarding, and `$external_user` request-context propagation
  - not support encrypted `resty.session` cookie parity, distributed session state, exact DingTalk error logging, or OpenResty worker-shared token cache semantics
- [x] [feishu-auth](https://apisix.apache.org/docs/apisix/plugins/feishu-auth/) 65%
  - support official plugin name, priority, schema, no-code redirect to `redirect_uri`, authorization code extraction from configurable header/query names, Feishu OAuth token POST with `auth_redirect_uri`, Feishu userinfo GET with Bearer token, signed `feishu_session` cookie, `cookie_expires_in`, `secret_fallbacks` verification, `ssl_verify`, timeout, clearing spoofed `X-Userinfo`, Base64 JSON `X-Userinfo` forwarding, and `$external_user` request-context propagation
  - not support encrypted `resty.session` cookie parity, distributed session state, exact Feishu error logging, or OpenResty worker/session semantics
- [x] [saml-auth](https://apisix.apache.org/docs/apisix/plugins/saml-auth/) 85%
  - support official plugin name, priority, schema, HTTP-Redirect and HTTP-POST authentication requests, SP-signed SAML requests, ACS `SAMLResponse` parsing and signature/condition validation through `github.com/crewjam/saml`, signed local SAML session cookies, `secret_fallbacks` verification, SP-initiated logout redirect, logout callback cleanup, `X-Userinfo` forwarding, and local `$external_user` request context attachment when APISIX vars exist
  - not support exact `lua-resty-saml` session/runtime behavior, which remains OpenResty-specific
- [x] [wolf-rbac](https://apisix.apache.org/zh/docs/apisix/plugins/wolf-rbac/) 75%
  - support `V1#appid#wolf_token` parsing, token extraction from query/header/cookie, consumer lookup by `appid`, Wolf `/wolf/rbac/access_check`, configured TLS verification, APISIX-style transient 5xx retry/backoff, user info header injection, consumer attachment, and public login/change-password/user-info APIs at `/apisix/plugin/wolf-rbac/*`
  - not support cross-process OpenResty consumer-cache fidelity
- [x] [openid-connect](https://apisix.apache.org/zh/docs/apisix/plugins/openid-connect/) 98%
  - support Bearer token extraction from `Authorization` and `X-Access-Token`, discovery fallback for `introspection_endpoint`, token introspection, `client_secret_basic` / `client_secret_post` / `private_key_jwt` / `client_secret_jwt` client authentication, `public_key` and `use_jwks` RSA JWT verification, `token_signing_alg_values_expected`, `required_scopes`, `claim_validator.audience` required/client-id matching, bearer and session-flow `claim_schema` validation, `realm`, `unauth_action = pass`, output header clearing, `X-Access-Token`, `X-Userinfo`, `ssl_verify`, `timeout`, `introspection_addon_headers`, and `proxy_opts` HTTP/HTTPS proxy selection, Basic proxy credentials, and `no_proxy` host/domain bypasses; authorization-code cookie and Redis sessions with encrypted AES-GCM state, opaque encrypted Redis cookie IDs, Redis TLS/auth/database/prefix/timeouts, state validation, PKCE S256, configured/automatic redirect URIs, `authorization_params`, `force_reauthorize`, token exchange, access-token renewal via `renew_access_token_on_expiry`, `access_token_expires_in`, and `access_token_expires_leeway`, silent `refresh_session_interval` reauthentication, optional userinfo forwarding, end-session logout redirects, and `revoke_tokens_on_logout` refresh/access-token revocation
  - not support exact `lua-resty-session`/OpenResty behavior
- [x] [cas-auth](https://apisix.apache.org/zh/docs/apisix/plugins/cas-auth/) 85%
  - support CAS login redirect, absolute/relative `cas_callback_uri`, ticket `serviceValidate`, HMAC-signed initiation cookie, per-config session cookie, local session refresh, logout redirect, and IdP single-logout XML `SessionIndex` session deletion
  - not support OpenResty shared-dict clustering; upstream CAS user metadata forwarding is not an APISIX 3.17 plugin behavior
- [x] [hmac-auth](https://apisix.apache.org/zh/docs/apisix/plugins/hmac-auth/) 82%
  - support `hmac-sha1`, `hmac-sha256`, `hmac-sha512`, `signed_headers`, `clock_skew`, request body digest validation, `hide_credentials`, and `anonymous_consumer` fallback
- [x] [authz-casbin](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casbin/) 85%
  - support Casbin `model` / `policy` text config, `model_path` / `policy_path` file config, APISIX plugin-metadata `model` / `policy` fallback with reload on metadata updates, configured username header, and anonymous fallback
- [x] [ldap-auth](https://apisix.apache.org/zh/docs/apisix/plugins/ldap-auth/) 75%
  - support HTTP Basic credential extraction, LDAP bind using configured `base_dn`, host/URL `ldap_uri`, `uid`, direct-LDAPS `use_tls`, and `tls_verify`, matching consumers by `ldap-auth.user_dn`, and attaching the consumer context
  - LDAP search filters, StartTLS, and anonymous-consumer behavior are not APISIX 3.17 plugin configurations
- [x] [opa](https://apisix.apache.org/zh/docs/apisix/plugins/opa/) 78%
  - support OPA HTTP decision calls, custom deny status/body/headers, `send_headers_upstream`, `with_consumer`, and `with_route` / `with_service` full resource documents from the route builder, with ID/name/matched-URI fallback for direct plugin use
- [x] [forward-auth](https://apisix.apache.org/zh/docs/apisix/plugins/forward-auth/) 90%
  - support `GET` / `POST`, POST body and transport metadata forwarding, `request_headers`, `extra_headers`, APISIX-style variable resolution in `extra_headers`, `upstream_headers`, `client_headers`, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
  - APISIX 3.17's schema requires string `extra_headers` values; its numeric runtime fallback is not a normal configurable feature
- [x] [multi-auth](https://apisix.apache.org/zh/docs/apisix/plugins/multi-auth/) 85%
  - support ordered fallback across every APISIX 3.17 plugin marked `type = auth`: `basic-auth`, `key-auth`, `jwt-auth`, `hmac-auth`, `ldap-auth`, `jwe-decrypt`, and `wolf-rbac`; request passes when any configured auth plugin succeeds
  - retain APISIX's generic final denial response; per-plugin failure diagnostics remain runtime logging detail

### Security

> 13/13

- [x] [cors](https://apisix.apache.org/zh/docs/apisix/plugins/cors/) 87%
  - support `allow_origins`, `allow_origins = "**"` request-origin echo, `allow_origins_by_regex`, `allow_origins_by_metadata`, `timing_allow_origins`, `timing_allow_origins_by_regex`, method wildcards, `allow_headers = "**"` request-header reflection, APISIX-style 200 preflight responses, explicit exposed headers with no custom expose header by default, `max_age`, and wildcard/credential cross-field validation
  - not support every exact APISIX wildcard response-header semantic in the third-party CORS middleware
- [x] [acl](https://apisix.apache.org/zh/docs/apisix/plugins/acl/) 85%
  - support authenticated consumer `labels`, including string, JSON/segmented-text, numeric, boolean, and array label values, plus `$external_user` label extraction with bounded dotted fields, `$..field[.suffix]` recursive lookup, and `json`/`segmented_text`/`table` parsers
  - not support full OpenResty JSONPath and parser-engine behavior
- [x] [uri-blocker](https://apisix.apache.org/zh/docs/apisix/plugins/uri-blocker/) 95%
  - support `block_rules`, `rejected_code`, `rejected_msg`, `case_insensitive`, APISIX-style empty default rejection bodies, and `error_msg` JSON custom rejections
  - not support APISIX PCRE/JIT regex engine parity exactly
- [x] [ip-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ip-restriction/) 95%
  - support `whitelist`, `blacklist`, CIDR/IP matching, custom messages, `response_code` 403/404 selection, fail-fast IP/CIDR validation, `remote_addr` context overrides, and APISIX-style JSON rejection bodies
  - not support shared OpenResty LRU matcher caching
- [x] [ua-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ua-restriction/) 95%
  - support mutually exclusive APISIX `allowlist` or `denylist` schema branches, allow-before-deny matching, `bypass_missing`, trimmed User-Agent matching, and APISIX-style JSON rejection bodies
  - not support OpenResty multi-value User-Agent header fidelity exactly
- [x] [referer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/referer-restriction/) 98%
  - support `whitelist`, `blacklist`, `bypass_missing`, custom rejection messages, APISIX-style JSON rejection bodies, leading-`*` suffix host matching, and APISIX `host_def` character/ wildcard schema validation
  - not support exact OpenResty host parsing/cache lifecycle
- [x] [consumer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/consumer-restriction/) 85%
  - support `consumer_name`, `service_id`, `route_id`, `consumer_group_id`, blacklist, whitelist, `allowed_by_methods` with the official GET/POST/PUT/DELETE/PATCH/HEAD/OPTIONS/CONNECT/TRACE/PURGE method enum, custom rejection status/message, and APISIX-style rejection bodies
  - not support exact OpenResty consumer-context lifecycle beyond the propagated consumer `group_id`
- [x] [csrf](https://apisix.apache.org/zh/docs/apisix/plugins/csrf/) 80%
  - support official token cookie/header validation, safe method bypass, token expiry/signature checks including `expires = 0` no-expiry validation, configurable `key` / `expires` / `name`, strict `key` resolution through `apisix.data_encryption`, and APISIX-style JSON error bodies
  - not support exact Lua random-number formatting parity for token signatures
- [x] [public-api](https://apisix.apache.org/zh/docs/apisix/plugins/public-api/) 75%
  - support exposing registered internal public APIs such as `batch-requests`, `node-status`, and `server-info`, with optional `uri` override
  - not support arbitrary internal API discovery, Prometheus public metrics proxying, or exposing non-registered runtime endpoints
- [x] [GM](https://apisix.apache.org/zh/docs/apisix/plugins/GM/) 25%
  - support official plugin name, priority, empty route schema, no-op HTTP handler, and APISIX SSL `gm` marker validation requiring encryption cert/key plus exactly one sign cert/key pair
  - not support Tongsuo/APISIX-Runtime NTLS enablement, SM2/SM3/SM4 TLS handshakes, dynamic TLS certificate installation, SSL schema injection, or real dual-certificate serving
- [x] [chaitin-waf](https://apisix.apache.org/zh/docs/apisix/plugins/chaitin-waf/) 80%
  - support `mode`, bounded nested `match.vars` expression operators, official metadata schema, metadata/config `nodes`, Go round-robin selection with five-minute failure quarantine, timeout/body/keepalive defaults, monitor/block/off behavior, request body restoration, official WAF response headers, and block response body with `event_id`
  - APISIX 3.17's plugin source has no user-configurable active-probe contract; not support native `resty.t1k`, full `lua-resty-expr`, Unix socket nodes, or real SafeLine binary protocol details
- [x] [data-mask](https://apisix.apache.org/zh/docs/apisix/plugins/data-mask/) 80%
  - support query/header/urlencoded-body masking, bounded `max_req_post_args` prefix parsing, APISIX conditional rule-schema validation, bounded JSONPath body masking for dot paths, root-array selectors, quoted bracket fields, recursive descent, `[*]`, and numeric array indexes, `remove` / `replace` / `regex`, `max_body_size`, `max_req_post_args`, official plugin name/schema/priority, and downstream logger visibility for masked `$request_uri`/`$request_line`
  - not support exact APISIX log-phase timing, full `jsonpath` syntax, temporary-file request bodies, or preserving original upstream request data while masking logger output
- [x] [oas-validator](https://apisix.apache.org/docs/apisix/plugins/oas-validator/) 99%
  - support official plugin name, priority, schema, inline JSON `spec`, `spec_url` fetch with custom headers, timeout and `ssl_verify`, method/path matching, required path/query/header/cookie parameters, JSON request body schema validation, local component refs for parameters/request bodies/schemas, skip flags, `reject_if_not_match`, `verbose_errors`, and configurable rejection status
  - support free-form exploded `form` query objects backed by `additionalProperties` with schema-guided coercion; duplicate scalar properties remain rejected
  - support bounded Parameter Object `content` serialization for one `application/json`/`application/*+json` or `text/plain` media type, including schema validation and explicit rejection of malformed JSON, duplicate values, and unsupported codecs
  - support external HTTP(S) `$ref` resolution with JSON Pointer fragments, relative refs from `spec_url`, bounded cycle/depth/count handling, plugin metadata `spec_url_ttl` refresh semantics, URL-encoded, multipart, `text/plain`, `application/octet-stream`, bounded XML (`application/xml`, `text/xml`, `application/*+xml`) and YAML (`application/yaml`, `text/yaml`, `application/x-yaml`, `application/*+yaml`) body validation, specificity-ordered wildcard OpenAPI content-key matching for structured `+json`/`+xml`/`+yaml` media types, `application/*+json` reuse, scalar opaque bodies for arbitrary media-type entries, bounded `form` arrays and repeated values with OpenAPI default `explode=true`, repeated `spaceDelimited`/`pipeDelimited` array values and objects, repeated Cookie arrays and exploded/non-exploded Cookie objects, comma-preserving exploded arrays, exploded and non-exploded objects, repeated `deepObject` array properties, rejection of repeated `deepObject` scalar properties, `spaceDelimited`, `pipeDelimited`, `deepObject`, matrix, label, and simple path/header parameter parsing; location-specific style/schema mismatches are rejected instead of silently decoded as another style, explicit `deepObject` `explode=false` is rejected, repeated non-exploded form arrays and duplicate structured object fields are rejected, structured custom codecs without a local decoder are rejected explicitly, and other uncommon nested/explode combinations remain unsupported
  - APISIX 3.17's official plugin validates requests only; response validation is not part of the official parity surface and would require a separate project extension

### Traffic

> 19/19

- [x] [limit-req](https://apisix.apache.org/zh/docs/apisix/plugins/limit-req/) 96%
  - support local, Redis, and Redis Cluster request-rate limiting, official Redis/Redis Cluster TLS, timeout, and pool config, route-scoped keys, `key_type = var`, `var_combination`, HTTP header variables, `rejected_code`, APISIX-style empty/custom rejection bodies, `nodelay`, and `allow_degradation`
  - not support exact OpenResty `resty.limit.req`/LRU timing or APISIX's internal config-version key suffix
- [x] [limit-conn](https://apisix.apache.org/zh/docs/apisix/plugins/limit-conn/) 96%
  - support local, Redis, and Redis Cluster concurrent request limiting, official Redis/Redis Cluster TLS, timeout, pool, and `key_ttl` config, route-scoped admission/release keys, adaptive unit-delay feedback, top-level and rule-level string variable values for `conn` / `burst`, `rules`, `key_type = var`, `var_combination`, HTTP header variables, `rejected_code`, APISIX-style empty/custom rejection bodies, `only_use_default_delay`, and `allow_degradation`
  - not support exact OpenResty log-phase timing, request-ID sorted-set cleanup, or APISIX's internal config-version key suffix
- [x] [limit-count](https://apisix.apache.org/zh/docs/apisix/plugins/limit-count/) 98%
  - support local, Redis, and Redis Cluster fixed-window quotas, APISIX root-level Redis/Redis Cluster fields and legacy nested configs, TLS/pool settings, top-level and rule-level string variable values for `count` / `time_window`, `rules`, per-rule `header_prefix`, shared `group` quotas with configuration mismatch validation, route-scoped keys, `key_type = var`, `constant`, and `var_combination`, HTTP header variables, quota headers, plugin metadata custom quota header names, `rejected_code`, APISIX-style empty/custom rejection bodies, and `allow_degradation`
  - not support exact OpenResty shared-dict/LRU lifecycle or `resty.limit.count` behavior
- [x] [graphql-limit-count](https://apisix.apache.org/docs/apisix/plugins/graphql-limit-count/) 98%
  - support official plugin name, priority, schema, POST `application/json` and `application/graphql` requests bounded by `graphql.max_size`, GraphQL selection-depth cost counting with alias/argument/directive syntax, undefined/cyclic fragment rejection, static group-configuration mismatch rejection before sharing local quota state, local, standalone Redis, and Redis Cluster fixed-window quotas, official Redis/TLS/pool config fields, top-level and rule-level string variable values for `count` / `time_window`, `rules`, per-rule `header_prefix`, shared `group` quotas, route-scoped Redis keys, limit-count metadata header names, `allow_degradation`, `rejected_code`, `rejected_msg`, `key`, `key_type`, and fragment/inline-fragment/chained-fragment depth expansion
  - not support exact `resty.limit.count` behavior
- [x] [proxy-cache](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-cache/) 99%
  - support in-memory response caching and configured absolute disk zones with atomic versioned response envelopes, cross-plugin-instance reload, persisted `Vary` indexes and `PURGE`, observed-expired entry cleanup, bounded once-per-minute traffic-triggered expiry sweeps, lifecycle-owned background expiry cleanup, configured memory-zone sharing across plugin instances, shared configured memory/disk storage with `graphql-proxy-cache`, complete static zone-registry preflight before route replacement, validated process-local dynamic zone snapshots through `RefreshConfiguredZones`, duplicate/empty name and bounded `cache_levels` registry validation, cache strategy/zone storage matching, `$request_method` cache-key rejection, unique/method/status/variable-name cache-filter validation, strict cache initialization failure during route replacement, write-time `disk_size` eviction of expired/oldest files, plus `cache_key`, `cache_method`, `cache_http_status`, `cache_ttl`, `cache_bypass`, `no_cache`, `hide_cache_headers`, `consumer_isolation`, strategy-specific `cache_control` request/TTL behavior (memory zones only and disabled for identity-bearing cache keys), strategy-specific `cache_set_cookie` behavior (memory opt-in; disk never stores `Set-Cookie`), `Age` on cache hits, upstream `private` / `no-store` / `no-cache` non-storage, upstream `s-maxage` / `max-age` / `Expires` TTL derivation, and `Apisix-Cache-Status`
  - not support full NGINX cache-manager or cross-worker runtime fidelity; changed configured memory-zone definitions are isolated by generation, rejected refresh snapshots leave the last valid configuration in place, cross-plugin stale-if-error is not implicitly enabled, and design/phased acceptance criteria are in [`docs/apisix-3.17-proxy-cache-design.md`](docs/apisix-3.17-proxy-cache-design.md)
- [x] [graphql-proxy-cache](https://apisix.apache.org/docs/apisix/plugins/graphql-proxy-cache/) 95%
  - support official plugin name, priority, schema, GET/POST GraphQL request validation with configured `graphql.max_size`, bounded grammar parsing for operation definitions, variables/types, arguments, directives, fragments, aliases, input values, strings, numbers, and trailing-token rejection, JSON and `application/graphql` bodies, mutation bypass with `Apisix-Cache-Status: BYPASS`, route/service/host/consumer-isolated MD5 cache keys, `APISIX-Cache-Key`, configured memory-zone sharing, configured disk-zone persistence/expiry cleanup, upstream `Cache-Control: s-maxage/max-age` and `Expires` TTL derivation for disk zones with `cache_ttl` fallback, unconditional upstream `private`/`no-store`/`no-cache` non-storage, `consumer_isolation`, memory-only `cache_set_cookie` opt-in (disk zones never store `Set-Cookie`), and the route-aware public `PURGE` endpoint
  - not support GraphQL schema/type validation or exact APISIX OpenResty phase/cache-manager internals
- [x] [request-validation](https://apisix.apache.org/zh/docs/apisix/plugins/request-validation/) 90%
  - support JSON and `application/x-www-form-urlencoded` `body_schema`, JSON body normalization before proxying, nested-schema validation at configuration time, repeated-header array validation, `header_schema`, `rejected_code`, and `rejected_msg` for schema and decode failures
  - not support exact OpenResty header normalization/secret-reference behavior
- [x] [proxy-mirror](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-mirror/) 73%
  - support HTTP mirror `host`, `path`, `path_concat_mode`, `sample_ratio`, and APISIX-style `host` / `path` schema validation
  - not support gRPC mirroring
  - not support APISIX DNS resolver behavior
- [x] [kafka-proxy](https://apisix.apache.org/docs/apisix/plugins/kafka-proxy/) 75%
  - support official plugin name, priority, schema, strict password resolution, request-context propagation, the APISIX PubSub protobuf WebSocket owner (`cmd_kafka_list_offset` and `cmd_kafka_fetch` with sequence preservation), Kafka offset/timestamp/key/value conversion through a bounded `kafka-go` consumer, PLAIN SASL credentials, upstream TLS `verify`, inline client certificate/key configuration, and `client_cert_id` lookup through the local SSL resource bucket
  - route/fake-consumer coverage verifies fetch, list-offset, malformed-request close, sequence handling, sanitized auth/timeout mapping, invalid TLS resource rejection, SSL-resource resolution, and TLS option propagation; an in-process TLS wire fixture verifies the actual PLAIN payload and broker auth error; external broker smoke tests are optional, while mechanisms beyond PLAIN and REST semantics are outside the APISIX 3.17 schema; the raw Kafka frame bridge is compatibility-only and transport boundaries are documented in [`apisix-3.17-protocol-bridge-design.md`](docs/apisix-3.17-protocol-bridge-design.md)
- [x] [dubbo-proxy](https://apisix.apache.org/docs/apisix/plugins/dubbo-proxy/) 82%
  - support official plugin name, priority, schema, required `service_name` / `service_version`, URI-derived method fallback, Hessian2 `Map<String,Object>` HTTP-context request encoding, route-upstream TCP terminal integration, provider response map conversion to HTTP status/headers/body, request cancellation, and `upstream_multiplex_count` as a cancellation-aware per-target in-flight bound
  - support bounded connect-only retries from the selected upstream's `retries` setting and passive `checks` thresholds through the shared Go load-balancer state; not support retry after any request bytes are written, active probes, OpenResty/Tengine runtime lifecycle, persistent shared-connection multiplexing/response-ID matching, or exact native phase behavior; the separate fastjson/Hessian2 design is documented in [`apisix-3.17-protocol-bridge-design.md`](docs/apisix-3.17-protocol-bridge-design.md)
- [x] [http-dubbo](https://apisix.apache.org/docs/apisix/plugins/http-dubbo/) 75%
  - support official plugin name, priority, schema, route-upstream TCP dialing, Dubbo 2.x fastjson request frame construction, `service_name`, `service_version`, `method`, `params_type_desc`, `serialized`, `serialization_header_key`, bounded connect/send/read timeouts, request cancellation, JSON-array generic invocation parameter serialization with APISIX-compatible string escaping, pre-serialized body passthrough, response-size/malformed-frame rejection, transport-error mapping, Dubbo header/status parsing, HTTP 200 body mapping for application response/exception payloads, and bounded connect-only retries from the upstream `retries` setting
  - support passive `checks` thresholds through the shared Go load-balancer state; not support retry after any request bytes are written, active probes, APISIX `before_proxy` phase fidelity, OpenResty cosocket behavior, hessian2 serialization, full fastjson precision/type features, multiplexing, or route-builder support for non-round-robin upstream algorithms; revisit only after a concrete APISIX-vs-Go mismatch, with protocol notes in [`apisix-3.17-protocol-bridge-design.md`](docs/apisix-3.17-protocol-bridge-design.md)
- [x] [api-breaker](https://apisix.apache.org/zh/docs/apisix/plugins/api-breaker/) 95%
  - support `break_response_code`, `break_response_body`, `break_response_headers` with bounded variable resolution, `max_breaker_sec`, `unhealthy.http_statuses`, `unhealthy.failures`, `healthy.http_statuses`, and `healthy.successes`
  - not support APISIX shared-dict state keyed by host and URI, exponential breaker windows, or exact OpenResty log-phase timing
- [x] [traffic-split](https://apisix.apache.org/zh/docs/apisix/plugins/traffic-split/) 97%
  - support ordered `match.vars`, pre-match validation of every referenced `upstream_id`, weighted inline/upstream-ID targets, route fallback entries, explicit zero weights including referenced upstream resources, numeric/string `upstream_id`, `pass_host` / `upstream_host`, bracketed IPv6 node targets, deterministic `chash` selection for `vars`, `header`, `cookie`, `consumer`, and APISIX-compatible `vars_combinations` templates with `$variable` concatenation and bounded `??` defaults, and selected-upstream timeout propagation through the Go request context
  - support passive `checks` thresholds through the shared Go load-balancer state, including HTTP-status, TCP-failure, and timeout quarantine with fail-open selection when every node is unhealthy; APISIX 3.17's traffic-split generated-upstream contract does not propagate `retries`; remaining gaps are active health probes, phase-specific transport deadlines, full APISIX upstream balancer fidelity, and full `lua-resty-expr` syntax
- [x] [traffic-label](https://apisix.apache.org/zh/docs/apisix/plugins/traffic-label/) 96%
  - support schema-validated first-match rules, match-all rules, string/numeric `set_headers` with variable resolution, weighted actions, config-time expression validation, nested `AND` / `OR` / `!AND` / `!OR`, and comparison/regex/list/IP/negation operators over common NGINX, APISIX, and request variables
  - not support exact OpenResty cached round-robin behavior, the complete NGINX variable catalog, or exact access-phase timing
- [x] [request-id](https://apisix.apache.org/zh/docs/apisix/plugins/request-id/) 90%
  - support custom header names, response header opt-out, incoming request ID preservation, `uuid`, `uuidv7`, `nanoid`, `ksuid`, and `range_id`
- [x] [proxy-control](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-control/) 75%
  - support route/global `request_buffering` flag by buffering the Go proxy request body before upstream forwarding
  - not support APISIX-Runtime/NGINX dynamic `proxy_request_buffering` control or disk-backed buffering
- [x] [proxy-buffering](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-buffering/) 75%
  - support route/global `disable_proxy_buffering` by switching to immediate reverse-proxy response flushing
  - not support NGINX `proxy_buffering` internals or disk-backed response buffering controls
- [x] [client-control](https://apisix.apache.org/zh/docs/apisix/plugins/client-control/) 100%
- [x] [workflow](https://apisix.apache.org/zh/docs/apisix/plugins/workflow/) 85%
  - support official action-array config shape, first matching `case`, bounded APISIX expression operators/request variables, validated `return` actions with configured status code, and delegated `limit-req` / `limit-count` / `limit-conn` actions
  - unsupported actions and invalid return status codes are rejected during plugin initialization; exact Lua/OpenResty phase behavior remains out of scope
  - not support full `lua-resty-expr` or other delegated plugin actions/log handlers

### Observability

Tracers:

> 3/3

- [x] [zipkin](https://apisix.apache.org/zh/docs/apisix/plugins/zipkin/) 82%
  - support B3 extraction/injection, parent/child span identity, probabilistic `sample_ratio`, `endpoint`, `service_name`, local/remote endpoints, status/error tags, `server_addr`, `span_version`, and Zipkin v2 span reporting
  - not support APISIX multi-phase span tree, batch processor behavior, `plugin_attr.zipkin.set_ngx_var`, or exact OpenResty phase timing
- [x] [skywalking](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking/) 78%
  - support probabilistic `sample_ratio`, `plugin_attr.skywalking` defaults, `sw8` parse/injection, trace/segment IDs, protocol-correct span references/tags, request timing, status/error tags, service and instance names, `$hostname`, `report_interval` buffering, shutdown flush, and HTTP segment reporting to `/v3/segments`
  - not support the native OpenResty SkyWalking tracer, shared `tracing_buffer`, or exact delayed body-filter/streaming phase timing
- [x] [opentelemetry](https://apisix.apache.org/zh/docs/apisix/plugins/opentelemetry/) 82%
  - support official plugin name, APISIX sampler names (`always_on`, `always_off`, `trace_id_ratio`, `parent_base`), sampler defaults, configurable middleware server name, `additional_attributes`, `additional_header_prefix_attributes`, OTLP/HTTP collector address/timeout/headers, resource attributes, batch processor sizing/timeouts/blocking behavior, and `x-request-id` trace IDs
  - keep `otel` as a compatibility alias
  - not support `set_ngx_var`, phase child spans, or exact OpenResty log-phase timing

Metrics:

> 3/3

- [x] [prometheus](https://apisix.apache.org/zh/docs/apisix/plugins/prometheus/) 82%
  - support official plugin name, priority, `prefer_name` schema validation, route/service metric labels using IDs by default and names when `prefer_name` is true, pass-through route/global plugin config, and public API metrics endpoint registration at `/apisix/prometheus/metrics`
  - collect status, matched route/host, consumer, balancer, request/upstream/APISIX latency, ingress/egress bytes, AI dimensions, LLM latency/tokens/active connections, and bounded extra-label variables; support `metric_prefix`, `default_buckets`, `llm_latency_buckets`, `export_uri`, `enable_export_server`, and `export_addr`
  - not support metric expiration, privileged-agent offload, stream metrics, or exact `nginx-lua-prometheus` lifecycle behavior
- [x] [node-status](https://apisix.apache.org/zh/docs/apisix/plugins/node-status/) 78%
  - support `/apisix/status`, configured IDs, persisted generated `conf/apisix.uid` IDs, and process-wide active/accepted/handled/total HTTP request counters
  - not support exact NGINX reading/writing/waiting connection-state counters
- [x] [datadog](https://apisix.apache.org/zh/docs/apisix/plugins/datadog/) 88%
  - support DogStatsD UDP metrics, metadata `host`, `port`, `namespace`, `constant_tags`, route `constant_tags`, `prefer_name`, route/service ID-vs-name tags, consumer/balancer/status/scheme tags, optional matched path/method tags, request/upstream/APISIX latency, ingress/egress sizes, APISIX batch processor fields, retry policy, route/server buffering metrics, and shutdown flush
  - not support exact OpenResty log-phase timing or stale batch-manager object cache behavior

Loggers:

> 19/19

- [x] [http-logger](https://apisix.apache.org/zh/docs/apisix/plugins/http-logger/) 76%
  - support `uri`, `auth_header`, `timeout`, `log_format`, `concat_method`, `ssl_verify`, HTTP POST delivery, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), JSON/newline batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and the route/server-aware shared `batch_process_entries` gauge hook
  - `auth_header` stays encrypted through store parsing and is strictly resolved in the plugin boundary; invalid ciphertext/missing keys fail before client creation; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [skywalking-logger](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking-logger/) 76%
  - support `endpoint_addr`, `service_name`, `service_instance_name`, `timeout`, `log_format`, `/v3/logs` delivery, basic `sw8` trace correlation, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), SkyWalking JSON-array batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - not support exact APISIX Lua batch-manager stale-object cache cleanup
- [x] [tcp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/tcp-logger/) 70%
  - support `host`, `port`, `timeout`, `log_format`, `tls`, `tls_options` as TLS server name / SNI, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), JSON batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - not support OpenResty cosocket connection pooling or exact APISIX Lua batch-manager stale-object cache cleanup
- [x] [kafka-logger](https://apisix.apache.org/zh/docs/apisix/plugins/kafka-logger/) 76%
  - support `brokers`, deprecated `broker_list`, broker `sasl_config` for `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`, `kafka_topic`, `key`, `producer_type`, `required_acks`, `timeout`, producer batch defaults, `meta_format = origin`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), single-object / JSON-array Kafka batch payloads, and `max_pending_entries`
  - broker SASL passwords stay encrypted through store parsing and are strictly resolved before the Kafka writer is created; rotated keys are supported and invalid ciphertext fails initialization
- [x] [rocketmq-logger](https://apisix.apache.org/zh/docs/apisix/plugins/rocketmq-logger/) 72%
  - support `nameserver_list`, `topic`, `key`, `tag`, `timeout`, `access_key`, `secret_key`, `meta_format = origin`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), single-object / JSON-array RocketMQ batch payloads, and `max_pending_entries`
  - `secret_key` stays encrypted through store parsing and is strictly resolved before producer setup; rotated keys are supported and invalid ciphertext fails initialization; `use_tls` remains unsupported because the current RocketMQ Go client exposes no TLS option
- [x] [udp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/udp-logger/) 70%
  - support `host`, `port`, `timeout`, `log_format`, UDP delivery, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), JSON batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - not support exact APISIX Lua batch-manager stale-object cache cleanup
- [x] [clickhouse-logger](https://apisix.apache.org/zh/docs/apisix/plugins/clickhouse-logger/) 76%
  - support `endpoint_addr`, random selection from `endpoint_addrs`, `user`, `password`, `database`, `logtable`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), ClickHouse JSONEachRow batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - `password` stays encrypted through store parsing and is strictly resolved before the ClickHouse client is created; rotated keys are supported and invalid ciphertext fails initialization; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [syslog](https://apisix.apache.org/zh/docs/apisix/plugins/syslog/) 70%
  - support `host`, `port`, `timeout`, `sock_type`, `flush_limit`, `drop_limit`, `pool_size`, `tls` schema/config acceptance, `log_format`, `include_req_body`, `include_req_body_expr`, Go-side `include_resp_body`, `include_resp_body_expr`, capped body-size capture, UDP/TCP syslog delivery through the Go syslog writer, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), batched JSON payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - not support OpenResty syslog connection pooling/TLS behavior parity or exact APISIX Lua batch-manager stale-object cache cleanup
- [x] [log-rotate](https://apisix.apache.org/zh/docs/apisix/plugins/log-rotate/) 72%
  - support `plugin_attr.log-rotate` defaults for `interval`, `max_kept`, `max_size`, `timeout`, and `enable_compression`, APISIX timestamped `__access.log` / `__error.log` naming, max-size rotation, interval checks on request, current-file recreation, Go-side `file-logger` current-path writes after rotation, history pruning, and `.tar.gz` compression
  - not support OpenResty timer lifecycle, NGINX master `USR1` log reopening, NGINX config path discovery, or shelling out to system `tar`
- [x] [error-log-logger](https://apisix.apache.org/zh/docs/apisix/plugins/error-log-logger/) 69%
  - support official metadata-shaped config for `tcp`, `skywalking`, `clickhouse`, and `kafka`, level filtering, TCP/TLS delivery, SkyWalking `/v3/logs` entries with `$hostname` service-instance resolution, ClickHouse JSONEachRow inserts, Kafka topic/key publishing, Kafka broker `sasl_config` with `PLAIN`, legacy `host` / `port` TCP config, official batch/default knobs, shared batch processor buffering/retry semantics, and graceful reload/shutdown buffer flush
  - nested ClickHouse/Kafka credentials stay encrypted through store parsing and are strictly resolved before the corresponding sender is created; rotated keys are supported and invalid ciphertext fails initialization; direct `ngx.errlog` capture, OpenResty timer lifecycle, route/server `batch_process_entries` labels for global explicit delivery, Lua-resty-kafka producer cache exactness, and exact APISIX Lua batch-manager stale-object cache cleanup remain
- [x] [sls-logger](https://apisix.apache.org/zh/docs/apisix/plugins/sls-logger/) 72%
  - support RFC5424 SLS log messages over TLS TCP with `host`, `port`, `project`, `logstore`, `access_key_id`, `access_key_secret`, `timeout`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), concatenated RFC5424 batch writes, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - `access_key_secret` stays encrypted through store parsing and is strictly resolved before sender setup; rotated keys are supported and invalid ciphertext fails initialization; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [google-cloud-logging](https://apisix.apache.org/zh/docs/apisix/plugins/google-cloud-logging/) 67%
  - support service-account `auth_config`, `auth_file`, JWT bearer token exchange with access-token caching/refresh, `entries_uri`, `resource`, `log_id`, `ssl_verify`, `log_format`, default Cloud Logging `httpRequest` entry expansion, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), multi-entry Cloud Logging writes, and `max_pending_entries`
  - `auth_config.private_key` stays encrypted through store parsing and is strictly resolved before the logger batch processor starts; rotated keys are supported and invalid ciphertext fails initialization; request/response body capture remains unsupported
- [x] [splunk-hec-logging](https://apisix.apache.org/zh/docs/apisix/plugins/splunk-hec-logging/) 62%
  - support Splunk HEC `endpoint.uri`, `endpoint.token`, `endpoint.channel`, `endpoint.timeout`, `endpoint.keepalive_timeout`, `ssl_verify`, `log_format`, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), concatenated JSON event-object batch payloads, `max_pending_entries`, and HEC error-text extraction
  - `endpoint.token` stays encrypted through store parsing and is strictly resolved before the HTTP client is created; rotated keys are supported and invalid ciphertext fails initialization; request/response body capture remains unsupported
- [x] [file-logger](https://apisix.apache.org/zh/docs/apisix/plugins/file-logger/) 82%
  - support `path` from plugin config or plugin metadata, `log_format` from plugin config or plugin metadata, bounded `match` expressions for common request variables and `$status`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, and Go-native current-path writes after external rotation
  - not support APISIX/OpenResty file-cache semantics exactly
- [x] [loggly](https://apisix.apache.org/zh/docs/apisix/plugins/loggly/) 76%
  - support RFC5424 Loggly syslog messages over UDP and HTTP/S bulk endpoint delivery
  - support `customer_token`, `severity`, `severity_map`, `tags`, `host`, `port`, `protocol`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), HTTP/S newline bulk batching, UDP per-entry batch delivery, metadata delivery config fallback, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - `customer_token` stays encrypted through store parsing and is strictly resolved before the logger batch processor starts; rotated keys are supported and invalid ciphertext fails initialization; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [elasticsearch-logger](https://apisix.apache.org/zh/docs/apisix/plugins/elasticsearch-logger/) 84%
  - support `endpoint_addr`, random `endpoint_addrs` selection, `field.index`, time/APISIX variable expansion in `field.index`, Elasticsearch major-version discovery for legacy `_type`, `auth`, `headers`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), `_bulk` NDJSON batch delivery, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - `auth.password` stays encrypted through store parsing and is strictly resolved before the logger batch processor starts; rotated keys are supported and invalid ciphertext fails initialization; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [tencent-cloud-cls](https://apisix.apache.org/zh/docs/apisix/plugins/tencent-cloud-cls/) 76%
  - support `cls_host`, `cls_topic`, `scheme`, `ssl_verify`, `secret_id`, `secret_key`, `sample_ratio`, `global_tag`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, Tencent CLS sha1 authorization, `/structuredlog` protobuf delivery, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), multi-log protobuf batch payloads, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - `secret_key` stays encrypted through store parsing and is strictly resolved before the shared HTTP client is created; rotated keys are supported and invalid ciphertext fails initialization; exact APISIX Lua batch-manager stale-object cache cleanup remains
- [x] [loki-logger](https://apisix.apache.org/zh/docs/apisix/plugins/loki-logger/) 76%
  - support random selection from `endpoint_addrs`, `endpoint_uri`, `tenant_id`, custom headers, `log_labels`, `ssl_verify`, `timeout`, `log_format`, `include_req_body`, `include_req_body_expr`, `include_resp_body`, `include_resp_body_expr`, capped body-size capture, APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`), one-stream multi-value Loki batches, `max_pending_entries`, graceful reload/shutdown buffer flush, and route/server-aware `batch_process_entries`
  - not support exact APISIX Lua batch-manager stale-object cache cleanup
- [x] [lago](https://apisix.apache.org/docs/apisix/plugins/lago/) 76%
  - support official plugin name, priority, schema, Lago batch event payload shape, random selection from `endpoint_addrs`, Bearer token delivery to `endpoint_uri`, request/response variable templates for transaction ID, subscription ID, and event properties, request-start event timestamps, dynamic `${arg_*}`, `${cookie_*}`, `${http_*}`, `${sent_http_*}`, and `${upstream_http_*}` template variables, `include_req_body`, `include_resp_body`, capped body-size capture through `${request_body}` / `${response_body}` templates, `ssl_verify`, timeout, keepalive config defaults, APISIX `batch_max_size` default of 100, and APISIX batch processor fields (`batch_max_size`, `max_retry_count`, `retry_delay`, `buffer_duration`, `inactive_timeout`)
  - `token` stays encrypted through store parsing and is strictly resolved before the logger batch processor starts; rotated keys are supported and invalid ciphertext fails initialization; exotic OpenResty/NGINX-only variable fidelity remains deferred

### AI

> 12/12

- [x] [ai](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/ai.lua) 20%
  - support official plugin name, priority, empty schema, and pass-through compatibility registration
  - not support APISIX global-scope router matching cache replacement, upstream handler replacement, balancer phase replacement, route feature analysis, OpenResty keepalive pool behavior, event hook registration, or any runtime acceleration behavior from the Lua implementation
- [x] [ai-aliyun-content-moderation](https://apisix.apache.org/docs/apisix/plugins/ai-aliyun-content-moderation/) 96%
  - support official request/response checks, signing/config, shared six-protocol extraction, provider-native JSON/SSE deny responses, Unicode-safe length chunks, incremental realtime checks triggered by `stream_check_cache_size` / `stream_check_interval`, final-packet risk-level annotation, request-scoped moderation sessions, `$llm_content_risk_level`, request replay, fail-open provider errors, and encrypted `access_key_secret`
  - exact OpenResty body-filter chunk timing remains out of scope
- [x] [ai-aws-content-moderation](https://apisix.apache.org/docs/apisix/plugins/ai-aws-content-moderation/) 98%
  - support official AWS Comprehend request checks, SigV4 with static/session credentials, encrypted secrets, endpoint/TLS controls, category/toxicity thresholds, request replay, and rejection behavior
  - exact `resty.aws` module/cache lifecycle remains out of scope; APISIX 3.17 itself requires explicit access key, secret, and region configuration
- [x] [ai-rag](https://apisix.apache.org/docs/apisix/plugins/ai-rag/) 98%
  - support the official Azure OpenAI embeddings and Azure AI Search providers, request options, provider status propagation, request replay, and protocol-native result append for OpenAI Chat/Responses, Anthropic Messages, and Bedrock Converse
  - Azure API keys are handled by the shared encrypted-field pipeline; exact OpenResty HTTP-client lifecycle remains out of scope
- [x] [ai-prompt-decorator](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-decorator/) 90%
  - support the shared APISIX 3.17 protocol registry for OpenAI Chat/Responses/Embeddings, Anthropic Messages, Bedrock Converse, and passthrough, including protocol-native prepend/append and request replay
  - embeddings and passthrough decoration are no-ops as upstream defines; exact OpenResty phase/runtime behavior remains out of scope
- [x] [ai-prompt-guard](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-guard/) 88%
  - support regex policy evaluation and shared extraction across all six APISIX 3.17 AI protocols, including nested content blocks and official generic rejection responses
  - not support exact OpenResty regex-engine flags or phase/runtime fidelity
- [x] [ai-prompt-template](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-template/) 95%
  - support named templates, JSON body selection, nested dotted and bounded indexed lookup, request replay, and official missing/unknown-template errors
  - exact OpenResty template LRU behavior remains out of scope; APISIX 3.17 hardcodes JSON input for this plugin
- [x] [ai-request-rewrite](https://apisix.apache.org/docs/apisix/plugins/ai-request-rewrite/) 90%
  - support shared protocol-native simple requests/response extraction, Anthropic/Bedrock-native shapes, AWS SigV4, GCP token caching, provider paths, encrypted auth, request replacement/replay, transport controls, and rewrite markers
  - not support streaming sidecar responses or exact APISIX error/log lifecycle
- [x] [ai-rate-limiting](https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/) 90%
  - support official global, per-instance, and rule quotas; string variables; quota headers; all four token strategies; bounded `cost_expr`; two-phase proxy execution; rate-aware multi-instance fallback; and non-buffering streaming usage charging
  - not support cross-process shared counter state or exact OpenResty log-phase timing
- [x] [ai-proxy](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/) 99%
  - support six-protocol detection, Anthropic-to-OpenAI request/response/SSE conversion with tools, OpenAI Responses/Embeddings, Vertex embeddings conversion, provider auth/endpoints, protocol overrides, SSE, CRC-validated Bedrock EventStream, usage/timing variables, `logging.summaries` / `logging.payloads`, response/stream-duration limits, scheduled flush intervals, active connection metrics, and two-phase rate limiting
  - exact OpenResty transport, phase, and log lifecycle remains out of scope
- [x] [ai-proxy-multi](https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/) 98%
  - support weighted priorities, round-robin/chash across vars/header/cookie/consumer/variable-combination keys with remote-address fallback, HTTP/rate fallback, retries, official active-health schema and threshold/interval behavior, all-unhealthy fallback, shared protocol conversion, provider auth, Vertex embeddings, Anthropic SSE conversion, Bedrock EventStream, logging summaries/payloads, stream-duration/flush controls, timing/active metrics, and two-phase rate limiting
  - explicit APISIX per-address DNS snapshots are deferred; standard Go HTTP preserves hostname/SNI while resolving DNS
- [x] [mcp-bridge](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/mcp-bridge.lua) 75%
  - support official plugin name, priority, schema, configurable `base_uri`, stdio subprocess launch with `command` / `args`, `GET {base_uri}/sse`, advertised message endpoints, 30-second JSON-RPC ping keepalives, `POST {base_uri}/message` forwarding, cancellation-aware stdout/stderr line forwarding, `notifications/stderr`, and subprocess cleanup on disconnect
  - not support APISIX shared-dict cross-worker session recovery, exact `ngx.pipe` timeout/worker-exit semantics, distributed session state, or behavior beyond the official MCP-over-SSE wrapper

### Stream

> 1/1

- [x] [mqtt-proxy](https://apisix.apache.org/docs/apisix/plugins/mqtt-proxy/) 65%
  - support official plugin name, priority, schema, required `protocol_level`, default `protocol_name = "MQTT"`, registry/config validation, bounded MQTT 3.1.1/5.0 CONNECT parsing, variable-length and flags/property validation, UTF-8 validation, client-ID extraction, peer-address fallback, plugin-owned `ServeStream` preread/replay plus bidirectional cancellation, and a plugin-owned `ServeListener` accept loop with `StreamInfo` callbacks
  - support main-server TCP `stream_proxy.tcp` listener ownership, `stream_routes` store parsing, `server_addr` / `server_port` / `remote_addr` matching, inline or `upstream_id` resolution, weighted TCP upstream selection, deterministic `chash` keyed by `mqtt_client_id` with peer fallback, cancellation, runtime result/log callbacks, and bounded cancellation of a large write to a non-reading upstream; in-process fixtures cover exact forwarding, client-ID selection, malformed rejection before dialing, reload, shutdown cancellation, and no-route rejection; general stream-variable/plugin-chain support and stream mTLS remain deferred; the required stream design is documented in [`apisix-3.17-protocol-bridge-design.md`](docs/apisix-3.17-protocol-bridge-design.md)

### Development

> 1/1

- [x] [example-plugin](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/example-plugin.lua) 80%
  - support official name/priority/schema and metadata schema, required `i`, optional `s` / `t` / `ip` / `port`, pass-through middleware, route upstream override through the existing Go traffic-split override path when `ip` is configured, and control API `GET /v1/plugin/example-plugin/hello` with text or JSON response
  - not support plugin-attr logging, OpenResty phase-specific logging, delayed body filter behavior, direct `apisix.upstream.set` parity, or treating this upstream demonstration plugin as a production feature

## TODO

- [ ] standalone mode
- [ ] handle etcd compact
- [ ] github action: go releaser
- [ ] logforamt change didn't take effect immediately
