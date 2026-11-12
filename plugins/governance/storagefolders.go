package governance

import (
	"fmt"
	"strings"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// Storage folders (TXE-7121).
//
// A stored object is recorded for the virtual key that uploaded it, under its one name. That
// leaves what a provider writes on its own: the result files of a batch, the artifact of a
// task. They are written beneath the output location the request named, they have no rows,
// and any key that learned their names could read them.
//
// The rule:
//
//  1. A virtual key that names a storage location as the place its output is written (a
//     batch's output folder, a task's output_storage_uri) claims it as a folder, before the
//     provider is called. The claim is a row of kind storage_folder whose object id is the
//     location's storage URI ending in "/".
//  2. The claim is made only on an exclusive location: one with no folder of another key at
//     or above it, and no record of another key (a file, a folder, a batch result file)
//     beneath it. Two keys cannot write under one prefix, or one inside the other's. Sibling
//     folders under a shared parent are fine.
//  3. A stored object a request names, as an input or as an output location, that lies in a
//     folder claimed by another key is not found. The closest record decides: an object with
//     a row of its own follows that row, so a key's own upload stays its own wherever it is.
//  4. Objects nobody recorded, outside every claimed folder, stay shared as before. A virtual
//     key's allowed_storage_prefixes (TXE-7088) is the tool for those.
//
// What makes a folder a folder is the row's kind. A file row never answers for names beneath
// it: a key that uploaded an object called "team" does not own "team/...". A doubled or
// trailing slash is literal, as it is in object storage: "s3://b/out//" is a folder inside
// "s3://b/out/". An output location written without its trailing slash is the folder with
// it, since that is where Vertex and Bedrock write.
//
// A claim is never released: a batch's results outlive the batch, and other batches of the
// key share the folder.

// maxStorageFolderLevels bounds the folders above one stored object. Each is one id in a
// single lookup; an object nested deeper cannot be checked in full and is refused.
const maxStorageFolderLevels = 64

// maxStorageFolderLength is the longest folder URI a claim can be recorded under
// (provider_objects.object_id).
const maxStorageFolderLength = 512

// governanceStorageFolderClaimsContextKey carries the ids of the folder rows this request
// wrote, so a request that ends in an error withdraws them: nothing was written there.
const governanceStorageFolderClaimsContextKey schemas.BifrostContextKey = "bf-governance-storage-folder-claims"

// storageFolderProvider is the provider name folder rows are recorded under: the storage
// system, not the provider the request went to. A bucket is the same bucket through every
// provider name configured on it, and one row per folder is what makes a claim exclusive.
func storageFolderProvider(uri string) string {
	if strings.HasPrefix(uri, "gs://") {
		return "gcs"
	}
	return "s3"
}

// storageURIForm returns the storage URI among the forms of an object reference.
func storageURIForm(forms []string) (string, bool) {
	for _, form := range forms {
		for _, scheme := range storageURISchemes {
			if strings.HasPrefix(form, scheme) && len(form) > len(scheme) {
				return form, true
			}
		}
	}
	return "", false
}

// storageFolderOf is the folder an output location names: the location itself when it ends
// in "/", the location with one added otherwise.
func storageFolderOf(location string) string {
	if strings.HasSuffix(location, "/") {
		return location
	}
	return location + "/"
}

// storageFoldersAbove returns the folders a stored object lies in, from the bucket down: every
// prefix of uri that ends in "/" after the scheme. A name that itself ends in "/" is its own
// last folder. tooDeep is true when there are more than maxStorageFolderLevels.
func storageFoldersAbove(uri string) (folders []string, tooDeep bool) {
	start := strings.Index(uri, "://")
	if start < 0 {
		return nil, false
	}
	for i := start + len("://"); i < len(uri); i++ {
		if uri[i] != '/' {
			continue
		}
		if len(folders) == maxStorageFolderLevels {
			return nil, true
		}
		folders = append(folders, uri[:i+1])
	}
	return folders, false
}

// storageFolderRefusal builds the answers of the folder check.
func storageFolderRefusal(ctx *schemas.BifrostContext, status int, message string) *schemas.LLMPluginShortCircuit {
	ctx.SetValue(governanceRejectedContextKey, true)
	shortCircuit := &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
		StatusCode: bifrost.Ptr(status),
		Error:      &schemas.ErrorField{Message: message},
	}}
	if status != 500 {
		shortCircuit.Error.Type = bifrost.Ptr(string(DecisionAccessBlocked))
	}
	return shortCircuit
}

// storageFolderOwners returns the keys that claimed any of folders, by folder.
func (p *GovernancePlugin) storageFolderOwners(ctx *schemas.BifrostContext, folders []string) (map[string]string, error) {
	rows, err := p.configStore.GetProviderObjectsByObjectIDs(ctx, configstoreTables.ProviderObjectKindStorageFolder, folders)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string, len(rows))
	for _, row := range rows {
		if row != nil && row.VirtualKeyID != "" {
			owners[row.ObjectID] = row.VirtualKeyID
		}
	}
	return owners, nil
}

// foreignRecordBeneath reports whether another key has a record under folder: a file or a
// folder row, or a result file named by one of its batch jobs.
func (p *GovernancePlugin) foreignRecordBeneath(ctx *schemas.BifrostContext, folder, vkID string) (bool, error) {
	kinds := []string{configstoreTables.ProviderObjectKindFile, configstoreTables.ProviderObjectKindStorageFolder}
	foreign, err := p.configStore.HasForeignProviderObjectUnderPrefix(ctx, kinds, folder, vkID)
	if err != nil || foreign {
		return foreign, err
	}
	return p.configStore.HasForeignProviderJobFileUnderPrefix(ctx, folder, vkID)
}

// refuseForeignStorageFolder is the folder rule for one referenced object. It refuses a
// stored object that lies in another key's folder, and an output location another key has
// records beneath. own says the object has a row of its own for the request's key, which
// settles it as an input. claim is the folder the request has to claim before it goes on: an
// output location no folder of the key covers yet.
func (p *GovernancePlugin) refuseForeignStorageFolder(ctx *schemas.BifrostContext, ref providerObjectRef, own bool, vkID string) (claim string, shortCircuit *schemas.LLMPluginShortCircuit) {
	if p.configStore == nil || ref.kind != configstoreTables.ProviderObjectKindFile {
		return "", nil
	}
	// A destination is a prefix the provider writes new objects beneath, not a reference to
	// the one object of that name: the key's own file row there does not settle it.
	if own && !ref.destination {
		return "", nil
	}
	uri, stored := storageURIForm(ref.ids)
	if !stored {
		return "", nil
	}
	notFound := func() *schemas.LLMPluginShortCircuit {
		return storageFolderRefusal(ctx, 404, fmt.Sprintf("file '%s' not found", ref.requestID))
	}
	failed := func(err error) *schemas.LLMPluginShortCircuit {
		p.logger.Error("failed to load the storage folders of %s for ownership check: %v", ref.requestID, err)
		return storageFolderRefusal(ctx, 500, fmt.Sprintf("failed to verify access to file '%s'", ref.requestID))
	}
	folder := ""
	if ref.destination {
		folder = storageFolderOf(uri)
		uri = folder
	}
	folders, tooDeep := storageFoldersAbove(uri)
	if tooDeep || len(folder) > maxStorageFolderLength {
		return "", storageFolderRefusal(ctx, 400, fmt.Sprintf("file '%s' cannot be verified for this virtual key: it lies more than %d folders deep, or names an output location longer than %d characters", ref.requestID, maxStorageFolderLevels, maxStorageFolderLength))
	}
	if len(folders) == 0 {
		return "", nil
	}
	owners, err := p.storageFolderOwners(ctx, folders)
	if err != nil {
		return "", failed(err)
	}
	// The closest folder on record decides.
	for i := len(folders) - 1; i >= 0; i-- {
		owner, claimed := owners[folders[i]]
		if !claimed {
			continue
		}
		if owner != vkID {
			return "", notFound()
		}
		return "", nil
	}
	if !ref.destination {
		return "", nil
	}
	foreign, err := p.foreignRecordBeneath(ctx, folder, vkID)
	if err != nil {
		return "", failed(err)
	}
	if foreign {
		return "", notFound()
	}
	return folder, nil
}

// storageFolderLookupChunk bounds the folders read in one lookup of a list page.
const storageFolderLookupChunk = 500

// listedInForeignFolders returns, from a page of listed file ids, the stored objects that
// have no row of their own (owners holds the rows found) and lie in a folder another key
// claimed: the ones a request naming them would not find. A page under one prefix shares its
// folders, so they are read once for the page. An object nested too deep to check is hidden.
func (p *GovernancePlugin) listedInForeignFolders(ctx *schemas.BifrostContext, provider, vkID string, ids []string, owners map[string]string) (map[string]struct{}, error) {
	if p.configStore == nil {
		return nil, nil
	}
	type listed struct {
		id      string
		folders []string
	}
	var objects []listed
	var distinct []string
	seen := make(map[string]struct{})
	var hidden map[string]struct{}
	hide := func(id string) {
		if hidden == nil {
			hidden = make(map[string]struct{})
		}
		hidden[id] = struct{}{}
	}
	for _, id := range ids {
		forms, _ := providerObjectLookupForms(configstoreTables.ProviderObjectKindFile, schemas.ModelProvider(provider), id)
		uri, stored := storageURIForm(forms)
		if !stored {
			continue
		}
		recorded := false
		for _, form := range forms {
			if _, ok := owners[form]; ok {
				recorded = true
				break
			}
		}
		if recorded {
			continue
		}
		folders, tooDeep := storageFoldersAbove(uri)
		if tooDeep {
			hide(id)
			continue
		}
		if len(folders) == 0 {
			continue
		}
		objects = append(objects, listed{id: id, folders: folders})
		for _, folder := range folders {
			if _, dup := seen[folder]; !dup {
				seen[folder] = struct{}{}
				distinct = append(distinct, folder)
			}
		}
	}
	if len(objects) == 0 {
		return hidden, nil
	}
	claimed := make(map[string]string)
	for start := 0; start < len(distinct); start += storageFolderLookupChunk {
		page, err := p.storageFolderOwners(ctx, distinct[start:min(start+storageFolderLookupChunk, len(distinct))])
		if err != nil {
			return nil, err
		}
		for folder, owner := range page {
			claimed[folder] = owner
		}
	}
	if len(claimed) == 0 {
		return hidden, nil
	}
	for _, object := range objects {
		for i := len(object.folders) - 1; i >= 0; i-- {
			if owner, ok := claimed[object.folders[i]]; ok {
				if owner != vkID {
					hide(object.id)
				}
				break
			}
		}
	}
	return hidden, nil
}

// claimStorageFolders records folders, each already found exclusive, for the request's key,
// and checks again once the rows are written: two keys that claimed overlapping folders at
// the same moment each passed the first check, and each now sees the other's row. Both are
// then refused and withdraw what they wrote. The ids written are kept on the context so a
// request the provider goes on to refuse withdraws them too.
func (p *GovernancePlugin) claimStorageFolders(ctx *schemas.BifrostContext, folders []string, vkID string) *schemas.LLMPluginShortCircuit {
	if len(folders) == 0 || p.configStore == nil {
		return nil
	}
	var written []string
	withdraw := func() {
		for _, id := range written {
			if err := p.configStore.DeleteProviderObject(ctx, id); err != nil {
				p.logger.Warn("failed to withdraw the storage folder claim %s: %v", id, err)
			}
		}
	}
	refuse := func(folder string, err error) *schemas.LLMPluginShortCircuit {
		withdraw()
		if err != nil {
			p.logger.Error("failed to claim the storage folder %s: %v", folder, err)
			return storageFolderRefusal(ctx, 500, fmt.Sprintf("failed to verify access to file '%s'", folder))
		}
		return storageFolderRefusal(ctx, 404, fmt.Sprintf("file '%s' not found", folder))
	}
	for _, folder := range folders {
		row := &configstoreTables.TableProviderObject{
			ID:           configstoreTables.ProviderObjectID(configstoreTables.ProviderObjectKindStorageFolder, storageFolderProvider(folder), folder),
			Kind:         configstoreTables.ProviderObjectKindStorageFolder,
			Provider:     storageFolderProvider(folder),
			ObjectID:     folder,
			VirtualKeyID: vkID,
		}
		inserted, err := p.configStore.InsertProviderObject(ctx, row)
		if err != nil {
			return refuse(folder, err)
		}
		if inserted {
			written = append(written, row.ID)
		}
		above, _ := storageFoldersAbove(folder)
		owners, err := p.storageFolderOwners(ctx, above)
		if err != nil {
			return refuse(folder, err)
		}
		for _, level := range above {
			if owner, claimed := owners[level]; claimed && owner != vkID {
				return refuse(folder, nil)
			}
		}
		if owners[folder] != vkID {
			// The row is gone again: another request of this key withdrew it. Nothing
			// covers the folder, so the request does not go on.
			return refuse(folder, nil)
		}
		foreign, err := p.foreignRecordBeneath(ctx, folder, vkID)
		if err != nil {
			return refuse(folder, err)
		}
		if foreign {
			return refuse(folder, nil)
		}
	}
	if len(written) > 0 {
		ctx.SetValue(governanceStorageFolderClaimsContextKey, written)
	}
	return nil
}

// withdrawStorageFolderClaims forgets the folders a request claimed when the request ended in
// an error: the provider wrote nothing there, and a claim nothing stands behind would keep
// other keys out of the folder for good.
func (p *GovernancePlugin) withdrawStorageFolderClaims(ctx *schemas.BifrostContext) {
	written, _ := ctx.Value(governanceStorageFolderClaimsContextKey).([]string)
	if len(written) == 0 || p.configStore == nil {
		return
	}
	ctx.SetValue(governanceStorageFolderClaimsContextKey, []string(nil))
	for _, id := range written {
		if err := p.configStore.DeleteProviderObject(ctx, id); err != nil {
			p.logger.Warn("failed to withdraw the storage folder claim %s: %v", id, err)
		}
	}
}
