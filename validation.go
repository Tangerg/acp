package acp

import (
	"errors"
	"fmt"
)

// An outboundChecked payload knows what it may not carry to a peer. Every
// standard payload this side sends passes through [connection.checkOutbound], so
// each rule is stated once, on the type it constrains, and enforced at one
// boundary.
type outboundChecked interface {
	checkOutbound(agreed *Negotiated) error
}

// A parameterCapability is a value a peer may be sent only because it advertised
// the capability describing it. Such capabilities are comprehension rather than
// authority — an agent that did not advertise images cannot read one, it does not
// forbid being sent one — so they are enforced on the way out, where the
// specification puts the obligation, and not on the way in.
type parameterCapability struct {
	value      string
	capability string
	advertised func(*Negotiated) bool
}

// refusedInbound answers a peer that sent a value this side never advertised:
// invalid-params, because the method is available and the value inside it is not.
func (c parameterCapability) refusedInbound() *Error {
	return newError(ErrorCodeInvalidParams, "%s was not advertised because %s is not set", c.value, c.capability)
}

func (c parameterCapability) check(agreed *Negotiated) error {
	if c.advertised(agreed) {
		return nil
	}
	return notAdvertised(c.value, c.capability)
}

var (
	promptImage = parameterCapability{
		value:      `an "image" content block`,
		capability: "agentCapabilities.promptCapabilities.image",
		advertised: func(n *Negotiated) bool { return n.AgentCapabilities.PromptCapabilities.Image },
	}
	promptAudio = parameterCapability{
		value:      `an "audio" content block`,
		capability: "agentCapabilities.promptCapabilities.audio",
		advertised: func(n *Negotiated) bool { return n.AgentCapabilities.PromptCapabilities.Audio },
	}
	promptResource = parameterCapability{
		value:      `a "resource" content block`,
		capability: "agentCapabilities.promptCapabilities.embeddedContext",
		advertised: func(n *Negotiated) bool { return n.AgentCapabilities.PromptCapabilities.EmbeddedContext },
	}
	mcpHTTP = parameterCapability{
		value:      `an "http" MCP server`,
		capability: "agentCapabilities.mcpCapabilities.http",
		advertised: func(n *Negotiated) bool { return n.AgentCapabilities.McpCapabilities.HTTP },
	}
	mcpSse = parameterCapability{
		value:      `an "sse" MCP server`,
		capability: "agentCapabilities.mcpCapabilities.sse",
		advertised: func(n *Negotiated) bool { return n.AgentCapabilities.McpCapabilities.Sse },
	}
	additionalDirectories = parameterCapability{
		value:      "additionalDirectories",
		capability: "agentCapabilities.sessionCapabilities.additionalDirectories",
		advertised: func(n *Negotiated) bool {
			return hasCapability(n.AgentCapabilities.SessionCapabilities.AdditionalDirectories)
		},
	}
	systemPrompt = parameterCapability{
		value:      "systemPrompt",
		capability: "agentCapabilities.sessionCapabilities.systemPrompt",
		advertised: func(n *Negotiated) bool {
			return hasCapability(n.AgentCapabilities.SessionCapabilities.SystemPrompt)
		},
	}
	systemPromptOverride = parameterCapability{
		value:      `the "override" system prompt mode`,
		capability: "agentCapabilities.sessionCapabilities.systemPrompt.mode.override",
		advertised: func(n *Negotiated) bool {
			prompt, _ := n.AgentCapabilities.SessionCapabilities.SystemPrompt.Get()
			modes, _ := prompt.Mode.Get()
			return hasCapability(modes.Override)
		},
	}
	// The boolean option type is the one parameter capability a side reads about
	// itself: the schema grants it through the client's advertisement, for
	// options an agent offers and values a client sets.
	booleanConfigOptions = parameterCapability{
		value:      "a boolean session configuration option",
		capability: "clientCapabilities.session.configOptions.boolean",
		advertised: func(n *Negotiated) bool {
			session, _ := n.ClientCapabilities.Session.Get()
			options, _ := session.ConfigOptions.Get()
			return hasCapability(options.Boolean)
		},
	}
	noticeUpdates = parameterCapability{
		value:      `a "notice" session update`,
		capability: "clientCapabilities.session.notices",
		advertised: func(n *Negotiated) bool {
			session, _ := n.ClientCapabilities.Session.Get()
			return hasCapability(session.Notices)
		},
	}
	compactionUpdates = parameterCapability{
		value:      "a compaction session update",
		capability: "clientCapabilities.session.compaction",
		advertised: func(n *Negotiated) bool {
			session, _ := n.ClientCapabilities.Session.Get()
			return hasCapability(session.Compaction)
		},
	}
	planUpdates = parameterCapability{
		value:      `a "plan_update" or "plan_removed" session update`,
		capability: "clientCapabilities.plan",
		advertised: func(n *Negotiated) bool { return hasCapability(n.ClientCapabilities.Plan) },
	}
	subagentUpdates = parameterCapability{
		value:      "a subagent update or session-directed message",
		capability: "clientCapabilities.subagents",
		advertised: func(n *Negotiated) bool { return hasCapability(n.ClientCapabilities.Subagents) },
	}
)

func checkPromptContent(blocks []ContentBlock, agreed *Negotiated) error {
	for _, block := range blocks {
		// Text, resource links and every future arm pass by not being named.
		var gate *parameterCapability
		switch block.(type) {
		case *ImageContent:
			gate = &promptImage
		case *AudioContent:
			gate = &promptAudio
		case *EmbeddedResource:
			gate = &promptResource
		}
		if gate != nil {
			if err := gate.check(agreed); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSessionSetup is what session/new, session/load and session/resume all
// carry: a workspace, MCP servers and a system prompt, each constrained by what
// the agent advertised and by the absolute-path rule.
func checkSessionSetup(
	agreed *Negotiated,
	cwd string,
	directories []string,
	servers []McpServer,
	prompt Opt[SystemPrompt],
) error {
	if err := absolutePath("cwd", cwd); err != nil {
		return err
	}
	if len(directories) > 0 {
		if err := additionalDirectories.check(agreed); err != nil {
			return err
		}
	}
	if err := absolutePaths("additionalDirectories", directories); err != nil {
		return err
	}
	for i, server := range servers {
		// A transport with no arm here is one the schema does not gate.
		switch server := server.(type) {
		case *McpServerHTTP:
			if err := mcpHTTP.check(agreed); err != nil {
				return err
			}
		case *McpServerSse:
			if err := mcpSse.check(agreed); err != nil {
				return err
			}
		case *McpServerStdio:
			if server != nil {
				if err := absolutePath(fmt.Sprintf("mcpServers[%d].command", i), server.Command); err != nil {
					return err
				}
			}
		}
	}
	return checkSystemPrompt(agreed, prompt)
}

func checkSystemPrompt(agreed *Negotiated, prompt Opt[SystemPrompt]) error {
	value, set := prompt.Get()
	if !set {
		return nil
	}
	if err := systemPrompt.check(agreed); err != nil {
		return err
	}
	switch value.Mode {
	case SystemPromptModeAppend:
	case SystemPromptModeOverride:
		if err := systemPromptOverride.check(agreed); err != nil {
			return err
		}
	default:
		return notAdvertised(fmt.Sprintf("the %q system prompt mode", value.Mode),
			"a capability this schema version does not define")
	}
	if len(value.Content) == 0 {
		return errors.New("acp: systemPrompt.content must not be empty")
	}
	for i, block := range value.Content {
		if _, text := block.(*TextContent); !text {
			return fmt.Errorf("acp: systemPrompt.content[%d] is %T; only text is permitted", i, block)
		}
	}
	return nil
}

func checkConfigOptions(agreed *Negotiated, options []SessionConfigOption) error {
	for _, option := range options {
		if _, boolean := option.Value.(*SessionConfigBoolean); boolean {
			return booleanConfigOptions.check(agreed)
		}
	}
	return nil
}

func checkToolCallPaths(content []ToolCallContent, locations []ToolCallLocation) error {
	for i, item := range content {
		if diff, ok := item.(*Diff); ok && diff != nil {
			if err := absolutePath(fmt.Sprintf("content[%d].path", i), diff.Path); err != nil {
				return err
			}
		}
	}
	for i, location := range locations {
		if err := absolutePath(fmt.Sprintf("locations[%d].path", i), location.Path); err != nil {
			return err
		}
	}
	return nil
}

func (x *NewSessionRequest) checkOutbound(agreed *Negotiated) error {
	return checkSessionSetup(agreed, x.Cwd, x.AdditionalDirectories, x.McpServers, x.SystemPrompt)
}

func (x *LoadSessionRequest) checkOutbound(agreed *Negotiated) error {
	return checkSessionSetup(agreed, x.Cwd, x.AdditionalDirectories, x.McpServers, x.SystemPrompt)
}

func (x *ResumeSessionRequest) checkOutbound(agreed *Negotiated) error {
	return checkSessionSetup(agreed, x.Cwd, x.AdditionalDirectories, x.McpServers, x.SystemPrompt)
}

func (x *ListSessionsRequest) checkOutbound(*Negotiated) error {
	return absoluteOptionalPath("cwd", x.Cwd)
}

func (x *PromptRequest) checkOutbound(agreed *Negotiated) error {
	return checkPromptContent(x.Prompt, agreed)
}

func (x *SetSessionConfigOptionRequest) checkOutbound(agreed *Negotiated) error {
	if _, boolean := x.Value.(*SetSessionConfigOptionRequestBoolean); boolean {
		return booleanConfigOptions.check(agreed)
	}
	return nil
}

func (x *SessionNotification) checkOutbound(agreed *Negotiated) error {
	switch update := x.Update.(type) {
	case *Notice:
		return noticeUpdates.check(agreed)
	case *CompactionUpdate, *CompactionSummaryChunk:
		return compactionUpdates.check(agreed)
	case *PlanUpdate:
		if err := planUpdates.check(agreed); err != nil || update == nil {
			return err
		}
		if markdown, ok := update.Plan.(*PlanMarkdown); ok && markdown != nil {
			return absoluteOptionalPath("plan.path", markdown.Path)
		}
		return nil
	case *PlanRemoved:
		return planUpdates.check(agreed)
	case *SessionMessage, *SessionMessageChunk:
		return subagentUpdates.check(agreed)
	case *SubagentUpdate:
		if err := subagentUpdates.check(agreed); err != nil || update == nil {
			return err
		}
		if err := absoluteOptionalPath("cwd", update.Cwd); err != nil {
			return err
		}
		directories, _ := update.AdditionalDirectories.Get()
		return absolutePaths("additionalDirectories", directories)
	case *ConfigOptionUpdate:
		if update == nil {
			return nil
		}
		return checkConfigOptions(agreed, update.ConfigOptions)
	case *ToolCall:
		if update == nil {
			return nil
		}
		return checkToolCallPaths(update.Content, update.Locations)
	case *ToolCallUpdate:
		if update == nil {
			return nil
		}
		content, _ := update.Content.Get()
		locations, _ := update.Locations.Get()
		return checkToolCallPaths(content, locations)
	default:
		return nil
	}
}

func (x *RequestPermissionRequest) checkOutbound(*Negotiated) error {
	content, _ := x.ToolCall.Content.Get()
	locations, _ := x.ToolCall.Locations.Get()
	if err := checkToolCallPaths(content, locations); err != nil {
		return err
	}
	return distinctOptions(x.Options)
}

func (x *ReadTextFileRequest) checkOutbound(*Negotiated) error {
	return absolutePath("path", x.Path)
}

func (x *WriteTextFileRequest) checkOutbound(*Negotiated) error {
	return absolutePath("path", x.Path)
}

func (x *CreateTerminalRequest) checkOutbound(*Negotiated) error {
	return absoluteOptionalPath("cwd", x.Cwd)
}

func (x *CreateElicitationRequest) checkOutbound(agreed *Negotiated) error {
	if mode, known := elicitationModeOf(x.Value); known {
		return mode.check(agreed)
	}
	return nil
}

func (x *NewSessionResponse) checkOutbound(agreed *Negotiated) error {
	options, _ := x.ConfigOptions.Get()
	return checkConfigOptions(agreed, options)
}

func (x *LoadSessionResponse) checkOutbound(agreed *Negotiated) error {
	options, _ := x.ConfigOptions.Get()
	return checkConfigOptions(agreed, options)
}

func (x *ResumeSessionResponse) checkOutbound(agreed *Negotiated) error {
	options, _ := x.ConfigOptions.Get()
	return checkConfigOptions(agreed, options)
}

func (x *SetSessionConfigOptionResponse) checkOutbound(agreed *Negotiated) error {
	return checkConfigOptions(agreed, x.ConfigOptions)
}

func (x *ListSessionsResponse) checkOutbound(*Negotiated) error {
	for i, session := range x.Sessions {
		prefix := fmt.Sprintf("sessions[%d]", i)
		if err := absolutePath(prefix+".cwd", session.Cwd); err != nil {
			return err
		}
		if err := absolutePaths(prefix+".additionalDirectories", session.AdditionalDirectories); err != nil {
			return err
		}
	}
	return nil
}
