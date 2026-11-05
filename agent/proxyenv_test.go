package agent

import (
	"strings"
	"testing"
)

func TestProxyEnvDirectClearsServerCredentials(t *testing.T) {
	env := proxyEnv("", "")
	for _, want := range []string{"ARTEX_PG_DSN=", "POSTGRES_PASSWORD=", "ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "ARTEX_UPDATE_GITHUB_TOKEN="} {
		found := false
		for _, entry := range env {
			if entry == want {
				found = true
			}
		}
		if !found {
			t.Errorf("direct child environment does not clear %s", want)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "HTTP_PROXY=") {
			t.Fatal("direct connection unexpectedly configured a proxy")
		}
	}
}

func TestProxyEnvSetsAllProxyForSocks5(t *testing.T) {
	// Capture-off egress path: a socks5 proxy, no MITM CA. ALL_PROXY must be set
	// (curl reads socks5 only from there), and no CA vars should appear.
	env := proxyEnv("socks5://10.0.0.1:1080", "")
	has := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "all_proxy="} {
		if !has(want) {
			t.Errorf("proxyEnv missing %s: %v", want, env)
		}
	}
	if has("SSL_CERT_FILE=") || has("CURL_CA_BUNDLE=") {
		t.Errorf("proxyEnv without CA must not inject CA vars: %v", env)
	}
}

func TestProxyEnvInjectsCAWhenRecording(t *testing.T) {
	env := proxyEnv("http://127.0.0.1:8788", "/data/ca.pem")
	has := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"SSL_CERT_FILE=", "CURL_CA_BUNDLE=", "REQUESTS_CA_BUNDLE=", "NODE_EXTRA_CA_CERTS="} {
		if !has(want) {
			t.Errorf("proxyEnv with CA missing %s: %v", want, env)
		}
	}
}
