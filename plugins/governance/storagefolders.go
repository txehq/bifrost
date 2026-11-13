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
// A request also names storage in untyped data (an extra param, a passthrough body, an inline
// batch item), where nothing says whether the provider reads the object of that name or writes
// beneath it. Such a name is checked both ways, as an object and as a folder, and claims
// nothing. The extra params known to be output locations (outputLocationKey) are locations.
//
// A claim is never released by a request, not even when the request that made it fails: a
// batch's results outlive the batch, other requests of the key may already rely on the
// folder, and withdrawing it under them would leave what they write unclaimed. An operator
// lists and releases claims through /api/governance/storage-folders (TXE-7259), and a
// deleted key's claims stay until one is released: dropping them would hand the folder's
// contents to every key with no storage list.
//
// A bucket root is an output location only for a key with allowed_storage_prefixes, whose list
// then covers it, and that key claims it (TXE-7259). A key with no list is refused with 400:
// claiming the root would take the whole bucket from every other key, and writing there
// unclaimed would leave its results readable by them.
//
// Two keys that claim nested folders in the same instant can both pass the exclusivity check.
// Both claims then stand, and the closest one decides for every object, so neither key reaches
// into the other's folder; only the "nothing of another key beneath" promise is weakened, for
// folders that did not exist a moment earlier. The same folder has one row and one owner.

// maxStorageFolderLevels bounds the folders above one stored object. Each is one id in a
// single lookup; an object nested deeper cannot be checked in full and is refused.
const maxStorageFolderLevels = 64

// maxStorageFolderLength is the longest folder URI a claim can be recorded under
// (provider_objects.object_id).
const maxStorageFolderLength = 512

// storageFolderProvider is the provider name folder rows are recorded under
// (configstoreTables.StorageFolderProvider).
func storageFolderProvider(uri string) string {
	return configstoreTables.StorageFolderProvider(uri)
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
// stored object that lies in another key's folder, and a name the provider may write beneath
// (an output location, a name in untyped data) that another key has records beneath. own says
// the object has a row of its own for the request's key, which settles it as an input. claim
// is the folder the request has to claim before it goes on: an output location no folder of
// the key covers yet.
func (p *GovernancePlugin) refuseForeignStorageFolder(ctx *schemas.BifrostContext, ref providerObjectRef, own bool, vkID string) (claim string, shortCircuit *schemas.LLMPluginShortCircuit) {
	if p.configStore == nil || ref.kind != configstoreTables.ProviderObjectKindFile {
		return "", nil
	}
	// A name the provider may write beneath is a prefix, not a reference to the one object of
	// that name: the key's own file row there does not settle it.
	asFolder := ref.destination || ref.open
	if own && !asFolder {
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
	if asFolder {
		folder = storageFolderOf(uri)
		uri = folder
	}
	if ref.destination {
		if bucket, root := storageBucketRoot(folder); root && !storageRestricted(ctx) {
			// TXE-7259 (A1): a key with no storage list may not write to a bucket root, whether or
			// not it holds a claim there (one made before this rule, or while it had a list).
			// Claiming the root would take the whole bucket from every other key; leaving it
			// unclaimed would leave the key's results readable by them.
			return "", storageFolderRefusal(ctx, 400, fmt.Sprintf("output location '%s' is the root of bucket %s: name a folder inside the bucket, or give the virtual key an allowed_storage_prefixes entry that covers the bucket", ref.requestID, bucket))
		}
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
	if !asFolder {
		return "", nil
	}
	foreign, err := p.foreignRecordBeneath(ctx, folder, vkID)
	if err != nil {
		return "", failed(err)
	}
	if foreign {
		return "", notFound()
	}
	if !ref.destination {
		return "", nil
	}
	return folder, nil
}

// storageBucketRoot reports whether folder is the root of its bucket ("gs://b/", "s3://b/"),
// and names the bucket.
func storageBucketRoot(folder string) (bucket string, root bool) {
	start := strings.Index(folder, "://")
	if start < 0 {
		return "", false
	}
	rest := folder[start+len("://"):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash != len(rest)-1 {
		return "", false
	}
	return folder[:start+len("://")+slash], true
}

// storageRestricted reports whether the request's virtual key has allowed_storage_prefixes
// (TXE-7088); a key that has them can name only storage inside them.
func storageRestricted(ctx *schemas.BifrostContext) bool {
	prefixes, ok := ctx.Value(governanceAllowedStoragePrefixesContextKey).([]string)
	return ok && len(prefixes) > 0
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

// claimStorageFolders records folders, each already found exclusive, for the request's key.
// A folder has one row, and the first writer keeps it: when the row was already there, the
// request goes on only if it is the key's own.
func (p *GovernancePlugin) claimStorageFolders(ctx *schemas.BifrostContext, folders []string, vkID string) *schemas.LLMPluginShortCircuit {
	if len(folders) == 0 || p.configStore == nil {
		return nil
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
		if err == nil && !inserted {
			var owners map[string]string
			if owners, err = p.storageFolderOwners(ctx, []string{folder}); err == nil && owners[folder] != vkID {
				return storageFolderRefusal(ctx, 404, fmt.Sprintf("file '%s' not found", folder))
			}
		}
		if err != nil {
			p.logger.Error("failed to claim the storage folder %s: %v", folder, err)
			return storageFolderRefusal(ctx, 500, fmt.Sprintf("failed to verify access to file '%s'", folder))
		}
	}
	return nil
}

// claimBatchOutputFolder claims, for the batch's recorded creator, the output directory a batch
// retrieve names (Vertex's gcsOutputDirectory, Bedrock's output S3 URI), when nothing claims it
// yet (TXE-7259). A batch created since output folders are claimed at create time lies in its
// key's folder already; this covers one created before, whose results would otherwise have no
// owner. The folder is claimed only when it is exclusive: no folder of any key at or above it,
// no record of another key beneath it, and not a bucket root. Anything less leaves it as it was.
func (p *GovernancePlugin) claimBatchOutputFolder(ctx *schemas.BifrostContext, location, owner string) {
	// Claimed only under enforcement, as at create time: a claim changes what other keys reach.
	if p.configStore == nil || owner == "" || !p.objectOwnershipEnforced() {
		return
	}
	uri, stored := storageURIForm([]string{location})
	if !stored {
		return
	}
	folder := storageFolderOf(uri)
	if _, root := storageBucketRoot(folder); root || len(folder) > maxStorageFolderLength {
		return
	}
	folders, tooDeep := storageFoldersAbove(folder)
	if tooDeep || len(folders) == 0 {
		return
	}
	owners, err := p.storageFolderOwners(ctx, folders)
	if err == nil && len(owners) == 0 {
		// Another key's output at this folder or one above (an earlier batch's output directory,
		// recorded as a file or on its job row, with or without its trailing slash, which no
		// folder claim covers) also stands in the way: the claim would hide what that key could
		// reach until now. The slashless name of the folder itself is not beneath it.
		above := make([]string, 0, 2*len(folders))
		for _, ancestor := range folders {
			above = append(above, ancestor, strings.TrimSuffix(ancestor, "/"))
		}
		var foreign bool
		if foreign, err = p.configStore.HasForeignRecordAt(ctx, above, owner); err == nil && !foreign {
			foreign, err = p.foreignRecordBeneath(ctx, folder, owner)
		}
		if err == nil && !foreign {
			_, err = p.configStore.InsertProviderObject(ctx, &configstoreTables.TableProviderObject{
				ID:           configstoreTables.ProviderObjectID(configstoreTables.ProviderObjectKindStorageFolder, storageFolderProvider(folder), folder),
				Kind:         configstoreTables.ProviderObjectKindStorageFolder,
				Provider:     storageFolderProvider(folder),
				ObjectID:     folder,
				VirtualKeyID: owner,
			})
		}
	}
	if err != nil {
		p.logger.Warn("failed to claim the output folder of a batch for virtual key %s: %v", owner, err)
	}
}
