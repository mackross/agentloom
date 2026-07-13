package xai

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"

	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

func (s *ResponsesStreamer) LastUsedPreviousResponseID() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsedPreviousResponseID
}

func (s *ResponsesStreamer) clearContinuation() {
	s.mu.Lock()
	s.continuation = responseContinuation{}
	s.lastUsedPreviousResponseID = false
	s.mu.Unlock()
}

func (s *ResponsesStreamer) usePreviousResponseID() bool {
	return s.UsePreviousResponseID || !s.DisablePreviousResponseID
}

func (s *ResponsesStreamer) applyPreviousResponseID(params *responses.ResponseNewParams, fullInputItems responses.ResponseInputParam, paramsHash [32]byte) bool {
	s.mu.Lock()
	cont := s.continuation
	s.mu.Unlock()

	if cont.responseID == "" || cont.prefixLen > len(fullInputItems) || cont.paramsHash != paramsHash {
		return false
	}
	prefixHash, err := hashInputItems(fullInputItems[:cont.prefixLen])
	if err != nil || prefixHash != cont.prefixHash {
		return false
	}

	params.PreviousResponseID = openaiapi.String(cont.responseID)
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: fullInputItems[cont.prefixLen:]}
	// Providers (notably xAI) reject instructions together with previous_response_id;
	// the stored response already carries the prior instructions. Leave the hash on
	// the full params (including instructions) so follow-up requests still match.
	params.Instructions = param.Opt[string]{}
	return true
}

func (s *ResponsesStreamer) rememberContinuation(responseID string, inputItems, outputItems responses.ResponseInputParam, paramsHash [32]byte) {
	if responseID == "" {
		return
	}
	prefixHash, err := hashInputItems(inputItems)
	if err != nil {
		return
	}

	s.mu.Lock()
	s.continuation = responseContinuation{
		responseID: responseID,
		prefixHash: prefixHash,
		prefixLen:  len(inputItems),
		paramsHash: paramsHash,
	}
	s.mu.Unlock()
}

func hashResponseParams(params responses.ResponseNewParams) ([32]byte, error) {
	params.Input = responses.ResponseNewParamsInputUnion{}
	params.PreviousResponseID = param.Opt[string]{}
	return hashCanonical(params)
}

func hashInputItems(items responses.ResponseInputParam) ([32]byte, error) {
	h := sha256.New()
	for _, item := range items {
		if err := writeCanonicalHashItem(h, item); err != nil {
			return [32]byte{}, err
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func hashCanonical(v any) ([32]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func writeCanonicalHashItem(h hash.Hash, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write(b)
	return nil
}

func outputItemInputParam(item responses.ResponseOutputItemUnion) (responses.ResponseInputItemUnionParam, bool) {
	if item.ID == "" {
		return responses.ResponseInputItemUnionParam{}, false
	}
	return responses.ResponseInputItemUnionParam{OfItemReference: &responses.ResponseInputItemItemReferenceParam{ID: item.ID, Type: "item_reference"}}, true
}
