package xai

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// xAI's Files API (TXE-6664, contract C8): POST /v1/files takes a multipart form whose only
// documented field is file; a JSONL file uploaded there can be named by a batch's
// input_file_id. The object and list fields are read under OpenAI's names and the alternates
// xAI's other APIs use (file_id, size_bytes, files, pagination_token), since no response
// was recorded (see the TXE-6664 PR).

// XAIFile is xAI's file object.
type XAIFile struct {
	ID        string       `json:"id"`
	FileID    string       `json:"file_id"`
	Object    string       `json:"object"`
	Bytes     int64        `json:"bytes"`
	SizeBytes int64        `json:"size_bytes"`
	Filename  string       `json:"filename"`
	Purpose   string       `json:"purpose"`
	CreatedAt xaiTimestamp `json:"created_at"`
	ExpiresAt xaiTimestamp `json:"expires_at"`
}

// XAIFileListResponse is the body of GET /v1/files.
type XAIFileListResponse struct {
	Data            []XAIFile `json:"data"`
	Files           []XAIFile `json:"files"`
	HasMore         bool      `json:"has_more"`
	PaginationToken string    `json:"pagination_token"`
}

// XAIFileDeleteResponse is the body of DELETE /v1/files/{file_id}.
type XAIFileDeleteResponse struct {
	ID      string `json:"id"`
	FileID  string `json:"file_id"`
	Deleted bool   `json:"deleted"`
}

func (f *XAIFile) id() string {
	if f.ID != "" {
		return f.ID
	}
	return f.FileID
}

func (f *XAIFile) size() int64 {
	if f.Bytes != 0 {
		return f.Bytes
	}
	return f.SizeBytes
}

func (f *XAIFile) createdAt() int64 {
	if f.CreatedAt.unix != nil {
		return *f.CreatedAt.unix
	}
	return 0
}

// ToBifrostFileObject converts an xAI file to the unified file object. purpose is the one
// the caller uploaded with when xAI reports none.
func (f *XAIFile) ToBifrostFileObject(purpose schemas.FilePurpose) schemas.FileObject {
	if f.Purpose != "" {
		purpose = schemas.FilePurpose(f.Purpose)
	}
	object := f.Object
	if object == "" {
		object = "file"
	}
	return schemas.FileObject{
		ID:        f.id(),
		Object:    object,
		Bytes:     f.size(),
		CreatedAt: f.createdAt(),
		Filename:  f.Filename,
		Purpose:   purpose,
		Status:    schemas.FileStatusProcessed,
		ExpiresAt: firstTime(f.ExpiresAt),
	}
}

// FileUpload uploads a file with POST /v1/files as multipart: the request's purpose, which
// xAI stores and echoes for OpenAI compatibility, then the file part.
func (provider *XAIProvider) FileUpload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostFileUploadRequest) (*schemas.BifrostFileUploadResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.FileUploadRequest); err != nil {
		return nil, err
	}
	if request == nil || len(request.File) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("file content is required")
	}
	if request.ExpiresAfter != nil {
		return nil, schemas.NewUnsupportedParameterError("expires_after", "")
	}
	if request.StorageConfig != nil {
		return nil, schemas.NewUnsupportedParameterError("storage_config", "")
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	filename := request.Filename
	if filename == "" {
		filename = "batch_requests.jsonl"
	}
	if request.Purpose != "" {
		if err := writer.WriteField("purpose", string(request.Purpose)); err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to write purpose field", err)
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to create form file", err)
	}
	if _, err := part.Write(request.File); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to write file content", err)
	}
	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to close multipart writer", err)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.collectionURL(ctx, "/v1/files", schemas.FileUploadRequest))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	req.SetBody(buf.Bytes())

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		return nil, providerUtils.SetErrorLatency(ParseXAIError(resp), latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	decoded := &xaiResponse{
		body:    append([]byte(nil), body...),
		headers: providerUtils.ExtractProviderResponseHeaders(resp),
		latency: latency,
	}
	var file XAIFile
	// The multipart body is not a JSON request, so no raw request is attached.
	extra, bifrostErr := decodeXAI(ctx, provider, decoded, nil, &file)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if file.Filename == "" {
		file.Filename = filename
	}
	if file.size() == 0 {
		file.Bytes = int64(len(request.File))
	}
	obj := file.ToBifrostFileObject(request.Purpose)
	return &schemas.BifrostFileUploadResponse{
		ID:             obj.ID,
		Object:         obj.Object,
		Bytes:          obj.Bytes,
		CreatedAt:      obj.CreatedAt,
		Filename:       obj.Filename,
		Purpose:        obj.Purpose,
		Status:         obj.Status,
		ExpiresAt:      obj.ExpiresAt,
		StorageBackend: schemas.FileStorageAPI,
		ExtraFields:    extra,
	}, nil
}

// FileList lists files, paged by xAI's pagination_token.
func (provider *XAIProvider) FileList(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileListRequest) (*schemas.BifrostFileListResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.FileListRequest); err != nil {
		return nil, err
	}
	if request == nil {
		request = &schemas.BifrostFileListRequest{}
	}
	helper, err := providerUtils.NewSerialListHelper(keys, request.After, provider.logger, true)
	if err != nil {
		return nil, providerUtils.NewBifrostBadRequestError("invalid pagination cursor: " + err.Error())
	}
	key, nativeCursor, ok := helper.GetCurrentKey()
	if !ok {
		return &schemas.BifrostFileListResponse{Object: "list", Data: []schemas.FileObject{}}, nil
	}
	query := url.Values{}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Order != nil && *request.Order != "" {
		query.Set("order", *request.Order)
	}
	if nativeCursor != "" {
		query.Set("pagination_token", nativeCursor)
	}
	resp, bifrostErr := provider.do(ctx, key, http.MethodGet, provider.collectionURL(ctx, "/v1/files", schemas.FileListRequest), query, nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var page XAIFileListResponse
	extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &page)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	files := page.Data
	if len(files) == 0 {
		files = page.Files
	}
	data := make([]schemas.FileObject, 0, len(files))
	for i := range files {
		obj := files[i].ToBifrostFileObject("")
		if request.Purpose != "" && obj.Purpose != "" && obj.Purpose != request.Purpose {
			continue
		}
		data = append(data, obj)
	}
	next, hasMore := helper.BuildNextCursor(page.PaginationToken != "", page.PaginationToken)
	out := &schemas.BifrostFileListResponse{Object: "list", Data: data, HasMore: hasMore, ExtraFields: extra}
	if next != "" {
		out.After = schemas.Ptr(next)
	}
	return out, nil
}

// FileRetrieve reads one file's metadata.
func (provider *XAIProvider) FileRetrieve(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.FileRetrieveRequest); err != nil {
		return nil, err
	}
	escapedID, idErr := xaiFileID(request)
	if idErr != nil {
		return nil, idErr
	}
	requestURL, urlErr := provider.resourceURL("/v1/files/", escapedID, "", schemas.FileRetrieveRequest)
	if urlErr != nil {
		return nil, urlErr
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
		resp, bifrostErr := provider.do(ctx, key, http.MethodGet, requestURL, nil, nil)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		var file XAIFile
		extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &file)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		obj := file.ToBifrostFileObject("")
		return &schemas.BifrostFileRetrieveResponse{
			ID:             obj.ID,
			Object:         obj.Object,
			Bytes:          obj.Bytes,
			CreatedAt:      obj.CreatedAt,
			Filename:       obj.Filename,
			Purpose:        obj.Purpose,
			Status:         obj.Status,
			ExpiresAt:      obj.ExpiresAt,
			StorageBackend: schemas.FileStorageAPI,
			ExtraFields:    extra,
		}, nil
	})
}

// FileDelete deletes a file.
func (provider *XAIProvider) FileDelete(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.FileDeleteRequest); err != nil {
		return nil, err
	}
	if request == nil || request.FileID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("file_id is required")
	}
	escapedID, idErr := providerUtils.EscapeResourceID(request.FileID, "file_id")
	if idErr != nil {
		return nil, idErr
	}
	requestURL, urlErr := provider.resourceURL("/v1/files/", escapedID, "", schemas.FileDeleteRequest)
	if urlErr != nil {
		return nil, urlErr
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
		resp, bifrostErr := provider.do(ctx, key, http.MethodDelete, requestURL, nil, nil)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		out := &schemas.BifrostFileDeleteResponse{ID: request.FileID, Object: "file", Deleted: true}
		if len(bytes.TrimSpace(resp.body)) == 0 {
			out.ExtraFields = schemas.BifrostResponseExtraFields{Latency: resp.latency.Milliseconds(), ProviderResponseHeaders: resp.headers}
			return out, nil
		}
		var deleted XAIFileDeleteResponse
		extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &deleted)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		// A 2xx is the deletion; a body that says deleted:false is believed.
		if strings.Contains(string(resp.body), `"deleted"`) {
			out.Deleted = deleted.Deleted
		}
		out.ExtraFields = extra
		return out, nil
	})
}

// FileContent downloads a file's bytes from /v1/files/{file_id}/content.
func (provider *XAIProvider) FileContent(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileContentRequest) (*schemas.BifrostFileContentResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.FileContentRequest); err != nil {
		return nil, err
	}
	if request == nil || request.FileID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("file_id is required")
	}
	escapedID, idErr := providerUtils.EscapeResourceID(request.FileID, "file_id")
	if idErr != nil {
		return nil, idErr
	}
	requestURL, urlErr := provider.resourceURL("/v1/files/", escapedID, "/content", schemas.FileContentRequest)
	if urlErr != nil {
		return nil, urlErr
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostFileContentResponse, *schemas.BifrostError) {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodGet)
		if value := key.Value.GetValue(); value != "" {
			req.Header.Set("Authorization", "Bearer "+value)
		}
		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		defer wait()
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
			return nil, providerUtils.SetErrorLatency(ParseXAIError(resp), latency)
		}
		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
		}
		contentType := string(resp.Header.ContentType())
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return &schemas.BifrostFileContentResponse{
			FileID:      request.FileID,
			Content:     append([]byte(nil), body...),
			ContentType: contentType,
			ExtraFields: schemas.BifrostResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerUtils.ExtractProviderResponseHeaders(resp),
			},
		}, nil
	})
}

func xaiFileID(request *schemas.BifrostFileRetrieveRequest) (string, *schemas.BifrostError) {
	if request == nil || request.FileID == "" {
		return "", providerUtils.NewBifrostBadRequestError("file_id is required")
	}
	return providerUtils.EscapeResourceID(request.FileID, "file_id")
}
