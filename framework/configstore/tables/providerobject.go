package tables

import "time"

// Provider object kinds: the families of provider-side objects whose creating
// virtual key is recorded so later requests can be bound to it.
const (
	ProviderObjectKindBatch         = "batch"
	ProviderObjectKindVideo         = "video"
	ProviderObjectKindFile          = "file"
	ProviderObjectKindContainer     = "container"
	ProviderObjectKindCachedContent = "cached_content"
	ProviderObjectKindContainerFile = "container_file"
	ProviderObjectKindLiveSession   = "live_session"
	ProviderObjectKindImageTask     = "image_task" // fork (TXE-6431): a task of the image task routes
	// ProviderObjectKindStorageFolder (fork, TXE-7121) is a folder in object storage a virtual
	// key writes its output into, recorded when the key first names it as an output location.
	// Its object id is the folder's storage URI ending in "/". Unlike a file row it answers
	// for every object beneath it that has no closer record of its own.
	ProviderObjectKindStorageFolder = "storage_folder"
)

// TableProviderObject records which virtual key created a provider-side object.
//
// Objects such as files, videos, containers and cached contents are created
// through the operator's provider key, which every virtual key allowed on the
// provider shares, and are afterwards addressed by their provider-side id alone.
// This row is the only place the gateway remembers who created one. It is written
// once, when the create response comes back, and removed when the object is
// deleted through the gateway. Batches and videos also have a TableProviderJob
// row for accounting; this table is the ownership record for every kind.
type TableProviderObject struct {
	ID           string    `gorm:"primaryKey;type:varchar(768)" json:"id"`
	Kind         string    `gorm:"type:varchar(50);not null;index:idx_provider_objects_kind_provider,priority:1" json:"kind"`
	Provider     string    `gorm:"type:varchar(255);not null;index:idx_provider_objects_kind_provider,priority:2" json:"provider"`
	ObjectID     string    `gorm:"type:varchar(512);not null" json:"object_id"`
	VirtualKeyID string    `gorm:"type:varchar(255);not null;index:idx_provider_objects_virtual_key_id" json:"virtual_key_id"`
	CreatedAt    time.Time `gorm:"not null" json:"created_at"`
}

// TableName returns the backing table name.
func (TableProviderObject) TableName() string {
	return "provider_objects"
}

// ProviderObjectID builds the stable primary key for a provider object from its
// kind, provider and provider-side id.
func ProviderObjectID(kind, provider, objectID string) string {
	return kind + ":" + provider + ":" + objectID
}
