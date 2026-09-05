package threads

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
)

// This file centralizes item metadata handling: validation, canonical JSON
// cloning, encoding, decoding, merging, and deep-copying. Item metadata is the
// per-node map carried by PatchItemMetadata and is distinct from tool-result
// Data, which is encoded/decoded in durable.go.
//
// Metadata values are plain JSON data. Every metadata map is clamped (validated
// and deep-cloned) at the thread boundary so callers cannot later mutate queued
// or persisted nested maps, and persisted metadata is always a JSON object.

type metadataPatch struct {
	Set        map[string]any
	DeleteKeys []string
}

func (p metadataPatch) empty() bool {
	return len(p.Set) == 0 && len(p.DeleteKeys) == 0
}

func isDeleteValue(v any) bool {
	_, ok := v.(metadataDeleteValue)
	return ok
}

// normalizeMetadataPatches validates and combines patches in argument order.
// Later operations on the same top-level key win.
func normalizeMetadataPatches(patches []PatchItemMetadata) (metadataPatch, error) {
	var set map[string]any
	deleted := map[string]struct{}{}
	for _, p := range patches {
		values := make(map[string]any, len(p.Metadata))
		var deleteKeys []string
		for key, value := range p.Metadata {
			if isDeleteValue(value) {
				deleteKeys = append(deleteKeys, key)
				continue
			}
			values[key] = value
		}
		cloned, err := clampMetadata(values)
		if err != nil {
			return metadataPatch{}, err
		}
		for _, key := range deleteKeys {
			delete(set, key)
			deleted[key] = struct{}{}
		}
		for key, value := range cloned {
			delete(deleted, key)
			if set == nil {
				set = map[string]any{}
			}
			set[key] = value
		}
	}
	deleteKeys := make([]string, 0, len(deleted))
	for key := range deleted {
		deleteKeys = append(deleteKeys, key)
	}
	sort.Strings(deleteKeys)
	return metadataPatch{Set: set, DeleteKeys: deleteKeys}, nil
}

func applyMetadataPatch(dst map[string]any, patch metadataPatch) map[string]any {
	for _, key := range patch.DeleteKeys {
		delete(dst, key)
	}
	dst = mergeMetadataMaps(dst, patch.Set)
	if len(dst) == 0 {
		return nil
	}
	return dst
}

// clampMetadata validates that meta is a JSON object and returns a deep copy so
// callers cannot later mutate queued nested maps or slices. It returns (nil,
// nil) for an empty map, distinguishing "no metadata" from an explicit patch.
func clampMetadata(meta map[string]any) (map[string]any, error) {
	if len(meta) == 0 {
		return nil, nil
	}
	buf, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("metadata must be JSON-serializable: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("metadata must be a JSON object: %w", err)
	}
	return out, nil
}

// mergeMetadataMaps merges src into dst by top-level key; later values win.
// It preserves a nil destination when src is empty.
func mergeMetadataMaps(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]any{}
	}
	maps.Copy(dst, src)
	return dst
}

// mergeInitialMetadata clamps and merges the variadic initial-metadata patches
// allowed on QueueItem. Each patch must target zero (the queued item itself).
// Empty patches are dropped; later patches win per top-level key.
func mergeInitialMetadata(patches []PatchItemMetadata) (map[string]any, error) {
	var out map[string]any
	for _, p := range patches {
		if p.Target != 0 {
			return nil, fmt.Errorf("initial metadata patch must target the queued item, got target %d", p.Target)
		}
		if len(p.Metadata) == 0 {
			continue
		}
		for _, value := range p.Metadata {
			if isDeleteValue(value) {
				return nil, fmt.Errorf("DeleteValue is not valid initial metadata")
			}
		}
		cloned, err := clampMetadata(p.Metadata)
		if err != nil {
			return nil, err
		}
		out = mergeMetadataMaps(out, cloned)
	}
	return out, nil
}

// encodeMetadata serializes a metadata map to a canonical JSON object string.
// The empty map encodes to "", the wire form for "no metadata".
func encodeMetadata(data map[string]any) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	return string(buf), nil
}

// decodeMetadata parses persisted item metadata. The empty string means no
// metadata. A present value must be a JSON object; an empty object is accepted
// and canonicalized to nil, while null and non-object values fail closed.
func decodeMetadata(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("metadata must be a JSON object")
	}
	if len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

// decodeMetadataPatch parses a durable metadata patch. A patch must carry at
// least one set or delete operation because live empty patches are no-ops and
// never receive a mutation sequence or WAL event.
func decodeMetadataPatch(raw string, deleteKeys []string) (metadataPatch, error) {
	set, err := decodeMetadata(raw)
	if err != nil {
		return metadataPatch{}, err
	}
	seen := make(map[string]struct{}, len(deleteKeys))
	for _, key := range deleteKeys {
		if _, ok := seen[key]; ok {
			return metadataPatch{}, fmt.Errorf("metadata patch contains duplicate delete key %q", key)
		}
		if _, ok := set[key]; ok {
			return metadataPatch{}, fmt.Errorf("metadata patch both sets and deletes key %q", key)
		}
		seen[key] = struct{}{}
	}
	if len(set) == 0 && len(deleteKeys) == 0 {
		return metadataPatch{}, fmt.Errorf("metadata patch must contain a set or delete operation")
	}
	return metadataPatch{Set: set, DeleteKeys: append([]string(nil), deleteKeys...)}, nil
}

// cloneData returns a deep copy of a metadata map suitable for exposing to
// request builders and Req. A shallow map clone is not enough because provider
// metadata (such as Anthropic cache_control) contains nested maps that callers
// must not be able to mutate in place. Values are guaranteed to be JSON data,
// so the JSON round-trip cannot fail; the maps.Clone fallback guards against a
// caller-supplied map surviving clamping.
func cloneData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return maps.Clone(data)
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		return maps.Clone(data)
	}
	return out
}
