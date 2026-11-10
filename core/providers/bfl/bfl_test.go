package bfl

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func newTestProvider(t *testing.T, network schemas.NetworkConfig) *BFLProvider {
	t.Helper()
	provider, err := NewBFLProvider(&schemas.ProviderConfig{NetworkConfig: network}, nil)
	if err != nil {
		t.Fatalf("NewBFLProvider: %v", err)
	}
	return provider
}

// TestBFLBase pins the global endpoint as the default and that a regional base replaces it.
func TestBFLBase(t *testing.T) {
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	if got := newTestProvider(t, schemas.NetworkConfig{}).requestURL(ctx, "/v1/flux-2-pro"); got != "https://api.bfl.ai/v1/flux-2-pro" {
		t.Errorf("default URL = %q, want the global endpoint", got)
	}
	if got := newTestProvider(t, schemas.NetworkConfig{BaseURL: "https://api.eu.bfl.ai/"}).requestURL(ctx, "/v1/flux-2-pro"); got != "https://api.eu.bfl.ai/v1/flux-2-pro" {
		t.Errorf("regional URL = %q, want the configured base", got)
	}
}

// TestBFLRequestSendsXKeyToConfiguredBase pins, against a recording server, that a request
// the provider builds goes to the configured base with the key in x-key, no Authorization
// header, and that an extra header named x-key cannot replace the key.
func TestBFLRequestSendsXKeyToConfiguredBase(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotKey, gotAuth, gotTrace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotKey, gotAuth, gotTrace = r.URL.Path, r.Header.Get("x-key"), r.Header.Get("Authorization"), r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{
		BaseURL:             server.URL + "/",
		AllowPrivateNetwork: true,
		ExtraHeaders: map[string]schemas.SecretVar{
			"x-key":   *schemas.NewSecretVar("extra-key"),
			"X-Trace": *schemas.NewSecretVar("t-1"),
		},
	})
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(provider.requestURL(ctx, "/v1/flux-2-pro"))
	req.Header.SetMethod(http.MethodPost)
	provider.setRequestHeaders(ctx, req, schemas.Key{Value: *schemas.NewSecretVar("bfl-key")})

	if err := provider.client.Do(req, resp); err != nil {
		t.Fatalf("request to the recording server failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/flux-2-pro" {
		t.Errorf("path = %q, want /v1/flux-2-pro on the configured base", gotPath)
	}
	if gotKey != "bfl-key" {
		t.Errorf("x-key = %q, want the key", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none", gotAuth)
	}
	if gotTrace != "t-1" {
		t.Errorf("X-Trace = %q, want the extra header", gotTrace)
	}
}

// TestBFLOperationsAreUnsupported pins that every operation answers the
// unsupported-operation error, naming its own request type, without calling upstream.
func TestBFLOperationsAreUnsupported(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	var _ schemas.Provider = provider

	iface := reflect.TypeOf((*schemas.Provider)(nil)).Elem()
	value := reflect.ValueOf(provider)
	for i := 0; i < iface.NumMethod(); i++ {
		method := iface.Method(i)
		if method.Name == "GetProviderKey" {
			continue
		}
		fn := value.MethodByName(method.Name)
		args := make([]reflect.Value, fn.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(fn.Type().In(j))
		}
		out := fn.Call(args)
		bifrostErr, _ := out[len(out)-1].Interface().(*schemas.BifrostError)
		if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "unsupported_operation" {
			t.Errorf("%s: got %+v, want the unsupported-operation error", method.Name, bifrostErr)
			continue
		}
		if bifrostErr.ExtraFields.RequestType == "" || bifrostErr.ExtraFields.Provider != schemas.BFL {
			t.Errorf("%s: extra fields %+v, want provider bfl and the request type", method.Name, bifrostErr.ExtraFields)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("unsupported operations made %d upstream calls", n)
	}
}

// TestBFLErrorParsing pins that BFL's error bodies keep their status (a 429 stays 429),
// read FastAPI's detail in both shapes, and land in the right error_type family.
func TestBFLErrorParsing(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantMessage string
		wantParam   string
		wantCode    string
		want        schemas.ErrorType
	}{
		{name: "too many active tasks", status: 429, body: `{"detail":"You have exceeded the number of active tasks."}`, wantMessage: "You have exceeded the number of active tasks.", want: schemas.ErrorTypeProviderRateLimited},
		{name: "insufficient credits", status: 402, body: `{"detail":"Insufficient credits"}`, wantMessage: "Insufficient credits", want: schemas.ErrorTypeProviderBilling},
		{name: "bad key", status: 403, body: `{"detail":"Not authenticated - Invalid Authentication"}`, wantMessage: "Not authenticated - Invalid Authentication", want: schemas.ErrorTypeProviderAuthFailed},
		{name: "validation list", status: 422, body: `{"detail":[{"loc":["body","width"],"msg":"Input should be a multiple of 16","type":"multiple_of"},{"loc":["body","steps"],"msg":"Input should be less than or equal to 50","type":"less_than_equal"}]}`,
			wantMessage: "width: Input should be a multiple of 16; steps: Input should be less than or equal to 50", wantParam: "width", wantCode: "multiple_of", want: schemas.ErrorTypeCallerInvalidRequest},
		{name: "server error", status: 500, body: `{"detail":"Internal Server Error"}`, wantMessage: "Internal Server Error", want: schemas.ErrorTypeProviderServerError},
		{name: "non-JSON body", status: 503, body: `upstream unavailable`, want: schemas.ErrorTypeProviderOverloaded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			resp.SetStatusCode(tc.status)
			resp.Header.SetContentType("application/json")
			resp.SetBodyString(tc.body)

			bifrostErr := parseBFLError(resp)

			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Fatalf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
				t.Fatalf("error = %+v, want a message", bifrostErr.Error)
			}
			if tc.wantMessage != "" && bifrostErr.Error.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", bifrostErr.Error.Message, tc.wantMessage)
			}
			if tc.wantParam != "" && bifrostErr.Error.Param != tc.wantParam {
				t.Errorf("param = %v, want %q", bifrostErr.Error.Param, tc.wantParam)
			}
			if tc.wantCode != "" && (bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode) {
				t.Errorf("code = %v, want %q", bifrostErr.Error.Code, tc.wantCode)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.ImageGenerationRequest); got != tc.want {
				t.Errorf("error_type = %q, want %q", got, tc.want)
			}
		})
	}
}

// BenchmarkBFLUnsupportedOperation measures what a request routed to bfl costs while its
// operations are unsupported: one unsupported-operation error, no network call. The
// constructor runs once per provider config load and is not on the request path.
func BenchmarkBFLUnsupportedOperation(b *testing.B) {
	provider, err := NewBFLProvider(&schemas.ProviderConfig{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(b.Context(), schemas.NoDeadline)
	request := &schemas.BifrostImageGenerationRequest{Provider: schemas.BFL, Model: "flux-2-pro"}
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}
	b.ReportAllocs()
	for b.Loop() {
		if _, bifrostErr := provider.ImageGeneration(ctx, key, request); bifrostErr == nil {
			b.Fatal("want the unsupported-operation error")
		}
	}
}
