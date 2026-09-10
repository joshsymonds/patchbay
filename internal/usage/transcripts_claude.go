package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joshsymonds/patchbay/internal/jsonstrict"
)

type transcriptTimestamp struct {
	time.Time
	valid bool
}

func (timestamp *transcriptTimestamp) UnmarshalJSON(contents []byte) error {
	if string(contents) == "null" {
		timestamp.Time = time.Time{}
		timestamp.valid = false
		return nil
	}
	var text string
	if err := json.Unmarshal(contents, &text); err == nil {
		parsed, parseErr := time.Parse(time.RFC3339Nano, text)
		if parseErr != nil {
			return fmt.Errorf("parse RFC3339 timestamp: %w", parseErr)
		}
		timestamp.Time = parsed
		timestamp.valid = true
		return nil
	}
	var number float64
	if err := json.Unmarshal(contents, &number); err != nil {
		return fmt.Errorf("timestamp must be RFC3339 or milliseconds: %w", err)
	}
	if number < 0 || number > float64(1<<63-1) {
		return fmt.Errorf("timestamp milliseconds out of range")
	}
	seconds := int64(number) / 1000
	nanos := (int64(number) % 1000) * int64(time.Millisecond)
	timestamp.Time = time.Unix(seconds, nanos).UTC()
	timestamp.valid = true
	return nil
}

type transcriptUsage struct {
	input, output, cacheCreation, cacheRead, reasoning *int64
}

type claudeUsage struct {
	InputTokens         *int64                 `json:"input_tokens"`
	OutputTokens        *int64                 `json:"output_tokens"`
	CacheCreationInput  *int64                 `json:"cache_creation_input_tokens"`
	CacheReadInput      *int64                 `json:"cache_read_input_tokens"`
	OutputTokensDetails *claudeOutputTokenInfo `json:"output_tokens_details"`
}

type claudeOutputTokenInfo struct {
	ThinkingTokens *int64 `json:"thinking_tokens"`
}

type claudeContentBlock struct {
	Type string `json:"type"`
}

type claudeContent []claudeContentBlock

func (content *claudeContent) UnmarshalJSON(contents []byte) error {
	var text *string
	if err := json.Unmarshal(contents, &text); err == nil {
		if text == nil {
			*content = nil
		} else {
			*content = claudeContent{{Type: "text"}}
		}
		return nil
	}
	var blocks []claudeContentBlock
	if err := json.Unmarshal(contents, &blocks); err != nil {
		return fmt.Errorf("content must be a string or block array: %w", err)
	}
	*content = blocks
	return nil
}

type claudeMessage struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Usage      *claudeUsage  `json:"usage"`
	Content    claudeContent `json:"content"`
	StopReason *string       `json:"stop_reason"`
	Role       string        `json:"role"`
}

type claudeTranscriptRow struct {
	Type              string              `json:"type"`
	UUID              string              `json:"uuid"`
	ParentUUID        string              `json:"-"`
	Timestamp         transcriptTimestamp `json:"timestamp"`
	SessionID         string              `json:"-"`
	AgentID           string              `json:"-"`
	IsSidechain       bool                `json:"-"`
	IsAPIErrorMessage bool                `json:"-"`
	RequestID         string              `json:"-"`
	Message           *claudeMessage      `json:"message"`
}

const (
	transcriptJSONTimestamp = "timestamp"
	transcriptJSONMessage   = "message"
)

func decodeTranscriptFields(contents []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		return nil, fmt.Errorf("decode transcript object: %w", err)
	}
	return fields, nil
}

func decodeTranscriptField(fields map[string]json.RawMessage, name string, target any) error {
	value, found := fields[name]
	if !found {
		return nil
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("decode transcript field %q: %w", name, err)
	}
	return nil
}

func (row *claudeTranscriptRow) UnmarshalJSON(contents []byte) error {
	fields, err := decodeTranscriptFields(contents)
	if err != nil {
		return err
	}
	*row = claudeTranscriptRow{}
	for _, field := range []struct {
		name   string
		target any
	}{
		{name: "type", target: &row.Type},
		{name: "uuid", target: &row.UUID},
		{name: "parentUuid", target: &row.ParentUUID},
		{name: transcriptJSONTimestamp, target: &row.Timestamp},
		{name: "sessionId", target: &row.SessionID},
		{name: "agentId", target: &row.AgentID},
		{name: "isSidechain", target: &row.IsSidechain},
		{name: "isApiErrorMessage", target: &row.IsAPIErrorMessage},
		{name: "requestId", target: &row.RequestID},
		{name: transcriptJSONMessage, target: &row.Message},
	} {
		if fieldErr := decodeTranscriptField(fields, field.name, field.target); fieldErr != nil {
			return fieldErr
		}
	}
	return nil
}

type claudeAttemptBuilder struct {
	firstLine     int
	first         claudeTranscriptRow
	lastTimestamp time.Time
	model         string
	responseID    string
	usage         transcriptUsage
	stopReason    string
	shape         []string
	apiError      bool
}

func readClaudeTranscript(path, digest string) ([]transcriptAttempt, int, error) {
	file, err := openTranscriptFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read Claude transcript %q: %w", path, err)
	}
	defer file.Close()
	rows, order, builders, unreadableRows, scanErr := scanClaudeTranscript(file, path, maxTranscriptLineBytes)
	if scanErr != nil {
		return nil, 0, scanErr
	}
	return buildClaudeAttempts(rows, order, builders, digest), unreadableRows, nil
}

func scanClaudeTranscript(
	file *os.File,
	path string,
	maxLineBytes int,
) (map[string]claudeTranscriptRow, []string, map[string]*claudeAttemptBuilder, int, error) {
	reader := bufio.NewReaderSize(file, initialLedgerScanBuffer)
	rows := make(map[string]claudeTranscriptRow)
	order := make([]string, 0)
	builders := make(map[string]*claudeAttemptBuilder)
	unreadableRows := 0
	for line := 1; ; line++ {
		contents, oversized, readErr := readTranscriptLine(reader, maxLineBytes)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, nil, 0, fmt.Errorf("read Claude transcript %q: %w", path, readErr)
		}
		if oversized {
			unreadableRows++
			continue
		}
		row, decodeErr := decodeClaudeTranscriptRow(contents, path, line)
		if decodeErr != nil {
			unreadableRows++
			continue
		}
		if recordErr := recordClaudeTranscriptRow(rows, builders, &order, row, path, line); recordErr != nil {
			unreadableRows++
			continue
		}
	}
	return rows, order, builders, unreadableRows, nil
}

func decodeClaudeTranscriptRow(contents []byte, path string, line int) (claudeTranscriptRow, error) {
	if duplicateErr := jsonstrict.RejectDuplicateNames(contents); duplicateErr != nil {
		return claudeTranscriptRow{}, transcriptLineError(path, line, duplicateErr)
	}
	var row claudeTranscriptRow
	if unmarshalErr := json.Unmarshal(contents, &row); unmarshalErr != nil {
		return claudeTranscriptRow{}, transcriptLineError(path, line, unmarshalErr)
	}
	return row, nil
}

func recordClaudeTranscriptRow(
	rows map[string]claudeTranscriptRow,
	builders map[string]*claudeAttemptBuilder,
	order *[]string,
	row claudeTranscriptRow,
	path string,
	line int,
) error {
	if row.UUID != "" {
		rows[row.UUID] = row
	}
	if row.Type != "assistant" {
		return nil
	}
	if !row.Timestamp.valid {
		return transcriptLineError(path, line, fmt.Errorf("assistant record has no timestamp"))
	}
	key := row.RequestID
	if key == "" {
		key = fmt.Sprintf("line:%d", line)
	}
	builder, found := builders[key]
	if !found {
		builder = &claudeAttemptBuilder{firstLine: line, first: row}
		builders[key] = builder
		*order = append(*order, key)
	}
	builder.lastTimestamp = row.Timestamp.Time
	updateClaudeAttemptBuilder(builder, row.Message)
	builder.apiError = builder.apiError || row.IsAPIErrorMessage
	return nil
}

func updateClaudeAttemptBuilder(builder *claudeAttemptBuilder, message *claudeMessage) {
	if message == nil {
		return
	}
	updateClaudeAttemptMessage(builder, message)
}

func updateClaudeAttemptMessage(builder *claudeAttemptBuilder, message *claudeMessage) {
	if message.Model != "" {
		builder.model = message.Model
	}
	if message.ID != "" {
		builder.responseID = message.ID
	}
	if message.Usage != nil {
		builder.usage = claudeTranscriptUsage(*message.Usage)
	}
	builder.stopReason = ""
	if message.StopReason != nil {
		builder.stopReason = *message.StopReason
	}
	builder.shape = claudeBlockShape(message.Content)
}

func buildClaudeAttempts(
	rows map[string]claudeTranscriptRow,
	order []string,
	builders map[string]*claudeAttemptBuilder,
	digest string,
) []transcriptAttempt {
	attempts := make([]transcriptAttempt, 0, len(order))
	for _, key := range order {
		builder := builders[key]
		started := claudeAttemptStart(rows, builder.first)
		if !started.valid {
			started = builder.first.Timestamp
		}
		responseID := builder.responseID
		if responseID == "" {
			responseID = fmt.Sprintf("transcript:%s:%d", digest, builder.firstLine)
		}
		outcome := OutcomeSuccess
		if builder.apiError {
			outcome = OutcomeUpstreamHTTPFailure
		}
		attempts = append(attempts, transcriptAttempt{
			line:        builder.firstLine,
			identity:    responseID,
			startedAt:   started.Time,
			completedAt: builder.lastTimestamp,
			model:       builder.model,
			usage:       builder.usage,
			stopReason:  builder.stopReason,
			shape:       builder.shape,
			outcome:     outcome,
		})
	}
	return attempts
}

func claudeTranscriptUsage(usage claudeUsage) transcriptUsage {
	result := transcriptUsage{
		input:         usage.InputTokens,
		output:        usage.OutputTokens,
		cacheCreation: usage.CacheCreationInput,
		cacheRead:     usage.CacheReadInput,
	}
	if usage.OutputTokensDetails != nil {
		result.reasoning = usage.OutputTokensDetails.ThinkingTokens
	}
	return result
}

func claudeBlockShape(contents claudeContent) []string {
	shape := make([]string, 0, len(contents))
	for _, content := range contents {
		shape = append(shape, content.Type)
	}
	return shape
}

func claudeAttemptStart(rows map[string]claudeTranscriptRow, first claudeTranscriptRow) transcriptTimestamp {
	current := first.ParentUUID
	seen := make(map[string]bool)
	for current != "" && !seen[current] {
		seen[current] = true
		parent, found := rows[current]
		if !found {
			break
		}
		if parent.Timestamp.valid && !parent.Timestamp.Time.After(first.Timestamp.Time) &&
			claudeIsUserOrToolResult(parent) {
			return parent.Timestamp
		}
		current = parent.ParentUUID
	}
	return transcriptTimestamp{}
}

func claudeIsUserOrToolResult(row claudeTranscriptRow) bool {
	if row.Type == "user" {
		return true
	}
	if row.Message == nil {
		return false
	}
	for _, block := range row.Message.Content {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

func transcriptLineError(path string, line int, err error) error {
	return fmt.Errorf("read transcript %s:%d: %w", path, line, err)
}

func millisecondsBetween(started, completed time.Time) (int64, error) {
	duration := completed.Sub(started)
	if duration < 0 {
		return 0, fmt.Errorf("completed timestamp precedes started timestamp")
	}
	return duration.Milliseconds(), nil
}
