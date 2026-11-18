package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

var invalidProviderURLCases = []struct{ name, base string }{
	{"empty", ""},
	{"relative_host", "fixture.invalid/v1?token=url-secret"},
	{"relative_path", "/v1?token=url-secret"},
	{"network_path", "//fixture.invalid/v1?token=url-secret"},
	{"opaque", "https:fixture.invalid/v1?token=url-secret"},
	{"non_http", "ftp://fixture.invalid/v1?token=url-secret"},
	{"missing_host", "https:///v1?token=url-secret"},
	{"port_only", "https://:443/v1?token=url-secret"},
	{"parse_path", "https://user:url-secret@fixture.invalid/%zz?token=url-secret"},
	{"parse_userinfo", "https://user:url-secret%zz@fixture.invalid/v1"},
	{"parse_port", "https://fixture.invalid:port-secret/v1"},
	{"invalid_literal", "https://[host-secret]/v1"},
	{"unbracketed_ipv6", "https://::1/v1"},
	{"port_overflow", "https://fixture.invalid:65536/v1?token=url-secret"},
	{"control_character", "https://fixture.invalid/v1\nurl-secret"},
}

func requireProviderURLRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid provider URL accepted")
	}
	for _, private := range []string{"secret", "fixture.invalid", "/v1", "%zz"} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("provider validation error exposed endpoint data")
		}
	}
	var parsed *url.Error
	if errors.As(err, &parsed) {
		t.Fatal("provider validation retained a credential-bearing parse error")
	}
}

func TestProviderIdentityPreservesValidEndpointHashes(t *testing.T) {
	for _, tc := range []struct{ name, base, normalized string }{
		{"http", "http://fixture.invalid", "http://fixture.invalid"},
		{"https", "https://fixture.invalid/v1", "https://fixture.invalid/v1"},
		{"credentials", "https://user:url-secret@fixture.invalid/v1?token=url-secret#url-secret", "https://fixture.invalid/v1"},
		{"completion", "https://user:url-secret@fixture.invalid/v1/chat/completions?token=url-secret#url-secret", "https://fixture.invalid/v1"},
		{"loopback", "http://localhost:8080/v1", "http://localhost:8080/v1"},
		{"ipv6", "http://[::1]:8080/v1", "http://[::1]:8080/v1"},
		{"ipv6_zone", "http://[fe80::1%25eth0]:8080/v1", "http://[fe80::1%25eth0]:8080/v1"},
		{"escaped_path", "https://fixture.invalid/%76%31", "https://fixture.invalid/%76%31"},
		{"trailing_slash", "https://fixture.invalid/v1/", "https://fixture.invalid/v1/"},
		{"empty_query", "https://fixture.invalid/v1?", "https://fixture.invalid/v1?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := providerIdentity(tc.base)
			if err != nil || identity != promptHash(tc.normalized) {
				t.Fatalf("valid endpoint hash changed: %q, %v", identity, err)
			}
			p := &OpenAIProvider{BaseURL: tc.base, Model: "fixture"}
			a, err := New(nil, p, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = a.ConfigureModels(map[string]*OpenAIProvider{"named": p}, RoleModels{}); err != nil {
				t.Fatal(err)
			}
			var request ModelRequest
			a.stampRequest(&request)
			if request.ProviderID != identity {
				t.Fatal("request ledger and selection identity differ")
			}
		})
	}
}

func TestProviderIdentityRejectsInvalidURLs(t *testing.T) {
	for _, tc := range invalidProviderURLCases {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := providerIdentity(tc.base)
			requireProviderURLRejected(t, err)
			if identity != "" {
				t.Fatal("invalid URL acquired an identity")
			}
		})
	}
}

func TestProviderConfigurationRejectsInvalidURLsAtomically(t *testing.T) {
	for _, tc := range invalidProviderURLCases {
		t.Run(tc.name, func(t *testing.T) {
			invalid := &OpenAIProvider{BaseURL: tc.base, Model: "fixture"}
			_, err := New(nil, invalid, "")
			requireProviderURLRejected(t, err)

			valid := &OpenAIProvider{BaseURL: "https://fixture.invalid/v1", Model: "fixture"}
			a, err := New(nil, valid, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = a.ConfigureModels(map[string]*OpenAIProvider{"kept": valid}, RoleModels{Worker: &ModelSelection{Provider: "kept", Model: "fixture"}}); err != nil {
				t.Fatal(err)
			}
			beforeDefault, beforeNamed, beforeRole := a.modelProviders["default"], a.modelProviders["kept"], a.roleModels.Worker
			// Unused named providers must be rejected too, before policy publication.
			err = a.ConfigureModels(map[string]*OpenAIProvider{"invalid": invalid}, RoleModels{})
			requireProviderURLRejected(t, err)
			if a.modelProviders["default"] != beforeDefault || a.modelProviders["kept"] != beforeNamed || a.roleModels.Worker != beforeRole || a.modelProviders["invalid"] != nil {
				t.Fatal("failed named-provider configuration changed published routes")
			}
			a.provider = invalid
			requireProviderURLRejected(t, a.ConfigureModels(nil, RoleModels{}))
			if a.modelProviders["default"] != beforeDefault || a.modelProviders["kept"] != beforeNamed || a.roleModels.Worker != beforeRole {
				t.Fatal("failed default-provider configuration changed published routes")
			}
		})
	}
}

func TestResolveModelRejectsInvalidURLs(t *testing.T) {
	for _, tc := range invalidProviderURLCases {
		t.Run(tc.name, func(t *testing.T) {
			p := &OpenAIProvider{BaseURL: tc.base, Model: "fixture"}
			for _, name := range []string{"default", "named"} {
				a := &Agent{provider: p, modelProviders: map[string]*OpenAIProvider{name: p}}
				for _, restoring := range []bool{false, true} {
					// An empty saved identity used to match a failed parse's empty identity.
					selection, provider, err := a.resolveModel(&ModelSelection{Provider: name, Model: "fixture"}, restoring)
					requireProviderURLRejected(t, err)
					if selection != nil || provider != nil {
						t.Fatal("invalid URL returned a usable route")
					}
				}
			}
			fallback := &Agent{provider: p}
			_, _, err := fallback.resolveModel(nil, false)
			requireProviderURLRejected(t, err)
		})
	}
}

func TestProviderIdentityRestoreAndLegacyRun(t *testing.T) {
	p := &OpenAIProvider{BaseURL: "https://fixture.invalid/v1", Model: "fixture"}
	a, err := New(nil, p, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default", "named"} {
		if err = a.ConfigureModels(map[string]*OpenAIProvider{"named": p}, RoleModels{}); err != nil {
			t.Fatal(err)
		}
		selection, _, err := a.resolveModel(&ModelSelection{Provider: name, Model: "fixture"}, false)
		if err != nil || selection == nil || selection.ProviderID == "" {
			t.Fatalf("valid selection failed: %v", err)
		}
		if _, err = a.withRunModel(Run{ModelSelection: selection}); err != nil {
			t.Fatalf("valid saved identity rejected: %v", err)
		}
		for _, id := range []string{"", "changed"} {
			invalid := *selection
			invalid.ProviderID = id
			if _, err = a.withRunModel(Run{ModelSelection: &invalid}); err == nil {
				t.Fatal("empty or changed saved identity accepted")
			}
		}
	}
	legacy, err := a.withRunModel(Run{})
	if err != nil || legacy != a {
		t.Fatal("legacy Run acquired a new model binding")
	}
}

type providerIdentityTransport func(*http.Request) (*http.Response, error)

func (f providerIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProviderRequestRejectsInvalidURLsBeforeTransport(t *testing.T) {
	for _, tc := range invalidProviderURLCases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := &OpenAIProvider{BaseURL: tc.base, Model: "fixture", Client: &http.Client{Transport: providerIdentityTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected transport call")
			})}}
			for _, stream := range []bool{false, true} {
				for _, model := range []string{"", "fixture"} {
					_, err := p.request(context.Background(), ModelRequest{ProviderModel: model}, stream, time.Now(), &ModelDiagnostics{})
					requireProviderURLRejected(t, err)
				}
			}
			if calls != 0 {
				t.Fatal("invalid URL reached transport")
			}
		})
	}
}

func TestProviderRequestValidRoutesAndEmptyArchivedIdentity(t *testing.T) {
	calls := 0
	p := &OpenAIProvider{BaseURL: "https://fixture.invalid/v1", Model: "fixture", Client: &http.Client{Transport: providerIdentityTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://fixture.invalid/v1/chat/completions" {
			t.Error("valid request endpoint changed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	for _, stream := range []bool{false, true} {
		for _, req := range []ModelRequest{{}, {ProviderModel: "fixture", ProviderID: promptHash(p.BaseURL)}} {
			response, err := p.request(context.Background(), req, stream, time.Now(), &ModelDiagnostics{})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		}
		_, err := p.request(context.Background(), ModelRequest{ProviderModel: "fixture"}, stream, time.Now(), &ModelDiagnostics{})
		if err == nil {
			t.Fatal("empty archived identity accepted")
		}
	}
	if calls != 4 {
		t.Fatalf("valid/invalid request admission changed: %d calls", calls)
	}
}
