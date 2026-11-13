package handlers

import (
	"errors"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/valyala/fasthttp"
)

// Storage folder claims (TXE-7259).
//
// A virtual key claims an output folder in object storage the first time it names it as the
// place its output is written (TXE-7121); the folder and everything in it are then that key's.
// A request never releases a claim. These two routes are how an operator sees and releases
// them: a claim left by a deleted key, or one a key took on a folder other keys need.

// StorageFolderClaim is one claimed folder as the management API shows it.
type StorageFolderClaim struct {
	// Folder is the claimed folder's storage URI, ending in "/".
	Folder       string `json:"folder"`
	VirtualKeyID string `json:"virtual_key_id"`
	CreatedAt    string `json:"created_at"`
}

// getStorageFolders lists claimed folders in byte order of their URI, optionally those of one
// virtual key (virtual_key_id) or under one URI prefix (prefix, matched literally). A caller
// with a row scope (Enterprise DAC) sees the claims of the virtual keys its scope shows it.
func (h *GovernanceHandler) getStorageFolders(ctx *fasthttp.RequestCtx) {
	query := configstore.ProviderObjectQuery{
		Kind:         configstoreTables.ProviderObjectKindStorageFolder,
		VirtualKeyID: strings.TrimSpace(string(ctx.QueryArgs().Peek("virtual_key_id"))),
		Prefix:       string(ctx.QueryArgs().Peek("prefix")),
	}
	for name, target := range map[string]*int{"limit": &query.Limit, "offset": &query.Offset} {
		raw := string(ctx.QueryArgs().Peek(name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Invalid "+name+" parameter: must be a non-negative number")
			return
		}
		*target = n
	}
	query.Limit, query.Offset = ClampPaginationParams(query.Limit, query.Offset)
	rows, total, err := h.configStore.ListProviderObjects(ctx, query)
	if err != nil {
		logger.Error("failed to list storage folder claims: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to list storage folder claims")
		return
	}
	claims := make([]StorageFolderClaim, 0, len(rows))
	for _, row := range rows {
		claims = append(claims, StorageFolderClaim{Folder: row.ObjectID, VirtualKeyID: row.VirtualKeyID, CreatedAt: row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")})
	}
	SendJSON(ctx, map[string]interface{}{
		"storage_folders": claims,
		"count":           len(claims),
		"total_count":     total,
		"limit":           query.Limit,
		"offset":          query.Offset,
	})
}

// deleteStorageFolder releases the claim on one folder (folder, its exact URI). What lies in
// the folder is then reachable as any unclaimed storage is, and the next key to name the
// folder as an output location claims it.
func (h *GovernanceHandler) deleteStorageFolder(ctx *fasthttp.RequestCtx) {
	folder := string(ctx.QueryArgs().Peek("folder"))
	if folder == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "folder is required")
		return
	}
	// A caller with a row scope (Enterprise DAC) releases only a claim of a virtual key its scope
	// shows it; any other reads as not found.
	released, err := h.configStore.ReleaseProviderObject(ctx, configstoreTables.ProviderObjectKindStorageFolder, folder)
	if errors.Is(err, configstore.ErrNotFound) {
		SendError(ctx, fasthttp.StatusNotFound, "Storage folder claim not found")
		return
	}
	if err != nil {
		logger.Error("failed to release the storage folder claim: %v", err)
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to release the storage folder claim")
		return
	}
	logger.Info("released the storage folder claim of virtual key %s", released)
	SendJSON(ctx, map[string]interface{}{
		"message":        "Storage folder claim released",
		"folder":         folder,
		"virtual_key_id": released,
	})
}
