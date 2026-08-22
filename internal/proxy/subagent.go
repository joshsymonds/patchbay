package proxy

import (
	"net/http"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// Claude Code direct agent-observation headers.
const (
	ClaudeCodeAgentIDHeader       = "X-Claude-Code-Agent-Id"
	ClaudeCodeParentAgentIDHeader = "X-Claude-Code-Parent-Agent-Id"
	maxAgentIDBytes               = 256
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
	return observation
}

func observedID(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	if len(value) < 1 || len(value) > maxAgentIDBytes {
		return "", false
	}
	for _, char := range []byte(value) {
		if char < 0x21 || char > 0x7e {
			return "", false
		}
	}
	return value, true
}
