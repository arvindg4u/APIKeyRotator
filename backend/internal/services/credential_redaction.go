package services

import "strings"

// proxyCredentialHeaders are headers that always belong to the proxy's own
// authentication layer, regardless of their value. They must never be
// forwarded upstream: a client may send a stale key, an admin token, or a
// Bearer-wrapped credential.
//
// Other credential aliases (X-Api-Key, X-Goog-Api-Key, X-Anthropic-Api-Key,
// api-key, Referer, Cookie, custom headers, ...) are handled by the value scan
// below and by the adapters' own filters, so an unrelated upstream header with
// the same name is not stripped accidentally.
var proxyCredentialHeaders = []string{
	"authorization",
	"proxy-authorization",
	"x-proxy-key",
	"x-admin-token",
}

// RedactTargetCredentials removes client-side proxy credentials from a request
// that is about to be forwarded upstream.
//
// Besides the known proxy-auth header names, any header or query parameter
// whose value contains a configured proxy key is dropped. This catches
// credentials in Referer, Cookie, custom headers, and arbitrary query names
// (for example ?foo=proxy-secret) without requiring each adapter to maintain
// its own exhaustive list.
//
// The real upstream key is injected later by InjectUpstreamKey, so removing
// headers such as Authorization / X-Api-Key here does not break authentication.
func RedactTargetCredentials(target *TargetRequest, proxyKeys []string) {
	if target == nil {
		return
	}

	for name, value := range target.Headers {
		if isProxyCredentialHeader(name) || containsProxyKey(value, proxyKeys) {
			delete(target.Headers, name)
		}
	}

	for name, value := range target.Params {
		if containsProxyKey(value, proxyKeys) {
			delete(target.Params, name)
		}
	}
}

func isProxyCredentialHeader(name string) bool {
	for _, credentialHeader := range proxyCredentialHeaders {
		if strings.EqualFold(name, credentialHeader) {
			return true
		}
	}
	return false
}

func containsProxyKey(value string, proxyKeys []string) bool {
	if value == "" {
		return false
	}
	for _, key := range proxyKeys {
		if key != "" && strings.Contains(value, key) {
			return true
		}
	}
	return false
}
