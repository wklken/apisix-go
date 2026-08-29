# APISIX compatibility contract

apisix-go implements one Apache APISIX 3.17 HTTP data-plane contract. Runtime
configuration does not select a target, security mode, plugin set, or evidence
mode.

The capability manifest records implemented behavior, evidence, known gaps,
and approved Go-native divergences. The differential suite compares the same
runtime that ships to users against the pinned official APISIX oracle. Neither
source changes runtime behavior.

User-visible incompatibilities are projected into the README and generated
plugin status. An APISIX user should need only to review those items, replace
the image, and restart with the existing configuration.
