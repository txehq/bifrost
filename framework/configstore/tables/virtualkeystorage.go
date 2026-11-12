package tables

import (
	"fmt"
	"strings"
)

// Limits on a virtual key's cloud resource allowlists (fork, TXE-7088).
const (
	MaxVirtualKeyAllowedStoragePrefixes = 64
	MaxVirtualKeyAllowedPubSubTopics    = 64
	MaxVirtualKeyAllowedResourceLength  = 1024
)

// virtualKeyStorageSchemes are the object-storage schemes a storage prefix is written in.
var virtualKeyStorageSchemes = [...]string{"gs://", "s3://"}

// NormalizeVirtualKeyStoragePrefix validates one entry of a virtual key's
// allowed_storage_prefixes and returns it in the form it is compared in: the scheme in lower
// case and no trailing slash, so "gs://bucket/tenant-a" and "gs://bucket/tenant-a/" are one
// entry. An entry is "gs://<bucket>[/<folder>...]" or "s3://<bucket>[/<folder>...]". It names a
// bucket or a folder, never a pattern: a wildcard, an empty folder name ("//") and a "." or
// ".." segment are refused, since each would make what the entry covers depend on who reads it.
func NormalizeVirtualKeyStoragePrefix(prefix string) (string, error) {
	trimmed := strings.TrimSpace(prefix)
	if len(trimmed) > MaxVirtualKeyAllowedResourceLength {
		return "", fmt.Errorf("invalid storage prefix %q: at most %d characters", prefix, MaxVirtualKeyAllowedResourceLength)
	}
	scheme := ""
	for _, known := range virtualKeyStorageSchemes {
		if len(trimmed) >= len(known) && strings.EqualFold(trimmed[:len(known)], known) {
			scheme = known
		}
	}
	if scheme == "" {
		return "", fmt.Errorf("invalid storage prefix %q: it must start with gs:// or s3://", prefix)
	}
	rest := strings.TrimRight(trimmed[len(scheme):], "/")
	if rest == "" {
		return "", fmt.Errorf("invalid storage prefix %q: it names no bucket", prefix)
	}
	if strings.ContainsAny(rest, "*?[") {
		return "", fmt.Errorf("invalid storage prefix %q: it names a bucket or a folder, not a pattern", prefix)
	}
	for _, segment := range strings.Split(rest, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid storage prefix %q: a folder name cannot be empty, '.' or '..'", prefix)
		}
	}
	return scheme + rest, nil
}

// NormalizeVirtualKeyPubSubTopic validates one entry of a virtual key's allowed_pubsub_topics
// and returns it as it is compared: a full topic name, "projects/<project>/topics/<topic>".
func NormalizeVirtualKeyPubSubTopic(topic string) (string, error) {
	trimmed := strings.TrimSpace(topic)
	if len(trimmed) > MaxVirtualKeyAllowedResourceLength {
		return "", fmt.Errorf("invalid Pub/Sub topic %q: at most %d characters", topic, MaxVirtualKeyAllowedResourceLength)
	}
	if !IsPubSubTopicName(trimmed) {
		return "", fmt.Errorf("invalid Pub/Sub topic %q: it must be projects/<project>/topics/<topic>", topic)
	}
	return trimmed, nil
}

// IsPubSubTopicName reports whether name has the shape of a full Pub/Sub topic name,
// "projects/<project>/topics/<topic>", with nothing before or after it.
func IsPubSubTopicName(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 4 && parts[0] == "projects" && parts[1] != "" && parts[2] == "topics" && parts[3] != "" && !strings.ContainsAny(name, " \t\r\n*?")
}

// NormalizeVirtualKeyAllowlists validates a virtual key's allowed_storage_prefixes and
// allowed_pubsub_topics and returns them normalized, with duplicates removed and order kept.
// nil or empty lists are valid and come back nil: no list means no restriction.
func NormalizeVirtualKeyAllowlists(storagePrefixes, pubSubTopics []string) ([]string, []string, error) {
	if len(storagePrefixes) > MaxVirtualKeyAllowedStoragePrefixes {
		return nil, nil, fmt.Errorf("allowed_storage_prefixes can have at most %d entries, got %d", MaxVirtualKeyAllowedStoragePrefixes, len(storagePrefixes))
	}
	if len(pubSubTopics) > MaxVirtualKeyAllowedPubSubTopics {
		return nil, nil, fmt.Errorf("allowed_pubsub_topics can have at most %d entries, got %d", MaxVirtualKeyAllowedPubSubTopics, len(pubSubTopics))
	}
	normalize := func(entries []string, one func(string) (string, error)) ([]string, error) {
		var out []string
		seen := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			normalized, err := one(entry)
			if err != nil {
				return nil, err
			}
			if _, dup := seen[normalized]; dup {
				continue
			}
			seen[normalized] = struct{}{}
			out = append(out, normalized)
		}
		return out, nil
	}
	prefixes, err := normalize(storagePrefixes, NormalizeVirtualKeyStoragePrefix)
	if err != nil {
		return nil, nil, err
	}
	topics, err := normalize(pubSubTopics, NormalizeVirtualKeyPubSubTopic)
	if err != nil {
		return nil, nil, err
	}
	return prefixes, topics, nil
}
