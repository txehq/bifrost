package bifrost

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/capsohq/bifrost/core/internal/testtime"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateExternalURL(t *testing.T) {
	tests := []struct {
		name                string
		url                 string
		allowPrivateNetwork bool
		wantErr             bool
		errMsg              string
	}{
		// Valid URLs
		{
			name:    "valid https URL",
			url:     "https://api.openai.com",
			wantErr: false,
		},
		{
			name:    "valid http URL",
			url:     "http://api.openai.com",
			wantErr: false,
		},
		{
			name:    "valid https URL with path",
			url:     "https://api.openai.com/v1",
			wantErr: false,
		},
		{
			name:    "valid https URL with port",
			url:     "https://api.openai.com:443",
			wantErr: false,
		},

		// Empty / malformed
		{
			name:    "empty URL",
			url:     "",
			wantErr: true,
			errMsg:  "URL cannot be empty",
		},
		{
			name:    "no scheme",
			url:     "api.openai.com",
			wantErr: true,
			errMsg:  "only https and http schemes are allowed",
		},
		{
			name:    "ftp scheme",
			url:     "ftp://api.openai.com",
			wantErr: true,
			errMsg:  "only https and http schemes are allowed",
		},
		{
			name:    "file scheme",
			url:     "file:///etc/passwd",
			wantErr: true,
			errMsg:  "only https and http schemes are allowed",
		},
		{
			name:    "missing hostname",
			url:     "http://",
			wantErr: true,
			errMsg:  "URL must have a hostname",
		},

		// Localhost / loopback — allowed for local deployments (Ollama, vLLM, SGL)
		{
			name:    "localhost",
			url:     "http://localhost",
			wantErr: false,
		},
		{
			name:    "localhost with port",
			url:     "http://localhost:8080",
			wantErr: false,
		},
		{
			name:    "loopback 127.0.0.1",
			url:     "http://127.0.0.1",
			wantErr: false,
		},
		{
			name:    "loopback ::1",
			url:     "http://[::1]",
			wantErr: false,
		},
		{
			name:    "all-zeros 0.0.0.0",
			url:     "http://0.0.0.0",
			wantErr: true,
			errMsg:  "unspecified IP addresses are not allowed",
		},

		// Private IP ranges (RFC 1918)
		{
			name:    "private 10.x.x.x",
			url:     "http://10.0.0.1",
			wantErr: true,
			errMsg:  "private IP addresses are not allowed",
		},
		{
			name:    "private 172.16.x.x",
			url:     "http://172.16.0.1",
			wantErr: true,
			errMsg:  "private IP addresses are not allowed",
		},
		{
			name:    "private 192.168.x.x",
			url:     "http://192.168.1.1",
			wantErr: true,
			errMsg:  "private IP addresses are not allowed",
		},

		// Link-local / cloud metadata — always blocked even with AllowPrivateNetwork
		{
			name:    "AWS metadata service",
			url:     "http://169.254.169.254",
			wantErr: true,
			errMsg:  "link-local IP addresses are not allowed",
		},
		{
			name:    "AWS metadata with path",
			url:     "http://169.254.169.254/latest/meta-data/",
			wantErr: true,
			errMsg:  "link-local IP addresses are not allowed",
		},
		{
			name:    "link-local 169.254.x.x",
			url:     "http://169.254.1.1",
			wantErr: true,
			errMsg:  "link-local IP addresses are not allowed",
		},

		// Query-parameter injection (the PoC vector from the advisory)
		{
			name:    "query param injection targeting metadata service",
			url:     "http://169.254.169.254/latest/meta-data/iam/security-credentials/role?x=",
			wantErr: true,
			errMsg:  "link-local IP addresses are not allowed",
		},
		{
			name:    "query param injection targeting internal host",
			url:     "http://10.0.0.1/arbitrary/path?x=",
			wantErr: true,
			errMsg:  "private IP addresses are not allowed",
		},

		// AllowPrivateNetwork=true: RFC 1918 allowed, link-local still blocked
		{
			name:                "allow_private_network permits 10.x.x.x",
			url:                 "http://10.0.0.5:8000",
			allowPrivateNetwork: true,
			wantErr:             false,
		},
		{
			name:                "allow_private_network permits 192.168.x.x",
			url:                 "http://192.168.1.50:11434",
			allowPrivateNetwork: true,
			wantErr:             false,
		},
		{
			name:                "allow_private_network still blocks 169.254.169.254",
			url:                 "http://169.254.169.254",
			allowPrivateNetwork: true,
			wantErr:             true,
			errMsg:              "link-local IP addresses are not allowed",
		},
		{
			name:                "allow_private_network still blocks 0.0.0.0",
			url:                 "http://0.0.0.0",
			allowPrivateNetwork: true,
			wantErr:             true,
			errMsg:              "unspecified IP addresses are not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateExternalURL(tt.url, tt.allowPrivateNetwork)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %q", err.Error())
				}
			}
		})
	}
}

// TestPrepareContextForInternalEmbeddingRequestDisablesRawCapture ensures plugin-owned embeddings never inherit provider raw capture.
func TestPrepareContextForInternalEmbeddingRequestDisablesRawCapture(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	PrepareContextForInternalEmbeddingRequest(ctx)

	applyRawCaptureSignals(ctx, &schemas.ProviderConfig{
		SendBackRawRequest:      true,
		SendBackRawResponse:     true,
		StoreRawRequestResponse: true,
	})

	for key, name := range map[schemas.BifrostContextKey]string{
		schemas.BifrostContextKeyCaptureRawRequest:    "capture raw request",
		schemas.BifrostContextKeyCaptureRawResponse:   "capture raw response",
		schemas.BifrostContextKeyShouldStoreRawInLogs: "store raw request/response",
	} {
		if enabled, _ := ctx.Value(key).(bool); enabled {
			t.Errorf("%s = true, want false", name)
		}
	}
}

// TestValidateExternalURLBoundsDNSLookup guards against regressing to the package-level
// net.LookupIP (which has no context/deadline of its own, and previously let a stalled
// resolver block the caller indefinitely -- see externalURLDNSLookupTimeout's doc). A
// hostname under the .invalid TLD (reserved by RFC 2606 to never resolve) still exercises
// the real resolution path; the assertion is on wall-clock bound, not on the specific
// error, since what matters is that resolution can never run unbounded.
func TestValidateExternalURLBoundsDNSLookup(t *testing.T) {
	start := time.Now()
	err := ValidateExternalURL("https://this-host-does-not-exist-12345.invalid", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error resolving a .invalid hostname, got nil")
	}
	// Generous margin above externalURLDNSLookupTimeout (5s): proves resolution is bounded
	// by that timeout rather than by an unrelated, much longer OS/runtime resolver ceiling.
	if elapsed > 8*time.Second {
		t.Errorf("ValidateExternalURL took %v to fail on an unresolvable host, want well under externalURLDNSLookupTimeout + margin", elapsed)
	}
}

// TestValidateExternalURLBoundsInFlightDNSLookup complements
// TestValidateExternalURLBoundsDNSLookup: a .invalid hostname NXDOMAINs almost immediately,
// so it never proves the timeout can cut off a lookup that's actually in flight -- the exact
// scenario externalURLDNSLookupTimeout exists to guard against (a hung/blackholed resolver).
// This substitutes a resolver that blocks until its context is canceled, so the only way
// ValidateExternalURL returns is via externalURLDNSLookupTimeout firing. Runs inside a
// synctest bubble -- context.WithTimeout is explicitly supported there (see the
// testing/synctest package docs' Context.WithTimeout example) -- so the 5s timeout resolves
// against a deterministic fake clock instead of a real 5-second wait.
func TestValidateExternalURLBoundsInFlightDNSLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := lookupIPAddr
		defer func() { lookupIPAddr = original }()

		lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}

		err := ValidateExternalURL("https://api.openai.com", false)
		if err == nil {
			t.Fatal("expected an error when the resolver blocks past the timeout, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected error to wrap context.DeadlineExceeded (proving externalURLDNSLookupTimeout fired), got %v", err)
		}
	})
}

// staticLookuper answers every name with ips, or fails when err is set.
type staticLookuper struct {
	ips []net.IP
	err error
}

func (s staticLookuper) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return s.ips, s.err
}

// TestValidateExternalURLWithResolverUsesTheInjectedResolver pins that the injected
// resolver alone decides the answer ValidateExternalURLWithResolver checks, under the
// same rules as ValidateExternalURL: public and loopback pass, private and link-local
// are refused, and a failed lookup is reported. The names do not exist in real DNS.
func TestValidateExternalURLWithResolverUsesTheInjectedResolver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       staticLookuper
		wantErr string
	}{
		{"public", staticLookuper{ips: []net.IP{net.ParseIP("93.184.216.34")}}, ""},
		{"loopback", staticLookuper{ips: []net.IP{net.ParseIP("127.0.0.1")}}, ""},
		{"private", staticLookuper{ips: []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}}, "private IP addresses are not allowed"},
		{"link-local", staticLookuper{ips: []net.IP{net.ParseIP("169.254.169.254")}}, "link-local IP addresses are not allowed"},
		{"unresolvable", staticLookuper{err: errors.New("no such host")}, "failed to resolve hostname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalURLWithResolver("https://catalog.invalid/pricing.json", false, tc.r)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestResolverLookupIPAddrRespectsContextDeadline pins down the stdlib behavior
// ValidateExternalURL's fix relies on: unlike the package-level net.LookupIP (which
// always uses context.Background() internally and ignores any external deadline),
// net.Resolver.LookupIPAddr honors a context deadline and returns promptly once it
// elapses, even against a real hostname that would otherwise resolve.
func TestResolverLookupIPAddrRespectsContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testtime.Scale(time.Nanosecond))
	defer cancel()
	// Let the deadline definitely elapse before the lookup starts.
	time.Sleep(testtime.Scale(time.Millisecond))

	start := time.Now()
	_, err := (&net.Resolver{}).LookupIPAddr(ctx, "api.openai.com")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected LookupIPAddr to fail against an already-expired context, got nil error")
	}
	if elapsed > testtime.Scale(time.Second) {
		t.Errorf("LookupIPAddr took %v to return after an already-expired deadline, want near-instant", elapsed)
	}
}

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		hostname string
		want     bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"api.openai.com", false},
		{"10.0.0.1", false}, // private but not localhost — handled by IsPrivateIP
		{"169.254.169.254", false},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			if got := network.IsLocalhost(tt.hostname); got != tt.want {
				t.Errorf("IsLocalhost(%q) = %v, want %v", tt.hostname, got, tt.want)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// Private IPv4 (RFC 1918)
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		// Link-local
		{"169.254.0.1", true},
		{"169.254.169.254", true},
		// Loopback
		{"127.0.0.1", true},
		{"127.255.255.255", true},
		// Public IPv4
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"104.21.0.1", false},
		// Private IPv6
		{"::1", true},     // loopback
		{"fe80::1", true}, // link-local
		{"fc00::1", true}, // unique local
		{"fd00::1", true}, // unique local
		// Public IPv6
		{"2606:4700::1", false},
		// Unspecified addresses (fail-closed)
		{"0.0.0.0", true},         // IPv4 unspecified
		{"0:0:0:0:0:0:0:0", true}, // IPv6 unspecified long form
		{"::", true},              // IPv6 unspecified short form
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}
			if got := network.IsPrivateIP(ip); got != tt.want {
				t.Errorf("IsPrivateIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// Failing over starts a fresh attempt, so the state the previous attempt resolved for itself must not
// survive into it. What the request may reach is not among it: that is a fact about the caller, not
// the attempt, and the presented credential and the caller's identity survive with it so the limits
// of the next attempt can be resolved against the same access.
func TestClearCtxForFallback(t *testing.T) {
	cleared := []schemas.BifrostContextKey{
		schemas.BifrostContextKeyAPIKeyID,
		schemas.BifrostContextKeyAPIKeyName,
		schemas.BifrostContextKeyGovernanceIncludeOnlyKeys,
		schemas.BifrostContextKeyChangeRequestType,
		schemas.BifrostContextKeyAttemptTrail,
		schemas.BifrostContextKeyStreamEndIndicator,
		schemas.BifrostContextKeyConnectionClosed,
		schemas.BifrostContextKeyStreamBodyExhausted,
		schemas.BifrostContextKeyStreamParkedAfterFinish,
		schemas.BifrostContextKeySupportsAssistantPrefill,
		// Set by the Bedrock InvokeModel stream path; a fallback to a plain-SSE
		// provider must not inherit the AWS event-stream reader (#6825).
		schemas.BifrostContextKeySSEReaderFactory,
		// The load balancer's record of the attempt that just failed.
		schemas.BifrostContextKeyLoadBalancerAttempt,
	}
	// The next attempt resolves its own limits, which needs the same credential and caller.
	preserved := []schemas.BifrostContextKey{
		schemas.BifrostContextKeyVirtualKey,
		schemas.BifrostContextKeyUserID,
		// What the caller sent is the same for every attempt of the request.
		schemas.BifrostContextKeyRequestedProvider,
		schemas.BifrostContextKeyRequestedModel,
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for _, key := range append(append([]schemas.BifrostContextKey{}, cleared...), preserved...) {
		ctx.SetValue(key, "set")
	}

	clearCtxForFallback(ctx)

	for _, key := range cleared {
		if ctx.Value(key) != nil {
			t.Errorf("%v survived into the next attempt", key)
		}
	}
	for _, key := range preserved {
		if ctx.Value(key) == nil {
			t.Errorf("%v was cleared, leaving the next attempt unable to resolve its own limits", key)
		}
	}
}

// TestCanProviderKeyValueBeEmptyGithubCopilot pins that a GitHub Copilot key may carry an
// empty value. The GitHub App bundle in github_copilot_key_config is a complete credential
// on its own, and the HTTP key handlers consult this allowlist before their provider-specific
// checks, so dropping the entry rejects every App-authenticated key at creation with a
// misleading "value must not be empty" error.
func TestCanProviderKeyValueBeEmptyGithubCopilot(t *testing.T) {
	if !CanProviderKeyValueBeEmpty(schemas.GithubCopilot) {
		t.Fatal("GitHub Copilot keys authenticated through a GitHub App have no value; the allowlist must include the provider")
	}
}

// TestStampRequestedRoute pins what core records as the caller's route before any
// PreRequestHook runs: plugins read it to report the model a routing rule rewrote.
func TestStampRequestedRoute(t *testing.T) {
	requested := func(ctx *schemas.BifrostContext) (schemas.ModelProvider, string) {
		provider, _ := ctx.Value(schemas.BifrostContextKeyRequestedProvider).(schemas.ModelProvider)
		model, _ := ctx.Value(schemas.BifrostContextKeyRequestedModel).(string)
		return provider, model
	}

	t.Run("records the caller's provider and model", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		stampRequestedRoute(ctx, schemas.Route{Provider: schemas.OpenAI, Model: "gpt-4o-mini"})
		provider, model := requested(ctx)
		assert.Equal(t, schemas.OpenAI, provider)
		assert.Equal(t, "gpt-4o-mini", model)
	})

	// Large-payload requests carry the model only in the metadata, and the routing plugin
	// rewrites it there in place, so the value must be copied before the hooks run.
	t.Run("large payload model is copied before routing rewrites it", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		metadata := &schemas.LargePayloadMetadata{Model: "gpt-4o-mini"}
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadMetadata, metadata)
		stampRequestedRoute(ctx, schemas.Route{Provider: schemas.OpenAI})
		metadata.Model = "gpt-4o"
		_, model := requested(ctx)
		assert.Equal(t, "gpt-4o-mini", model)
	})

	// bifrost.ctx is shared by every nil-ctx caller, so an empty provider must overwrite the
	// previous request's rather than leave it in place.
	t.Run("reused context does not keep the previous request's route", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		stampRequestedRoute(ctx, schemas.Route{Provider: schemas.OpenAI, Model: "gpt-4o-mini"})
		stampRequestedRoute(ctx, schemas.Route{Model: "claude-sonnet-4-6"})
		provider, model := requested(ctx)
		assert.Empty(t, provider)
		assert.Equal(t, "claude-sonnet-4-6", model)
	})
}

// TestValidateKeyGithubCopilot pins that validateKey rejects the same credentials the
// provider would reject at request time. Accepting a whitespace-only field here defers the
// failure to the first inference call, where it reads like a runtime fault rather than a
// setup mistake.
func TestValidateKeyGithubCopilot(t *testing.T) {
	fullAppConfig := func() *schemas.GithubCopilotKeyConfig {
		return &schemas.GithubCopilotKeyConfig{
			AppID:          *schemas.NewSecretVar("123456"),
			InstallationID: *schemas.NewSecretVar("87654321"),
			RepositoryID:   *schemas.NewSecretVar("999000111"),
			PrivateKey:     *schemas.NewSecretVar("pem"),
		}
	}
	blank := func(mutate func(*schemas.GithubCopilotKeyConfig)) *schemas.GithubCopilotKeyConfig {
		c := fullAppConfig()
		mutate(c)
		return c
	}

	tests := []struct {
		name      string
		key       schemas.Key
		wantError string
	}{
		{
			name: "a direct token is sufficient",
			key:  schemas.Key{Value: *schemas.NewSecretVar("tid=abc")},
		},
		{
			name: "the full app config is sufficient",
			key:  schemas.Key{GithubCopilotKeyConfig: fullAppConfig()},
		},
		{
			name:      "a whitespace-only token is not a credential",
			key:       schemas.Key{Value: *schemas.NewSecretVar("   ")},
			wantError: "github_copilot_key_config is required",
		},
		{
			name:      "no credential at all",
			key:       schemas.Key{},
			wantError: "github_copilot_key_config is required",
		},
		{
			name:      "whitespace-only app_id",
			key:       schemas.Key{GithubCopilotKeyConfig: blank(func(c *schemas.GithubCopilotKeyConfig) { c.AppID = *schemas.NewSecretVar("  ") })},
			wantError: "github_copilot_key_config.app_id is required",
		},
		{
			name:      "whitespace-only installation_id",
			key:       schemas.Key{GithubCopilotKeyConfig: blank(func(c *schemas.GithubCopilotKeyConfig) { c.InstallationID = *schemas.NewSecretVar("\t") })},
			wantError: "github_copilot_key_config.installation_id is required",
		},
		{
			name:      "whitespace-only repository_id",
			key:       schemas.Key{GithubCopilotKeyConfig: blank(func(c *schemas.GithubCopilotKeyConfig) { c.RepositoryID = *schemas.NewSecretVar(" ") })},
			wantError: "github_copilot_key_config.repository_id is required",
		},
		{
			name:      "whitespace-only private_key",
			key:       schemas.Key{GithubCopilotKeyConfig: blank(func(c *schemas.GithubCopilotKeyConfig) { c.PrivateKey = *schemas.NewSecretVar("\n") })},
			wantError: "github_copilot_key_config.private_key is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := tt.key
			err := validateKey(schemas.GithubCopilot, &key)

			if tt.wantError == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})

	}
}

func TestValidateKeyOAuthKeyConfig(t *testing.T) {
	cc := func(mutate func(*schemas.OAuthKeyConfig)) *schemas.OAuthKeyConfig {
		c := &schemas.OAuthKeyConfig{
			GrantType:    schemas.OAuthGrantClientCredentials,
			TokenURL:     *schemas.NewSecretVar("https://idp.example/oauth2/token"),
			ClientID:     schemas.NewSecretVar("id"),
			ClientSecret: schemas.NewSecretVar("secret"),
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}
	jb := func(mutate func(*schemas.OAuthKeyConfig)) *schemas.OAuthKeyConfig {
		c := &schemas.OAuthKeyConfig{
			GrantType:  schemas.OAuthGrantJWTBearer,
			TokenURL:   *schemas.NewSecretVar("https://idp.example/oauth2/token"),
			PrivateKey: schemas.NewSecretVar("pem"),
			Issuer:     "client",
			Audience:   "https://idp.example/oauth2/token",
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}

	tests := []struct {
		name      string
		key       schemas.Key
		wantError string
	}{
		{name: "client_credentials is sufficient", key: schemas.Key{OAuthKeyConfig: cc(nil)}},
		{name: "jwt_bearer is sufficient", key: schemas.Key{OAuthKeyConfig: jb(nil)}},
		// Key selection runs this on every request. A static value always wins over the block, so
		// an incomplete block must not take a usable key out of rotation; the management API
		// still refuses to save one (handlers.validateProviderKey).
		{name: "a value alongside an incomplete config keeps the key usable", key: schemas.Key{Value: *schemas.NewSecretVar("sk"), OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.ClientSecret = nil })}},
		{name: "missing grant", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.GrantType = "" })}, wantError: "grant_type is required"},
		{name: "unknown grant", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.GrantType = "password" })}, wantError: "must be client_credentials or jwt_bearer"},
		{name: "missing token url", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar(" ") })}, wantError: "token_url is required"},
		{name: "relative token url", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("/token") })}, wantError: "must be https"},
		{name: "plain http on a public host", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("http://idp.example/token") })}, wantError: "must be https"},
		{name: "token url with userinfo", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("https://user:pass@idp.example/token")
		})}, wantError: "must not carry userinfo, a query, or a fragment"},
		{name: "token url with a query", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("https://idp.example/token?client_secret=x")
		})}, wantError: "must not carry userinfo, a query, or a fragment"},
		{name: "token url with a fragment", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("https://idp.example/token#frag") })}, wantError: "must not carry userinfo, a query, or a fragment"},
		{name: "plain http on loopback is accepted", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("http://127.0.0.1:9/token") })}},
		{name: "jwt_bearer negative lifetime names the accepted range", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.AssertionLifetimeSeconds = -1 })}, wantError: "between 0 (default) and 3600"},
		{name: "missing client id", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.ClientID = schemas.NewSecretVar(" ") })}, wantError: "client_id and oauth_key_config.client_secret are required"},
		{name: "bad auth style", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.AuthStyle = "query" })}, wantError: "auth_style must be header or body"},
		{name: "jwt_bearer missing key", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.PrivateKey = nil })}, wantError: "private_key is required"},
		{name: "jwt_bearer missing issuer", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.Issuer = "" })}, wantError: "issuer is required"},
		{name: "jwt_bearer missing audience", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.Audience = "" })}, wantError: "audience is required"},
		{name: "jwt_bearer symmetric algorithm", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.SigningAlgorithm = "HS256" })}, wantError: "signing_algorithm must be one of"},
		{name: "jwt_bearer lifetime too long", key: schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.AssertionLifetimeSeconds = 7200 })}, wantError: "assertion_lifetime_seconds"},
		{name: "env-backed references count as present", key: schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("env.IDP_TOKEN_URL_UNSET_IN_TEST")
			c.ClientSecret = schemas.NewSecretVar("env.IDP_SECRET_UNSET_IN_TEST")
		})}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := tt.key
			err := validateKey(schemas.OpenAI, &key)
			if tt.wantError == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}

	t.Run("a key with oauth_key_config may have no value on an openai-based provider", func(t *testing.T) {
		assert.True(t, keyValueMayBeEmpty(schemas.OpenAI, &schemas.Key{OAuthKeyConfig: cc(nil)}))
		assert.False(t, keyValueMayBeEmpty(schemas.OpenAI, &schemas.Key{}))
		assert.False(t, keyValueMayBeEmpty(schemas.Anthropic, &schemas.Key{OAuthKeyConfig: cc(nil)}))
		assert.True(t, keyValueMayBeEmpty(schemas.Databricks, &schemas.Key{}))
	})

	t.Run("oauth_key_config is refused on a provider that does not resolve it", func(t *testing.T) {
		key := schemas.Key{OAuthKeyConfig: cc(nil)}
		err := validateKey(schemas.Anthropic, &key)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only supported on openai")
	})
}

// A request cancelled at the same instant its backoff expires must still be
// reported as cancelled: Go's select picks uniformly among ready cases, so a
// timer-only check lets roughly half of such requests run one more attempt
// (maximhq/bifrost#7105 review). With a cancelled ctx and a zero backoff both
// cases are ready on every call, so 64 calls expose a missing ctx check with
// probability 1 - 2^-64.
func TestWaitRetryBackoffReportsCancellationAtTimerExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 64; i++ {
		if !waitRetryBackoff(ctx, 0) {
			t.Fatalf("call %d: backoff expired together with the cancel and the wait reported not cancelled", i)
		}
	}
}

// OpenAI-compatible clients take the assistant role from the first chunk of a
// choice and drop tool calls without it (maximhq/bifrost#7693). Some clients
// concatenate a repeated role, so it must appear exactly once.
func TestWrapConvertedStreamPostHookRunnerSendsAssistantRoleOnce(t *testing.T) {
	reasoning := schemas.ResponsesMessageTypeReasoning
	functionCall := schemas.ResponsesMessageTypeFunctionCall
	message := schemas.ResponsesMessageTypeMessage

	tests := []struct {
		name   string
		events []*schemas.BifrostResponsesStreamResponse
	}{
		{
			name: "tool call turn without a message item",
			events: []*schemas.BifrostResponsesStreamResponse{
				{Type: schemas.ResponsesStreamResponseTypeCreated},
				{Type: schemas.ResponsesStreamResponseTypeInProgress},
				{Type: schemas.ResponsesStreamResponseTypeOutputItemAdded, Item: &schemas.ResponsesMessage{Type: &reasoning}},
				{
					Type:        schemas.ResponsesStreamResponseTypeOutputItemAdded,
					OutputIndex: schemas.Ptr(1),
					Item: &schemas.ResponsesMessage{
						Type: &functionCall,
						ResponsesToolMessage: &schemas.ResponsesToolMessage{
							CallID: schemas.Ptr("call_1"),
							Name:   schemas.Ptr("get_weather"),
						},
					},
				},
				{Type: schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, Delta: schemas.Ptr("{}")},
				{Type: schemas.ResponsesStreamResponseTypeCompleted},
			},
		},
		{
			name: "text turn",
			events: []*schemas.BifrostResponsesStreamResponse{
				{Type: schemas.ResponsesStreamResponseTypeCreated},
				{Type: schemas.ResponsesStreamResponseTypeOutputItemAdded, Item: &schemas.ResponsesMessage{Type: &message}},
				{Type: schemas.ResponsesStreamResponseTypeOutputTextDelta, Delta: schemas.Ptr("Hello")},
				{Type: schemas.ResponsesStreamResponseTypeCompleted},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var roles []*string
			runner := wrapConvertedStreamPostHookRunner(func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				require.NotNil(t, result.ChatResponse)
				require.NotEmpty(t, result.ChatResponse.Choices)
				roles = append(roles, result.ChatResponse.Choices[0].Delta.Role)
				return result, bifrostErr
			}, schemas.ResponsesRequest)

			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			for _, event := range tt.events {
				runner(ctx, &schemas.BifrostResponse{ResponsesStreamResponse: event}, nil)
			}

			require.Len(t, roles, len(tt.events))
			require.NotNil(t, roles[0], "first chunk has no role")
			assert.Equal(t, string(schemas.ChatMessageRoleAssistant), *roles[0])
			for i, role := range roles[1:] {
				assert.Nil(t, role, "chunk %d repeats the role", i+1)
			}
		})
	}
}

// Only the response.created, in_progress and completed events carry the Response
// object, so the deltas between them convert with no id, model or created. OpenAI
// SDK stream accumulators drop a chunk whose id differs from the stream's, which
// left the streamed text empty.
func TestWrapConvertedStreamPostHookRunnerKeepsStreamIdentityOnEveryChunk(t *testing.T) {
	message := schemas.ResponsesMessageTypeMessage
	functionCall := schemas.ResponsesMessageTypeFunctionCall
	upstream := &schemas.BifrostResponsesResponse{ID: schemas.Ptr("resp_0dd8"), Model: "gpt-6-luna", CreatedAt: 1760000000}
	tests := []struct {
		name   string
		events []*schemas.BifrostResponsesStreamResponse
	}{
		{
			name: "text turn",
			events: []*schemas.BifrostResponsesStreamResponse{
				{Type: schemas.ResponsesStreamResponseTypeCreated, Response: upstream},
				{Type: schemas.ResponsesStreamResponseTypeInProgress, Response: upstream},
				{Type: schemas.ResponsesStreamResponseTypeOutputItemAdded, Item: &schemas.ResponsesMessage{Type: &message}},
				{Type: schemas.ResponsesStreamResponseTypeContentPartAdded},
				{Type: schemas.ResponsesStreamResponseTypeOutputTextDelta, Delta: schemas.Ptr("ok")},
				{Type: schemas.ResponsesStreamResponseTypeOutputTextDone},
				{Type: schemas.ResponsesStreamResponseTypeOutputItemDone},
				{Type: schemas.ResponsesStreamResponseTypeCompleted, Response: upstream},
			},
		},
		{
			name: "tool call turn",
			events: []*schemas.BifrostResponsesStreamResponse{
				{Type: schemas.ResponsesStreamResponseTypeCreated, Response: upstream},
				{
					Type: schemas.ResponsesStreamResponseTypeOutputItemAdded,
					Item: &schemas.ResponsesMessage{
						Type:                 &functionCall,
						ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: schemas.Ptr("call_1"), Name: schemas.Ptr("get_weather")},
					},
				},
				{Type: schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, Delta: schemas.Ptr("{}")},
				{Type: schemas.ResponsesStreamResponseTypeCompleted, Response: upstream},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var chunks []*schemas.BifrostChatResponse
			runner := wrapConvertedStreamPostHookRunner(func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				require.NotNil(t, result.ChatResponse)
				chunks = append(chunks, result.ChatResponse)
				return result, bifrostErr
			}, schemas.ResponsesRequest)

			ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
			for _, event := range tt.events {
				runner(ctx, &schemas.BifrostResponse{ResponsesStreamResponse: event}, nil)
			}

			require.Len(t, chunks, len(tt.events))
			for i, chunk := range chunks {
				assert.Equal(t, "resp_0dd8", chunk.ID, "chunk %d (%s) id", i, tt.events[i].Type)
				assert.Equal(t, "gpt-6-luna", chunk.Model, "chunk %d (%s) model", i, tt.events[i].Type)
				assert.Equal(t, 1760000000, chunk.Created, "chunk %d (%s) created", i, tt.events[i].Type)
			}
		})
	}
}

// BenchmarkWrapConvertedStreamPostHookRunner measures one converted text turn,
// created through completed, as a chat stream served through Responses sees it.
func BenchmarkWrapConvertedStreamPostHookRunner(b *testing.B) {
	message := schemas.ResponsesMessageTypeMessage
	upstream := &schemas.BifrostResponsesResponse{ID: schemas.Ptr("resp_0dd8"), Model: "gpt-6-luna", CreatedAt: 1760000000}
	events := []*schemas.BifrostResponsesStreamResponse{
		{Type: schemas.ResponsesStreamResponseTypeCreated, Response: upstream},
		{Type: schemas.ResponsesStreamResponseTypeInProgress, Response: upstream},
		{Type: schemas.ResponsesStreamResponseTypeOutputItemAdded, Item: &schemas.ResponsesMessage{Type: &message}},
		{Type: schemas.ResponsesStreamResponseTypeContentPartAdded},
		{Type: schemas.ResponsesStreamResponseTypeOutputTextDelta, Delta: schemas.Ptr("ok")},
		{Type: schemas.ResponsesStreamResponseTypeOutputTextDone},
		{Type: schemas.ResponsesStreamResponseTypeOutputItemDone},
		{Type: schemas.ResponsesStreamResponseTypeCompleted, Response: upstream},
	}
	noop := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return result, bifrostErr
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runner := wrapConvertedStreamPostHookRunner(noop, schemas.ResponsesRequest)
		for _, event := range events {
			runner(ctx, &schemas.BifrostResponse{ResponsesStreamResponse: event}, nil)
		}
	}
}

// A converted stream chunk still carries the upstream API's raw bytes, so it must
// say it was converted: integrations echo RawResponse only for unconverted chunks.
func TestWrapConvertedStreamPostHookRunnerMarksConvertedChunks(t *testing.T) {
	tests := []struct {
		name       string
		targetType schemas.RequestType
		result     *schemas.BifrostResponse
		extra      func(*schemas.BifrostResponse) *schemas.BifrostResponseExtraFields
	}{
		{
			name:       "chat served through responses",
			targetType: schemas.ResponsesRequest,
			result: &schemas.BifrostResponse{ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
				Type:        schemas.ResponsesStreamResponseTypeCreated,
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI, RawResponse: `{"type":"response.created"}`},
			}},
			extra: func(r *schemas.BifrostResponse) *schemas.BifrostResponseExtraFields {
				return &r.ChatResponse.ExtraFields
			},
		},
		{
			name:       "text served through chat",
			targetType: schemas.ChatCompletionRequest,
			result: &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
				ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI, RawResponse: `{"object":"chat.completion.chunk"}`},
			}},
			extra: func(r *schemas.BifrostResponse) *schemas.BifrostResponseExtraFields {
				return &r.TextCompletionResponse.ExtraFields
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *schemas.BifrostResponse
			runner := wrapConvertedStreamPostHookRunner(func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				got = result
				return result, bifrostErr
			}, tt.targetType)
			runner(schemas.NewBifrostContext(context.Background(), time.Time{}), tt.result, nil)
			require.NotNil(t, got)
			assert.Equal(t, tt.targetType, tt.extra(got).ConvertedRequestType)
		})
	}
}

// customProviderKeyPending lists the bases whose custom providers still report the base's
// name, each with the ticket that fixes it. The test below fails on a stale entry, so the
// map empties as those fixes land and is meant to stay empty.
var customProviderKeyPending = map[schemas.ModelProvider]string{}

// A custom provider reports its own name from GetProviderKey, on every base a custom
// provider can be built on. Core finds a provider by that name (getProviderByKey) and picks
// the keys of a request by it, so a custom provider that reported its base's name was never
// found under its own name and was served with the base provider's keys, which it then sent
// to its own base_url (deepseek, glm, moonshot and qwen did before TXE-7097). A base added to
// schemas.SupportedBaseProviders is covered without being listed here.
func TestCustomProviderReportsItsOwnKeyOnEveryBase(t *testing.T) {
	client := &Bifrost{logger: NewNoOpLogger()}
	for _, base := range schemas.SupportedBaseProviders {
		t.Run(string(base), func(t *testing.T) {
			name := schemas.ModelProvider("custom-" + string(base))
			provider, err := client.createBaseProvider(name, &schemas.ProviderConfig{
				NetworkConfig:        schemas.NetworkConfig{BaseURL: "http://127.0.0.1:1", DefaultRequestTimeoutInSeconds: 1},
				CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: string(name), BaseProviderType: base},
			})
			require.NoError(t, err)
			got := provider.GetProviderKey()
			ticket, pending := customProviderKeyPending[base]
			switch {
			case pending && got == name:
				t.Errorf("%s now reports its custom name: remove it from customProviderKeyPending (%s)", base, ticket)
			case !pending && got != name:
				t.Errorf("a custom provider on the %s base reports the provider key %q, want its own name %q", base, got, name)
			}
		})
	}
	for base := range customProviderKeyPending {
		assert.Contains(t, schemas.SupportedBaseProviders, base, "customProviderKeyPending names a base that is not supported")
	}
}

// GetProviderKey runs several times per request (key selection, routing info, extra fields).
func BenchmarkCustomProviderGetProviderKey(b *testing.B) {
	client := &Bifrost{logger: NewNoOpLogger()}
	for _, bench := range []struct {
		name   string
		key    schemas.ModelProvider
		custom *schemas.CustomProviderConfig
	}{
		{"standard", schemas.DeepSeek, nil},
		{"custom", "custom-deepseek", &schemas.CustomProviderConfig{CustomProviderKey: "custom-deepseek", BaseProviderType: schemas.DeepSeek}},
	} {
		provider, err := client.createBaseProvider(bench.key, &schemas.ProviderConfig{
			NetworkConfig:        schemas.NetworkConfig{BaseURL: "http://127.0.0.1:1", DefaultRequestTimeoutInSeconds: 1},
			CustomProviderConfig: bench.custom,
		})
		require.NoError(b, err)
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = provider.GetProviderKey()
			}
		})
	}
}

// TestDeclaredErrorTypeIsSentAsDeclared: the constructors' declaration survives
// ResolveErrorType, which the transport runs on every error it sends, and each type is sent
// with its status: bifrost_internal 500, bifrost_dropped 503, any other 400.
func TestDeclaredErrorTypeIsSentAsDeclared(t *testing.T) {
	for _, errorType := range schemas.ErrorTypes {
		if errorType == schemas.ErrorTypeOther {
			continue
		}
		want := http.StatusBadRequest
		switch errorType {
		case schemas.ErrorTypeBifrostInternal:
			want = http.StatusInternalServerError
		case schemas.ErrorTypeBifrostDropped:
			want = http.StatusServiceUnavailable
		}
		for name, e := range map[string]*schemas.BifrostError{
			"newBifrostErrorFromMsg": newBifrostErrorFromMsg(errorType, "refused"),
			"newBifrostError":        newBifrostError(errorType, context.Canceled),
		} {
			if got := e.ResolveErrorType(); got != errorType {
				t.Errorf("%s(%s): resolved %q", name, errorType, got)
			}
			if got := e.EffectiveHTTPStatus(); got != want {
				t.Errorf("%s(%s): status %d, want %d", name, errorType, got, want)
			}
			// A caller_ (but caller_cancelled) or bifrost_dropped refusal is before dispatch:
			// upstream_sent false. Any other type stays untold (TXE-7651).
			wantSent := (strings.HasPrefix(string(errorType), "caller_") && errorType != schemas.ErrorTypeCallerCancelled) ||
				errorType == schemas.ErrorTypeBifrostDropped
			if got := e.ExtraFields.UpstreamSent; wantSent != (got != nil && !*got) || (!wantSent && got != nil) {
				t.Errorf("%s(%s): upstream_sent %v, want false=%v", name, errorType, got, wantSent)
			}
		}
	}
}

// callUpstreamSent makes upstream_sent call-wide (C6): false only when every attempt was refused
// before dispatch, true when any attempt says it was sent, untold otherwise.
func TestCallUpstreamSentAcrossAttempts(t *testing.T) {
	unsent := func() *schemas.BifrostError {
		return &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{UpstreamSent: schemas.Ptr(false)}}
	}
	sent := func() *schemas.BifrostError {
		return &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{UpstreamSent: schemas.Ptr(true)}}
	}
	untold := func() *schemas.BifrostError { return &schemas.BifrostError{} }
	for _, c := range []struct {
		name      string
		primary   *schemas.BifrostError
		fallbacks []*schemas.BifrostError
		want      *bool
	}{
		{"every attempt refused", unsent(), []*schemas.BifrostError{unsent()}, schemas.Ptr(false)},
		{"no fallback ran", unsent(), nil, schemas.Ptr(false)},
		{"a fallback untold", unsent(), []*schemas.BifrostError{untold()}, nil},
		{"a fallback sent", unsent(), []*schemas.BifrostError{unsent(), sent()}, schemas.Ptr(true)},
		{"primary untold", untold(), []*schemas.BifrostError{unsent()}, nil},
	} {
		acc := newCallUpstreamSent(c.primary)
		for _, f := range c.fallbacks {
			acc.observe(f)
		}
		got := acc.apply(c.primary).ExtraFields.UpstreamSent
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: upstream_sent %v, want %v", c.name, got, c.want)
		}
	}
}

// A refusal's literal reclassified as bifrost_internal (a nil response after dispatch) drops the
// refusal's upstream_sent false: untold (connector P2 on #366).
func TestReclassifyingARefusalClearsUpstreamSentFalse(t *testing.T) {
	e := &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{ErrorType: schemas.ErrorTypeCallerInvalidRequest, UpstreamSent: schemas.Ptr(false)}}
	declareErrorType(e, schemas.ErrorTypeBifrostInternal)
	if e.ExtraFields.UpstreamSent != nil {
		t.Errorf("upstream_sent %v after reclassifying as bifrost_internal, want untold", *e.ExtraFields.UpstreamSent)
	}
}

// bodyReadingGate is a stand-in for a plugin gate: it records that it ran.
type bodyReadingGate struct {
	fakeRoutingPlugin
	ran bool
}

func (g *bodyReadingGate) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	g.ran = true
	return req, nil, nil
}

// TestRunLLMPreHooksRefusesAmbiguousForwardedBody (TXE-7994): a forwarded body that repeats a
// member is refused 400 caller_invalid_request before any plugin reads it, also when the plugin
// pipeline is skipped, and without fallbacks; a body naming each member once reaches the plugins.
func TestRunLLMPreHooksRefusesAmbiguousForwardedBody(t *testing.T) {
	passthrough := func(body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI, Method: "POST", Path: "/v1/chat/completions", Body: []byte(body),
		}}
	}
	for _, skip := range []bool{false, true} {
		gate := &bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}
		pipeline := newRoutingCommitPipeline(gate)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if skip {
			ctx.SetValue(schemas.BifrostContextKeySkipPluginPipeline, true)
		}
		_, shortCircuit, ran := pipeline.RunLLMPreHooks(ctx, passthrough(`{"model":"gpt-4o","model":"o1"}`))
		require.NotNil(t, shortCircuit, "skip=%t: an ambiguous body was admitted", skip)
		require.NotNil(t, shortCircuit.Error)
		assert.Equal(t, 400, *shortCircuit.Error.StatusCode)
		assert.Equal(t, schemas.ErrorTypeCallerInvalidRequest, shortCircuit.Error.ExtraFields.ErrorType)
		assert.False(t, *shortCircuit.Error.AllowFallbacks)
		assert.Contains(t, shortCircuit.Error.Error.Message, "gives model more than once")
		assert.Zero(t, ran)
		assert.False(t, gate.ran, "skip=%t: a plugin read the ambiguous body", skip)

		_, shortCircuit, _ = pipeline.RunLLMPreHooks(ctx, passthrough(`{"model":"gpt-4o"}`))
		assert.Nil(t, shortCircuit)
		assert.Equal(t, !skip, gate.ran)
	}
}

// TestRunLLMPreHooksForwardsABodyDeclaredAsAnotherType: the declared type decides how a forwarded
// body is read (TXE-8012). A text/plain or octet-stream upload opening with "{" is content and is
// forwarded to the plugins, not refused as JSON that does not parse; a body declared JSON that
// repeats a member is still refused.
func TestRunLLMPreHooksForwardsABodyDeclaredAsAnotherType(t *testing.T) {
	typed := func(contentType, body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI, Method: "POST", Path: "/v1/chat/completions", Body: []byte(body),
			SafeHeaders: map[string]string{"Content-Type": contentType},
		}}
	}
	for _, tc := range []struct{ contentType, body string }{
		{"text/plain", "{hello, a text file"},
		{"text/plain; charset=utf-8", `{"note":"a","note":"b",}`},
		{"application/octet-stream", "[1, 2"},
		{"application/octet-stream", "{\"a\":1}\n{\"a\":2}\n"},
	} {
		gate := &bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}
		pipeline := newRoutingCommitPipeline(gate)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		_, shortCircuit, _ := pipeline.RunLLMPreHooks(ctx, typed(tc.contentType, tc.body))
		if shortCircuit != nil && shortCircuit.Error != nil && shortCircuit.Error.Error != nil {
			t.Errorf("%s body %q refused: %s", tc.contentType, tc.body, shortCircuit.Error.Error.Message)
		}
		assert.True(t, gate.ran, "%s body %q did not reach the plugins", tc.contentType, tc.body)
	}

	pipeline := newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}})
	_, shortCircuit, _ := pipeline.RunLLMPreHooks(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		typed("application/json", `{"model":"a","model":"b"}`))
	require.NotNil(t, shortCircuit, "a body declared JSON that repeats a member was admitted")
	assert.Contains(t, shortCircuit.Error.Error.Message, "gives model more than once")

	// JSON naming a model under another type would run a model no gate read: refused, 400. So is
	// JSON a lenient parser accepts that mentions one.
	for _, tc := range []struct{ contentType, body string }{
		{"text/plain", `{"model":"o1","messages":[]}`},
		{"application/octet-stream", `{"model":"o1","messages":[]}`},
		{"text/plain", `{"model":"gpt-4o","model":"o1",}`},
	} {
		contentType := tc.contentType
		gate := &bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}
		_, shortCircuit, _ = newRoutingCommitPipeline(gate).RunLLMPreHooks(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
			typed(contentType, tc.body))
		require.NotNil(t, shortCircuit, "JSON naming a model declared %s was admitted", contentType)
		assert.Equal(t, 400, *shortCircuit.Error.StatusCode)
		assert.Equal(t, schemas.ErrorTypeCallerInvalidRequest, shortCircuit.Error.ExtraFields.ErrorType)
		assert.Contains(t, shortCircuit.Error.Error.Message, "is JSON naming model but is declared "+contentType)
		assert.False(t, gate.ran)
	}
	// UTF-16 JSON with no declared type is content to every reader, but a vendor detecting the
	// encoding parses it: refused too.
	utf16Body := []byte{0xff, 0xfe}
	for _, r := range `{"model":"o1"}` {
		utf16Body = append(utf16Body, byte(r), 0)
	}
	_, shortCircuit, _ = newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), typed("", string(utf16Body)))
	require.NotNil(t, shortCircuit, "UTF-16 JSON naming a model with no Content-Type was admitted")
	assert.Contains(t, shortCircuit.Error.Error.Message, "is JSON naming model but is sent with no Content-Type")
}

// TestEveryPluginGateRunsBehindTheForwardedBodyGuard is the class check for TXE-7994: plugin
// gates read forwarded bodies with whatever parser suits them, so they are safe only behind
// refuseAmbiguousForwardedBody. It fails when production code outside RunLLMPreHooks calls a
// plugin's PreLLMHook (a new entry point that would skip the guard), or when RunLLMPreHooks
// stops calling the guard before its first plugin.
func TestEveryPluginGateRunsBehindTheForwardedBodyGuard(t *testing.T) {
	var sites []string
	for _, root := range []string{".", "../transports", "../framework", "../plugins"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			require.NoError(t, parseErr, path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "PreLLMHook" {
							sites = append(sites, filepath.ToSlash(path)+":"+fn.Name.Name)
						}
					}
					return true
				})
			}
			return nil
		}))
	}
	assert.Equal(t, []string{"bifrost.go:RunLLMPreHooks"}, sites,
		"a plugin's PreLLMHook is called outside RunLLMPreHooks; route the call through RunLLMPreHooks so refuseAmbiguousForwardedBody runs first")

	source, err := os.ReadFile("bifrost.go")
	require.NoError(t, err)
	body := string(source)
	start := strings.Index(body, "func (p *PluginPipeline) RunLLMPreHooks(")
	require.GreaterOrEqual(t, start, 0)
	guard := strings.Index(body[start:], "refuseAmbiguousForwardedBody(req)")
	firstHook := strings.Index(body[start:], ".PreLLMHook(")
	skip := strings.Index(body[start:], "BifrostContextKeySkipPluginPipeline")
	require.Greater(t, guard, 0, "RunLLMPreHooks no longer calls refuseAmbiguousForwardedBody")
	assert.Less(t, guard, skip, "the guard must run before the skip-pipeline early return")
	assert.Less(t, guard, firstHook, "the guard must run before the first plugin")
}

// TestNoProductionCodeStreamsABodyPastTheForwardedBodyGuard: large-payload mode streams a request
// body to the provider unread, so core's forwarded-body guard never sees it (TXE-8012). The fork
// keeps that mode unreachable, and this check fails when production code gains a way in: a
// SetLargePayloadHook that does not panic on a hook, an assignment to a largePayloadHook field
// outside the test-only setter, or code that stages a streamed body on the context itself.
func TestNoProductionCodeStreamsABodyPastTheForwardedBodyGuard(t *testing.T) {
	streamKeys := map[string]bool{
		"BifrostContextKeyLargePayloadMode":   true,
		"BifrostContextKeyLargePayloadReader": true,
	}
	var setters, unguardedSetters, assignments, staged []string
	for _, root := range []string{".", "../transports", "../framework", "../plugins"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			require.NoError(t, parseErr, path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				site := filepath.ToSlash(path) + ":" + fn.Name.Name
				if fn.Name.Name == "SetLargePayloadHook" {
					setters = append(setters, site)
					panics := false
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok {
							if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
								panics = true
							}
						}
						return true
					})
					if !panics {
						unguardedSetters = append(unguardedSetters, site)
					}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.AssignStmt:
						for i, lhs := range node.Lhs {
							sel, ok := lhs.(*ast.SelectorExpr)
							if !ok || sel.Sel.Name != "largePayloadHook" || fn.Name.Name == "setLargePayloadHookForTest" {
								continue
							}
							if i < len(node.Rhs) {
								if ident, ok := node.Rhs[i].(*ast.Ident); ok && ident.Name == "nil" {
									continue
								}
							}
							assignments = append(assignments, site)
						}
					case *ast.CallExpr:
						if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetValue" && len(node.Args) > 0 {
							if key, ok := node.Args[0].(*ast.SelectorExpr); ok && streamKeys[key.Sel.Name] {
								staged = append(staged, site)
							}
						}
					}
					return true
				})
			}
			return nil
		}))
	}
	assert.NotEmpty(t, setters, "no SetLargePayloadHook found; the check is not looking where the hook is installed")
	assert.Empty(t, unguardedSetters, "a SetLargePayloadHook installs a hook without refusing it; large-payload bodies stream past refuseAmbiguousForwardedBody")
	assert.Empty(t, assignments, "a largePayloadHook field is set outside setLargePayloadHookForTest")
	assert.Empty(t, staged, "production code stages a streamed request body on the context, past refuseAmbiguousForwardedBody")
}

// TestEveryForwardedBodyReaderUsesTheSharedClassifier is the class check for TXE-8012: a body the
// guard does not read as JSON must be a body no gate reads as JSON, so every reader decides how
// to read a forwarded body through schemas.ClassifyForwardedBody (or AmbiguousBodyMember, which
// does). It fails when production code reads a passthrough body (".PassthroughRequest.Body",
// ".ForwardedBody()", or a function named for parsing one) without classifying it in the same
// function. Each exemption names why the function reads no member a gate acts on.
func TestEveryForwardedBodyReaderUsesTheSharedClassifier(t *testing.T) {
	exempt := map[string]string{
		// Stores the bytes for the log view by its own Content-Type check; reads no member a gate acts on.
		"../plugins/logging/main.go:captureRequestInput": "log display only",
		// Called only by parsePassthroughBody, after it classified the body as multipart.
		"../transports/bifrost-http/integrations/router.go:parseMultipartPassthroughBody": "classified by its caller",
		// The vendor calls: they send the body's bytes upstream as written and read no member.
		"providers/anthropicmsg/passthrough.go:Passthrough":        "forwards bytes",
		"providers/anthropicmsg/passthrough.go:PassthroughStream":  "forwards bytes",
		"providers/azure/azure.go:Passthrough":                     "forwards bytes",
		"providers/azure/azure.go:PassthroughStream":               "forwards bytes",
		"providers/bedrock/passthrough.go:buildPassthroughRequest": "forwards bytes",
		"providers/bedrock/passthrough.go:PassthroughStream":       "forwards bytes",
		"providers/gemini/gemini.go:Passthrough":                   "forwards bytes",
		"providers/gemini/gemini.go:PassthroughStream":             "forwards bytes",
		"providers/openai/openai.go:Passthrough":                   "forwards bytes",
		"providers/openai/openai.go:PassthroughStream":             "forwards bytes",
		"providers/runware/runware.go:Passthrough":                 "forwards bytes",
		"providers/vertex/vertex.go:Passthrough":                   "forwards bytes",
		"providers/vertex/vertex.go:PassthroughStream":             "forwards bytes",
		// Rewrites project and region placeholders in the bytes of a body its callers check is
		// declared application/json; reads no member a gate acts on.
		"providers/vertex/utils.go:rewritePassthroughBody": "byte rewrite after the vendor call is built",
	}
	var readers, unclassified []string
	for _, root := range []string{".", "../transports", "../framework", "../plugins"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.ToSlash(path) == "schemas/forwardedbody.go" {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			require.NoError(t, parseErr, path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				reads := strings.Contains(strings.ToLower(fn.Name.Name), "passthroughbody")
				classifies := false
				// A parameter holding a passthrough request: reading its Body reads the body too.
				requests := map[string]bool{}
				for _, field := range fn.Type.Params.List {
					if star, ok := field.Type.(*ast.StarExpr); ok {
						if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "BifrostPassthroughRequest" {
							for _, name := range field.Names {
								requests[name.Name] = true
							}
						}
					}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.SelectorExpr:
						if inner, ok := node.X.(*ast.SelectorExpr); ok && node.Sel.Name == "Body" && inner.Sel.Name == "PassthroughRequest" {
							reads = true
						}
						if ident, ok := node.X.(*ast.Ident); ok && node.Sel.Name == "Body" && requests[ident.Name] {
							reads = true
						}
						switch node.Sel.Name {
						case "ForwardedBody":
							reads = true
						case "ClassifyForwardedBody", "AmbiguousBodyMember":
							classifies = true
						}
					}
					return true
				})
				if !reads {
					continue
				}
				site := filepath.ToSlash(path) + ":" + fn.Name.Name
				readers = append(readers, site)
				if !classifies && exempt[site] == "" {
					unclassified = append(unclassified, site)
				}
			}
			return nil
		}))
	}
	assert.Contains(t, readers, "../plugins/governance/providerjobs.go:passthroughObjectRefs", "the check no longer finds governance's object reader")
	assert.Contains(t, readers, "../plugins/governance/main.go:passthroughBodyModels", "the check no longer finds governance's body reader; it is not looking where readers are")
	assert.Contains(t, readers, "../transports/bifrost-http/integrations/router.go:parsePassthroughBody", "the check no longer finds the router's body reader")
	assert.Empty(t, unclassified, "a forwarded-body reader decides how to read the body without schemas.ClassifyForwardedBody; route it through the classifier so it reads what the guard checked (TXE-8012)")
	for site := range exempt {
		assert.Contains(t, readers, site, "exemption %s names no reader; drop it", site)
	}
}

// TestBodyReadersGateOnlyOnTheSharedKeyList: the guard's case-folding and mislabeled-JSON
// refusal cover the keys in schemas.GatedBodyKeys, so a reader that gates on another key would be
// read one way by it and another by the vendor (TXE-8012). It fails when a forwarded-body reader
// compares a body key to a string literal instead of a schemas.Body*Key constant, or uses a
// constant that is neither in GatedBodyKeys nor the named non-gate BodyStreamKey.
func TestBodyReadersGateOnlyOnTheSharedKeyList(t *testing.T) {
	readers := map[string]bool{
		"../plugins/governance/main.go:passthroughBodyModels":                             true,
		"../transports/bifrost-http/integrations/router.go:parsePassthroughBody":          true,
		"../transports/bifrost-http/integrations/router.go:passthroughObjectModel":        true,
		"../transports/bifrost-http/integrations/router.go:parseMultipartPassthroughBody": true,
	}
	allowed := map[string]bool{"BodyStreamKey": true}
	for _, key := range schemas.GatedBodyKeys {
		switch key {
		case schemas.BodyModelKey:
			allowed["BodyModelKey"] = true
		default:
			t.Fatalf("GatedBodyKeys holds %q with no Body*Key constant; add one", key)
		}
	}
	found := map[string]bool{}
	var literals, unlisted []string
	for _, root := range []string{"../transports", "../plugins"} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			require.NoError(t, parseErr, path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				site := filepath.ToSlash(path) + ":"
				if !ok || fn.Body == nil || !readers[site+fn.Name.Name] {
					continue
				}
				site += fn.Name.Name
				found[site] = true
				literal := func(e ast.Expr) {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value != `""` {
						literals = append(literals, site+" "+lit.Value)
					}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.CallExpr:
						if sel, ok := node.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Get" || sel.Sel.Name == "GetBytes" || sel.Sel.Name == "EqualFold") {
							for _, arg := range node.Args {
								literal(arg)
							}
						}
					case *ast.CaseClause:
						for _, e := range node.List {
							literal(e)
						}
					case *ast.BinaryExpr:
						// A key read from the body (gjson's key.Str) compared to a literal; a value
						// compared to one ("true", "1") is not a key.
						isKey := func(e ast.Expr) bool {
							sel, ok := e.(*ast.SelectorExpr)
							return ok && sel.Sel.Name == "Str"
						}
						if node.Op == token.EQL || node.Op == token.NEQ {
							if isKey(node.X) {
								literal(node.Y)
							}
							if isKey(node.Y) {
								literal(node.X)
							}
						}
					case *ast.SelectorExpr:
						if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "schemas" && strings.HasPrefix(node.Sel.Name, "Body") && strings.HasSuffix(node.Sel.Name, "Key") && !allowed[node.Sel.Name] {
							unlisted = append(unlisted, site+" schemas."+node.Sel.Name)
						}
					}
					return true
				})
			}
			return nil
		}))
	}
	for site := range readers {
		assert.True(t, found[site], "reader %s not found; update the list", site)
	}
	assert.Empty(t, literals, "a forwarded-body reader compares a key to a string literal; use schemas.BodyModelKey (gated, in GatedBodyKeys) or schemas.BodyStreamKey")
	assert.Empty(t, unlisted, "a forwarded-body reader gates on a key not in schemas.GatedBodyKeys; add it there so the guard covers it")
}

// TestRunLLMPreHooksForwardsABatchInputFileUpload: a batch input file is JSON Lines naming a model
// on every line, and SDKs upload it under whatever type they guess (Python's mimetypes knows no
// .jsonl, so a multipart part is often application/octet-stream). Uploading one to a file route
// stores it and runs no model, so the guard forwards it in each SDK's shape (TXE-8012), while the
// same lines sent as a body to an inference route are refused.
func TestRunLLMPreHooksForwardsABatchInputFileUpload(t *testing.T) {
	openAILines := "{\"custom_id\":\"r1\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"gpt-4o\",\"messages\":[]}}\n" +
		"{\"custom_id\":\"r2\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"gpt-4o\",\"messages\":[]}}\n"
	anthropicLines := "{\"custom_id\":\"r1\",\"params\":{\"model\":\"claude-sonnet-4-5\",\"max_tokens\":16,\"messages\":[]}}\n"
	geminiLines := "{\"key\":\"r1\",\"request\":{\"contents\":[{\"role\":\"user\",\"parts\":[{\"text\":\"hi\"}]},{\"role\":\"model\",\"parts\":[{\"text\":\"yo\"}]}]}}\n"
	multipartUpload := func(partType, lines string) (string, string) {
		const boundary = "----sdkboundary8012"
		body := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n" +
			"--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: " + partType + "\r\n\r\n" +
			lines + "\r\n--" + boundary + "--\r\n"
		return "multipart/form-data; boundary=" + boundary, body
	}
	passthrough := func(provider schemas.ModelProvider, path, contentType, body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: provider, Method: "POST", Path: path, Body: []byte(body),
			SafeHeaders: map[string]string{"content-type": contentType},
		}}
	}
	type upload struct {
		name string
		req  *schemas.BifrostRequest
	}
	var uploads []upload
	for _, partType := range []string{"application/octet-stream", "application/jsonl", "application/json", "text/plain"} {
		ct, body := multipartUpload(partType, openAILines)
		uploads = append(uploads, upload{"OpenAI /v1/files, part " + partType, passthrough(schemas.OpenAI, "/v1/files", ct, body)})
		ct, body = multipartUpload(partType, anthropicLines)
		uploads = append(uploads, upload{"Anthropic /v1/files, part " + partType, passthrough(schemas.Anthropic, "/v1/files", ct, body)})
	}
	ct, body := multipartUpload("application/octet-stream", openAILines)
	uploads = append(uploads, upload{"OpenAI /v1/uploads/{id}/parts", passthrough(schemas.OpenAI, "/v1/uploads/upload_1/parts", ct, body)})
	for _, rawType := range []string{"application/octet-stream", "application/jsonl", "text/plain", ""} {
		uploads = append(uploads, upload{"GenAI /upload/v1beta/files raw " + rawType, passthrough(schemas.Gemini, "/upload/v1beta/files", rawType, geminiLines)})
		uploads = append(uploads, upload{"GenAI /upload raw OpenAI-shape lines " + rawType, passthrough(schemas.Gemini, "/upload/v1beta/files", rawType, openAILines)})
	}
	for _, tc := range uploads {
		gate := &bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}
		_, shortCircuit, _ := newRoutingCommitPipeline(gate).RunLLMPreHooks(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), tc.req)
		if shortCircuit != nil && shortCircuit.Error != nil && shortCircuit.Error.Error != nil {
			t.Errorf("%s refused: %s", tc.name, shortCircuit.Error.Error.Message)
		}
		assert.True(t, gate.ran, "%s did not reach the plugins", tc.name)
	}

	// A body that may name a provider object, as content, to an inference route is refused:
	// governance's object gate reads objects only from a body classified JSON.
	_, refused, _ := newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		passthrough(schemas.OpenAI, "/v1/responses", "text/plain", `{"input":[{"type":"input_file","file_id":"file-foreign"}],}`))
	require.NotNil(t, refused, "content naming a file id to an inference route was admitted")
	assert.Contains(t, refused.Error.Error.Message, "is JSON naming a provider object but is declared text/plain")
	// A route that attaches an existing file by id is no upload: refused the same way.
	_, refused, _ = newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		passthrough(schemas.OpenAI, "/v1/vector_stores/vs_1/files", "text/plain", `{"file_id":"file-foreign"}`))
	require.NotNil(t, refused, "content naming a file id to a vector store attach was admitted")
	assert.Contains(t, refused.Error.Error.Message, "is JSON naming a provider object")
	// The same on an upload route is a file.
	_, admitted, _ := newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		passthrough(schemas.OpenAI, "/v1/files", "application/octet-stream", "{\"custom_id\":\"1\",\"file_id\":\"file-x\"}\n"))
	assert.Nil(t, admitted, "a batch file naming a file id on an upload route was refused")

	// An undeclared strict JSON document on an upload route is still checked for repeats: the
	// router and governance read an undeclared one.
	_, shortCircuit, _ := newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		passthrough(schemas.Gemini, "/upload/v1beta/files", "", `{"model":"a","model":"b"}`))
	require.NotNil(t, shortCircuit, "an undeclared JSON body repeating model on an upload route was admitted")
	assert.Contains(t, shortCircuit.Error.Error.Message, "gives model more than once")

	// The same lines as a body to an inference route are refused: a vendor parsing it as JSON
	// would run the model the first line names, past that model's own limits.
	for _, contentType := range []string{"application/octet-stream", "application/x-ndjson"} {
		_, shortCircuit, _ := newRoutingCommitPipeline(&bodyReadingGate{fakeRoutingPlugin: fakeRoutingPlugin{name: "gate"}}).RunLLMPreHooks(
			schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
			passthrough(schemas.OpenAI, "/v1/chat/completions", contentType, `{"model":"o1","messages":[]}`+"\n"))
		require.NotNil(t, shortCircuit, "JSON naming a model as %s to an inference route was admitted", contentType)
		assert.Contains(t, shortCircuit.Error.Error.Message, "is JSON naming model but is declared "+contentType)
	}
}
