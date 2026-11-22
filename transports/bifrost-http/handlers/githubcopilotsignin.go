package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// githubCopilotClientID is GitHub's own Copilot editor App. Copilot exchanges only tokens
// issued to the GitHub Apps it recognises, so a sign-in through any other App's client ID
// yields a token Copilot refuses. Its tokens do not expire, so nothing needs refreshing.
const githubCopilotClientID = "Iv1.b507a08c87ecfe98"

const (
	githubDeviceCodeURL      = "https://github.com/login/device/code"
	githubAccessTokenURL     = "https://github.com/login/oauth/access_token"
	githubUserURL            = "https://api.github.com/user"
	githubDeviceCodeGrant    = "urn:ietf:params:oauth:grant-type:device_code"
	githubSignInTimeout      = 30 * time.Second
	githubSignInMaxBodyBytes = 1 << 20
)

// githubSignInEndpoints are the GitHub URLs the device flow talks to; tests point them at a fake.
type githubSignInEndpoints struct {
	deviceCodeURL string
	tokenURL      string
	userURL       string
}

// GithubCopilotSignInHandler serves "Sign in with GitHub" for GitHub Copilot keys. The browser
// cannot run GitHub's device flow itself (GitHub sends no CORS headers), so the gateway calls
// GitHub and hands the resulting token back to the key form, which saves it as the key's value.
//
// GitHub is reached the way the github-copilot provider reaches it for the token exchange:
// lib.ManagementEgress gives the provider's proxy (its own, else the inherited global one, else
// the global proxy when enabled for API traffic) and its network_config TLS settings (ca_cert_pem, insecure_skip_verify), and the client is built
// by providerUtils.NewProviderHTTPClient, which every provider's auth exchange uses. A
// deployment that must egress through a proxy or trust a TLS-inspecting CA therefore signs in
// through the same path its inference later takes. The client is built per call and not
// cached: sign-in is a rare operator action, and a cache keyed by proxy config would keep
// every historical proxy config for the life of the process.
type GithubCopilotSignInHandler struct {
	config    *lib.Config
	client    *http.Client // set only by tests; nil means resolve the provider's egress per call
	endpoints githubSignInEndpoints
}

// NewGithubCopilotSignInHandler creates the sign-in handler. config may be nil, which means
// the default network config and no proxy beyond the environment's.
func NewGithubCopilotSignInHandler(config *lib.Config) *GithubCopilotSignInHandler {
	return &GithubCopilotSignInHandler{
		config: config,
		endpoints: githubSignInEndpoints{
			deviceCodeURL: githubDeviceCodeURL,
			tokenURL:      githubAccessTokenURL,
			userURL:       githubUserURL,
		},
	}
}

// RegisterRoutes registers the sign-in routes.
func (h *GithubCopilotSignInHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.POST("/api/providers/{provider}/sign-in", lib.ChainMiddlewares(h.start, middlewares...))
	r.POST("/api/providers/{provider}/sign-in/poll", lib.ChainMiddlewares(h.poll, middlewares...))
}

// githubDeviceCode is what the operator needs to finish signing in on github.com.
type githubDeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// githubSignInPollResult is the outcome of one poll. Token is set only when Status is complete.
type githubSignInPollResult struct {
	Status   string `json:"status"`             // pending, complete, expired or denied
	Interval int    `json:"interval,omitempty"` // seconds to wait before the next poll
	Token    string `json:"token,omitempty"`
	Login    string `json:"login,omitempty"`
}

// githubOAuthError is an error GitHub returned in a 200 response body.
type githubOAuthError struct {
	Code        string
	Description string
	Interval    int // the poll interval GitHub asks for with slow_down
}

func (e *githubOAuthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// start asks GitHub for a device code.
//
// POST /api/providers/github-copilot/sign-in
func (h *GithubCopilotSignInHandler) start(ctx *fasthttp.RequestCtx) {
	provider, ok := h.signInProvider(ctx)
	if !ok {
		return
	}
	var code githubDeviceCode
	err := h.postForm(context.Background(), h.httpClient(provider), h.endpoints.deviceCodeURL, url.Values{"client_id": {githubCopilotClientID}, "scope": {"read:user"}}, &code)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadGateway, fmt.Sprintf("GitHub rejected the sign-in: %v", err))
		return
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		SendError(ctx, fasthttp.StatusBadGateway, "GitHub did not return a device code")
		return
	}
	sendSignInJSON(ctx, code)
}

// poll checks once whether the operator finished on github.com, and returns the token if so.
//
// POST /api/providers/github-copilot/sign-in/poll
func (h *GithubCopilotSignInHandler) poll(ctx *fasthttp.RequestCtx) {
	provider, ok := h.signInProvider(ctx)
	if !ok {
		return
	}
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil || strings.TrimSpace(req.DeviceCode) == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "device_code is required")
		return
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	client := h.httpClient(provider)
	err := h.postForm(context.Background(), client, h.endpoints.tokenURL, url.Values{
		"client_id":   {githubCopilotClientID},
		"device_code": {req.DeviceCode},
		"grant_type":  {githubDeviceCodeGrant},
	}, &token)
	var oauthErr *githubOAuthError
	if errors.As(err, &oauthErr) {
		switch oauthErr.Code {
		case "authorization_pending":
			sendSignInJSON(ctx, githubSignInPollResult{Status: "pending"})
			return
		case "slow_down":
			sendSignInJSON(ctx, githubSignInPollResult{Status: "pending", Interval: oauthErr.Interval})
			return
		case "expired_token":
			sendSignInJSON(ctx, githubSignInPollResult{Status: "expired"})
			return
		case "access_denied":
			sendSignInJSON(ctx, githubSignInPollResult{Status: "denied"})
			return
		case "incorrect_device_code", "bad_verification_code":
			SendError(ctx, fasthttp.StatusBadRequest, "GitHub does not recognize this sign-in attempt; start the sign-in again")
			return
		}
	}
	if err != nil {
		SendError(ctx, fasthttp.StatusBadGateway, fmt.Sprintf("GitHub rejected the sign-in: %v", err))
		return
	}
	if token.AccessToken == "" {
		SendError(ctx, fasthttp.StatusBadGateway, "GitHub did not return an access token")
		return
	}
	sendSignInJSON(ctx, githubSignInPollResult{
		Status: "complete",
		Token:  token.AccessToken,
		Login:  h.lookupLogin(context.Background(), client, token.AccessToken),
	})
}

// httpClient returns the client for one sign-in call: the egress of the provider the key
// belongs to (github-copilot, or a custom provider based on it), bounded by
// githubSignInTimeout whatever the provider's own request timeout is.
func (h *GithubCopilotSignInHandler) httpClient(provider schemas.ModelProvider) *http.Client {
	if h.client != nil {
		return h.client
	}
	proxyConfig, networkConfig := lib.ManagementEgress(h.config, provider)
	networkConfig.DefaultRequestTimeoutInSeconds = int(githubSignInTimeout / time.Second)
	return providerUtils.NewProviderHTTPClient(proxyConfig, networkConfig, signInLogger())
}

// signInLogger is the package logger, or a discarding one before the server sets it (tests).
func signInLogger() schemas.Logger {
	if logger != nil {
		return logger
	}
	return bifrost.NewDefaultLogger(schemas.LogLevelError)
}

// sendSignInJSON sends a sign-in reply. Both replies carry a credential (the device code that
// redeems the sign-in, then the GitHub token), so no cache may keep them.
func sendSignInJSON(ctx *fasthttp.RequestCtx, body any) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Pragma", "no-cache")
	SendJSON(ctx, body)
}

// signInProvider returns the provider a sign-in is for: github-copilot, or a configured custom
// provider whose base_provider_type is github-copilot (the key form offers sign-in for both,
// and a custom provider signs in through its own egress). Any other name is refused with 400.
func (h *GithubCopilotSignInHandler) signInProvider(ctx *fasthttp.RequestCtx) (schemas.ModelProvider, bool) {
	name, _ := ctx.UserValue("provider").(string)
	provider := schemas.ModelProvider(name)
	if provider == schemas.GithubCopilot {
		return provider, true
	}
	if h.config != nil {
		if cfg, err := h.config.GetProviderConfigRaw(provider); err == nil && cfg.CustomProviderConfig != nil &&
			cfg.CustomProviderConfig.BaseProviderType == schemas.GithubCopilot {
			return provider, true
		}
	}
	SendError(ctx, fasthttp.StatusBadRequest, "sign-in is only available for github-copilot and custom providers based on it; save a new custom provider before signing in")
	return "", false
}

// postForm sends a form to a GitHub OAuth endpoint and decodes the JSON reply. GitHub reports
// OAuth failures as HTTP 200 with an error field, returned here as a githubOAuthError. Callers
// pass a background context, not the fasthttp request: the client's timeout bounds the call.
func (h *GithubCopilotSignInHandler) postForm(ctx context.Context, client *http.Client, target string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubSignInMaxBodyBytes))
	if err != nil {
		return fmt.Errorf("could not read GitHub's response: %w", err)
	}
	var probe struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Interval         int    `json:"interval"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != "" {
		return &githubOAuthError{Code: probe.Error, Description: probe.ErrorDescription, Interval: probe.Interval}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("could not parse GitHub's response: %w", err)
	}
	return nil
}

// lookupLogin returns the GitHub login a token belongs to, or "" when it cannot tell. It only
// labels the sign-in in the UI, so a failure is not an error.
func (h *GithubCopilotSignInHandler) lookupLogin(ctx context.Context, client *http.Client, token string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.endpoints.userURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, githubSignInMaxBodyBytes)).Decode(&user) != nil {
		return ""
	}
	return user.Login
}
