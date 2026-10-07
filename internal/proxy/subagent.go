package proxy

import (
	"net/http"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// Claude Code direct agent-observation headers.
const (
	ClaudeCodeAgentIDHeader       = "X-Claude-Code-Agent-Id"
	ClaudeCodeParentAgentIDHeader = "X-Claude-Code-Parent-Agent-Id"
	// ClaudeCodeAgentTypeHeader names the agent type of a marked request.
	// Claude Code sends it only with CLAUDE_CODE_GATEWAY_HINT_HEADERS set.
	ClaudeCodeAgentTypeHeader = "X-Claude-Code-Agent-Type"
)

func observeSubagent(header http.Header) *registry.SubagentObservation {
	agentID, ok := observedID(header.Values(ClaudeCodeAgentIDHeader))
	if !ok {
		return nil
	}
	observation := &registry.SubagentObservation{AgentID: agentID}
	if parentID, valid := observedID(header.Values(ClaudeCodeParentAgentIDHeader)); valid {
		observation.ParentAgentID = parentID
	}
	if agentType, valid := observedID(header.Values(ClaudeCodeAgentTypeHeader)); valid {
		observation.AgentType = agentType
	}
	return observation
}

func observedID(values []string) (string, bool) {
	if len(values) != 1 || !registry.ValidObservedID(values[0]) {
		return "", false
	}
	return values[0], true
}
