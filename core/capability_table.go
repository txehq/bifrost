package bifrost

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// CapabilityEntry is one row of the capability table of this build: whether a provider
// type implements a request type for the upstream models whose id starts with
// ModelPrefix, and which request parameters its driver drops without sending them
// upstream. A gateway uses it to keep a request that sets a parameter away from a
// provider that would ignore it.
type CapabilityEntry struct {
	ProviderType  schemas.ModelProvider `json:"provider_type"`
	RequestType   schemas.RequestType   `json:"request_type"`
	ModelPrefix   string                `json:"model_prefix"`
	Implemented   bool                  `json:"implemented"`
	IgnoredParams []string              `json:"ignored_params"`
}

// capabilityValues are the values each parameter is probed with. A driver may replace a
// value its upstream does not list with its default, and may write its default when the
// parameter is unset, so a parameter is ignored only when none of its values changes the
// request.
var capabilityValues = map[string][]string{
	"dimensions": {"256", "1024"},
	"n":          {"2", "3"},
	"size":       {"1024x1024", "1024x1536", "1536x1024", "1280x720", "720x1280"},
	"quality":    {"low", "medium", "high", "standard", "hd"},
	"seconds":    {"4", "8", "12"},
	"audio":      {"true", "false"},
}

// capabilityParams are the parameters probed for each request type: the ones that change
// what a request produces or costs.
var capabilityParams = map[schemas.RequestType][]string{
	schemas.EmbeddingRequest:       {"dimensions"},
	schemas.ImageGenerationRequest: {"n", "size", "quality"},
	schemas.ImageEditRequest:       {"n", "size", "quality"},
	schemas.ImageVariationRequest:  {"n", "size"},
	schemas.VideoGenerationRequest: {"seconds", "size", "audio"},
	schemas.VideoRemixRequest:      nil,
	schemas.DecisionRequest:        nil,
}

// CapabilityTable runs every case of capabilityCases against a recording proxy: each
// driver reaches its real upstream host, the proxy records what it sent and answers
// without forwarding. A request type is implemented unless the driver refuses it as an
// unsupported operation. A parameter is ignored when, for every one of its probe values,
// the requests sent equal the requests sent without it, apart from the fields that differ
// between two identical requests (random ids, timestamps). A case whose request reaches
// no upstream is an error: the table would otherwise state a capability nobody observed.
func CapabilityTable(ctx context.Context) ([]CapabilityEntry, error) {
	proxy, err := startRecordingProxy()
	if err != nil {
		return nil, err
	}
	defer proxy.Close()
	sa, err := capabilityServiceAccount(proxy.URL() + "/token")
	if err != nil {
		return nil, err
	}
	b := &Bifrost{logger: NewNoOpLogger()}
	var out []CapabilityEntry
	for _, c := range capabilityCases {
		config := &schemas.ProviderConfig{
			ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 1, BufferSize: 1},
			ProxyConfig: &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(proxy.URL()),
				CACertPEM: schemas.NewSecretVar(proxy.caPEM)},
		}
		config.CheckAndSetDefaults()
		config.NetworkConfig.DefaultRequestTimeoutInSeconds, config.NetworkConfig.MaxRetries = 10, 0
		config.NetworkConfig.CACertPEM = schemas.NewSecretVar(proxy.caPEM)
		provider, err := b.createBaseProvider(c.provider, config)
		if err != nil {
			return nil, fmt.Errorf("capability table: create %s: %w", c.provider, err)
		}
		key := capabilityKey(c.provider, sa)
		for _, m := range c.models {
			entry, err := probeCapability(ctx, proxy, provider, key, c.provider, c.request, m)
			if err != nil {
				return nil, fmt.Errorf("capability table: %s %s %q: %w", c.provider, c.request, m.model, err)
			}
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ProviderType != b.ProviderType {
			return a.ProviderType < b.ProviderType
		}
		if a.RequestType != b.RequestType {
			return a.RequestType < b.RequestType
		}
		return a.ModelPrefix < b.ModelPrefix
	})
	return out, nil
}

// probeCapability is one (provider type, request type, model family) row.
func probeCapability(ctx context.Context, proxy *recordingProxy, provider schemas.Provider, key schemas.Key,
	providerType schemas.ModelProvider, request schemas.RequestType, m capabilityModel) (CapabilityEntry, error) {
	entry := CapabilityEntry{ProviderType: providerType, RequestType: request, ModelPrefix: m.prefix, IgnoredParams: []string{}}
	run := func(param, value string) ([]recordedRequest, *schemas.BifrostError) {
		proxy.take()
		bctx, cancel := schemas.NewBifrostContextWithTimeout(ctx, 20*time.Second)
		defer cancel()
		berr := invokeCapability(bctx, provider, key, providerType, request, m, param, value)
		return proxy.take(), berr
	}
	first, berr := run("", "")
	if isUnsupportedOperation(berr) {
		return entry, nil
	}
	if len(first) == 0 {
		return entry, fmt.Errorf("the request reached no upstream: %s", describeBifrostError(berr))
	}
	entry.Implemented = true
	second, _ := run("", "")
	base := requestFacts(first)
	volatile := map[string]bool{}
	for k, v := range requestFacts(second) {
		if base[k] != v {
			volatile[k] = true
		}
	}
	for k := range base {
		if _, ok := requestFacts(second)[k]; !ok {
			volatile[k] = true
		}
	}
	for _, param := range capabilityParams[request] {
		ignored := true
		for _, value := range capabilityValues[param] {
			sent, _ := run(param, value)
			// A value the driver refuses before sending, or one that changes the request,
			// shows the driver reads the parameter.
			if len(sent) == 0 || !sameFacts(base, requestFacts(sent), volatile) {
				ignored = false
				break
			}
		}
		if ignored {
			entry.IgnoredParams = append(entry.IgnoredParams, param)
		}
	}
	return entry, nil
}

func isUnsupportedOperation(err *schemas.BifrostError) bool {
	return err != nil && err.Error != nil && err.Error.Code != nil && *err.Error.Code == "unsupported_operation"
}

func describeBifrostError(err *schemas.BifrostError) string {
	if err == nil || err.Error == nil {
		return "no error"
	}
	return err.Error.Message
}

// invokeCapability sends one request of the type with the probe inputs, and param set to
// value (param "" sets nothing).
func invokeCapability(ctx *schemas.BifrostContext, provider schemas.Provider, key schemas.Key, providerType schemas.ModelProvider,
	request schemas.RequestType, m capabilityModel, param, value string) *schemas.BifrostError {
	model := m.model
	number, _ := strconv.Atoi(value)
	n, size, quality := &number, &value, &value
	switch request {
	case schemas.EmbeddingRequest:
		text := "a capability probe"
		params := &schemas.EmbeddingParameters{}
		if param == "dimensions" {
			params.Dimensions = &number
		}
		_, err := provider.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{Provider: providerType, Model: model,
			Input:  []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
			Params: params})
		return err
	case schemas.ImageGenerationRequest:
		params := &schemas.ImageGenerationParameters{}
		switch param {
		case "n":
			params.N = n
		case "size":
			params.Size = size
		case "quality":
			params.Quality = quality
		}
		_, err := provider.ImageGeneration(ctx, key, &schemas.BifrostImageGenerationRequest{Provider: providerType, Model: model,
			Input: &schemas.ImageGenerationInput{Prompt: "a lighthouse at dusk"}, Params: params})
		return err
	case schemas.ImageEditRequest:
		params := &schemas.ImageEditParameters{}
		if m.editType != "" {
			params.Type = &m.editType
		}
		switch param {
		case "n":
			params.N = n
		case "size":
			params.Size = size
		case "quality":
			params.Quality = quality
		}
		_, err := provider.ImageEdit(ctx, key, &schemas.BifrostImageEditRequest{Provider: providerType, Model: model,
			Input: &schemas.ImageEditInput{Images: []schemas.ImageInput{{Image: capabilityPNG}}, Prompt: "add a boat"}, Params: params})
		return err
	case schemas.ImageVariationRequest:
		params := &schemas.ImageVariationParameters{}
		switch param {
		case "n":
			params.N = n
		case "size":
			params.Size = size
		}
		_, err := provider.ImageVariation(ctx, key, &schemas.BifrostImageVariationRequest{Provider: providerType, Model: model,
			Input: &schemas.ImageVariationInput{Image: schemas.ImageInput{Image: capabilityPNG}}, Params: params})
		return err
	case schemas.VideoGenerationRequest:
		params := &schemas.VideoGenerationParameters{}
		switch param {
		case "seconds":
			params.Seconds = &value
		case "size":
			params.Size = value
		case "audio":
			audio := value == "true"
			params.Audio = &audio
		}
		_, err := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{Provider: providerType, Model: model,
			Input: &schemas.VideoGenerationInput{Prompt: "a road at dawn"}, Params: params})
		return err
	case schemas.VideoRemixRequest:
		_, err := provider.VideoRemix(ctx, key, &schemas.BifrostVideoRemixRequest{ID: "capability-probe:" + string(providerType),
			Provider: providerType, Input: &schemas.VideoGenerationInput{Prompt: "make it night"}})
		return err
	case schemas.DecisionRequest:
		text, name := "The order arrived broken.", "refund"
		_, err := provider.Decision(ctx, key, &schemas.BifrostDecisionRequest{Provider: providerType, Model: model,
			Input: schemas.DecisionInput{Text: &text}, Questions: []schemas.DecisionQuestion{{Type: schemas.DecisionTypePredicate, Name: &name,
				Instructions: &schemas.DecisionText{Text: &text}}}})
		return err
	}
	panic("capability table: no probe for request type " + string(request))
}

// requestFacts flattens a request sequence into comparable facts: each request's method,
// host, path, query values and body fields (JSON leaves, form values, multipart parts with
// files by digest).
func requestFacts(reqs []recordedRequest) map[string]string {
	out := map[string]string{}
	for i, r := range reqs {
		p := fmt.Sprintf("%d.", i)
		out[p+"method"], out[p+"host"], out[p+"path"] = r.Method, r.Host, r.Path
		q, _ := url.ParseQuery(r.RawQuery)
		for k, vs := range q {
			out[p+"query."+k] = strings.Join(vs, ",")
		}
		bodyFacts(out, p+"body", r.ContentType, r.Body)
	}
	return out
}

func bodyFacts(out map[string]string, prefix, contentType string, body []byte) {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	trimmed := bytes.TrimSpace(body)
	switch {
	case mediaType == "multipart/form-data":
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for i := 0; ; i++ {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			value, _ := io.ReadAll(part)
			name := fmt.Sprintf("%s.part.%s", prefix, part.FormName())
			if part.FileName() != "" {
				sum := sha256.Sum256(value)
				out[name] = "file:" + hex.EncodeToString(sum[:])
			} else {
				out[name] = string(value)
			}
		}
	case mediaType == "application/x-www-form-urlencoded":
		q, _ := url.ParseQuery(string(body))
		for k, vs := range q {
			out[prefix+".form."+k] = strings.Join(vs, ",")
		}
	case len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed):
		var v any
		_ = json.Unmarshal(trimmed, &v)
		jsonFacts(out, prefix, v)
	case len(body) > 0:
		sum := sha256.Sum256(body)
		out[prefix] = "raw:" + hex.EncodeToString(sum[:])
	}
}

func jsonFacts(out map[string]string, prefix string, v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			jsonFacts(out, prefix+"."+k, child)
		}
	case []any:
		out[prefix+".#"] = fmt.Sprint(len(t))
		for i, child := range t {
			jsonFacts(out, fmt.Sprintf("%s.%d", prefix, i), child)
		}
	default:
		raw, _ := json.Marshal(t)
		out[prefix] = string(raw)
	}
}

func sameFacts(a, b map[string]string, volatile map[string]bool) bool {
	for k, v := range a {
		if !volatile[k] && b[k] != v {
			return false
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && !volatile[k] {
			return false
		}
	}
	return true
}

// capabilityPNG is the input image of the edit and variation probes: 64×64, opaque.
var capabilityPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// capabilityServiceAccount is a Google service account whose token endpoint is tokenURL.
func capabilityServiceAccount(tokenURL string) (string, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}
	sa, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "capability-table", "private_key_id": "capability-table",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "capability-table@capability-table.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": tokenURL,
	})
	return string(sa), err
}
