package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

func TestSummarizeMessageNormalizesAccounting(t *testing.T) {
	got := summarize([]byte(`{
		"id":"msg_123",
		"content":[{"type":"thinking"},{"type":"text"}],
		"usage":{
			"input_tokens":1,
			"output_tokens":2,
			"cache_creation_input_tokens":3,
			"cache_read_input_tokens":4,
			"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},
			"cost":0.0000000015
		},
		"stop_reason":"end_turn"
	}`), "application/json")

	want := summary{
		responseID:                        "msg_123",
		blockTypes:                        []string{"thinking", "text"},
		inputTokens:                       1,
		inputTokensPresent:                true,
		outputTokens:                      2,
		outputTokensPresent:               true,
		cacheCreationInputTokens:          3,
		cacheCreationInputTokensPresent:   true,
		cacheReadInputTokens:              4,
		cacheReadInputTokensPresent:       true,
		cacheCreation5mInputTokens:        5,
		cacheCreation5mInputTokensPresent: true,
		cacheCreation1hInputTokens:        6,
		cacheCreation1hInputTokensPresent: true,
		providerCost:                      accounting.NanoUSD(2),
		providerCostPresent:               true,
		providerCostValid:                 true,
		stopReason:                        "end_turn",
		complete:                          true,
	}
	assertSummary(t, got, want)
}

func TestSummarizeProviderCostStates(t *testing.T) {
	valid := []struct {
		name string
		raw  string
		want accounting.NanoUSD
	}{
		{name: "zero", raw: "0", want: 0},
		{name: "rounds tie to even zero", raw: "0.0000000005", want: 0},
		{name: "rounds tie to even two", raw: "0.0000000015", want: 2},
		{name: "exponent", raw: "1e-9", want: 1},
		{
			name: "largest nano USD",
			raw:  "9223372036.854775807",
			want: accounting.NanoUSD(1<<63 - 1),
		},
	}

	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize([]byte(`{"usage":{"cost":`+tc.raw+`}}`), "application/json")
			want := summary{
				blockTypes:          []string{},
				providerCost:        tc.want,
				providerCostPresent: true,
				providerCostValid:   true,
				complete:            true,
			}
			assertSummary(t, got, want)
		})
	}

	for _, raw := range []string{
		"null",
		`"0.01"`,
		"true",
		"{}",
		"[]",
		"-0.01",
		"9223372036.854775808",
	} {
		t.Run("invalid "+raw, func(t *testing.T) {
			got := summarize([]byte(`{"usage":{"cost":`+raw+`}}`), "application/json")
			want := summary{
				blockTypes:          []string{},
				providerCostPresent: true,
				providerCostValid:   false,
				complete:            false,
			}
			assertSummary(t, got, want)
		})
	}
}

func TestSummarizeStreamMergesPresentUsage(t *testing.T) {
	body := stream(
		`data: {"type":"message_start","message":{"id":"msg_456","usage":{"input_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},"cost":0.000000001}}}`,
		`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_start","content_block":{"type":"text"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9,"cache_creation":{"ephemeral_1h_input_tokens":11},"cost":0.0000000015}}`,
		`data: {"type":"message_stop"}`,
	)

	got := summarize([]byte(body), "text/event-stream")
	want := summary{
		responseID:                        "msg_456",
		blockTypes:                        []string{"thinking", "text"},
		inputTokens:                       2,
		inputTokensPresent:                true,
		outputTokens:                      9,
		outputTokensPresent:               true,
		cacheCreationInputTokens:          3,
		cacheCreationInputTokensPresent:   true,
		cacheReadInputTokens:              4,
		cacheReadInputTokensPresent:       true,
		cacheCreation5mInputTokens:        5,
		cacheCreation5mInputTokensPresent: true,
		cacheCreation1hInputTokens:        11,
		cacheCreation1hInputTokensPresent: true,
		providerCost:                      accounting.NanoUSD(2),
		providerCostPresent:               true,
		providerCostValid:                 true,
		stopReason:                        "end_turn",
		complete:                          true,
	}
	assertSummary(t, got, want)
}

func TestSummarizeAcceptsOptionalResponseFields(t *testing.T) {
	t.Run("message", func(t *testing.T) {
		got := summarize([]byte(`{}`), "application/json")
		want := summary{blockTypes: []string{}, complete: true}
		assertSummary(t, got, want)
	})

	t.Run("stream", func(t *testing.T) {
		got := summarize([]byte(stream(
			`data: {"type":"message_start","message":{}}`,
			`data: {"type":"message_stop"}`,
		)), "text/event-stream")
		want := summary{blockTypes: []string{}, complete: true}
		assertSummary(t, got, want)
	})
}

func TestSummarizeStreamRecognizesSSEAndDone(t *testing.T) {
	body := strings.Join([]string{
		": keepalive\r",
		"event: ping\r",
		`data: {"type":"ping"}` + "\r",
		"\r",
		"event: message_start\r",
		`data: {"type":"message_start","message":{"id":"msg_789","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":5},"cost":0.000000001}}}` + "\r",
		"\r",
		"data: [DONE]\r",
		"\r",
		`data: {"type":"content_block_start","content_block":{"type":"text"}}` + "\r",
		"\r",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7,"cost":0.0000000015}}` + "\r",
		"\r",
		`data: {"type":"message_stop"}` + "\r",
		"\r",
	}, "\n")

	got := summarize([]byte(body), "Text/Event-Stream; charset=UTF-8")
	want := summary{
		responseID:                        "msg_789",
		blockTypes:                        []string{"text"},
		inputTokens:                       10,
		inputTokensPresent:                true,
		outputTokens:                      7,
		outputTokensPresent:               true,
		cacheCreationInputTokens:          2,
		cacheCreationInputTokensPresent:   true,
		cacheReadInputTokens:              3,
		cacheReadInputTokensPresent:       true,
		cacheCreation5mInputTokens:        4,
		cacheCreation5mInputTokensPresent: true,
		cacheCreation1hInputTokens:        5,
		cacheCreation1hInputTokensPresent: true,
		providerCost:                      accounting.NanoUSD(2),
		providerCostPresent:               true,
		providerCostValid:                 true,
		stopReason:                        "end_turn",
		complete:                          true,
	}
	assertSummary(t, got, want)
}

func TestSummarizeRejectsIncompleteStructures(t *testing.T) {
	messageBodies := []string{
		"",
		"null",
		"[]",
		`{"id":"msg"} trailing`,
		`{"id":1}`,
		`{"content":{}}`,
		`{"usage":[]}`,
		`{"stop_reason":{}}`,
		`{"usage":{"cost":1.}}`,
	}
	for _, body := range messageBodies {
		t.Run("message "+body, func(t *testing.T) {
			if got := summarize([]byte(body), "application/json"); got.complete {
				t.Fatalf("summary = %#v; want incomplete", got)
			}
		})
	}

	streamBodies := []struct {
		name string
		body string
		want summary
	}{
		{
			name: "malformed data retains start",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_malformed","usage":{"input_tokens":2}}}`,
				"data: {",
				`data: {"type":"message_stop"}`,
			),
			want: summary{
				responseID:         "msg_malformed",
				blockTypes:         []string{},
				inputTokens:        2,
				inputTokensPresent: true,
				complete:           false,
			},
		},
		{
			name: "missing stop retains parsed fields",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_missing_stop","usage":{"input_tokens":3}}}`,
				`data: {"type":"content_block_start","content_block":{"type":"text"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}`,
			),
			want: summary{
				responseID:          "msg_missing_stop",
				blockTypes:          []string{"text"},
				inputTokens:         3,
				inputTokensPresent:  true,
				outputTokens:        4,
				outputTokensPresent: true,
				stopReason:          "max_tokens",
				complete:            false,
			},
		},
		{
			name: "duplicate start retains merged starts",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_first","usage":{"input_tokens":5}}}`,
				`data: {"type":"message_start","message":{"id":"msg_second","usage":{"output_tokens":6}}}`,
				`data: {"type":"message_stop"}`,
			),
			want: summary{
				responseID:          "msg_second",
				blockTypes:          []string{},
				inputTokens:         5,
				inputTokensPresent:  true,
				outputTokens:        6,
				outputTokensPresent: true,
				complete:            false,
			},
		},
		{
			name: "delta before start retains usage",
			body: stream(
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
				`data: {"type":"message_start","message":{"id":"msg_after_delta","usage":{"input_tokens":8}}}`,
				`data: {"type":"message_stop"}`,
			),
			want: summary{
				responseID:          "msg_after_delta",
				blockTypes:          []string{},
				inputTokens:         8,
				inputTokensPresent:  true,
				outputTokens:        7,
				outputTokensPresent: true,
				stopReason:          "end_turn",
				complete:            false,
			},
		},
		{
			name: "duplicate stop retains parsed fields",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_duplicate_stop","usage":{"input_tokens":9}}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`,
				`data: {"type":"message_stop"}`,
				`data: {"type":"message_stop"}`,
			),
			want: summary{
				responseID:          "msg_duplicate_stop",
				blockTypes:          []string{},
				inputTokens:         9,
				inputTokensPresent:  true,
				outputTokens:        10,
				outputTokensPresent: true,
				stopReason:          "end_turn",
				complete:            false,
			},
		},
		{
			name: "delta after stop retains updated usage",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_after_stop","usage":{"output_tokens":11}}}`,
				`data: {"type":"message_stop"}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":12}}`,
			),
			want: summary{
				responseID:          "msg_after_stop",
				blockTypes:          []string{},
				outputTokens:        12,
				outputTokensPresent: true,
				stopReason:          "max_tokens",
				complete:            false,
			},
		},
		{
			name: "done does not replace missing stop",
			body: stream(
				`data: {"type":"message_start","message":{"id":"msg_done","usage":{"cache_read_input_tokens":13}}}`,
				"data: [DONE]",
			),
			want: summary{
				responseID:                  "msg_done",
				blockTypes:                  []string{},
				cacheReadInputTokens:        13,
				cacheReadInputTokensPresent: true,
				complete:                    false,
			},
		},
	}
	for _, tc := range streamBodies {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize([]byte(tc.body), "text/event-stream")
			assertSummary(t, got, tc.want)
		})
	}
}

func TestSummarizeTokenPresenceAndValidation(t *testing.T) {
	tests := []struct {
		name         string
		usage        string
		field        string
		wantValue    int64
		wantPresent  bool
		wantComplete bool
	}{
		{
			name:         "explicit zero input",
			usage:        `{"input_tokens":0}`,
			field:        "input",
			wantPresent:  true,
			wantComplete: true,
		},
		{
			name:         "explicit zero output",
			usage:        `{"output_tokens":0}`,
			field:        "output",
			wantPresent:  true,
			wantComplete: true,
		},
		{
			name:         "explicit zero cache creation",
			usage:        `{"cache_creation_input_tokens":0}`,
			field:        "cache creation",
			wantPresent:  true,
			wantComplete: true,
		},
		{
			name:         "explicit zero cache read",
			usage:        `{"cache_read_input_tokens":0}`,
			field:        "cache read",
			wantPresent:  true,
			wantComplete: true,
		},
		{
			name:         "explicit zero cache creation 5m",
			usage:        `{"cache_creation":{"ephemeral_5m_input_tokens":0}}`,
			field:        "cache creation 5m",
			wantPresent:  true,
			wantComplete: true,
		},
		{
			name:         "explicit zero cache creation 1h",
			usage:        `{"cache_creation":{"ephemeral_1h_input_tokens":0}}`,
			field:        "cache creation 1h",
			wantPresent:  true,
			wantComplete: true,
		},
		{name: "null input", usage: `{"input_tokens":null}`, field: "input", wantComplete: false},
		{
			name:         "string output",
			usage:        `{"output_tokens":"7"}`,
			field:        "output",
			wantComplete: false,
		},
		{
			name:         "fractional cache creation",
			usage:        `{"cache_creation_input_tokens":1.5}`,
			field:        "cache creation",
			wantComplete: false,
		},
		{
			name:         "null nested cache creation",
			usage:        `{"cache_creation":{"ephemeral_5m_input_tokens":null}}`,
			field:        "cache creation 5m",
			wantComplete: false,
		},
		{
			name:         "string nested cache creation",
			usage:        `{"cache_creation":{"ephemeral_1h_input_tokens":"7"}}`,
			field:        "cache creation 1h",
			wantComplete: false,
		},
		{
			name:         "fractional nested cache creation",
			usage:        `{"cache_creation":{"ephemeral_5m_input_tokens":1.5}}`,
			field:        "cache creation 5m",
			wantComplete: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize([]byte(`{"usage":`+tc.usage+`}`), "application/json")
			value, present := summaryTokenState(t, got, tc.field)
			if value != tc.wantValue || present != tc.wantPresent ||
				got.complete != tc.wantComplete {
				t.Fatalf("token state = (%d, %t, complete %t); want (%d, %t, complete %t)",
					value, present, got.complete, tc.wantValue, tc.wantPresent, tc.wantComplete)
			}
		})
	}
}

func TestSummarizeClampsNegativeUsage(t *testing.T) {
	got := summarize([]byte(`{
		"usage":{
			"input_tokens":-1,
			"output_tokens":-2,
			"cache_creation_input_tokens":-3,
			"cache_read_input_tokens":-4,
			"cache_creation":{"ephemeral_5m_input_tokens":-5,"ephemeral_1h_input_tokens":-6}
		}
	}`), "application/json")
	want := summary{
		blockTypes:                        []string{},
		inputTokensPresent:                true,
		outputTokensPresent:               true,
		cacheCreationInputTokensPresent:   true,
		cacheReadInputTokensPresent:       true,
		cacheCreation5mInputTokensPresent: true,
		cacheCreation1hInputTokensPresent: true,
		complete:                          false,
	}
	assertSummary(t, got, want)
}

func summaryTokenState(t *testing.T, got summary, field string) (int64, bool) {
	t.Helper()

	switch field {
	case "input":
		return got.inputTokens, got.inputTokensPresent
	case "output":
		return got.outputTokens, got.outputTokensPresent
	case "cache creation":
		return got.cacheCreationInputTokens, got.cacheCreationInputTokensPresent
	case "cache read":
		return got.cacheReadInputTokens, got.cacheReadInputTokensPresent
	case "cache creation 5m":
		return got.cacheCreation5mInputTokens, got.cacheCreation5mInputTokensPresent
	case "cache creation 1h":
		return got.cacheCreation1hInputTokens, got.cacheCreation1hInputTokensPresent
	default:
		t.Fatalf("unknown token field %q", field)
		return 0, false
	}
}

func assertSummary(t *testing.T, got, want summary) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v\nwant    %#v", got, want)
	}
}

func stream(lines ...string) string {
	return strings.Join(lines, "\n\n") + "\n"
}
