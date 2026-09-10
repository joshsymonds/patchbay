package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/joshsymonds/patchbay/internal/jsonstrict"
)

type piUsage struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  *int64 `json:"-"`
	CacheWrite *int64 `json:"-"`
	Reasoning  *int64 `json:"reasoning"`
}

type piContentBlock struct {
	Type string `json:"type"`
}

type piMessage struct {
	Role       string              `json:"role"`
	Model      string              `json:"model"`
	Provider   string              `json:"provider"`
	Timestamp  transcriptTimestamp `json:"timestamp"`
	Usage      *piUsage            `json:"usage"`
	StopReason string              `json:"-"`
	ResponseID string              `json:"-"`
	Content    []piContentBlock    `json:"content"`
}

type piTranscriptRow struct {
	Type       string              `json:"type"`
	ID         string              `json:"id"`
	ParentID   string              `json:"-"`
	Timestamp  transcriptTimestamp `json:"timestamp"`
	ResponseID string              `json:"-"`
	Message    *piMessage          `json:"message"`
	StopReason string              `json:"-"`
}

func (usage *piUsage) UnmarshalJSON(contents []byte) error {
	fields, err := decodeTranscriptFields(contents)
	if err != nil {
		return err
	}
	*usage = piUsage{}
	for _, field := range []struct {
		name   string
		target any
	}{
		{name: "input", target: &usage.Input},
		{name: "output", target: &usage.Output},
		{name: "cacheRead", target: &usage.CacheRead},
		{name: "cacheWrite", target: &usage.CacheWrite},
		{name: "reasoning", target: &usage.Reasoning},
	} {
		if fieldErr := decodeTranscriptField(fields, field.name, field.target); fieldErr != nil {
			return fieldErr
		}
	}
	return nil
}

func (message *piMessage) UnmarshalJSON(contents []byte) error {
	fields, err := decodeTranscriptFields(contents)
	if err != nil {
		return err
	}
	*message = piMessage{}
	for _, field := range []struct {
		name   string
		target any
	}{
		{name: "role", target: &message.Role},
		{name: "model", target: &message.Model},
		{name: "provider", target: &message.Provider},
		{name: transcriptJSONTimestamp, target: &message.Timestamp},
		{name: "usage", target: &message.Usage},
		{name: "stopReason", target: &message.StopReason},
		{name: "responseId", target: &message.ResponseID},
		{name: "content", target: &message.Content},
	} {
		if fieldErr := decodeTranscriptField(fields, field.name, field.target); fieldErr != nil {
			return fieldErr
		}
	}
	return nil
}

func (row *piTranscriptRow) UnmarshalJSON(contents []byte) error {
	fields, err := decodeTranscriptFields(contents)
	if err != nil {
		return err
	}
	*row = piTranscriptRow{}
	for _, field := range []struct {
		name   string
		target any
	}{
		{name: "type", target: &row.Type},
		{name: "id", target: &row.ID},
		{name: "parentId", target: &row.ParentID},
		{name: transcriptJSONTimestamp, target: &row.Timestamp},
		{name: "responseId", target: &row.ResponseID},
		{name: transcriptJSONMessage, target: &row.Message},
		{name: "stopReason", target: &row.StopReason},
	} {
		if fieldErr := decodeTranscriptField(fields, field.name, field.target); fieldErr != nil {
			return fieldErr
		}
	}
	return nil
}

func readPiTranscript(path, digest string) ([]transcriptAttempt, int, error) {
	file, err := openTranscriptFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read pi transcript %q: %w", path, err)
	}
	defer file.Close()
	attempts, unreadableRows, scanErr := scanPiTranscript(file, path, digest, maxTranscriptLineBytes)
	if scanErr != nil {
		return nil, 0, scanErr
	}
	return attempts, unreadableRows, nil
}

func scanPiTranscript(file *os.File, path, digest string, maxLineBytes int) ([]transcriptAttempt, int, error) {
	reader := bufio.NewReaderSize(file, initialLedgerScanBuffer)
	rows := make(map[string]piTranscriptRow)
	attempts := make([]transcriptAttempt, 0)
	unreadableRows := 0
	for line := 1; ; line++ {
		contents, oversized, readErr := readTranscriptLine(reader, maxLineBytes)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, fmt.Errorf("read pi transcript %q: %w", path, readErr)
		}
		if oversized {
			unreadableRows++
			continue
		}
		row, decodeErr := decodePiTranscriptRow(contents, path, line)
		if decodeErr != nil {
			unreadableRows++
			continue
		}
		if row.ID != "" {
			rows[row.ID] = row
		}
		attempt, include, attemptErr := piTranscriptAttempt(rows, row, digest, line)
		if attemptErr != nil {
			unreadableRows++
			continue
		}
		if include {
			attempts = append(attempts, attempt)
		}
	}
	return attempts, unreadableRows, nil
}

func decodePiTranscriptRow(contents []byte, path string, line int) (piTranscriptRow, error) {
	if duplicateErr := jsonstrict.RejectDuplicateNames(contents); duplicateErr != nil {
		return piTranscriptRow{}, transcriptLineError(path, line, duplicateErr)
	}
	var row piTranscriptRow
	if unmarshalErr := json.Unmarshal(contents, &row); unmarshalErr != nil {
		return piTranscriptRow{}, transcriptLineError(path, line, unmarshalErr)
	}
	return row, nil
}

func piTranscriptAttempt(
	rows map[string]piTranscriptRow,
	row piTranscriptRow,
	digest string,
	line int,
) (transcriptAttempt, bool, error) {
	if row.Type != "message" || row.Message == nil || row.Message.Role != "assistant" {
		return transcriptAttempt{}, false, nil
	}
	completed := row.Timestamp
	if !completed.valid {
		completed = row.Message.Timestamp
	}
	if !completed.valid {
		return transcriptAttempt{}, false, transcriptLineError(
			"pi transcript",
			line,
			fmt.Errorf("assistant message has no timestamp"),
		)
	}
	started := piAttemptStart(rows, row)
	if !started.valid {
		started = completed
	}
	identity := row.ResponseID
	if identity == "" {
		identity = row.Message.ResponseID
	}
	if identity == "" {
		identity = fmt.Sprintf("transcript:%s:%d", digest, line)
	}
	usage := piTranscriptUsage(row.Message.Usage)
	stopReason := piTranscriptStopReason(row)
	outcome := piTranscriptOutcome(stopReason)
	return transcriptAttempt{
		line:        line,
		identity:    identity,
		startedAt:   started.Time,
		completedAt: completed.Time,
		model:       row.Message.Model,
		provider:    row.Message.Provider,
		usage:       usage,
		stopReason:  stopReason,
		shape:       piBlockShape(row.Message.Content),
		outcome:     outcome,
	}, true, nil
}

func piTranscriptUsage(usage *piUsage) transcriptUsage {
	if usage == nil {
		return transcriptUsage{}
	}
	return transcriptUsage{
		input:         usage.Input,
		output:        usage.Output,
		cacheRead:     usage.CacheRead,
		cacheCreation: usage.CacheWrite,
		reasoning:     usage.Reasoning,
	}
}

func piTranscriptStopReason(row piTranscriptRow) string {
	if row.StopReason != "" {
		return row.StopReason
	}
	if row.Message != nil {
		return row.Message.StopReason
	}
	return ""
}

func piTranscriptOutcome(stopReason string) Outcome {
	switch stopReason {
	case "error":
		return OutcomeUpstreamHTTPFailure
	case "aborted":
		return OutcomeResponseInterrupted
	default:
		return OutcomeSuccess
	}
}

func piBlockShape(contents []piContentBlock) []string {
	shape := make([]string, 0, len(contents))
	for _, content := range contents {
		shape = append(shape, content.Type)
	}
	return shape
}

func piAttemptStart(rows map[string]piTranscriptRow, row piTranscriptRow) transcriptTimestamp {
	current := row.ParentID
	seen := make(map[string]bool)
	for current != "" && !seen[current] {
		seen[current] = true
		parent, found := rows[current]
		if !found {
			break
		}
		stamp := parent.Timestamp
		if !stamp.valid && parent.Message != nil {
			stamp = parent.Message.Timestamp
		}
		if stamp.valid && piIsUserOrToolResult(parent) {
			return stamp
		}
		current = parent.ParentID
	}
	return transcriptTimestamp{}
}

func piIsUserOrToolResult(row piTranscriptRow) bool {
	if row.Message == nil {
		return false
	}
	return row.Message.Role == "user" || row.Message.Role == "toolResult"
}
