package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

// The stream event types the ledger summary is built from.
const (
	eventMessageStart      = "message_start"
	eventContentBlockStart = "content_block_start"
	eventMessageDelta      = "message_delta"
	eventMessageStop       = "message_stop"
)

// sseDataPrefix introduces the JSON payload of a server-sent event.
const sseDataPrefix = "data:"

// eventStreamType is the content type of a streaming Anthropic response.
const eventStreamType = "text/event-stream"

// usage is the token accounting Anthropic reports.
type usage struct {
	InputTokens                *int64
	OutputTokens               *int64
	CacheCreationInputTokens   *int64
	CacheReadInputTokens       *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	Cost                       json.RawMessage
}

// contentBlock is one block of a response, of which only the type matters here.
type contentBlock struct {
	Type string
}

// summary is what a captured response tells the ledger: the shape it came back
// in, what it cost, and why it stopped.
type summary struct {
	responseID string

	inputTokens                       int64
	inputTokensPresent                bool
	outputTokens                      int64
	outputTokensPresent               bool
	cacheCreationInputTokens          int64
	cacheCreationInputTokensPresent   bool
	cacheReadInputTokens              int64
	cacheReadInputTokensPresent       bool
	cacheCreation5mInputTokens        int64
	cacheCreation5mInputTokensPresent bool
	cacheCreation1hInputTokens        int64
	cacheCreation1hInputTokensPresent bool

	providerCost        accounting.NanoUSD
	providerCostPresent bool
	providerCostValid   bool

	blockTypes []string
	stopReason string
	complete   bool
}

// summarize reads a captured response body. It reports whatever it can
// understand and nothing it cannot: a truncated, unparsable or empty body
// yields an empty summary rather than an error, because a record is written
// either way.
func summarize(body []byte, contentType string) summary {
	if strings.Contains(strings.ToLower(contentType), eventStreamType) {
		return summarizeStream(body)
	}

	return summarizeMessage(body)
}

// summarizeStream walks a captured event stream in order. Output tokens are
// taken from the last usage reported, since a stream revises the count as it
// goes and only the final figure is the real one.
func summarizeStream(body []byte) summary {
	result := emptySummary()
	result.complete = true
	state := streamState{}

	for line := range bytes.Lines(body) {
		payload, isData := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte(sseDataPrefix))
		if !isData {
			continue
		}

		payload = bytes.TrimSpace(payload)
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		fields, ok := decodeObject(payload)
		if !ok {
			result.complete = false

			continue
		}

		result.applyStreamEvent(fields, &state)
	}

	if !state.started || !state.stopped {
		result.complete = false
	}

	return result
}

// streamState tracks the message lifecycle expected in an Anthropic event stream.
type streamState struct {
	started bool
	stopped bool
}

// applyStreamEvent folds one recognized stream event into the summary.
func (s *summary) applyStreamEvent(fields map[string]json.RawMessage, state *streamState) {
	rawType, found := fields["type"]
	if !found {
		return
	}

	var eventType string
	if err := json.Unmarshal(rawType, &eventType); err != nil {
		s.complete = false

		return
	}

	switch eventType {
	case eventMessageStart:
		s.applyStreamStart(fields, state)
	case eventContentBlockStart:
		s.applyStreamContentBlock(fields, state)
	case eventMessageDelta:
		s.applyStreamDelta(fields, state)
	case eventMessageStop:
		s.applyStreamStop(state)
	}
}

// applyStreamStart records the initial message only once.
func (s *summary) applyStreamStart(fields map[string]json.RawMessage, state *streamState) {
	rawMessage, found := fields["message"]
	valid := found && s.applyMessageStart(rawMessage)
	if !valid || state.started || state.stopped {
		s.complete = false

		return
	}

	state.started = true
}

// applyStreamContentBlock records a block without accepting it as a valid
// lifecycle transition before message_start or after message_stop.
func (s *summary) applyStreamContentBlock(fields map[string]json.RawMessage, state *streamState) {
	block, valid := decodeContentBlock(fields["content_block"])
	if !valid {
		s.complete = false
	} else {
		s.blockTypes = append(s.blockTypes, block.Type)
	}

	if !state.started || state.stopped {
		s.complete = false
	}
}

// applyStreamDelta merges parsed delta fields before checking lifecycle order,
// retaining safe partial data while marking out-of-order events incomplete.
func (s *summary) applyStreamDelta(fields map[string]json.RawMessage, state *streamState) {
	s.applyMessageDelta(fields)
	if !state.started || state.stopped {
		s.complete = false
	}
}

// applyStreamStop ends a started, not-yet-stopped message.
func (s *summary) applyStreamStop(state *streamState) {
	if !state.started || state.stopped {
		s.complete = false

		return
	}

	state.stopped = true
}

// applyMessageStart records a stream's response ID and initial usage.
func (s *summary) applyMessageStart(raw json.RawMessage) bool {
	fields, ok := decodeObject(raw)
	if !ok {
		return false
	}

	valid := true
	if rawID, found := fields["id"]; found {
		id, idOK := decodeString(rawID)
		if !idOK {
			valid = false
		} else {
			s.responseID = id
		}
	}

	if rawUsage, found := fields["usage"]; found {
		usageValue, usageOK := decodeUsage(rawUsage)
		s.mergeUsage(usageValue)
		if !usageOK {
			valid = false
		}
	}

	return valid
}

// applyMessageDelta records a stream's latest usage and nonempty stop reason.
func (s *summary) applyMessageDelta(fields map[string]json.RawMessage) {
	if rawDelta, found := fields["delta"]; found {
		s.applyDeltaStopReason(rawDelta)
	}

	s.applyTopLevelUsage(fields)
}

// applyDeltaStopReason retains the latest nonempty stop reason from a delta.
func (s *summary) applyDeltaStopReason(raw json.RawMessage) {
	delta, ok := decodeObject(raw)
	if !ok {
		s.complete = false

		return
	}

	rawStopReason, found := delta["stop_reason"]
	if !found {
		return
	}

	stopReason, ok := decodeNullableString(rawStopReason)
	if !ok {
		s.complete = false

		return
	}
	if stopReason != "" {
		s.stopReason = stopReason
	}
}

// applyTopLevelUsage merges usage reported directly on a stream event.
func (s *summary) applyTopLevelUsage(fields map[string]json.RawMessage) {
	rawUsage, found := fields["usage"]
	if !found {
		return
	}

	usageValue, ok := decodeUsage(rawUsage)
	s.mergeUsage(usageValue)
	if !ok {
		s.complete = false
	}
}

// summarizeMessage reads a complete non-streaming answer.
func summarizeMessage(body []byte) summary {
	result := emptySummary()
	fields, ok := decodeObject(body)
	if !ok {
		return result
	}

	result.complete = true

	if rawID, found := fields["id"]; found {
		id, idOK := decodeString(rawID)
		if !idOK {
			result.complete = false
		} else {
			result.responseID = id
		}
	}

	if rawContent, found := fields["content"]; found {
		blocks, blocksOK := decodeContentBlocks(rawContent)
		for _, block := range blocks {
			result.blockTypes = append(result.blockTypes, block.Type)
		}
		if !blocksOK {
			result.complete = false
		}
	}

	if rawUsage, found := fields["usage"]; found {
		usageValue, usageOK := decodeUsage(rawUsage)
		result.mergeUsage(usageValue)
		if !usageOK {
			result.complete = false
		}
	}

	if rawStopReason, found := fields["stop_reason"]; found {
		stopReason, stopReasonOK := decodeString(rawStopReason)
		if !stopReasonOK {
			result.complete = false
		} else {
			result.stopReason = stopReason
		}
	}

	return result
}

// emptySummary returns the summary shape the ledger has always consumed.
func emptySummary() summary {
	return summary{blockTypes: make([]string, 0)}
}

// decodeObject decodes exactly one non-null JSON object.
func decodeObject(raw []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}

	return fields, true
}

// decodeContentBlocks decodes the ordered block shapes in a message response.
func decodeContentBlocks(raw json.RawMessage) ([]contentBlock, bool) {
	if isNull(raw) {
		return nil, false
	}

	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false
	}

	blocks := make([]contentBlock, 0, len(values))
	valid := true
	for _, value := range values {
		block, blockOK := decodeContentBlock(value)
		if !blockOK {
			valid = false

			continue
		}
		blocks = append(blocks, block)
	}

	return blocks, valid
}

// decodeContentBlock decodes the block type without retaining block content.
func decodeContentBlock(raw json.RawMessage) (contentBlock, bool) {
	fields, ok := decodeObject(raw)
	if !ok {
		return contentBlock{}, false
	}

	block := contentBlock{}
	if rawType, found := fields["type"]; found {
		typeValue, typeOK := decodeString(rawType)
		if !typeOK {
			return contentBlock{}, false
		}
		block.Type = typeValue
	}

	return block, true
}

// decodeUsage decodes only the accounting fields a response can report.
func decodeUsage(raw json.RawMessage) (usage, bool) {
	fields, ok := decodeObject(raw)
	if !ok {
		return usage{}, false
	}

	result := usage{}
	valid := true

	result.InputTokens, ok = decodeToken(fields, "input_tokens")
	valid = valid && ok
	result.OutputTokens, ok = decodeToken(fields, "output_tokens")
	valid = valid && ok
	result.CacheCreationInputTokens, ok = decodeToken(fields, "cache_creation_input_tokens")
	valid = valid && ok
	result.CacheReadInputTokens, ok = decodeToken(fields, "cache_read_input_tokens")
	valid = valid && ok

	if rawCreation, found := fields["cache_creation"]; found {
		creation, creationOK := decodeObject(rawCreation)
		if !creationOK {
			valid = false
		} else {
			result.CacheCreation5mInputTokens, ok = decodeToken(creation, "ephemeral_5m_input_tokens")
			valid = valid && ok
			result.CacheCreation1hInputTokens, ok = decodeToken(creation, "ephemeral_1h_input_tokens")
			valid = valid && ok
		}
	}

	if rawCost, found := fields["cost"]; found {
		result.Cost = append(json.RawMessage(nil), bytes.TrimSpace(rawCost)...)
	}

	return result, valid
}

// decodeToken distinguishes an omitted token count from an explicit zero.
func decodeToken(fields map[string]json.RawMessage, name string) (*int64, bool) {
	raw, found := fields[name]
	if !found {
		return nil, true
	}
	if isNull(raw) {
		return nil, false
	}

	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}

	return &value, true
}

// decodeString accepts a JSON string and rejects null and every other shape.
func decodeString(raw json.RawMessage) (string, bool) {
	if isNull(raw) {
		return "", false
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}

	return value, true
}

// decodeNullableString accepts a JSON string or null, which carries no stop reason.
func decodeNullableString(raw json.RawMessage) (string, bool) {
	if isNull(raw) {
		return "", true
	}

	return decodeString(raw)
}

// isNull reports whether raw is a JSON null after whitespace is removed.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// mergeUsage folds every present usage category into the summary.
func (s *summary) mergeUsage(value usage) {
	s.mergeToken(value.InputTokens, &s.inputTokens, &s.inputTokensPresent)
	s.mergeToken(value.OutputTokens, &s.outputTokens, &s.outputTokensPresent)
	s.mergeToken(value.CacheCreationInputTokens, &s.cacheCreationInputTokens, &s.cacheCreationInputTokensPresent)
	s.mergeToken(value.CacheReadInputTokens, &s.cacheReadInputTokens, &s.cacheReadInputTokensPresent)
	s.mergeToken(value.CacheCreation5mInputTokens, &s.cacheCreation5mInputTokens, &s.cacheCreation5mInputTokensPresent)
	s.mergeToken(value.CacheCreation1hInputTokens, &s.cacheCreation1hInputTokens, &s.cacheCreation1hInputTokensPresent)

	if value.Cost == nil {
		return
	}

	s.providerCost = 0
	s.providerCostPresent = true
	providerCost, err := accounting.ParseUSD(string(value.Cost))
	if err != nil {
		s.providerCostValid = false
		s.complete = false

		return
	}

	s.providerCost = providerCost
	s.providerCostValid = true
}

// mergeToken records the latest present token count without allowing negatives
// into the legacy ledger fields.
func (s *summary) mergeToken(value *int64, target *int64, present *bool) {
	if value == nil {
		return
	}

	*present = true
	if *value < 0 {
		*target = 0
		s.complete = false

		return
	}

	*target = *value
}
