package parser

func validClaudeIdentity(state State) bool {
	switch state.ClaudeIdentityScope {
	case "":
		return state.ClaudeSessionID == "" && state.ClaudeAgentID == ""
	case "root":
		return state.ClaudeSessionID != "" && state.NativeID == state.ClaudeSessionID && state.ClaudeAgentID == ""
	case "agent":
		return state.ClaudeSessionID != "" && state.ClaudeAgentID != "" && state.NativeID == "claude-agent:"+ID(state.ClaudeSessionID, state.ClaudeAgentID)
	case "ambiguous":
		return state.ClaudeSessionID != "" && state.NativeID == "" && state.ParentThreadID == ""
	default:
		return false
	}
}

// Claude sidechain records use sessionId for their containing session, not a
// unique child thread. Identity comes from payload evidence, never a file path.
func observeClaudeIdentity(state *State, session, agent string, sidechain bool) bool {
	if session == "" || state.ClaudeIdentityScope == "ambiguous" {
		return false
	}
	if state.ClaudeIdentityScope == "" {
		state.ClaudeSessionID = session
		if sidechain {
			if agent == "" {
				state.ClaudeIdentityScope = "ambiguous"
				state.NativeID = ""
				state.ParentThreadID = ""
				return true
			}
			state.ClaudeIdentityScope = "agent"
			state.ClaudeAgentID = agent
			state.NativeID = "claude-agent:" + ID(session, agent)
			state.ParentThreadID = session
		} else {
			state.ClaudeIdentityScope = "root"
			state.NativeID = session
		}
		return false
	}
	// A root source may include embedded sidechain records, but a source first
	// identified as a single child must not silently adopt another child/root.
	if session != state.ClaudeSessionID || state.ClaudeIdentityScope == "agent" && (!sidechain || agent != state.ClaudeAgentID) {
		state.ClaudeIdentityScope = "ambiguous"
		state.NativeID, state.ParentThreadID = "", ""
		return true
	}
	return false
}
