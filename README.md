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

## Plugins

> still working on it

### General

> 16/16

- [x] [batch-requests](https://apisix.apache.org/zh/docs/apisix/plugins/batch-requests/) 70%
  - support `/apisix/batch-requests`, global and per-item query/header merging, default methods, request body forwarding, response aggregation, `X-Real-IP` subrequest injection, `plugin_attr.batch-requests.uri`, and plugin metadata `max_body_size` / `max_pipeline_items`
  - not support true HTTP pipelining, custom real-ip header name from NGINX config, or `ssl_verify`
- [x] [redirect](https://apisix.apache.org/zh/docs/apisix/plugins/redirect/) 90%
  - support `uri`, `regex_uri`, `http_to_https`, `ret_code`, `append_query_string`, `encode_uri`, and `plugin_attr.redirect.https_port`
  - not support plugin_attr get random https port from `apisix.ssl.listen`
- [x] [echo](https://apisix.apache.org/zh/docs/apisix/plugins/echo/) 90%
  - support `before_body`, `body`, `after_body`, and response `headers`
- [x] [gzip](https://apisix.apache.org/zh/docs/apisix/plugins/gzip/) 98%
  - support `types`, `types = "*"`, `min_length`, `comp_level`, `http_version`, and `vary`
  - not support `buffers`(it's nginx native feature)
- [x] [brotli](https://apisix.apache.org/zh/docs/apisix/plugins/brotli/) 75%
  - support `Accept-Encoding: br` / `*`, `types`, `min_length`, `comp_level`, `http_version`, `vary`, content-encoding skip, content-length removal, and strong ETag weakening
  - not support NGINX-native streaming compression, `mode`, `lgwin`, or `lgblock` runtime tuning beyond schema/default acceptance
- [x] [real-ip](https://apisix.apache.org/zh/docs/apisix/plugins/real-ip/) 100%
- [x] [server-info](https://apisix.apache.org/zh/docs/apisix/plugins/server-info/) 40%
  - support `/v1/server_info` response shape when `server-info` is enabled in `conf.plugins`
  - not support periodic etcd server-info reporting or lease keepalive
- [x] [error-page](https://apisix.apache.org/zh/docs/apisix/plugins/error-page/) 55%
  - support official plugin name, priority, empty route schema, metadata-shaped `enable` and `error_404` / `error_500` / `error_502` / `error_503`, custom body/content-type/content-length, and default APISIX-style HTML bodies
  - not support APISIX response-source detection, header/body filter phases, plugin metadata schema exposure through the plugin interface, or limiting rewrites only to APISIX-generated errors instead of upstream error responses
- [x] [exit-transformer](https://apisix.apache.org/zh/docs/apisix/plugins/exit-transformer/) 30%
  - support official plugin name, priority, schema, chained response capture, documented status-remap Lua pattern, and documented normalized JSON error body / `X-Error-Code` header pattern
  - not support arbitrary Lua execution, APISIX `core.response.exit()` callback integration, Lua cache behavior, transforming only APISIX-generated exits, or general Lua table/header/body mutation
- [x] [attach-consumer-label](https://apisix.apache.org/zh/docs/apisix/plugins/attach-consumer-label/) 70%
  - support official plugin name, priority, schema, and copying configured authenticated consumer string labels to request headers before upstream proxying
  - not support non-string consumer label serialization, independent authentication behavior, or APISIX Lua/OpenResty phase fidelity beyond this middleware position
- [x] [serverless-pre-function](https://apisix.apache.org/zh/docs/apisix/plugins/serverless/) 45%
  - support official plugin name, priority, schema, Lua chunks that return functions, sequential execution, `code` / `body` return short-circuiting, `ngx.log`, `ngx.say`, `ngx.req.set_header`, `ngx.header`, `ngx.status`, `ngx.arg`, `cjson`, and selected `apisix.core` helpers
  - not support the full OpenResty/APISIX Lua runtime, shared-dict/lrucache semantics, custom variable registration effects, streaming body chunks, or exact phase lifecycle fidelity
- [x] [serverless-post-function](https://apisix.apache.org/zh/docs/apisix/plugins/serverless/) 45%
  - support official plugin name, priority, schema, Lua chunks that return functions, request-phase execution, response capture for `header_filter` / `body_filter` / `log`, response header/status/body mutation, and the documented JSON body-filter rewrite pattern
  - not support the full OpenResty/APISIX Lua runtime, shared-dict/lrucache semantics, custom variable registration effects, streaming body chunks, or exact phase lifecycle fidelity
- [x] [azure-functions](https://apisix.apache.org/zh/docs/apisix/plugins/azure-functions/) 65%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers to `function_uri`, Azure `x-functions-key` / `x-functions-clientid` injection without overwriting client headers, relaying function status/body/headers, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
  - not support plugin metadata master-key fallback, wildcard `:ext` path forwarding, or HTTP/2 connection-header filtering
- [x] [openfunction](https://apisix.apache.org/zh/docs/apisix/plugins/openfunction/) 65%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers to `function_uri`, Basic authorization from `authorization.service_token`, relaying function status/body/headers, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
  - not support wildcard `:ext` path forwarding or HTTP/2 connection-header filtering
- [x] [openwhisk](https://apisix.apache.org/zh/docs/apisix/plugins/openwhisk/) 75%
  - support official plugin name, priority, schema, OpenWhisk action endpoint construction with optional package, POST body forwarding, Basic authorization from `service_token`, default `blocking` / `result` / `timeout` query parameters, JSON result `statusCode` / `headers` / `body`, invalid result fallback to 503, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
  - not support OpenResty response-header behavior or every OpenWhisk result body type edge case
- [x] [aws-lambda](https://apisix.apache.org/zh/docs/apisix/plugins/aws-lambda/) 70%
  - support official plugin name, priority, schema, terminating the APISIX request, forwarding method/query/body/headers to `function_uri`, API Gateway `x-api-key` injection without overwriting client headers, IAM SigV4 `Authorization` / `X-Amz-Date` signing, relaying function status/body/headers, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
  - not support exact APISIX/OpenResty SigV4 canonicalization parity for every header/query/path edge case or wildcard `:ext` path forwarding
- &#x2612; [ext-plugin-pre-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-pre-req/)      NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-req/)    NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-resp](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-resp/)  NOT SUPPORTED, No need
- &#x2612; [inspect](https://apisix.apache.org/zh/docs/apisix/plugins/inspect/)                            NOT SUPPORTED, lua feature
- &#x2612; [ocsp-stapling](https://apisix.apache.org/zh/docs/apisix/plugins/ocsp-stapling/)                NOT SUPPORTED, nginx feature

### Transformation

> 8/8

- [x] [response-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/response-rewrite/) 80%
  - support `status_code`, `body`, `body_base64`, header `add` / `set` / `remove`, bounded `vars`, header value variable resolution, and response body `filters`
  - not support full `lua-resty-expr` parity, compressed response-body decoding before filters, or streaming chunk-level body filters
- [x] [proxy-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-rewrite/) 95%
  - support `uri`, `regex_uri`, `use_real_request_uri_unsafe`, `method`, `host`, `scheme`, request header `add` / `set` / `remove`, legacy header set config, and bounded header value variable resolution
  - not support exact OpenResty URI safe-encoding parity or regex-capture variable resolution in header values
- [x] [grpc-transcode](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-transcode/) 55%
  - support base64 `.pb` FileDescriptorSet proto resources, string/integer `proto_id`, GET query and POST JSON request mapping, gRPC request framing, `grpc-timeout`, JSON response decoding, and gRPC status to HTTP status mapping
  - not support plain `.proto` text compilation, imported source resolution without `.pb`, `pb_option` encoding variants, `grpc-status-details-bin` body decoding, or streaming response chunk filters
- [x] [grpc-web](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-web/) 55%
  - support CORS preflight, `POST` validation, binary/text gRPC-Web request body translation, upstream `application/grpc` content type, response content type restoration, and basic gRPC-Web trailer chunk encoding
  - not support route `:ext` URI rewriting, OpenResty `upstream_trailer_*` fidelity, or streaming chunk-level response filters
- [x] [fault-injection](https://apisix.apache.org/zh/docs/apisix/plugins/fault-injection/) 70%
  - support `abort`, `delay`, omitted-vs-explicit `percentage`, empty abort bodies, and abort response bodies
  - not support `vars`, body variable resolution, or exact fractional-delay precision
- [x] [mocking](https://apisix.apache.org/zh/docs/apisix/plugins/mocking/) 97%
  - support `response_example`, `response_schema` object generation, JSON/plain-text/XML schema bodies, response headers, bounded variable resolution, delay, status, content type, and mock marker header
  - not support APISIX random response value distribution exactly for schema fields without examples
- [x] [degraphql](https://apisix.apache.org/zh/docs/apisix/plugins/degraphql/) 65%
  - support GET/POST rewriting to GraphQL `query`, `variables`, and `operationName`
  - not support GraphQL AST validation or multi-operation validation
- [x] [body-transformer](https://apisix.apache.org/zh/docs/apisix/plugins/body-transformer/) 55%
  - support request and response body template substitution for `json`, `xml`, `encoded`, `args`, and `plain`, plus `_body`, `_ctx.var.*`, `_escape_json()`, `_escape_xml()`, and base64 templates
  - not support multipart decoding or full `lua-resty-template` expression syntax

### Authentication

> 18/18

- [x] [key-auth](https://apisix.apache.org/zh/docs/apisix/plugins/key-auth/) 65%
  - support header/query API key lookup, APISIX-style missing/invalid key errors, consumer attachment, and `hide_credentials` removal from headers or query strings
  - not support encrypted consumer fields or anonymous consumer fallback
- [x] [jwt-auth](https://apisix.apache.org/zh/docs/apisix/plugins/jwt-auth/) 60%
  - only support `HS256`, `HS384`, `HS512`
  - not support `RS*`, `ES*`, `PS*`, or `EdDSA`
  - not support `anonymous_consumer`
- [x] [jwe-decrypt](https://apisix.apache.org/zh/docs/apisix/plugins/jwe-decrypt/) 65%
  - support compact JWE parsing, `Bearer` token extraction, `kid` consumer lookup, AES-256-GCM decrypt, base64url consumer secrets, and forwarding plaintext to `forward_header`
  - not support alternate JWE algorithms, AAD/header authentication, encrypted consumer field handling, or anonymous consumer behavior
- [x] [basic-auth](https://apisix.apache.org/zh/docs/apisix/plugins/basic-auth/) 70%
  - support Basic credential extraction, APISIX-style credential whitespace normalization, consumer attachment, password validation, missing/malformed authorization errors, and `hide_credentials`
  - not support encrypted consumer fields
- [x] [authz-keycloak](https://apisix.apache.org/zh/docs/apisix/plugins/authz-keycloak/) 60%
  - support explicit `token_endpoint`, discovery, static `permissions`, lazy path resource lookup, UMA decision requests, `http_method_as_scope`, `ENFORCING` access-denied behavior, `access_denied_redirect_uri`, `ssl_verify`, `timeout`, and password-grant token generation URI
  - not support shared-dict caches, refresh-token reuse, proxy options, request decorators, full Keycloak resource metadata handling, or all keepalive tuning semantics
- [x] [authz-casdoor](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casdoor/) 60%
  - support OAuth authorize redirect, per-`client_id` session cookie, callback state validation, access token exchange against `/api/login/oauth/access_token`, and authenticated session pass-through
  - not support encrypted `resty.session` cookies, distributed sessions, HTTPS config warnings, or forwarding Casdoor user/access token metadata upstream
- [x] [dingtalk-auth](https://apisix.apache.org/docs/apisix/plugins/dingtalk-auth/) 60%
  - support official plugin name, priority, schema, no-code redirect to `redirect_uri`, authorization code extraction from configurable header/query names, DingTalk access token POST, access token caching, DingTalk userinfo POST, signed `dingtalk_session` cookie, `cookie_expires_in`, `secret_fallbacks` verification, `ssl_verify`, timeout, clearing spoofed `X-Userinfo`, and Base64 JSON `X-Userinfo` forwarding
  - not support encrypted `resty.session` cookie parity, distributed session state, APISIX `ctx.external_user` compatibility for downstream Lua plugins, encrypted storage for `app_secret` / `secret`, exact DingTalk error logging, or OpenResty worker-shared token cache semantics
- [x] [feishu-auth](https://apisix.apache.org/docs/apisix/plugins/feishu-auth/) 60%
  - support official plugin name, priority, schema, no-code redirect to `redirect_uri`, authorization code extraction from configurable header/query names, Feishu OAuth token POST with `auth_redirect_uri`, Feishu userinfo GET with Bearer token, signed `feishu_session` cookie, `cookie_expires_in`, `secret_fallbacks` verification, `ssl_verify`, timeout, clearing spoofed `X-Userinfo`, and Base64 JSON `X-Userinfo` forwarding
  - not support encrypted `resty.session` cookie parity, storing/reusing Feishu access tokens in the session, distributed session state, APISIX `ctx.external_user` compatibility for downstream Lua plugins, encrypted storage for `app_secret` / `secret`, exact Feishu error logging, or OpenResty worker/session semantics
- [x] [saml-auth](https://apisix.apache.org/docs/apisix/plugins/saml-auth/) 55%
  - support official plugin name, priority, schema, HTTP-Redirect and HTTP-POST authentication requests, SP-signed SAML requests, ACS `SAMLResponse` parsing and signature/condition validation through `github.com/crewjam/saml`, signed local SAML session cookies, `secret_fallbacks` verification, SP-initiated logout redirect, logout callback cleanup, `X-Userinfo` forwarding, and local `$external_user` request context attachment when APISIX vars exist
  - not support encrypted `resty.session` cookie parity, IdP metadata documents beyond configured `idp_uri` / `idp_cert`, SAML artifact binding, complete IdP-initiated SSO/SLO semantics, distributed session state, APISIX Lua `ctx.external_user` parity for downstream Lua plugins, encrypted storage for `sp_private_key` / `secret`, or exact `lua-resty-saml` behavior
- [x] [wolf-rbac](https://apisix.apache.org/zh/docs/apisix/plugins/wolf-rbac/) 65%
  - support `V1#appid#wolf_token` parsing, token extraction from query/header/cookie, consumer lookup by `appid`, Wolf `/wolf/rbac/access_check`, user info header injection, and consumer attachment
  - not support built-in `/apisix/plugin/wolf-rbac/*` public APIs for login/change password/user info, retry backoff, or full APISIX consumer plugin metadata behavior
- [x] [openid-connect](https://apisix.apache.org/zh/docs/apisix/plugins/openid-connect/) 50%
  - support Bearer token extraction from `Authorization` and `X-Access-Token`, discovery fallback for `introspection_endpoint`, token introspection, `client_secret_basic` / `client_secret_post`, `required_scopes`, `realm`, `unauth_action = pass`, output header clearing, `X-Access-Token`, `X-Userinfo`, `ssl_verify`, `timeout`, and `introspection_addon_headers`
  - not support authorization-code/session flow, logout/revocation, JWKS/public-key JWT verification, PKCE, Redis sessions, token renewal, claim schema validation, proxy options, or all client assertion auth methods
- [x] [cas-auth](https://apisix.apache.org/zh/docs/apisix/plugins/cas-auth/) 60%
  - support CAS login redirect, absolute/relative `cas_callback_uri`, ticket `serviceValidate`, HMAC-signed initiation cookie, per-config session cookie, local session refresh, and logout redirect
  - not support OpenResty shared-dict clustering, IdP single logout XML session deletion, or attaching authenticated CAS user metadata upstream
- [x] [hmac-auth](https://apisix.apache.org/zh/docs/apisix/plugins/hmac-auth/) 75%
  - support `hmac-sha1`, `hmac-sha256`, `hmac-sha512`, `signed_headers`, `clock_skew`, request body digest validation, and `hide_credentials`
  - not support `anonymous_consumer`
- [x] [authz-casbin](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casbin/) 70%
  - support Casbin `model` / `policy` text config, `model_path` / `policy_path` file config, configured username header, and anonymous fallback
  - not support plugin metadata fallback
- [x] [ldap-auth](https://apisix.apache.org/zh/docs/apisix/plugins/ldap-auth/) 65%
  - support HTTP Basic credential extraction, LDAP bind using configured `base_dn`, `ldap_uri`, `uid`, `use_tls`, and `tls_verify`, matching consumers by `ldap-auth.user_dn`, and attaching the consumer context
  - not support LDAP search filters, StartTLS fallback discovery, or anonymous consumer behavior
- [x] [opa](https://apisix.apache.org/zh/docs/apisix/plugins/opa/) 70%
  - support OPA HTTP decision calls, custom deny status/body/headers, and `send_headers_upstream`
  - not support full APISIX `with_route` / `with_service` payloads
- [x] [forward-auth](https://apisix.apache.org/zh/docs/apisix/plugins/forward-auth/) 86%
  - support `GET` / `POST`, `request_headers`, `extra_headers`, APISIX-style variable resolution in `extra_headers`, `upstream_headers`, `client_headers`, `ssl_verify`, `keepalive`, `keepalive_timeout`, and `keepalive_pool`
- [x] [multi-auth](https://apisix.apache.org/zh/docs/apisix/plugins/multi-auth/) 60%
  - support ordered fallback across configured `basic-auth`, `key-auth`, `jwt-auth`, and `hmac-auth`; request passes when any configured auth plugin succeeds
  - not support every APISIX auth plugin type yet or preserving per-plugin failure details in the final response

### Security

> 13/13

- [x] [cors](https://apisix.apache.org/zh/docs/apisix/plugins/cors/) 70%
  - support `allow_origins`, `allow_origins = "**"` request-origin echo, `allow_origins_by_regex`, method wildcards, `allow_headers = "**"` request-header reflection, APISIX-style 200 preflight responses, headers/exposed headers, `max_age`, and `allow_credential`
  - not support `allow_origins_by_metadata`, timing allow origins, or exact APISIX wildcard response-header semantics for methods/exposed headers
- [x] [acl](https://apisix.apache.org/zh/docs/apisix/plugins/acl/) 70%
  - only support authenticated consumer `labels`
  - not support `external_user` label fields
- [x] [uri-blocker](https://apisix.apache.org/zh/docs/apisix/plugins/uri-blocker/) 95%
  - support `block_rules`, `rejected_code`, `rejected_msg`, `case_insensitive`, APISIX-style empty default rejection bodies, and `error_msg` JSON custom rejections
  - not support APISIX PCRE/JIT regex engine parity exactly
- [x] [ip-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ip-restriction/) 100%
- [x] [ua-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ua-restriction/) 95%
  - support `allowlist`, `denylist`, using both lists together, allow-before-deny matching, `bypass_missing`, trimmed User-Agent matching, and APISIX-style JSON rejection bodies
  - not support OpenResty multi-value User-Agent header fidelity exactly
- [x] [referer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/referer-restriction/) 95%
  - support `whitelist`, `blacklist`, `bypass_missing`, custom rejection messages, APISIX-style JSON rejection bodies, and leading-`*` suffix host matching
  - not support APISIX `host_def` schema validation exactly
- [x] [consumer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/consumer-restriction/) 80%
  - support `consumer_name`, `service_id`, `route_id`, `consumer_group_id`, blacklist, whitelist, `allowed_by_methods`, custom rejection status/message, and APISIX-style rejection bodies
  - not support APISIX schema method enum validation or automatic consumer-group attachment
- [x] [csrf](https://apisix.apache.org/zh/docs/apisix/plugins/csrf/) 72%
  - support official token cookie/header validation, safe method bypass, token expiry/signature checks including `expires = 0` no-expiry validation, configurable `key` / `expires` / `name`, and APISIX-style JSON error bodies
  - not support encrypted consumer fields or exact Lua random-number formatting parity for token signatures
- [x] [public-api](https://apisix.apache.org/zh/docs/apisix/plugins/public-api/) 60%
  - support exposing registered internal public APIs such as `batch-requests`, `node-status`, and `server-info`, with optional `uri` override
  - not support arbitrary internal API discovery, Prometheus public metrics proxying, or exposing non-registered runtime endpoints
- [x] [GM](https://apisix.apache.org/zh/docs/apisix/plugins/GM/) 25%
  - support official plugin name, priority, empty route schema, no-op HTTP handler, and APISIX SSL `gm` marker validation requiring encryption cert/key plus exactly one sign cert/key pair
  - not support Tongsuo/APISIX-Runtime NTLS enablement, SM2/SM3/SM4 TLS handshakes, dynamic TLS certificate installation, SSL schema injection, or real dual-certificate serving
- [x] [chaitin-waf](https://apisix.apache.org/zh/docs/apisix/plugins/chaitin-waf/) 55%
  - support `mode`, `match.vars` for common request variables, `append_waf_resp_header`, `append_waf_debug_header`, metadata/config `nodes`, config timeout/body/keepalive defaults, monitor/block/off behavior, request body restoration, official WAF response headers, and block response body with `event_id`
  - not support native `resty.t1k`, APISIX health checker/round-robin picker fidelity, full `lua-resty-expr`, response header-filter integration, Unix socket nodes, or real SafeLine binary protocol details
- [x] [data-mask](https://apisix.apache.org/zh/docs/apisix/plugins/data-mask/) 60%
  - support query/header/urlencoded-body masking, simple JSONPath body masking for dot paths and `[*]`, `remove` / `replace` / `regex`, `max_body_size`, `max_req_post_args`, and official plugin name/schema/priority
  - not support APISIX log-phase-only behavior, full `jsonpath` syntax, temporary-file request bodies, access-log `$request_line` rewriting, or preserving original upstream request data while masking logger output
- [x] [oas-validator](https://apisix.apache.org/docs/apisix/plugins/oas-validator/) 55%
  - support official plugin name, priority, schema, inline JSON `spec`, `spec_url` fetch with custom headers, timeout and `ssl_verify`, method/path matching, required path/query/header parameters, JSON request body schema validation, skip flags, `reject_if_not_match`, `verbose_errors`, and configurable rejection status
  - not support OpenAPI `$ref` / `components` resolution, plugin metadata `spec_url_ttl` refresh semantics, all OpenAPI parameter style/explode combinations, non-JSON request body schema validation, or response validation

### Traffic

> 19/19

- [x] [limit-req](https://apisix.apache.org/zh/docs/apisix/plugins/limit-req/) 70%
  - only support `policy = "local"`
  - not support `redis` or `redis-cluster`
- [x] [limit-conn](https://apisix.apache.org/zh/docs/apisix/plugins/limit-conn/) 68%
  - support local concurrent request limiting, `rules`, `key_type = var`, `var_combination`, HTTP header variables, `rejected_code`, `rejected_msg`, and `allow_degradation`
  - not support string expression values for `conn` / `burst`, `only_use_default_delay`, `redis`, or `redis-cluster`
- [x] [limit-count](https://apisix.apache.org/zh/docs/apisix/plugins/limit-count/) 70%
  - support local/Redis fixed-window quotas, `rules`, per-rule `header_prefix`, `key_type = var`, `constant`, and `var_combination`, HTTP header variables, quota headers, `rejected_code`, `rejected_msg`, and `allow_degradation`
  - not support string expression values for `count` / `time_window`, plugin metadata custom quota header names, or `redis-cluster`
- [x] [graphql-limit-count](https://apisix.apache.org/docs/apisix/plugins/graphql-limit-count/) 55%
  - support official plugin name, priority, schema, POST `application/json` and `application/graphql` requests, GraphQL selection-depth cost counting, local fixed-window quotas, `X-RateLimit-*` headers, `rejected_code`, `rejected_msg`, `key`, `key_type`, and fragment/inline-fragment depth expansion
  - not support Redis/Redis Cluster quota sharing, full GraphQL spec parsing parity, APISIX `graphql.max_size`, or exact `resty.limit.count` behavior
- [x] [proxy-cache](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-cache/) 78%
  - support in-memory response caching with `cache_key`, `cache_method`, `cache_http_status`, `cache_ttl`, `cache_bypass`, `no_cache`, `hide_cache_headers`, `consumer_isolation`, `cache_control` request bypass for `no-cache` / `no-store`, `only-if-cached` misses, request stale refresh controls (`max-age`, `max-stale`, `min-fresh`), upstream `private` / `no-store` / `no-cache` non-storage, upstream `s-maxage` / `max-age` / `Expires` TTL derivation, `Vary`, `PURGE`, and `Apisix-Cache-Status`
  - not support disk cache zones or stale serving
- [x] [graphql-proxy-cache](https://apisix.apache.org/docs/apisix/plugins/graphql-proxy-cache/) 55%
  - support official plugin name, priority, schema, GET/POST GraphQL request validation, JSON and `application/graphql` bodies, mutation bypass with `Apisix-Cache-Status: BYPASS`, MD5 cache keys, `APISIX-Cache-Key`, in-memory TTL cache, `consumer_isolation`, and `cache_set_cookie`
  - not support NGINX disk cache zones, APISIX public `PURGE` endpoint, configured `graphql.max_size`, route/service ID participation in cache keys, full GraphQL spec parsing parity, or exact APISIX `proxy-cache` handler behavior
- [x] [request-validation](https://apisix.apache.org/zh/docs/apisix/plugins/request-validation/) 85%
  - support JSON and `application/x-www-form-urlencoded` `body_schema`, JSON body normalization before proxying, `header_schema`, `rejected_code`, and `rejected_msg`
- [x] [proxy-mirror](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-mirror/) 70%
  - support HTTP mirror `host`, `path`, `path_concat_mode`, `sample_ratio`
  - not support gRPC mirroring
  - not support APISIX DNS resolver behavior
- [x] [kafka-proxy](https://apisix.apache.org/docs/apisix/plugins/kafka-proxy/) 35%
  - support official plugin name, priority, schema, optional SASL/PLAIN config, and request-context propagation for future Kafka upstream transport integration
  - not support Kafka upstream transport/proxying, websocket-to-Kafka forwarding, SASL mechanisms beyond PLAIN, or encrypted storage for `sasl.password`
- [x] [dubbo-proxy](https://apisix.apache.org/docs/apisix/plugins/dubbo-proxy/) 30%
  - support official plugin name, priority, schema, required `service_name` / `service_version`, optional `method`, URI-derived method fallback, and request-context propagation for future Dubbo upstream transport integration
  - not support OpenResty/Tengine Dubbo runtime support, hessian2 Map request/response conversion, `upstream_multiplex_count`, HTTP-to-Dubbo proxy transport, or Dubbo response-to-HTTP conversion
- [x] [http-dubbo](https://apisix.apache.org/docs/apisix/plugins/http-dubbo/) 55%
  - support official plugin name, priority, schema, route-upstream TCP dialing, Dubbo 2.x fastjson request frame construction, `service_name`, `service_version`, `method`, `params_type_desc`, `serialized`, `serialization_header_key`, connect/send/read timeouts, JSON-array generic invocation parameter serialization, pre-serialized body passthrough, Dubbo header/status parsing, and HTTP 200 body mapping for application responses
  - not support APISIX `before_proxy` phase fidelity, OpenResty cosocket behavior, hessian2 serialization, full fastjson precision/type features, multiplexing, every Dubbo response status branch, upstream health checks/retries, or route-builder support for non-round-robin upstream algorithms
- [x] [api-breaker](https://apisix.apache.org/zh/docs/apisix/plugins/api-breaker/) 95%
  - support `break_response_code`, `break_response_body`, `break_response_headers` with bounded variable resolution, `max_breaker_sec`, `unhealthy.http_statuses`, `unhealthy.failures`, `healthy.http_statuses`, and `healthy.successes`
  - not support APISIX shared-dict state keyed by host and URI, exponential breaker windows, or exact OpenResty log-phase timing
- [x] [traffic-split](https://apisix.apache.org/zh/docs/apisix/plugins/traffic-split/) 75%
  - support weighted inline upstream selection, `upstream_id`, and bounded `match.vars` for common request variables
  - not support APISIX upstream balancer fidelity for all upstream algorithms, health checks, retries, or full `lua-resty-expr` syntax
- [x] [traffic-label](https://apisix.apache.org/zh/docs/apisix/plugins/traffic-label/) 55%
  - support first-match rules, `set_headers`, weighted actions, `arg_*`, `http_*`, `uri`, `request_uri`, `method`, `host`, `scheme`, and `remote_addr`
  - not support full `lua-resty-expr` syntax or full APISIX variable resolution
- [x] [request-id](https://apisix.apache.org/zh/docs/apisix/plugins/request-id/) 85%
  - support custom header names, response header opt-out, incoming request ID preservation, `uuid`, `nanoid`, `range_id`, and local numeric `snowflake` generation
  - not support APISIX plugin-attr `snowflake` configuration or etcd-backed distributed data-machine leasing
- [x] [proxy-control](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-control/) 60%
  - support route/global `request_buffering` flag by buffering the Go proxy request body before upstream forwarding
  - not support APISIX-Runtime/NGINX dynamic `proxy_request_buffering` control or disk-backed buffering
- [x] [proxy-buffering](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-buffering/) 60%
  - support route/global `disable_proxy_buffering` by switching to immediate reverse-proxy response flushing
  - not support NGINX `proxy_buffering` internals or disk-backed response buffering controls
- [x] [client-control](https://apisix.apache.org/zh/docs/apisix/plugins/client-control/) 100%
- [x] [workflow](https://apisix.apache.org/zh/docs/apisix/plugins/workflow/) 45%
  - support official action-array config shape, first matching `case`, and `return` actions with configured status code
  - not support `limit-count`, `limit-conn`, full `lua-resty-expr`, or delegated plugin log handlers

### Observability

Tracers:

> 3/3

- [x] [zipkin](https://apisix.apache.org/zh/docs/apisix/plugins/zipkin/) 45%
  - support B3 extraction/injection, `endpoint`, `sample_ratio`, `service_name`, `server_addr`, `span_version`, and Zipkin v2 span reporting
  - not support APISIX multi-phase span tree, batch processor behavior, `plugin_attr.zipkin.set_ngx_var`, or Zipkin v1 span layout
- [x] [skywalking](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking/) 50%
  - support `sample_ratio`, `plugin_attr.skywalking` defaults, `sw8` parse/injection, trace/segment IDs, request timing, status/error tags, service and instance names, `$hostname`, and HTTP segment reporting to `/v3/segments`
  - not support the native OpenResty SkyWalking tracer, shared `tracing_buffer`, delayed body-filter lifecycle, streaming span finish semantics, or full SkyWalking segment reference fidelity
- [x] [opentelemetry](https://apisix.apache.org/zh/docs/apisix/plugins/opentelemetry/) 47%
  - support official plugin name with existing Go tracing middleware, APISIX sampler names (`always_on`, `always_off`, `trace_id_ratio`, `parent_base`), sampler defaults, configurable middleware server name, `additional_attributes` from NGINX/APISIX/request vars, and `additional_header_prefix_attributes`
  - keep `otel` as a compatibility alias
  - not support APISIX collector/exporter metadata, `trace_id_source`, `set_ngx_var`, phase child spans, or log-phase timing parity

Metrics:

> 3/3

- [x] [prometheus](https://apisix.apache.org/zh/docs/apisix/plugins/prometheus/) 45%
  - support official plugin name, priority, `prefer_name` schema validation, route/service metric labels using IDs by default and names when `prefer_name` is true, pass-through route/global plugin config, and public API metrics endpoint registration at `/apisix/prometheus/metrics`
  - reuse existing Go Prometheus metrics collection and support `plugin_attr.prometheus.metric_prefix`, `default_buckets`, `export_uri`, `enable_export_server`, and `export_addr` for the dedicated metrics export server
  - not support APISIX exporter parity for all labels, stream metrics, extra-label variable expansion, metric expiration, privileged-agent offload, or exact `nginx-lua-prometheus` lifecycle behavior
- [x] [node-status](https://apisix.apache.org/zh/docs/apisix/plugins/node-status/) 50%
  - support `/apisix/status` response shape when `node-status` is enabled in `conf.plugins`
  - not support exact NGINX connection state counters
- [x] [datadog](https://apisix.apache.org/zh/docs/apisix/plugins/datadog/) 68%
  - support DogStatsD UDP metrics, metadata `host`, `port`, `namespace`, `constant_tags`, route `constant_tags`, `prefer_name`, route/service ID-vs-name tags, consumer tags, balancer IP tags, `include_path` with matched route pattern, `include_method`, upstream latency, and APISIX-side latency derived from upstream latency
  - not support APISIX batch processor behavior or exact APISIX log-entry timing/source parity yet

Loggers:

> 19/19

- [x] [http-logger](https://apisix.apache.org/zh/docs/apisix/plugins/http-logger/) 55%
  - support `uri`, `auth_header`, `timeout`, `log_format`, `concat_method`, `ssl_verify`, HTTP POST delivery, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior or `max_pending_entries`
  - not support `include_req_body_expr` or `include_resp_body_expr`
- [x] [skywalking-logger](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking-logger/) 60%
  - support `endpoint_addr`, `service_name`, `service_instance_name`, `timeout`, `log_format`, `/v3/logs` delivery, basic `sw8` trace correlation, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior or `max_pending_entries`
- [x] [tcp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/tcp-logger/) 50%
  - support `host`, `port`, `timeout`, `log_format`, `tls`, `tls_options` as TLS server name / SNI, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support `include_req_body_expr` or `include_resp_body_expr`
- [x] [kafka-logger](https://apisix.apache.org/zh/docs/apisix/plugins/kafka-logger/) 60%
  - support `brokers`, deprecated `broker_list`, broker `sasl_config` for `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`, `kafka_topic`, `key`, `producer_type`, `required_acks`, `timeout`, producer batch defaults, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior, `max_pending_entries`, `include_req_body_expr`, `include_resp_body_expr`, or `meta_format = origin`
- [x] [rocketmq-logger](https://apisix.apache.org/zh/docs/apisix/plugins/rocketmq-logger/) 55%
  - support `nameserver_list`, `topic`, `key`, `tag`, `timeout`, `access_key`, `secret_key`, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture with RocketMQ sync producer delivery
  - not support APISIX batch processor behavior, `max_pending_entries`, `include_req_body_expr`, `include_resp_body_expr`, `meta_format = origin`, or `use_tls`
- [x] [udp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/udp-logger/) 50%
  - support `host`, `port`, `timeout`, `log_format`, UDP delivery, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support `include_req_body_expr` or `include_resp_body_expr`
- [x] [clickhouse-logger](https://apisix.apache.org/zh/docs/apisix/plugins/clickhouse-logger/) 60%
  - support `endpoint_addr`, random selection from `endpoint_addrs`, `user`, `password`, `database`, `logtable`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior, `max_pending_entries`, `include_req_body_expr`, or `include_resp_body_expr`
- [x] [syslog](https://apisix.apache.org/zh/docs/apisix/plugins/syslog/) 50%
  - support `host`, `port`, `timeout`, `sock_type`, `flush_limit`, `drop_limit`, `pool_size`, `tls` schema/config acceptance, `log_format`, `include_req_body`, Go-side `include_resp_body`, capped body-size capture, and UDP/TCP syslog delivery through the Go syslog writer
  - not support APISIX batch processor buffering semantics, OpenResty syslog connection pooling/TLS behavior parity, `include_req_body_expr`, or `include_resp_body_expr`
- [x] [log-rotate](https://apisix.apache.org/zh/docs/apisix/plugins/log-rotate/) 60%
  - support `plugin_attr.log-rotate` defaults for `interval`, `max_kept`, `max_size`, `timeout`, and `enable_compression`, APISIX timestamped `__access.log` / `__error.log` naming, max-size rotation, interval checks on request, current-file recreation, history pruning, and `.tar.gz` compression
  - not support OpenResty timer lifecycle, NGINX master `USR1` log reopening, NGINX config path discovery, or shelling out to system `tar`
- [x] [error-log-logger](https://apisix.apache.org/zh/docs/apisix/plugins/error-log-logger/) 55%
  - support official metadata-shaped config for `tcp`, `skywalking`, `clickhouse`, and `kafka`, level filtering, TCP/TLS delivery, SkyWalking `/v3/logs` entries, ClickHouse JSONEachRow inserts, Kafka topic/key publishing, legacy `host` / `port` TCP config, and official batch/default knobs
  - not support direct `ngx.errlog` capture, OpenResty timer lifecycle, APISIX batch processor retry semantics, Kafka SASL, or encrypted metadata fields
- [x] [sls-logger](https://apisix.apache.org/zh/docs/apisix/plugins/sls-logger/) 55%
  - support RFC5424 SLS log messages over TLS TCP with `host`, `port`, `project`, `logstore`, `access_key_id`, `access_key_secret`, `timeout`, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior
- [x] [google-cloud-logging](https://apisix.apache.org/zh/docs/apisix/plugins/google-cloud-logging/) 55%
  - support service-account `auth_config`, `auth_file`, JWT bearer token exchange, `entries_uri`, `resource`, `log_id`, `ssl_verify`, `log_format`, and default Cloud Logging `httpRequest` entry expansion
  - not support APISIX batch processor behavior, `max_pending_entries`, or request/response body capture
- [x] [splunk-hec-logging](https://apisix.apache.org/zh/docs/apisix/plugins/splunk-hec-logging/) 50%
  - support Splunk HEC `endpoint.uri`, `endpoint.token`, `endpoint.channel`, `endpoint.timeout`, `ssl_verify`, and `log_format`
  - not support APISIX batch processor behavior, `max_pending_entries`, or request/response body capture
- [x] [file-logger](https://apisix.apache.org/zh/docs/apisix/plugins/file-logger/) 68%
  - support `path`, `log_format`, bounded `match` expressions for common request variables and `$status`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support `include_req_body_expr` or `include_resp_body_expr`
- [x] [loggly](https://apisix.apache.org/zh/docs/apisix/plugins/loggly/) 60%
  - support RFC5424 Loggly syslog messages over UDP and HTTP/S bulk endpoint delivery
  - support `customer_token`, `severity`, `severity_map`, `tags`, `host`, `port`, `protocol`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior, `max_pending_entries`, metadata-only delivery config parity, or `include_resp_body_expr`
- [x] [elasticsearch-logger](https://apisix.apache.org/zh/docs/apisix/plugins/elasticsearch-logger/) 73%
  - support `endpoint_addr`, random `endpoint_addrs` selection, `field.index`, time/APISIX variable expansion in `field.index`, Elasticsearch major-version discovery for legacy `_type`, `auth`, `headers`, `timeout`, `ssl_verify`, `log_format`, `include_req_body`, `include_resp_body`, capped body-size capture, and `_bulk` NDJSON delivery
  - not support APISIX batch processor behavior, `max_pending_entries`, `include_req_body_expr`, or `include_resp_body_expr`
- [x] [tencent-cloud-cls](https://apisix.apache.org/zh/docs/apisix/plugins/tencent-cloud-cls/) 55%
  - support `cls_host`, `cls_topic`, `scheme`, `ssl_verify`, `secret_id`, `secret_key`, `sample_ratio`, `global_tag`, `log_format`, `include_req_body`, `include_resp_body`, capped body-size capture, Tencent CLS sha1 authorization, and `/structuredlog` protobuf delivery
  - not support APISIX batch processor behavior, `max_pending_entries`, `include_req_body_expr`, `include_resp_body_expr`, or lz4/zstd compression
- [x] [loki-logger](https://apisix.apache.org/zh/docs/apisix/plugins/loki-logger/) 60%
  - support random selection from `endpoint_addrs`, `endpoint_uri`, `tenant_id`, custom headers, `log_labels`, `ssl_verify`, `timeout`, `log_format`, `include_req_body`, `include_resp_body`, and capped body-size capture
  - not support APISIX batch processor behavior or `max_pending_entries`
- [x] [lago](https://apisix.apache.org/docs/apisix/plugins/lago/) 64%
  - support official plugin name, priority, schema, Lago batch event payload shape, random selection from `endpoint_addrs`, Bearer token delivery to `endpoint_uri`, request/response variable templates for transaction ID, subscription ID, and event properties, `include_req_body`, `include_resp_body`, capped body-size capture through `${request_body}` / `${response_body}` templates, `ssl_verify`, timeout, and keepalive config defaults
  - not support APISIX batch processor buffering/retry semantics, full APISIX/NGINX variable coverage, or request start-time fidelity

### AI

> 12/12

- [x] [ai](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/ai.lua) 20%
  - support official plugin name, priority, empty schema, and pass-through compatibility registration
  - not support APISIX global-scope router matching cache replacement, upstream handler replacement, balancer phase replacement, route feature analysis, OpenResty keepalive pool behavior, event hook registration, or any runtime acceleration behavior from the Lua implementation
- [x] [ai-aliyun-content-moderation](https://apisix.apache.org/docs/apisix/plugins/ai-aliyun-content-moderation/) 50%
  - support official plugin name, priority, schema, Aliyun `TextModerationPlus` request checks, HMAC-SHA1 request signing, `endpoint`, `region_id`, `access_key_id`, `access_key_secret`, `check_request`, `request_check_service`, `request_check_length_limit`, `risk_level_bar`, `deny_code`, `deny_message`, timeout, keepalive, `ssl_verify`, basic OpenAI Chat/Responses-style content extraction, original request body replay, pass-through on moderation service errors, and rejection when risk level reaches the configured bar
  - not support APISIX `ai-proxy` / `ai-proxy-multi` picked-instance enforcement, full APISIX AI protocol registry extraction, response body moderation, streaming `realtime` or `final_packet` moderation, provider-compatible AI deny response shaping, response risk-level SSE annotation, APISIX log variables, or encrypted storage for `access_key_secret`
- [x] [ai-aws-content-moderation](https://apisix.apache.org/docs/apisix/plugins/ai-aws-content-moderation/) 55%
  - support official plugin name, priority, schema, AWS Comprehend `DetectToxicContent` request checks, SigV4 signing, `comprehend.access_key_id`, `comprehend.secret_access_key`, `comprehend.region`, optional `comprehend.endpoint`, `comprehend.ssl_verify`, `moderation_categories`, `moderation_threshold`, original request body replay, and rejection on toxicity/category thresholds
  - not support APISIX/OpenResty AWS SDK credential provider behavior, `session_token`, response body moderation, streaming moderation, provider-compatible AI deny response shaping, AI protocol content extraction, APISIX log variables, or encrypted storage for `comprehend.secret_access_key`
- [x] [ai-rag](https://apisix.apache.org/docs/apisix/plugins/ai-rag/) 55%
  - support official plugin name, priority, schema, Azure OpenAI embeddings, Azure AI Search vector search, `ssl_verify`, `ai_rag.embeddings.input`, `ai_rag.vector_search.fields`, provider status/body propagation, `ai_rag` request-body removal, OpenAI Chat message append, OpenAI Responses input append, and request body replay
  - not support providers beyond Azure OpenAI/Azure AI Search, full APISIX AI protocol append registry, every Azure embedding/search request option beyond passthrough embedding body and vector fields, encrypted storage, APISIX ctx variables/logging, streaming/body-filter behavior, or phase-perfect interaction with `ai-proxy`
- [x] [ai-prompt-decorator](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-decorator/) 55%
  - support official plugin name, priority, schema, JSON body rewrite, OpenAI Chat `messages` prepend/append, OpenAI Responses `instructions` prepend and `input` append, request body replay, and JSON error responses for empty/invalid bodies
  - not support Anthropic Messages, Bedrock Converse, OpenAI Embeddings, passthrough protocol decoration, APISIX AI protocol conversion registry, streaming-specific behavior, or integration with a real `ai-proxy` provider transport
- [x] [ai-prompt-guard](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-guard/) 60%
  - support official plugin name, priority, schema, regex validation, `allow_patterns` before `deny_patterns`, default last-user-message checking, `match_all_roles`, `match_all_conversation_history`, OpenAI Chat `messages`, OpenAI Responses `instructions` / `input`, and JSON rejection bodies
  - not support APISIX/OpenResty regex flags exactly, full Anthropic Messages or Bedrock Converse protocol extraction, OpenAI Embeddings, passthrough protocol detection, streaming-specific behavior, or AI-provider deny response shaping
- [x] [ai-prompt-template](https://apisix.apache.org/docs/apisix/plugins/ai-prompt-template/) 55%
  - support official plugin name, priority, schema, multiple named templates, `template_name` selection from JSON request bodies, `{{field}}` substitution from top-level request fields, OpenAI Chat-style `model` and `messages` output, request body replay, and official missing/unknown template errors
  - not support full APISIX `body-transformer` / `lua-resty-template` expression syntax, nested variable lookup, XML/form/multipart inputs, Anthropic or Responses-native template output, template LRU cache behavior, or integration with a real `ai-proxy` provider transport
- [x] [ai-request-rewrite](https://apisix.apache.org/docs/apisix/plugins/ai-request-rewrite/) 45%
  - support official plugin name, priority, schema, OpenAI Chat-compatible sidecar LLM requests, `prompt`, `provider`, `auth.header`, `auth.query`, `options`, `override.endpoint`, timeout, keepalive, `ssl_verify`, non-streaming `choices[].message.content` extraction, request body replacement, and request body replay
  - not support APISIX AI provider/protocol registry, protocol conversion, streaming LLM responses, Anthropic/Gemini/Vertex/Bedrock-native request construction, Azure OpenAI model omission behavior, token usage variables, provider response filters, or fallback/error-response policy integration
- [x] [ai-rate-limiting](https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/) 50%
  - support official plugin name, priority, schema, local token quota windows, global `limit` / `time_window`, per-instance `instances`, `show_limit_quota_header`, `limit_strategy` for `total_tokens`, `prompt_tokens`, and `completion_tokens`, `rejected_code`, `rejected_msg`, non-streaming JSON `usage` accounting, and context helpers for future `ai-proxy-multi` instance selection
  - not support APISIX `limit-count` policy sharing, `rules`, Lua `cost_expr` / `expression`, string variable limits, Redis or Redis Cluster storage, exact log-phase accounting, streaming token usage, automatic `ai-proxy` / `ai-proxy-multi` instance selection, or fallback-to-next-instance behavior
- [x] [ai-proxy](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/) 45%
  - support official plugin name, priority, schema, direct non-streaming OpenAI Chat-compatible proxying, `provider`, `auth.header`, `auth.query`, `options`, `override.endpoint`, `override.llm_options.max_tokens`, timeout, `max_req_body_size`, `max_response_bytes`, keepalive, `ssl_verify`, JSON content-type checks, provider response status/header/body forwarding, and default endpoints for OpenAI-compatible providers
  - not support APISIX AI protocol detection/conversion registry, OpenAI Responses or Embeddings routing, Anthropic/Gemini/Vertex/Bedrock-native request construction, AWS SigV4 or GCP token auth, streaming SSE/EventStream parsing and flushing, `override.request_body` deep merge, logging variables, active connection metrics, `ai-proxy-multi` fallback, or phase-perfect wrapping by lower-priority plugins such as `ai-rate-limiting`
- [x] [ai-proxy-multi](https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/) 45%
  - support official plugin name, priority, schema, multiple weighted instances, `balancer.algorithm` for `roundrobin` and header/cookie/basic-var `chash`, `fallback_strategy` for `http_429` and `http_5xx`, `max_retries`, `retry_on_failure_within_ms`, direct non-streaming OpenAI Chat-compatible proxying, per-instance `provider`, `auth.header`, `auth.query`, `options`, `override.endpoint`, `override.llm_options.max_tokens`, timeout, `max_req_body_size`, `max_response_bytes`, keepalive, `ssl_verify`, JSON content-type checks, and provider response status/header/body forwarding
  - not support APISIX health checks, DNS node resolution and host header/SNI preservation, priority balancer parity, `ai-rate-limiting` instance-health fallback integration, AI protocol detection/conversion registry, OpenAI Responses or Embeddings routing, Anthropic/Gemini/Vertex/Bedrock-native request construction, AWS SigV4 or GCP token auth, streaming SSE/EventStream parsing and flushing, `override.request_body` deep merge, logging variables, active connection metrics, or phase-perfect wrapping by lower-priority plugins
- [x] [mcp-bridge](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/mcp-bridge.lua) 55%
  - support official plugin name, priority, schema, configurable `base_uri`, stdio subprocess launch with `command` / `args`, `GET {base_uri}/sse` SSE endpoint, initial `endpoint` event advertising `{base_uri}/message?sessionId=...`, `POST {base_uri}/message` JSON-RPC body forwarding to subprocess stdin, stdout line forwarding as SSE `message` events, stderr line forwarding as `notifications/stderr`, and subprocess cleanup on SSE disconnect
  - not support APISIX shared-dict cross-worker session recovery, ping keepalive events, OpenResty coroutine/process semantics, partial-line buffering parity, process timeouts, worker-exit hooks, distributed session state, backpressure controls, or full MCP protocol validation

### Stream

> 1/1

- [x] [mqtt-proxy](https://apisix.apache.org/docs/apisix/plugins/mqtt-proxy/) 15%
  - support official plugin name, priority, schema, required `protocol_level`, default `protocol_name = "MQTT"`, and registry/config validation
  - not support APISIX stream routes, L4 TCP proxying, MQTT CONNECT preread parsing, `mqtt_client_id` variable registration, chash load balancing by MQTT client ID, stream mTLS behavior, or stream log phase

### Development

> 1/1

- [x] [example-plugin](https://github.com/apache/apisix/blob/release/3.17/apisix/plugins/example-plugin.lua) 60%
  - support official plugin name, priority, schema, required `i`, optional `s` / `t` / `ip` / `port`, pass-through middleware, route upstream override through the existing Go traffic-split override path when `ip` is configured, and control API `GET /v1/plugin/example-plugin/hello` with text or JSON response
  - not support APISIX metadata schema exposure, plugin attr logging, OpenResty phase-specific logging, delayed body filter behavior, direct `apisix.upstream.set` parity, or treating this upstream demonstration plugin as a production feature

## TODO

- [ ] standalone mode
- [ ] handle etcd compact
- [ ] github action: go releaser
- [ ] logforamt change didn't take effect immediately
