package streamerutil

import "github.com/mackross/agentloom/threads"

func ItemMetadata(req threads.Req, index int) map[string]any {
	if index < 0 || index >= len(req.ItemMeta) {
		return nil
	}
	return req.ItemMeta[index]
}

func LastMetadataValue(req threads.Req, key string) (any, bool) {
	var value any
	set := false
	for _, meta := range req.ItemMeta {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		if raw == nil || raw == false {
			value, set = nil, false
			continue
		}
		value, set = raw, true
	}
	return value, set
}

func LastStringMetadata(req threads.Req, key string) (string, bool) {
	value, ok := LastMetadataValue(req, key)
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	return s, ok && s != ""
}
