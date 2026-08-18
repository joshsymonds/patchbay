package ledger

import "slices"

// The reasoning block types. Anthropic emits redacted_thinking in place of
// thinking when the reasoning is withheld; both are reasoning blocks, and
// neither is visible to the user.
const (
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
)

// BlockShape returns the ordered content block types of a response as an
// independent copy, so a caller reusing its own buffer cannot alter a shape
// already handed to a record.
func BlockShape(blockTypes []string) []string {
	return slices.Clone(blockTypes)
}

// HasDropSignature reports whether the shape contains two adjacent reasoning
// blocks — the Fable mid-turn text drop signature.
//
// The pair means a block boundary existed with nothing user-visible between two
// reasoning blocks: the response streamed one reasoning block, closed it,
// opened another, and the text block that belongs between them is gone. Both
// thinking and redacted_thinking count, in any combination, because both are
// reasoning and neither is the missing user-visible content. A reasoning block
// separated from the next by text or tool_use is ordinary and not the
// signature.
func HasDropSignature(shape []string) bool {
	for i := 1; i < len(shape); i++ {
		if isReasoningBlock(shape[i-1]) && isReasoningBlock(shape[i]) {
			return true
		}
	}
	return false
}

func isReasoningBlock(blockType string) bool {
	return blockType == blockThinking || blockType == blockRedactedThinking
}
