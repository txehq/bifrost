package dashscope

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func newTestProvider(t *testing.T, network schemas.NetworkConfig) *DashScopeProvider {
	t.Helper()
	provider, err := NewDashScopeProvider(&schemas.ProviderConfig{NetworkConfig: network}, nil)
	if err != nil {
		t.Fatalf("NewDashScopeProvider: %v", err)
	}
	return provider
}

// TestDashScopeBases pins which base each operation family uses: the Singapore site by
// default, the configured regional base when set, and the workspace base for video only.
func TestDashScopeBases(t *testing.T) {
	const workspace = "https://ws-1.ap-southeast-1.maas.aliyuncs.com"
	cases := []struct {
		name          string
		network       schemas.NetworkConfig
		wantRegional  string
		wantWorkspace string
	}{
		{name: "defaults to the Singapore site", wantRegional: DefaultBaseURL, wantWorkspace: DefaultBaseURL},
		{name: "configured regional base, no workspace base", network: schemas.NetworkConfig{BaseURL: "https://dashscope.example.com/"}, wantRegional: "https://dashscope.example.com", wantWorkspace: "https://dashscope.example.com"},
		{name: "workspace base for video", network: schemas.NetworkConfig{WorkspaceBaseURL: workspace + "/"}, wantRegional: DefaultBaseURL, wantWorkspace: workspace},
	}
	regional := []schemas.RequestType{schemas.ImageGenerationRequest, schemas.ImageEditRequest, schemas.SpeechRequest, schemas.SpeechStreamRequest, schemas.TranscriptionRequest}
	video := []schemas.RequestType{schemas.VideoGenerationRequest, schemas.VideoRetrieveRequest, schemas.VideoDownloadRequest, schemas.VideoListRequest, schemas.VideoDeleteRequest}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProvider(t, tc.network)
			for _, rt := range regional {
				if got := provider.baseURLFor(rt); got != tc.wantRegional {
					t.Errorf("%s base = %q, want %q", rt, got, tc.wantRegional)
				}
			}
			for _, rt := range video {
				if got := provider.baseURLFor(rt); got != tc.wantWorkspace {
					t.Errorf("%s base = %q, want %q", rt, got, tc.wantWorkspace)
				}
			}
		})
	}
}

// TestDashScopeRequestHeaders pins DashScope's auth header, and that an extra header named
// Authorization cannot replace the key.
func TestDashScopeRequestHeaders(t *testing.T) {
	provider := newTestProvider(t, schemas.NetworkConfig{ExtraHeaders: map[string]schemas.SecretVar{
		"Authorization": *schemas.NewSecretVar("Bearer extra"),
		"X-Trace":       *schemas.NewSecretVar("t-1"),
	}})
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	provider.setRequestHeaders(schemas.NewBifrostContext(t.Context(), schemas.NoDeadline), req, schemas.Key{Value: *schemas.NewSecretVar("sk-dash")})

	if got := string(req.Header.Peek("Authorization")); got != "Bearer sk-dash" {
		t.Errorf("Authorization = %q, want the key as a bearer token", got)
	}
	if got := string(req.Header.Peek("X-Trace")); got != "t-1" {
		t.Errorf("X-Trace = %q, want the extra header", got)
	}
}

// TestDashScopeOperationsAreUnsupported pins that every operation answers the
// unsupported-operation error, naming its own request type, without calling upstream.
func TestDashScopeOperationsAreUnsupported(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, WorkspaceBaseURL: server.URL, AllowPrivateNetwork: true})
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
		if bifrostErr.ExtraFields.RequestType == "" || bifrostErr.ExtraFields.Provider != schemas.DashScope {
			t.Errorf("%s: extra fields %+v, want provider dashscope and the request type", method.Name, bifrostErr.ExtraFields)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("unsupported operations made %d upstream calls", n)
	}
}

// TestDashScopeErrorParsing pins that DashScope's documented error bodies keep their status
// and code and land in the right error_type family.
func TestDashScopeErrorParsing(t *testing.T) {
	cases := []struct {
		status      int
		code        string
		requestType schemas.RequestType
		want        schemas.ErrorType
	}{
		{400, "InvalidParameter", schemas.ImageGenerationRequest, schemas.ErrorTypeCallerInvalidRequest},
		{400, "Arrearage", schemas.ImageGenerationRequest, schemas.ErrorTypeProviderBilling},
		{403, "AllocationQuota.FreeTierOnly", schemas.SpeechRequest, schemas.ErrorTypeProviderBilling},
		{401, "InvalidApiKey", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderAuthFailed},
		{403, "AccessDenied.Unpurchased", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderAuthFailed},
		{403, "Model.AccessDenied", schemas.TranscriptionRequest, schemas.ErrorTypeProviderAuthFailed},
		{404, "ModelNotFound", schemas.VideoGenerationRequest, schemas.ErrorTypeCallerModelUnknown},
		{429, "Throttling.RateQuota", schemas.ImageGenerationRequest, schemas.ErrorTypeProviderRateLimited},
		{429, "Throttling.AllocationQuota", schemas.VideoGenerationRequest, schemas.ErrorTypeProviderRateLimited},
		{500, "InternalError.Algo", schemas.SpeechRequest, schemas.ErrorTypeProviderServerError},
		{503, "ServiceUnavailable", schemas.SpeechRequest, schemas.ErrorTypeProviderOverloaded},
		{400, "DataInspectionFailed", schemas.ImageGenerationRequest, schemas.ErrorTypeCallerContentFiltered},
		{400, "data_inspection_failed", schemas.SpeechRequest, schemas.ErrorTypeCallerContentFiltered},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			resp.SetStatusCode(tc.status)
			resp.Header.SetContentType("application/json")
			resp.SetBodyString(`{"request_id":"req-1","code":"` + tc.code + `","message":"vendor says ` + tc.code + `"}`)

			bifrostErr := parseDashScopeError(resp)

			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Errorf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.code {
				t.Errorf("error.code = %+v, want %q verbatim", bifrostErr.Error, tc.code)
			}
			if bifrostErr.Error.Message != "vendor says "+tc.code {
				t.Errorf("error.message = %q, want the vendor's message", bifrostErr.Error.Message)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, tc.requestType); got != tc.want {
				t.Errorf("error_type = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("body without a DashScope shape", func(t *testing.T) {
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(resp)
		resp.SetStatusCode(502)
		resp.SetBodyString("<html>bad gateway</html>")

		bifrostErr := parseDashScopeError(resp)

		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 502 || bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
			t.Fatalf("got %+v, want a 502 with a message", bifrostErr)
		}
		if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoGenerationRequest); got != schemas.ErrorTypeProviderServerError {
			t.Errorf("error_type = %q, want provider_server_error", got)
		}
	})
}
