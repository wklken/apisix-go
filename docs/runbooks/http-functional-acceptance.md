# HTTP Functional Acceptance Index

This index locates executable evidence for the implemented HTTP scope in
[HTTP data-plane compatibility](../http-data-plane.md). Apache APISIX 3.17
observable behavior remains the target; accepted native and dialect exceptions
are listed in that document, not encoded as runtime readiness metadata.

## How an acceptance stage closes

1. Pin the source revision and enumerate its implemented HTTP factories from
   `pkg/plugin/registry.go`. Registration supplies inventory only, not parity.
2. Map normal behavior and a meaningful rejection, failure, boundary, or
   lifecycle case to executable assertions. For a pass-through or system
   plugin, use its actual configuration/control endpoint and the owning
   runtime's failure path rather than inventing a plugin-specific error.
3. Reuse unchanged test evidence and inspect the uncovered paths. Add targeted
   tests or a fixed-upstream comparison only where an actual gap remains.
   Keep plugin unit behavior, real-process integration, and upstream comparison
   distinct. A manifest filename or plugin configuration key alone is not
   evidence that the behavior ran.
4. Resolve reproducible blocking defects such as authentication bypass, data
   disclosure outside an explicitly accepted exception, crashes, wrong upstream
   selection, or ignored supported validation constraints. Assess other
   differences individually; accepted is not the same as fixed.
5. Close when the mapped target scenarios have evidence, blocking findings have
   a verified disposition, and remaining exceptions are explicit. Do not
   require another unrestricted repository scan or a zero-finding review round.
   Later work starts from changed behavior, a demonstrated coverage gap, or a
   concrete failure.

The tables are navigation to representative assertions, not a claim that every
option and plugin combination has been exhaustively tested. Read the linked
case before extending its claim. Maintain the affected row when moving a test
or adding/removing a factory; do not add dated pass counts to this document.

## Core HTTP contracts

The request-serving and publication paths need their own evidence in addition
to plugin tests. A successful plugin handler test does not establish atomic
reload, TLS selection, or resource retirement.

| Contract | Normal behavior / primary assertion | Failure, boundary or isolation assertion |
| --- | --- | --- |
| Host, URI and service matching | [TestRouteDecisionIndexSelectsMostSpecificWildcardWithManyUnrelatedSuffixes](../../pkg/route/route_host_test.go#L14) | [TestRouteDecisionIndexFallsBackToBroaderWildcardAfterSpecificMethodMiss](../../pkg/route/route_host_test.go#L41) |
| Service inheritance and WebSocket overrides | [TestPreparedGenerationInheritsServiceHosts](../../pkg/route/compiler_generation_fixture_test.go#L235) | [TestPreparedGenerationExplicitFalseWebsocketOverridesService](../../pkg/route/compiler_generation_fixture_test.go#L374) |
| Path normalization before authorization | [TestNormalizeRequestPathCleansDotSegments](../../pkg/server/server_test.go#L506) | [TestRegressionCasbinUsesNormalizedServingPath](../../pkg/server/parity_regression_test.go#L11) |
| Consumer identity and prepared bindings | [TestBuildPreparedHandlerResolvesOnlyPreparedConsumerRecords](../../pkg/route/prepared_handler_test.go#L30) | [TestAuthenticatedRouteOverwritesForgedConsumerHeader](../../pkg/route/consumer_access_test.go#L96) |
| Retry and unsafe replay | [TestRetryTransportNonIdempotentConnectFailureCanFailOver](../../pkg/proxy/retry_sent_test.go#L17) | [TestRetryTransportPartialWriteDoesNotReplayPOST](../../pkg/proxy/retry_sent_test.go#L110) |
| Send timeout and informational responses | [TestSendTimeoutAllowsContinuousUpload](../../pkg/proxy/send_timeout_test.go#L99) | [TestSendTimeoutCoversFlushAfterInformationalResponse](../../pkg/proxy/send_timeout_test.go#L246) |
| Health checks and priority failover | [TestHealthAwareLoadBalanceActiveProbeRecoversTarget](../../pkg/proxy/health_test.go#L58) | [TestPriorityHealthAwareLoadBalanceFallsThroughAfterHigherUnavailable](../../pkg/proxy/priority_test.go#L70) |
| Frontend TLS and client certificates | [TestFrontendTLSHandshakeSelectsConfiguredCipher](../../pkg/server/tls_test.go#L499) | [TestFrontendTLSHandshakeRequiresTrustedClientCertificate](../../pkg/server/tls_test.go#L528) |
| Outbound TLS and client credentials | [TestNewTransportSendsConfiguredTLSClientCertificate](../../pkg/proxy/transport_test.go#L88) | [TestNewTransportHonorsTLSVerification](../../pkg/proxy/transport_test.go#L69) |
| Upgrade and HTTP streaming | [TestRouteEnabledWebsocketUpgradeUsesReverseProxyHijack](../../pkg/route/websocket_contract_test.go#L220) | [TestWebsocketUpgradeDisabledForwardsHTTPWithoutUpgradeHeaders](../../pkg/route/websocket_contract_test.go#L154) |
| Standalone templates and readiness | [TestStandaloneExpandsFileTemplatesBeforeResourceNormalization](../../pkg/config/standalone_templates_regression_test.go#L12) | [TestStandaloneWatcherRejectedOnlyAcknowledgementDoesNotEstablishReadiness](../../pkg/config/standalone_test.go#L390) |
| Desired state, publication failure and deletion | [TestCoordinatorPublishesTombstoneOnceThenCompactsDesiredState](../../pkg/generation/coordinator_test.go#L62) | [TestCoordinatorCommitsOnlyAfterSuccessfulPublish](../../pkg/generation/coordinator_test.go#L88) |
| Atomic activation and generation isolation | [TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets](../../pkg/server/generation_isolation_test.go#L49) | [TestGenerationEngineFailedPublishLeavesActiveBundleUnchanged](../../pkg/server/generation_engine_test.go#L221) |
| Lease drain and shutdown | [TestGenerationEngineRetiresPredecessorAfterLeaseDrain](../../pkg/server/generation_engine_test.go#L246) | [TestServerShutdownTimeoutDoesNotReleaseEngineOrResolver](../../pkg/server/server_test.go#L1742) |
| Etcd updates and server-info renewal | [TestServerInfoReporterCreatesLeasePutsValueAndRenews](../../pkg/etcd/server_info_test.go#L109) | [TestInvalidConsumerDoesNotBlockRouteDeletion](../../pkg/etcd/watcher_invalid_resource_test.go#L13) |

## High-risk composition

These cases exercise shared phase or ownership boundaries. They supplement
individual plugin tests without claiming all pairwise combinations.

| Boundary | Executable assertion |
| --- | --- |
| Authentication with effective rewrite | [TestPlan14V2JWTAuthPayloadFeedsEffectiveProxyRewrite](../../pkg/route/scoped_rewrite_test.go#L424) |
| Consumer response ownership | [TestResolvedConsumerResponseWinnerMaterializesBeforeRouteStageAndRunsOnce](../../pkg/route/buffered_response_cache_test.go#L141) |
| Cache miss stores the transformed representation | [TestBufferedRouteCacheMissStoresAfterTransformsPerInstance](../../pkg/route/buffered_response_cache_test.go#L275) |
| Cache hit does not repeat transformations | [TestBufferedRouteCacheHitConsumesOnceAndSkipsTransformsStores](../../pkg/route/buffered_response_cache_test.go#L249) |
| Authentication, CORS, response rewrite and logging | [TestPluginPhaseClosureBuildsAuthCORSResponseRewriteAndLogger](../../pkg/route/log_phase_test.go#L97) |
| Batch child credentials | [TestHandlerAuthenticatesWithPipelineCredentialHeaders](../../pkg/plugin/batch_requests/plugin_test.go#L1118) |
| Schema normal/invalid value pairs | [TestSchemaConstraintsRejectInvalidRequests](../../pkg/plugin/request_validation/schema_constraints_test.go#L14) |
| Explicit Schema dialect keeps format rejection | [TestExplicitSchemaDialectDoesNotDisableFormatValidation](../../pkg/plugin/request_validation/schema_constraints_test.go#L87) |

Real-process complements include the [batch-requests manifest](../../t/plugin/batch-requests.yaml)
(`grpc-transcode-direct-and-batch`), [proxy-cache manifest](../../t/plugin/proxy-cache.yaml)
(Vary representations and purge), [data-mask manifest](../../t/plugin/data-mask.yaml)
(actual log delivery while forwarded inputs remain unchanged), and
[request-validation manifest](../../t/plugin/request-validation.yaml)
(`explicit-dialect-keeps-format-assertions`, including invalid header/body and
one valid upstream request).

## HTTP plugin evidence

Each row names a current HTTP factory. Unit tests may themselves use real HTTP,
gRPC, process, or collector fixtures; that is different from launching the
complete APISIX-Go data plane with the YAML harness. The last column points to
an owning manifest when one exists, or explicitly identifies the narrower
available seam. The rows do not classify stream-only `mqtt-proxy` as HTTP.

| Factory | Normal behavior example | Boundary, rejection or lifecycle example | Real-process manifest / other seam |
| --- | --- | --- | --- |
| `acl` | [TestHandlerAppliesConsumerAllowAndDenyLabels](../../pkg/plugin/acl/plugin_test.go#L47) | [TestHandlerRejectsMissingAuthentication](../../pkg/plugin/acl/plugin_test.go#L28) | [Manifest](../../t/plugin/acl.yaml) |
| `ai-aliyun-content-moderation` | [TestHandlerCallsAliyunAndPreservesRequestBody](../../pkg/plugin/ai_aliyun_content_moderation/plugin_test.go#L113) | [TestHandlerRejectsRiskLevelAtBar](../../pkg/plugin/ai_aliyun_content_moderation/plugin_test.go#L177) | Provider HTTP and streaming fixtures in unit tests; no dedicated YAML manifest. |
| `ai-aws-content-moderation` | [TestHandlerCallsComprehendAndPreservesRequestBody](../../pkg/plugin/ai_aws_content_moderation/plugin_test.go#L86) | [TestHandlerRejectsToxicityAboveThreshold](../../pkg/plugin/ai_aws_content_moderation/plugin_test.go#L249) | [Manifest](../../t/plugin/ai-aws-content-moderation.yaml) |
| `ai-prompt-decorator` | [TestHandlerDecoratesOpenAIChatMessages](../../pkg/plugin/ai_prompt_decorator/plugin_test.go#L28) | [TestHandlerRejectsInvalidJSONBody](../../pkg/plugin/ai_prompt_decorator/plugin_test.go#L200) | [Manifest](../../t/plugin/ai-prompt-decorator.yaml) |
| `ai-prompt-guard` | [TestHandlerChecksAllowBeforeDenyPatterns](../../pkg/plugin/ai_prompt_guard/plugin_test.go#L242) | [TestHandlerRejectsEmptyRequestBody](../../pkg/plugin/ai_prompt_guard/plugin_test.go#L51) | [Manifest](../../t/plugin/ai-prompt-guard.yaml) |
| `ai-prompt-template` | [TestHandlerRendersSelectedPromptTemplate](../../pkg/plugin/ai_prompt_template/plugin_test.go#L37) | [TestHandlerRejectsMissingTemplateName](../../pkg/plugin/ai_prompt_template/plugin_test.go#L172) | [Manifest](../../t/plugin/ai-prompt-template.yaml) |
| `ai-proxy` | [TestHandlerProxiesOpenAICompatibleChatRequest](../../pkg/plugin/ai_proxy/plugin_test.go#L47) | [TestHandlerLeavesUpstreamResponseVarsUnsetWhenProviderRequestFails](../../pkg/plugin/ai_proxy/plugin_test.go#L640) | [Manifest](../../t/plugin/ai-proxy.yaml) |
| `ai-proxy-multi` | [TestHandlerRoundRobinBalancesAcrossInstances](../../pkg/plugin/ai_proxy_multi/plugin_test.go#L327) | [TestHandlerUsesDefaultPriorityWhenAllHealthChecksFail](../../pkg/plugin/ai_proxy_multi/plugin_test.go#L1084) | [AI proxy manifest](../../t/plugin/ai-proxy.yaml) (`ai-proxy-multi-retry-fast-failure`, `ai-proxy-multi-openai-compatible-stream`). |
| `ai-rag` | [TestHandlerRunsAzureRAGAndAppendsSearchResultToChat](../../pkg/plugin/ai_rag/plugin_test.go#L40) | [TestHandlerRejectsInvalidRAGRequestsWithSourceDiagnostics](../../pkg/plugin/ai_rag/plugin_test.go#L283) | [Manifest](../../t/plugin/ai-rag.yaml) |
| `ai-rate-limiting` | [TestHandlerReportsAPISIX317PreChargeSnapshots](../../pkg/plugin/ai_rate_limiting/plugin_test.go#L240) | [TestHandlerChargesTotalTokensAndRejectsNextRequest](../../pkg/plugin/ai_rate_limiting/plugin_test.go#L41) | [Manifest](../../t/plugin/ai-rate-limiting.yaml) |
| `ai-request-rewrite` | [TestHandlerRewritesRequestWithOpenAICompatibleProvider](../../pkg/plugin/ai_request_rewrite/plugin_test.go#L33) | [TestHandlerRejectsMissingRequestBody](../../pkg/plugin/ai_request_rewrite/plugin_test.go#L363) | [Manifest](../../t/plugin/ai-request-rewrite.yaml) |
| `api-breaker` | [TestHandlerResolvesBreakResponseHeaders](../../pkg/plugin/api_breaker/plugin_test.go#L51) | [TestHandlerHealthySuccessesClearAccumulatedFailures](../../pkg/plugin/api_breaker/plugin_test.go#L103) | [Manifest](../../t/plugin/api-breaker.yaml) |
| `attach-consumer-label` | [TestHandlerAttachesConfiguredConsumerLabels](../../pkg/plugin/attach_consumer_label/plugin_test.go#L61) | [TestHandlerPassesThroughWithoutAuthenticatedConsumer](../../pkg/plugin/attach_consumer_label/plugin_test.go#L94) | [Manifest](../../t/plugin/attach-consumer-label.yaml) |
| `authz-casbin` | [TestHandlerAllowsRequestWhenPolicyMatchesHeaderUser](../../pkg/plugin/authz_casbin/plugin_test.go#L231) | [TestHandlerRejectsRequestWhenPolicyDoesNotMatch](../../pkg/plugin/authz_casbin/plugin_test.go#L256) | [Manifest](../../t/plugin/authz-casbin.yaml) |
| `authz-casdoor` | [TestCallbackFetchesAccessTokenAndRedirectsOriginalURI](../../pkg/plugin/authz_casdoor/plugin_test.go#L504) | [TestCallbackRejectsInvalidState](../../pkg/plugin/authz_casdoor/plugin_test.go#L1024) | [Manifest](../../t/plugin/authz-casdoor.yaml) |
| `authz-keycloak` | [TestHandlerPostsUMADecisionWithStaticPermissions](../../pkg/plugin/authz_keycloak/plugin_test.go#L111) | [TestHandlerEnforcingEmptyPermissionsReturnsKeycloakAccessDenied](../../pkg/plugin/authz_keycloak/plugin_test.go#L173) | [Manifest](../../t/plugin/authz-keycloak.yaml) |
| `aws-lambda` | [TestHandlerInvokesAWSLambdaWithAPIKey](../../pkg/plugin/aws_lambda/plugin_test.go#L48) | [TestAWSLambdaStopRetiresScopedAndLegacyCredentialsFailClosed](../../pkg/plugin/aws_lambda/plugin_test.go#L418) | [Manifest](../../t/plugin/aws-lambda.yaml) |
| `azure-functions` | [TestHandlerInvokesAzureFunctionAndRelaysResponse](../../pkg/plugin/azure_functions/plugin_test.go#L185) | [TestAzureRouteMaterializationFailsBeforePostInit](../../pkg/plugin/azure_functions/plugin_test.go#L598) | [Manifest](../../t/plugin/azure-functions.yaml) |
| `basic-auth` | [TestHandlerAcceptsBasicAuthAndAttachesConsumer](../../pkg/plugin/basic_auth/plugin_test.go#L149) | [TestHandlerRecordsMissingConsumerProbeDiagnostic](../../pkg/plugin/basic_auth/plugin_test.go#L341) | [Manifest](../../t/plugin/basic-auth.yaml) |
| `batch-requests` | [TestHandlerAuthenticatesWithPipelineCredentialHeaders](../../pkg/plugin/batch_requests/plugin_test.go#L1118) | [TestHandlerRejectsNestedBatchBeforeConcurrencyLease](../../pkg/plugin/batch_requests/plugin_test.go#L95) | [Manifest](../../t/plugin/batch-requests.yaml) |
| `body-transformer` | [TestHandlerTransformsJSONRequestBody](../../pkg/plugin/body_transformer/plugin_test.go#L105) | [TestHandlerRejectsUnsupportedTemplateDirective](../../pkg/plugin/body_transformer/plugin_test.go#L395) | [Manifest](../../t/plugin/body-transformer.yaml) |
| `brotli` | [TestHandlerMatchesAPISIX317ETagRules](../../pkg/plugin/brotli/plugin_test.go#L279) | [TestBrotliHandlerStopsAfterDirectHijack](../../pkg/plugin/brotli/plugin_test.go#L496) | [Manifest](../../t/plugin/brotli.yaml) |
| `cas-auth` | [TestCallbackValidatesTicketAndCreatesSession](../../pkg/plugin/cas_auth/plugin_test.go#L477) | [TestCASScopedStopWaitsForInFlightCallback](../../pkg/plugin/cas_auth/plugin_test.go#L363) | [Manifest](../../t/plugin/cas-auth.yaml) |
| `chaitin-waf` | [TestHandlerSendsBoundedBodyToWAFAndPreservesFullBodyForUpstream](../../pkg/plugin/chaitin_waf/plugin_test.go#L111) | [TestHandlerSendsNoBodyToWAFForNegativeInspectionLimit](../../pkg/plugin/chaitin_waf/plugin_test.go#L150) | [Manifest](../../t/plugin/chaitin-waf.yaml) |
| `clickhouse-logger` | [TestSendBatchDeliversToEachConfiguredEndpoint](../../pkg/plugin/clickhouse_logger/plugin_test.go#L486) | [TestEffectiveLogFormatRejectsEmptyBeforeSideEffects](../../pkg/plugin/clickhouse_logger/plugin_test.go#L175) | [Manifest](../../t/plugin/clickhouse-logger.yaml) |
| `client-control` | [TestClientControlMaxBytesAtBoundary](../../pkg/plugin/client_control/plugin_test.go#L73) | [TestClientControlMaxBytesOneByteOver](../../pkg/plugin/client_control/plugin_test.go#L91) | [Manifest](../../t/plugin/client-control.yaml) |
| `consumer-restriction` | [TestConsumerGroupRestrictionUsesAttachedConsumerGroupID](../../pkg/plugin/consumer_restriction/plugin_test.go#L94) | [TestMissingConsumerReturnsOfficialMessage](../../pkg/plugin/consumer_restriction/plugin_test.go#L14) | [Manifest](../../t/plugin/consumer-restriction.yaml) |
| `cors` | [TestHandlerRegexOriginsRestrictDefaultWildcard](../../pkg/plugin/cors/plugin_test.go#L399) | [TestRunRequestPhaseStopsCORSPreflight](../../pkg/plugin/cors/plugin_test.go#L782) | [Manifest](../../t/plugin/cors.yaml) |
| `csrf` | [TestHandlerValidPostRefreshesCookie](../../pkg/plugin/csrf/plugin_test.go#L763) | [TestHandlerRejectsInvalidRequestsWithJSONErrors](../../pkg/plugin/csrf/plugin_test.go#L794) | [Manifest](../../t/plugin/csrf.yaml) |
| `data-mask` | [TestHandlerPreservesOriginalBodyOwnership](../../pkg/plugin/data_mask/plugin_test.go#L989) | [TestLogSnapshotSanitizerSkipsWhenURLEncodedBodyExceedsArgumentLimit](../../pkg/plugin/data_mask/plugin_test.go#L143) | [Manifest](../../t/plugin/data-mask.yaml) |
| `datadog` | [TestSendWritesOneDatagramPerMetric](../../pkg/plugin/datadog/plugin_test.go#L593) | [TestWatchConnectionCancellation](../../pkg/plugin/datadog/plugin_test.go#L42) | [Manifest](../../t/plugin/datadog.yaml) |
| `degraphql` | [TestHandlerRewritesPOSTBodyToGraphQLRequest](../../pkg/plugin/degraphql/plugin_test.go#L26) | [TestHandlerRejectsUnsupportedMethods](../../pkg/plugin/degraphql/plugin_test.go#L103) | [Manifest](../../t/plugin/degraphql.yaml) |
| `dingtalk-auth` | [TestHandlerAcceptsQueryCodeWithoutLocalOAuthState](../../pkg/plugin/dingtalk_auth/plugin_test.go#L611) | [TestHandlerRejectsInvalidDingTalkCode](../../pkg/plugin/dingtalk_auth/plugin_test.go#L803) | [Manifest](../../t/plugin/dingtalk-auth.yaml) |
| `dubbo-proxy` | [TestHandlerStoresDubboProxyMetadata](../../pkg/plugin/dubbo_proxy/plugin_test.go#L88) | [TestServeDubboWithRetriesRetriesConnectFailure](../../pkg/plugin/dubbo_proxy/transport_test.go#L156) | Dubbo wire transport fixtures in unit tests; no dedicated YAML manifest. |
| `echo` | [TestHandlerReplacesResponseBody](../../pkg/plugin/echo/plugin_test.go#L122) | [TestHandlerBodyReplacementInvalidatesRepresentationHeaders](../../pkg/plugin/echo/plugin_test.go#L144) | [Manifest](../../t/plugin/echo.yaml) |
| `elasticsearch-logger` | [TestSendBatchWritesBulkNDJSONWithHeadersAndAuth](../../pkg/plugin/elasticsearch_logger/plugin_test.go#L1340) | [TestStopDrainsActiveScopedElasticsearchSend](../../pkg/plugin/elasticsearch_logger/plugin_test.go#L746) | [Manifest](../../t/plugin/elasticsearch-logger.yaml) |
| `error-log-logger` | [TestSendLogsFiltersByLevelAndWritesTCP](../../pkg/plugin/error_log_logger/plugin_test.go#L1285) | [TestStopUnregistersObserverAndClosesKafkaWriterOnce](../../pkg/plugin/error_log_logger/plugin_test.go#L1848) | [Manifest](../../t/plugin/error-log-logger.yaml) |
| `error-page` | [TestHandlerKeepsSuccessfulResponses](../../pkg/plugin/error_page/plugin_test.go#L229) | [TestHandlerRewritesConfiguredErrorPage](../../pkg/plugin/error_page/plugin_test.go#L157) | [Manifest](../../t/plugin/error-page.yaml) |
| `example-plugin` | [TestHandlerPassesThrough](../../pkg/plugin/example_plugin/plugin_test.go#L34) | [TestHandlerSetsUpstreamOverrideWhenIPConfigured](../../pkg/plugin/example_plugin/plugin_test.go#L51) | [Manifest](../../t/plugin/example-plugin.yaml) |
| `exit-transformer` | [TestHandlerNilBodyPreservesRepresentationHeaders](../../pkg/plugin/exit_transformer/plugin_test.go#L350) | [TestExitTransformerLuaStatusKeepsPreviousResponseStatusForInvalidValues](../../pkg/plugin/exit_transformer/plugin_test.go#L278) | [Manifest](../../t/plugin/exit-transformer.yaml) |
| `fault-injection` | [TestAbortVarsMustMatch](../../pkg/plugin/fault_injection/plugin_test.go#L117) | [TestRunRequestPhasePublishesEarlyStopSourceOnAbort](../../pkg/plugin/fault_injection/plugin_test.go#L58) | [Manifest](../../t/plugin/fault-injection.yaml) |
| `feishu-auth` | [TestHandlerAcceptsQueryCodeWithoutLocalOAuthState](../../pkg/plugin/feishu_auth/plugin_test.go#L792) | [TestHandlerRejectsInvalidFeishuCode](../../pkg/plugin/feishu_auth/plugin_test.go#L976) | [Manifest](../../t/plugin/feishu-auth.yaml) |
| `file-logger` | [TestRunLogPhaseWritesLogWhenMatchPasses](../../pkg/plugin/file_logger/plugin_test.go#L85) | [TestBufferedWriterRecoversAfterTransientSyncFailure](../../pkg/plugin/file_logger/plugin_test.go#L257) | [Manifest](../../t/plugin/file-logger.yaml) |
| `forward-auth` | [TestHandlerForwardsOriginalRequestURIAfterPathRewrite](../../pkg/plugin/forward_auth/plugin_test.go#L118) | [TestHandlerRejectsAndCopiesClientHeaders](../../pkg/plugin/forward_auth/plugin_test.go#L215) | [Manifest](../../t/plugin/forward-auth.yaml) |
| `gm` | [TestHandlerPassesThrough](../../pkg/plugin/gm/plugin_test.go#L23) | [TestValidateSSLConfigRejectsWrongGMSignPairCount](../../pkg/plugin/gm/plugin_test.go#L85) | Certificate field validation only; native GM/NTLS handshake is excluded. |
| `google-cloud-logging` | [TestSendBatchExchangesTokenAndWritesEntries](../../pkg/plugin/google_cloud_logging/plugin_test.go#L1117) | [TestSendBatchCancelsGoogleEntriesPostWithContext](../../pkg/plugin/google_cloud_logging/plugin_test.go#L337) | [Manifest](../../t/plugin/google-cloud-logging.yaml) |
| `graphql-limit-count` | [TestHandlerAcceptsApplicationGraphQLBody](../../pkg/plugin/graphql_limit_count/plugin_test.go#L481) | [TestHandlerUsesRedisLimiterDepthCost](../../pkg/plugin/graphql_limit_count/plugin_test.go#L296) | [Manifest](../../t/plugin/graphql-limit-count.yaml) |
| `graphql-proxy-cache` | [TestHandlerCachesGraphQLPOSTResponses](../../pkg/plugin/graphql_proxy_cache/plugin_test.go#L930) | [TestHandlerBypassesMutationOperations](../../pkg/plugin/graphql_proxy_cache/plugin_test.go#L1086) | [Manifest](../../t/plugin/graphql-proxy-cache.yaml) |
| `grpc-transcode` | [TestHandlerTranscodesGETRequestAndResponse](../../pkg/plugin/grpc_transcode/plugin_test.go#L58) | [TestHandlerAcceptsEnumNameAndNumberAndRejectsUnknownName](../../pkg/plugin/grpc_transcode/plugin_test.go#L289) | [Batch manifest](../../t/plugin/batch-requests.yaml) (`grpc-transcode-direct-and-batch`); in-process gRPC server tests. |
| `grpc-web` | [TestHandlerTransformsTextRequestAndResponse](../../pkg/plugin/grpc_web/plugin_test.go#L107) | [TestHandlerRejectsInvalidRequest](../../pkg/plugin/grpc_web/plugin_test.go#L60) | Streaming executor and gRPC framing tests in the unit package; no dedicated YAML manifest. |
| `gzip` | [TestHandlerCompressesMultipleWritesOnce](../../pkg/plugin/gzip/plugin_test.go#L178) | [TestCompressionInvalidatesBodyDerivedHeaders](../../pkg/plugin/gzip/plugin_test.go#L371) | [Manifest](../../t/plugin/gzip.yaml) |
| `hmac-auth` | [TestHandlerAcceptsSignedDateAndAttachesConsumer](../../pkg/plugin/hmac_auth/plugin_test.go#L94) | [TestHandlerRecordsProbeAndMissingAnonymousDiagnostics](../../pkg/plugin/hmac_auth/plugin_test.go#L120) | [Manifest](../../t/plugin/hmac-auth.yaml) |
| `http-dubbo` | [TestServeDubboReturnsBodyForApplicationResponse](../../pkg/plugin/http_dubbo/plugin_test.go#L210) | [TestServeDubboReturnsInternalServerErrorForUnexpectedResponseStatus](../../pkg/plugin/http_dubbo/plugin_test.go#L269) | [Manifest](../../t/plugin/http-dubbo.yaml) |
| `http-logger` | [TestRunLogPhasePreservesDefaultFieldsAndRouteLabels](../../pkg/plugin/http_logger/plugin_test.go#L642) | [TestStopFlushesBufferedEntriesBeforeRetiringDelivery](../../pkg/plugin/http_logger/plugin_test.go#L68) | [Manifest](../../t/plugin/http-logger.yaml) |
| `ip-restriction` | [TestBlacklistUsesConfiguredResponseCode](../../pkg/plugin/ip_restriction/plugin_test.go#L54) | [TestWhitelistRejectsWithJSONMessage](../../pkg/plugin/ip_restriction/plugin_test.go#L13) | [Manifest](../../t/plugin/ip-restriction.yaml) |
| `jwe-decrypt` | [TestHandlerDecryptsBearerJWEAndForwardsPlaintext](../../pkg/plugin/jwe_decrypt/plugin_test.go#L133) | [TestHandlerRejectsMissingTokenWhenStrict](../../pkg/plugin/jwe_decrypt/plugin_test.go#L321) | [Manifest](../../t/plugin/jwe-decrypt.yaml) |
| `jwt-auth` | [TestHandlerAcceptsBearerTokenAndAttachesConsumer](../../pkg/plugin/jwt_auth/plugin_test.go#L330) | [TestHandlerRejectsMissingToken](../../pkg/plugin/jwt_auth/plugin_test.go#L359) | [Manifest](../../t/plugin/jwt-auth.yaml) |
| `kafka-logger` | [TestSendBatchEncodesLogAndPublishesToConfiguredTopic](../../pkg/plugin/kafka_logger/plugin_test.go#L894) | [TestKafkaStopDrainsActiveSendAndPreventsResurrection](../../pkg/plugin/kafka_logger/plugin_test.go#L485) | [Manifest](../../t/plugin/kafka-logger.yaml) |
| `kafka-proxy` | [TestHandlerStoresSASLConfigForKafkaUpstream](../../pkg/plugin/kafka_proxy/plugin_test.go#L677) | [TestKafkaProxyHandlerClearsRetainedRequestPasswordAfterPanic](../../pkg/plugin/kafka_proxy/plugin_test.go#L262) | [Manifest](../../t/plugin/kafka-proxy.yaml) |
| `key-auth` | [TestHandlerAcceptsHeaderKeyAndAttachesConsumer](../../pkg/plugin/key_auth/plugin_test.go#L170) | [TestHandlerRejectsEmptyHeaderAndDoesNotFallThroughToQuery](../../pkg/plugin/key_auth/plugin_test.go#L152) | [Manifest](../../t/plugin/key-auth.yaml) |
| `lago` | [TestRunLogPhasePreservesLagoTemplateFieldsAndBodies](../../pkg/plugin/lago/plugin_test.go#L567) | [TestLagoStopDrainsActiveSendAndPreventsResurrection](../../pkg/plugin/lago/plugin_test.go#L374) | [Manifest](../../t/plugin/lago.yaml) |
| `ldap-auth` | [TestHandlerPreservesAuthorizationHeaderByDefault](../../pkg/plugin/ldap_auth/plugin_test.go#L375) | [TestHandlerRejectsMissingAuthorization](../../pkg/plugin/ldap_auth/plugin_test.go#L419) | [Manifest](../../t/plugin/ldap-auth.yaml) |
| `limit-conn` | [TestRequestPhaseLimitConnReleasesAfterNormalCompletion](../../pkg/plugin/limit_conn/plugin_test.go#L1474) | [TestHandlerRejectsConcurrentRequestsAboveConnAndBurst](../../pkg/plugin/limit_conn/plugin_test.go#L320) | [Manifest](../../t/plugin/limit-conn.yaml) |
| `limit-count` | [TestHandlerResolvesRuleKeyDefaultValue](../../pkg/plugin/limit_count/plugin_test.go#L804) | [TestRunRequestPhasePublishesRejectedResponseAsEarlyStop](../../pkg/plugin/limit_count/plugin_test.go#L496) | [Manifest](../../t/plugin/limit-count.yaml) |
| `limit-req` | [TestHandlerTracksSeparateKeys](../../pkg/plugin/limit_req/plugin_test.go#L571) | [TestHandlerRejectsRequestsAboveRateAndBurst](../../pkg/plugin/limit_req/plugin_test.go#L521) | [Manifest](../../t/plugin/limit-req.yaml) |
| `log-rotate` | [TestRotateByMaxSizeRenamesLogsAndRecreatesCurrentFiles](../../pkg/plugin/log_rotate/plugin_test.go#L354) | [TestRotationPanicReportsPluginOwner](../../pkg/plugin/log_rotate/plugin_test.go#L228) | [Manifest](../../t/plugin/log-rotate.yaml) |
| `loggly` | [TestSendWritesUDPMessage](../../pkg/plugin/loggly/plugin_test.go#L914) | [TestSendBatchLogglyUDPDiscardsConnectionCanceledAfterWrite](../../pkg/plugin/loggly/plugin_test.go#L1519) | [Manifest](../../t/plugin/loggly.yaml) |
| `loki-logger` | [TestRunLogPhasePreservesLokiEnvelopeLabelsAndTimestamp](../../pkg/plugin/loki_logger/plugin_test.go#L24) | [TestResolveLabelsLeavesMissingDynamicValuesEmpty](../../pkg/plugin/loki_logger/plugin_test.go#L309) | [Manifest](../../t/plugin/loki-logger.yaml) |
| `mcp-bridge` | [TestSSEStartsProcessAndAdvertisesMessageEndpoint](../../pkg/plugin/mcp_bridge/plugin_test.go#L127) | [TestSSECommandStartFailureReturns500WithoutPublishingSession](../../pkg/plugin/mcp_bridge/plugin_test.go#L179) | Owned child-process/SSE fixtures in unit tests; no dedicated YAML manifest. |
| `mocking` | [TestHandlerMatchesAPISIX317TextPlainCharset](../../pkg/plugin/mocking/plugin_test.go#L236) | [TestRunRequestPhasePublishesEarlyStopSource](../../pkg/plugin/mocking/plugin_test.go#L253) | [Manifest](../../t/plugin/mocking.yaml) |
| `multi-auth` | [TestHandlerLeavesSuccessfulHMACBodyOwnedByServer](../../pkg/plugin/multi_auth/plugin_test.go#L549) | [TestHandlerPreservesRejectingConsumerPluginResponse](../../pkg/plugin/multi_auth/plugin_test.go#L315) | [Manifest](../../t/plugin/multi-auth.yaml) |
| `node-status` | [TestTrackReportsServerWideRequestCounters](../../pkg/plugin/node_status/plugin_test.go#L14) | [TestTrackConcurrentIncrementDecrement](../../pkg/plugin/node_status/plugin_test.go#L78) | [Manifest](../../t/plugin/node-status.yaml) |
| `oas-validator` | [TestHandlerMatchesOpenAPIServerURLPrefix](../../pkg/plugin/oas_validator/plugin_test.go#L276) | [TestHandlerRejectsMalformedYAMLBody](../../pkg/plugin/oas_validator/plugin_test.go#L726) | [Manifest](../../t/plugin/oas-validator.yaml) |
| `opa` | [TestHandlerReturnsOPAExtendedFinalStatus](../../pkg/plugin/opa/plugin_test.go#L485) | [TestHandlerRejectsWithOPAStatusReasonAndHeaders](../../pkg/plugin/opa/plugin_test.go#L454) | [Manifest](../../t/plugin/opa.yaml) |
| `openfunction` | [TestHandlerInvokesOpenFunctionWithBasicAuthorization](../../pkg/plugin/openfunction/plugin_test.go#L53) | [TestOpenFunctionScopedTokenUseAndStopDoNotRace](../../pkg/plugin/openfunction/plugin_test.go#L339) | [Manifest](../../t/plugin/openfunction.yaml) |
| `openid-connect` | [TestHandlerAcceptsXAccessTokenAsBearerInput](../../pkg/plugin/openid_connect/plugin_test.go#L372) | [TestHandlerRejectsMissingRequiredScope](../../pkg/plugin/openid_connect/plugin_test.go#L543) | [Manifest](../../t/plugin/openid-connect.yaml) |
| `opentelemetry` | [TestOTelHandlerPreservesResponseWriterCapabilities](../../pkg/plugin/otel/plugin_test.go#L260) | [TestTraceUsesRoutePatternNameAndServerErrorStatus](../../pkg/plugin/otel/plugin_test.go#L1100) | [Manifest](../../t/plugin/opentelemetry.yaml) |
| `openwhisk` | [TestHandlerInvokesOpenWhiskActionAndUsesJSONResult](../../pkg/plugin/openwhisk/plugin_test.go#L91) | [TestHandlerReturnsServiceUnavailableForInvalidOpenWhiskJSON](../../pkg/plugin/openwhisk/plugin_test.go#L174) | [Manifest](../../t/plugin/openwhisk.yaml) |
| `prometheus` | [TestHandlerPassesThrough](../../pkg/plugin/prometheus/plugin_test.go#L34) | [TestSchemaAcceptsOfficialConfigAndRejectsInvalidPreferName](../../pkg/plugin/prometheus/plugin_test.go#L18) | [Manifest](../../t/plugin/prometheus.yaml) |
| `proxy-buffering` | [TestHandlerSetsDisableProxyBufferingContext](../../pkg/plugin/proxy_buffering/plugin_test.go#L33) | [TestHandlerSetsDisableProxyBufferingContext](../../pkg/plugin/proxy_buffering/plugin_test.go#L33) | [Manifest](../../t/plugin/proxy-buffering.yaml) |
| `proxy-cache` | [TestHandlerCachesSuccessfulGETResponses](../../pkg/plugin/proxy_cache/plugin_test.go#L414) | [TestHandlerIsolatesCacheByConsumerByDefault](../../pkg/plugin/proxy_cache/plugin_test.go#L1436) | [Manifest](../../t/plugin/proxy-cache.yaml) |
| `proxy-control` | [TestPostInitDefaultsRequestBufferingToTrue](../../pkg/plugin/proxy_control/plugin_test.go#L24) | [TestHandlerSetsRequestBufferingContext](../../pkg/plugin/proxy_control/plugin_test.go#L32) | [Manifest](../../t/plugin/proxy-control.yaml) |
| `proxy-mirror` | [TestHandlerMirrorsMaterializedUpstreamHostAndPreservesUpstreamBody](../../pkg/plugin/proxy_mirror/plugin_test.go#L262) | [TestConfiguredResolverTimeoutBoundsLookup](../../pkg/plugin/proxy_mirror/plugin_test.go#L451) | [Manifest](../../t/plugin/proxy-mirror.yaml) |
| `proxy-rewrite` | [TestHandlerPreservesAndMergesQueryForConfiguredURI](../../pkg/plugin/proxy_rewrite/plugin_test.go#L100) | [TestPostInitRejectsOddRegexURI](../../pkg/plugin/proxy_rewrite/plugin_test.go#L407) | [Manifest](../../t/plugin/proxy-rewrite.yaml) |
| `public-api` | [TestPublicAPIHandlerDispatch](../../pkg/plugin/public_api/plugin_test.go#L81) | [TestMissingPublicAPIReturnsDefaultHTMLPage](../../pkg/plugin/public_api/missing_api_regression_test.go#L9) | [Manifest](../../t/plugin/public-api.yaml) |
| `real-ip` | [TestXForwardedForWithoutTrustedAddressesOverridesRemoteAddress](../../pkg/plugin/real_ip/plugin_test.go#L39) | [TestPostInitRejectsInvalidTrustedAddress](../../pkg/plugin/real_ip/plugin_test.go#L172) | [Manifest](../../t/plugin/real-ip.yaml) |
| `redirect` | [TestHandlerPreservesRedirectDollarEscapes](../../pkg/plugin/redirect/plugin_test.go#L230) | [TestRunRequestPhaseStopsWithEarlyStopSource](../../pkg/plugin/redirect/request_phase_test.go#L12) | [Manifest](../../t/plugin/redirect.yaml) |
| `referer-restriction` | [TestLeadingStarPreservesAPISIXSuffixMatch](../../pkg/plugin/referer_restriction/plugin_test.go#L11) | [TestWhitelistRejectsWithAPISIX317JSONMessageResponse](../../pkg/plugin/referer_restriction/plugin_test.go#L25) | [Manifest](../../t/plugin/referer-restriction.yaml) |
| `request-id` | [TestHandlerPreservesIncomingRequestID](../../pkg/plugin/request_id/plugin_test.go#L164) | [TestKSUIDIDReturnsErrorForFailingReader](../../pkg/plugin/request_id/plugin_test.go#L26) | [Manifest](../../t/plugin/request-id.yaml) |
| `request-validation` | [TestHandlerAcceptsScalarJSONBody](../../pkg/plugin/request_validation/plugin_test.go#L85) | [TestHandlerRejectsInvalidHeaders](../../pkg/plugin/request_validation/plugin_test.go#L28) | [Manifest](../../t/plugin/request-validation.yaml) |
| `response-rewrite` | [TestResponseRewritePreservesInnerBodyAcrossTransformPipeline](../../pkg/plugin/response_rewrite/plugin_test.go#L276) | [TestResponseBodyRotationDoesNotCrossGenerationsAndStopIsRepeatable](../../pkg/plugin/response_rewrite/plugin_test.go#L509) | [Manifest](../../t/plugin/response-rewrite.yaml) |
| `rocketmq-logger` | [TestRunLogPhaseOriginPreservesHTTPFraming](../../pkg/plugin/rocketmq_logger/plugin_test.go#L1181) | [TestRocketMQGenerationCancellationFlushesBeforeSenderShutdown](../../pkg/plugin/rocketmq_logger/plugin_test.go#L694) | [Manifest](../../t/plugin/rocketmq-logger.yaml) |
| `saml-auth` | [TestSignedLogoutRequestClearsSessionAndReturnsCorrelatedResponse](../../pkg/plugin/saml_auth/plugin_test.go#L675) | [TestInvalidSAMLResponseIsRejected](../../pkg/plugin/saml_auth/plugin_test.go#L907) | [Manifest](../../t/plugin/saml-auth.yaml) |
| `server-info` | [TestInfoHandlerWritesJSON](../../pkg/plugin/server_info/plugin_test.go#L86) | [TestReportTTLReadsAndBoundsPluginAttribute](../../pkg/plugin/server_info/plugin_test.go#L14) | [Etcd reporter lifecycle tests](../../pkg/etcd/server_info_test.go); no dedicated YAML manifest. |
| `serverless-post-function` | [TestPostFunctionCanRewriteBodyFilterJSONBody](../../pkg/plugin/serverless/plugin_test.go#L534) | [TestRequestCancellationPreemptsServerlessHardDeadline](../../pkg/plugin/serverless/plugin_test.go#L105) | [Error-page composition manifest](../../t/plugin/error-page.yaml); bounded Lua response/phase tests. |
| `serverless-pre-function` | [TestPreFunctionCanSetRequestHeaderAndContinue](../../pkg/plugin/serverless/plugin_test.go#L485) | [TestRequestCancellationPreemptsServerlessHardDeadline](../../pkg/plugin/serverless/plugin_test.go#L105) | [ACL composition manifest](../../t/plugin/acl.yaml); bounded Lua request/phase tests. |
| `skywalking` | [TestHandlerInjectsSW8AndReportsSegment](../../pkg/plugin/skywalking/plugin_test.go#L322) | [TestHandlerFailsClosedWhenSampleRandomUnavailable](../../pkg/plugin/skywalking/plugin_test.go#L78) | [Manifest](../../t/plugin/skywalking.yaml) |
| `skywalking-logger` | [TestSendBatchPostsSkyWalkingEntries](../../pkg/plugin/skywalking_logger/plugin_test.go#L429) | [TestMetadataDecodeFailsBeforeSkyWalkingClientAndProcessorAcquisition](../../pkg/plugin/skywalking_logger/plugin_test.go#L144) | [Manifest](../../t/plugin/skywalking-logger.yaml) |
| `sls-logger` | [TestSendMessageReturnsWithinWriteDeadline](../../pkg/plugin/sls_logger/plugin_test.go#L574) | [TestWatchConnectionCancellation](../../pkg/plugin/sls_logger/plugin_test.go#L63) | [Manifest](../../t/plugin/sls-logger.yaml) |
| `splunk-hec-logging` | [TestRunLogPhasePreservesSplunkDefaultEventFields](../../pkg/plugin/splunk_hec_logging/plugin_test.go#L547) | [TestSplunkRejectsPrePostInitLogEnqueue](../../pkg/plugin/splunk_hec_logging/plugin_test.go#L436) | [Manifest](../../t/plugin/splunk-hec-logging.yaml) |
| `syslog` | [TestRunLogPhaseWritesUDPMessage](../../pkg/plugin/syslog/plugin_test.go#L327) | [TestTransportFlushLimitTriggersImmediateWrite](../../pkg/plugin/syslog/transport_test.go#L132) | [Manifest](../../t/plugin/syslog.yaml) |
| `tcp-logger` | [TestSendBatchWritesTCPMessage](../../pkg/plugin/tcp_logger/plugin_test.go#L265) | [TestWatchConnectionCancellation](../../pkg/plugin/tcp_logger/plugin_test.go#L88) | [Manifest](../../t/plugin/tcp-logger.yaml) |
| `tencent-cloud-cls` | [TestSendBatchPostsCLSProtobufPayload](../../pkg/plugin/tencent_cloud_cls/plugin_test.go#L565) | [TestBuildBatchPayloadReportsOverLimitEntryDrops](../../pkg/plugin/tencent_cloud_cls/plugin_test.go#L1423) | [Manifest](../../t/plugin/tencent-cloud-cls.yaml) |
| `traffic-label` | [TestHandlerSetsHeadersForFirstMatchingRule](../../pkg/plugin/traffic_label/plugin_test.go#L27) | [TestPostInitRejectsInvalidMatchExpression](../../pkg/plugin/traffic_label/plugin_test.go#L282) | [Manifest](../../t/plugin/traffic-label.yaml) |
| `traffic-split` | [TestHandlerMatchesFormPostArgumentWithoutConsumingBody](../../pkg/plugin/traffic_split/plugin_test.go#L743) | [TestHandlerDoesNotCollapsePhaseTimeoutsIntoOverallDeadline](../../pkg/plugin/traffic_split/plugin_test.go#L168) | [Manifest](../../t/plugin/traffic-split.yaml) |
| `ua-restriction` | [TestUserAgentHeaderValuesAreMatchedIndividually](../../pkg/plugin/ua_restriction/plugin_test.go#L150) | [TestAllowlistMissIsRejected](../../pkg/plugin/ua_restriction/plugin_test.go#L132) | [Manifest](../../t/plugin/ua-restriction.yaml) |
| `udp-logger` | [TestSendBatchWritesUDPMessage](../../pkg/plugin/udp_logger/plugin_test.go#L292) | [TestSendBatchReportsWriteFailure](../../pkg/plugin/udp_logger/plugin_test.go#L780) | [Manifest](../../t/plugin/udp-logger.yaml) |
| `uri-blocker` | [TestAllowedURIFallsThrough](../../pkg/plugin/uri_blocker/plugin_test.go#L134) | [TestNormalizedPathCannotBypassAnchoredRule](../../pkg/plugin/uri_blocker/plugin_test.go#L116) | [Manifest](../../t/plugin/uri-blocker.yaml) |
| `wolf-rbac` | [TestRunRequestPhasePublishesAuthenticationStateAndLegacyHandlerCallsNextOnce](../../pkg/plugin/wolf_rbac/request_phase_test.go#L15) | [TestHandlerRejectsMissingAndInvalidToken](../../pkg/plugin/wolf_rbac/plugin_test.go#L530) | [Manifest](../../t/plugin/wolf-rbac.yaml) |
| `workflow` | [TestHandlerReturnsConfiguredStatusForMatchingCase](../../pkg/plugin/workflow/plugin_test.go#L511) | [TestScopedWorkflowHandlerLeaseDefersCloseWithoutBlockingStop](../../pkg/plugin/workflow/plugin_test.go#L930) | [Manifest](../../t/plugin/workflow.yaml) |
| `zipkin` | [TestHandlerInjectsB3AndReportsZipkinSpan](../../pkg/plugin/zipkin/plugin_test.go#L438) | [TestHandlerFailsClosedWhenSampleRandomUnavailable](../../pkg/plugin/zipkin/plugin_test.go#L170) | HTTP collector and tracing-lifecycle fixtures in unit tests; no dedicated YAML manifest. |

## Evidence use and verification boundaries

- A function or case reference identifies assertions to inspect. A passing run
  must additionally identify the exact revision, command, runtime, and result;
  keep that point-in-time evidence in the task/release record.
- For Schema validation, compare admitted schemas and valid/invalid request
  pairs separately. An unsupported schema rejected during admission is a
  different outcome from accepting a schema and forwarding a request that
  violates it. Do not switch the default dialect merely to make admission
  match while losing request-time constraints.
- Run affected package tests and the exact affected YAML cases after a change.
  Follow the repository build/lint requirements. Do not use this index as a
  command to rerun every package or the full YAML suite on every edit.
- [Candidate qualification](http-candidate-qualification.md) separately binds
  the release gates, container identity, and timed soak to one immutable
  candidate. Evidence for a released predecessor is not a passing candidate
  result for new code.
- Completing this functional acceptance stage does not validate an operator's
  deployment, upgrade/rollback procedure, production traffic, or capacity.
