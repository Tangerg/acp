package acp

import "fmt"

// Handlers supply values; the connection owns which of those values may cross
// its negotiated protocol boundary. This check also covers responses to callers
// that have already stopped waiting.
func (p PeerInfo) validateAgentResponse(response any) error {
	var options []SessionConfigOption
	switch response := response.(type) {
	case *NewSessionResponse:
		options, _ = response.ConfigOptions.Get()
	case *LoadSessionResponse:
		options, _ = response.ConfigOptions.Get()
	case *ResumeSessionResponse:
		options, _ = response.ConfigOptions.Get()
	case *SetSessionConfigOptionResponse:
		options = response.ConfigOptions
	case *ListSessionsResponse:
		for i, session := range response.Sessions {
			prefix := fmt.Sprintf("sessions[%d]", i)
			if err := absolutePath(prefix+".cwd", session.Cwd); err != nil {
				return err
			}
			if err := absoluteDirectories(prefix+".additionalDirectories", session.AdditionalDirectories); err != nil {
				return err
			}
		}
	}
	return p.permitsConfigOptions(options)
}

func (p PeerInfo) validateSessionUpdate(update SessionUpdate) error {
	switch update := update.(type) {
	case *Notice:
		session, _ := p.ClientCapabilities.Session.Get()
		if !hasCapability(session.Notices) {
			return noticeUpdateCapability.unadvertised()
		}
	case *CompactionUpdate, *CompactionSummaryChunk:
		session, _ := p.ClientCapabilities.Session.Get()
		if !hasCapability(session.Compaction) {
			return compactionUpdateCapability.unadvertised()
		}
	case *ConfigOptionUpdate:
		if update != nil {
			return p.permitsConfigOptions(update.ConfigOptions)
		}
	case *ToolCall:
		if update != nil {
			return absoluteToolCallPaths(update.Content, update.Locations)
		}
	case *ToolCallUpdate:
		if update != nil {
			content, _ := update.Content.Get()
			locations, _ := update.Locations.Get()
			return absoluteToolCallPaths(content, locations)
		}
	}
	return nil
}
