package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The stream event types the ledger summary is built from.
const (
	eventMessageStart      = "message_start"
	eventContentBlockStart = "content_block_start"
	eventMessageDelta      = "message_delta"
)

// sseDataPrefix introduces the JSON payload of a server-sent event.
const sseDataPrefix = "data:"

// eventStreamType is the content type of a streaming Anthropic response.
const eventStreamType = "text/event-stream"

// usage is the token accounting Anthropic reports.
type usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// contentBlock is one block of a response, of which only the type matters here.
type contentBlock struct {
	Type string `json:"type"`
}

// streamMessage is the message envelope a message_start event carries.
type streamMessage struct {
	Usage *usage `json:"usage"`
}

// streamDelta is the delta a message_delta event carries.
type streamDelta struct {
	StopReason string `json:"stop_reason"`
}

// streamEvent is the union of the event payload fields the summary reads. Every
// other field of every event is ignored.
type streamEvent struct {
	Type         string         `json:"type"`
	Message      *streamMessage `json:"message"`
	ContentBlock *contentBlock  `json:"content_block"`
	Delta        *streamDelta   `json:"delta"`
	Usage        *usage         `json:"usage"`
}

// messageResponse is a complete non-streaming answer.
type messageResponse struct {
	Content    []contentBlock `json:"content"`
	Usage      *usage         `json:"usage"`
	StopReason string         `json:"stop_reason"`
}

// summary is what a captured response tells the ledger: the shape it came back
// in, what it cost, and why it stopped.
type summary struct {
	blockTypes   []string
	inputTokens  int64
	outputTokens int64
	stopReason   string
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
	result := summary{blockTypes: make([]string, 0), inputTokens: 0, outputTokens: 0, stopReason: ""}

	for line := range bytes.Lines(body) {
		payload, isData := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte(sseDataPrefix))
		if !isData {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
			continue
		}

		result.apply(event)
	}

	return result
}

// apply folds one stream event into the summary.
func (s *summary) apply(event streamEvent) {
	switch event.Type {
	case eventMessageStart:
		if event.Message != nil && event.Message.Usage != nil {
			s.inputTokens = event.Message.Usage.InputTokens
		}
	case eventContentBlockStart:
		if event.ContentBlock != nil {
			s.blockTypes = append(s.blockTypes, event.ContentBlock.Type)
		}
	case eventMessageDelta:
		if event.Usage != nil {
			s.outputTokens = event.Usage.OutputTokens
		}

		if event.Delta != nil && event.Delta.StopReason != "" {
			s.stopReason = event.Delta.StopReason
		}
	}
}

// summarizeMessage reads a complete non-streaming answer.
func summarizeMessage(body []byte) summary {
	result := summary{blockTypes: make([]string, 0), inputTokens: 0, outputTokens: 0, stopReason: ""}

	var message messageResponse
	if err := json.Unmarshal(body, &message); err != nil {
		return result
	}

	for _, block := range message.Content {
		result.blockTypes = append(result.blockTypes, block.Type)
	}

	if message.Usage != nil {
		result.inputTokens = message.Usage.InputTokens
		result.outputTokens = message.Usage.OutputTokens
	}

	result.stopReason = message.StopReason

	return result
}
