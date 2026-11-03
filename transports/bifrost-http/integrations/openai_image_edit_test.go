package integrations

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"testing"

	"github.com/capsohq/bifrost/core/providers/openai"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func jsonEditCtx(body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBody([]byte(body))
	return ctx
}

// OpenAI's JSON image edit: image URLs are carried, data: URIs decoded, and what Bifrost has
// no form for refused.
func TestParseOpenAIImageEditJSON(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG-bytes"))
	req := &openai.OpenAIImageEditRequest{}
	require.NoError(t, parseOpenAIImageEditRequest(jsonEditCtx(`{"model":"openai/gpt-image-2","prompt":"add a hat","n":2,"size":"1024x1024","quality":"low",
		"images":[{"image_url":"https://example.com/a.png"},{"image_url":"data:image/png;base64,`+png+`"}],
		"mask":{"image_url":"data:image/png;base64,`+png+`"},"stream":true}`), req))
	require.Equal(t, "openai/gpt-image-2", req.Model)
	require.Equal(t, "add a hat", req.Input.Prompt)
	require.Len(t, req.Input.Images, 2)
	require.Equal(t, "https://example.com/a.png", req.Input.Images[0].URL)
	require.Equal(t, []byte("\x89PNG-bytes"), req.Input.Images[1].Image)
	require.Equal(t, []byte("\x89PNG-bytes"), req.Mask)
	require.Equal(t, 2, *req.N)
	require.Equal(t, "low", *req.Quality)
	require.True(t, *req.Stream)

	for name, body := range map[string]string{
		"no model":        `{"prompt":"x","images":[{"image_url":"https://e/a.png"}]}`,
		"no images":       `{"model":"m","prompt":"x","images":[]}`,
		"a file id":       `{"model":"m","prompt":"x","images":[{"file_id":"file-1"}]}`,
		"a mask by URL":   `{"model":"m","prompt":"x","images":[{"image_url":"https://e/a.png"}],"mask":{"image_url":"https://e/m.png"}}`,
		"a raw data: URI": `{"model":"m","prompt":"x","images":[{"image_url":"data:image/png,raw"}]}`,
		"a body not JSON": `{"model":`,
	} {
		require.Error(t, parseOpenAIImageEditRequest(jsonEditCtx(body), &openai.OpenAIImageEditRequest{}), name)
	}
}

// On the deployment route the path names the model, so a form without one is read.
func TestParseOpenAIImageEditWithModelInPath(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "add a hat")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write([]byte("\x89PNG"))
	_ = mw.Close()
	newCtx := func() *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.Header.SetContentType(mw.FormDataContentType())
		ctx.Request.SetBody(buf.Bytes())
		return ctx
	}
	require.Error(t, parseOpenAIImageEditRequest(newCtx(), &openai.OpenAIImageEditRequest{}))
	req := &openai.OpenAIImageEditRequest{}
	require.NoError(t, parseOpenAIImageEdit(newCtx(), req, false))
	require.Equal(t, "", req.Model)
	require.Len(t, req.Input.Images, 1)

	variation := &openai.OpenAIImageVariationRequest{}
	require.NoError(t, parseOpenAIImageVariation(newCtx(), variation, false))
	require.Error(t, parseOpenAIImageVariationMultipartRequest(newCtx(), &openai.OpenAIImageVariationRequest{}))
}
