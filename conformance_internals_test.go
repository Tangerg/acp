package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every normative sentence the published schema states, and what this package does
// about it.
//
// These are the part of the standard a generator cannot implement: not shapes but
// obligations, kept only by somebody having read them. Written down once and held
// against the schema in both directions, so that a release adding an obligation
// fails here rather than waiting to be noticed, and a row cannot outlive its
// clause.
//
// Being classified is all this asserts. That a clause is satisfied is asserted by
// the tests each row names.

type obligationOwner uint8

const (
	// ownedHere is an obligation this package keeps, because an application that
	// had to remember it would eventually not.
	ownedHere obligationOwner = iota
	// ownedByApplication is an obligation about what a program does with a value —
	// rendering it, connecting to it, deciding with it — which a protocol library
	// has no way to keep on the application's behalf. What this package owes is
	// that the application can keep it: the value reaches it, distinctly enough to
	// act on.
	ownedByApplication
)

type obligation struct {
	owner obligationOwner
	// Where it is kept, or why it cannot be.
	how string
}

// Keyed by the sentence as normativeClauses extracts it. Quoting in full is the
// point: a reviewer compares a row against the standard without leaving the file.
var obligations = map[string]obligation{
	"A supplied path MUST be absolute.": {
		ownedHere,
		"SessionNotification.checkOutbound refuses a plan_update whose markdown plan names a relative path",
	},
	"Each path MUST be absolute.": {
		ownedHere,
		"checkSessionSetup, SessionNotification.checkOutbound and ListSessionsResponse.checkOutbound refuse a relative additional directory before it is sent",
	},
	"Agents MUST NOT send this update unless the Client advertised `plan`.": {
		ownedHere,
		"SessionNotification.checkOutbound refuses plan_update and plan_removed unless clientCapabilities.plan was advertised",
	},
	"Agents MUST only send this update when the Client advertised the `subagents` Client capability.": {
		ownedHere,
		"SessionNotification.checkOutbound refuses subagent_update, session_message and session_message_chunk unless clientCapabilities.subagents was advertised",
	},
	"The first update for an unknown `sessionId` announces the child and MUST be sent before any live child traffic or live message naming the child as sender or recipient.": {
		ownedByApplication,
		"which child sessions exist is the agent application's state; this package carries each update in the order it is sent",
	},
	"Agents MUST return an error for a mode they do not support rather than treating it as `append` or `override`.": {
		ownedByApplication,
		"the agent application applies the system prompt; SystemPromptMode is an open union, so an unknown mode reaches it intact to refuse",
	},
	"An Agent MUST return an error for a mode it does not support rather than treating it as `append` or `override`.": {
		ownedByApplication,
		"the same rule stated on SystemPromptMode; the unknown value reaches the handler as received",
	},
	"Agents that cannot apply it, for example because of an unadvertised mode or an unsupported content block type, MUST return an error instead of creating, loading, or resuming the session.": {
		ownedByApplication,
		"the session handlers decide whether they can apply a system prompt and return an ACP Error when they cannot",
	},
	"Agents advertising the capability MUST accept `text` content blocks.": {
		ownedByApplication,
		"the agent application interprets system prompt content; this package delivers it without filtering",
	},
	"Agents SHOULD give the content system-level precedence, for example in the model's system prompt or developer instructions rather than in user-turn content.": {
		ownedByApplication,
		"how a model is prompted is the agent application's",
	},
	"Clients MUST NOT send an empty array.": {
		ownedHere,
		"checkSystemPrompt refuses an empty systemPrompt.content before the request is sent",
	},
	"Clients MUST NOT send other content block types until a future capability allows them, and Agents MUST return an error for content blocks they do not support.": {
		ownedHere,
		"checkSystemPrompt refuses any non-text block on the way out; refusing on the way in is the agent handler's, which receives the blocks as sent",
	},
	"Clients MUST only send this when the Agent advertises `sessionCapabilities.systemPrompt`, and SHOULD send the same value they used when the session was created.": {
		ownedHere,
		"checkSessionSetup refuses a systemPrompt the agent did not advertise; sending the same value again is the application's choice of value",
	},
	"Clients MUST only send this when the Agent advertises `sessionCapabilities.systemPrompt`.": {
		ownedHere,
		"checkSessionSetup refuses a systemPrompt on session/new when agentCapabilities.sessionCapabilities.systemPrompt is absent",
	},
	"Clients that use this mode take responsibility for any tool guidance or safety instructions the Agent's default instructions provided, and MUST only use it when the Agent advertises `sessionCapabilities.systemPrompt.mode.override`.": {
		ownedHere,
		"checkSystemPrompt refuses the override mode unless agentCapabilities.sessionCapabilities.systemPrompt.mode.override was advertised; the content of the instructions is the application's",
	},
	"Clients MUST ignore a removal for an unknown ID.": {
		ownedByApplication,
		"plans are the client application's display state; the removal reaches its SessionUpdate handler, which holds the set",
	},
	"The Agent MUST send all entries with their current status.": {
		ownedByApplication,
		"the agent application builds each plan update; this package carries it whole",
	},
	"The Client MUST replace the checklist with this `planId` on each update.": {
		ownedByApplication,
		"rendering a plan is the client application's",
	},
	"The Client MUST replace the entire checklist with each update.": {
		ownedByApplication,
		"rendering a plan is the client application's",
	},
	"The Client MUST stop presenting the previous state as confirmed current activity, but may retain it as last known.": {
		ownedByApplication,
		"presentation is the client application's; the unknown state reaches it as its own arm",
	},
	"Agents SHOULD catch these exceptions and return this semantically meaningful response to confirm successful cancellation.": {
		ownedHere,
		"AgentConn.servePrompt answers a cancelled turn with the cancelled stop reason whatever its handler returned, so an abort error is never relayed",
	},
	"Agents SHOULD include it.": {
		ownedByApplication,
		"whether an idle child reports its failure is the agent application's choice of what to send",
	},
	"Agents that do not understand this action SHOULD preserve the raw payload when storing, replaying, proxying, or forwarding elicitation responses.": {
		ownedHere,
		"the catch-all action arm keeps every undeclared property in Extra and re-encodes it unchanged",
	},
	"Clients that do not understand this mode SHOULD preserve the raw payload when storing, replaying, proxying, or forwarding elicitation requests.": {
		ownedHere,
		"CreateElicitationRequestOther keeps every undeclared property in Extra and re-encodes it unchanged",
	},
	"Clients that do not understand this property schema type SHOULD preserve the raw schema when storing, replaying, proxying, or forwarding elicitation requests.": {
		ownedHere,
		"ElicitationPropertySchemaOther keeps every undeclared property in Extra and re-encodes it unchanged",
	},
	"If not provided, the name SHOULD be used for display.": {
		ownedByApplication,
		"display is the application's; both fields reach it",
	},
	"Implementations MUST NOT make assumptions about values in `_meta`.": {
		ownedByApplication,
		"Meta carries values as encoded JSON and interprets none of them",
	},
	"Property definitions (MUST be primitive types).": {
		ownedHere,
		"the generated ElicitationPropertySchema union admits only the primitive property arms, and a form is compiled before it is shown or sent",
	},
	"The agent MUST cancel any ongoing work (as if `session/cancel` was called) and then free up any resources associated with the session.": {
		ownedHere,
		"the session/close binding cancels the session's turn before the CloseSession handler runs",
	},
	"The client SHOULD disconnect, if it doesn't support this version.": {
		ownedHere,
		"ClientConn.initialize refuses any version but 1, and Client.Connect closes the connection",
	},
	"The client SHOULD set this to `true` only when it can reproduce the configured agent invocation in an interactive terminal.": {
		ownedByApplication,
		"whether a client can reproduce the invocation is the application's knowledge; it sets auth.terminal through ClientConfig.Capabilities",
	},
	"The message SHOULD be limited to a concise single sentence.": {
		ownedByApplication,
		"an application writes the messages of the errors it returns; the ones this package writes are single sentences",
	},
	"The mode MUST be one of the modes advertised in `availableModes` during session creation or loading.": {
		ownedByApplication,
		"the modes are data the agent application returned; ClientSession.SetMode sends the identifier the application chose",
	},
	"The terminal MUST be added before calling `terminal/release`.": {
		ownedByApplication,
		"the agent application embeds terminals in its tool calls and decides when to release one",
	},
	"This MUST be an integer as defined in the JSON-RPC specification.": {
		ownedHere,
		"the generated ErrorCode is an int32 and refuses anything else in both directions",
	},
	"Agents MUST advertise this method only when the client enabled its terminal authentication capability.": {
		ownedHere,
		"authenticationMethods.offered drops terminal methods when the client did not " +
			"advertise auth.terminal, so the advertisement an agent configured cannot be the " +
			"one it sends to a client that cannot run one",
	},
	"The client MUST NOT pass this method to `authenticate`.": {
		ownedHere,
		"ClientConn.Authenticate refuses a terminal method before the write, and the agent " +
			"refuses one inbound: the obligation is the client's and the check is on both sides " +
			"because a peer outside this package can still send one",
	},

	"All agents MUST support text content blocks in prompts.": {
		ownedHere,
		"checkPromptContent gates image, audio and embedded resources and nothing " +
			"else, so text is never refused for want of a capability",
	},
	"All agents MUST support resource links in prompts.": {
		ownedHere,
		"the same switch: a resource link reaches no case, so it is never gated",
	},
	"As a baseline, the Agent MUST support `text` and `resource_link` content blocks, while other variants are optionally enabled via `PromptCapabilities`.": {
		ownedHere,
		"the sentence above, stated where PromptRequest.prompt is defined",
	},
	"As a baseline, all Agents **MUST** support `session/new`, `session/prompt`, `session/cancel`, and `session/update`.": {
		ownedHere,
		"NewAgent refuses a configuration missing NewSession or Prompt, the connection itself " +
			"serves session/cancel by cancelling the turn, and " +
			"session/update is an outbound operation every AgentSession has",
	},

	"When a client sends a `session/cancel` notification to cancel an ongoing prompt turn, it MUST respond to all pending `session/request_permission` requests with this `cancelled` outcome.": {
		ownedHere,
		"ClientSession.Cancel claims the turn's pending permission requests and answers them " +
			"before the notification goes out; see cancel.go",
	},
	"If the client cancels the prompt turn via `session/cancel`, it MUST respond to this request with the `cancelled` outcome.": {
		ownedHere,
		"the same claim, stated where RequestPermissionRequest is defined",
	},
	"This stop reason MUST be returned when the client sends a `session/cancel` notification, even if the cancellation causes exceptions in underlying operations.": {
		ownedHere,
		"the connection turns a committed cancellation into StopReasonCancelled rather than " +
			"letting a handler's context error escape as -32603",
	},
	"MUST send one of these responses for the original request: - Valid response with appropriate data (partial results or cancellation marker) - Error response with code `-32800` (Cancelled) See protocol docs: [Request Cancellation](https://agentclientprotocol.com/protocol/v1/cancellation)": {
		ownedHere,
		"a request cancelled by $/cancel_request is answered by the connection, with -32800 " +
			"when the handler could not produce a valid response",
	},
	"Upon receiving this notification, the Agent SHOULD: - Stop all language model requests as soon as possible - Abort all tool call invocations in progress - Send any pending `session/update` notifications - Respond to the original `session/prompt` request with the `cancelled` stop reason See protocol docs: [Cancellation](https://agentclientprotocol.com/protocol/v1/prompt-turn#cancellation)": {
		ownedHere,
		"the connection cancels the turn's context and owns the stop reason; stopping the model " +
			"and the tools is what the application's Cancel handler is called to do, and it is " +
			"called before the prompt may answer",
	},
	"Note: Clients SHOULD continue accepting tool call updates even after sending a `session/cancel` notification, as the agent may send final updates before responding with the cancelled stop reason.": {
		ownedByApplication,
		"the connection keeps delivering updates after Cancel, so this is only a rule about " +
			"what a handler does with them; documented on ClientSession.Cancel and shown in " +
			"ExampleClientSession_Cancel",
	},
	"Agents MUST only send notices when the Client advertised the `session.notices` Client capability.": {
		ownedHere,
		"SessionNotification.checkOutbound gates Notice on the advertised notices object; " +
			"TestSessionUpdatesRequireTheirSpecificClientCapabilities checks absent, null, " +
			"and independently advertised update capabilities before anything is sent",
	},
	"Agents MUST only send this update when the Client advertised the `session.notices` Client capability.": {
		ownedHere,
		"the same Notice gate, stated on the session/update union arm",
	},
	"Agents MUST only send this update when the Client advertised the `session.compaction` Client capability.": {
		ownedHere,
		"SessionNotification.checkOutbound gates both CompactionUpdate and CompactionSummaryChunk " +
			"on the compaction object; TestSessionUpdatesRequireTheirSpecificClientCapabilities " +
			"proves notice support does not also enable compaction",
	},

	"JSON RPC Request Id An identifier established by the Client that MUST contain a String, Number, or NULL value if included.": {
		ownedHere,
		"RequestId is jsonrpc.ID, whose three kinds are exactly those arms and which refuses anything else",
	},
	"The value SHOULD normally not be Null \\[1\\] and Numbers SHOULD NOT contain fractional parts \\[2\\] The Server MUST reply with the same value in the Response object if included.": {
		ownedHere,
		"a response is written under the identifier its request arrived with; identifiers are " +
			"minted by the pending-call table and never by a caller",
	},
	"Implementations MUST NOT make assumptions about values at these keys.": {
		ownedHere,
		"Meta holds encoded JSON and never interprets it; nothing in this package reads a " +
			"_meta key",
	},

	"Clients MUST handle missing or unknown categories gracefully.": {
		ownedHere,
		"SessionConfigOptionCategory is generated open — no validate — so a category from a " +
			"later release decodes instead of failing the message that carried it",
	},
	"It MUST NOT be required for correctness.": {
		ownedHere,
		"the category is carried and never read: no behaviour in this package depends on one",
	},

	"They MUST NOT render it as a known elicitation mode.": {
		ownedByApplication,
		"rendering is the application's; what this package owes is that it can tell, and a " +
			"custom mode arrives as CreateElicitationRequestOther rather than as either known arm",
	},
	"They MUST NOT treat it as a known elicitation action.": {
		ownedByApplication,
		"the same, for CreateElicitationResponseOther",
	},
	"They MUST NOT render it as a known input control.": {
		ownedByApplication,
		"the same, for the catch-all arm of AvailableCommandInput",
	},
	"Clients SHOULD render this text as Markdown.": {
		ownedByApplication,
		"a protocol library does not render",
	},
	"The Client MUST adapt its interface according to `PromptCapabilities`.": {
		ownedByApplication,
		"the interface is the application's; this package enforces the same capabilities on " +
			"the wire so that an application which does not adapt still cannot send what the " +
			"agent said it cannot read",
	},
	"The Client MUST ensure truncation happens at a character boundary to maintain valid string output, even if this means the retained output is slightly less than the specified limit.": {
		ownedByApplication,
		"the output is produced by the application's Terminal.Output handler; this package " +
			"carries outputByteLimit to it and does not truncate anything itself",
	},
	"Stdio transport configuration All Agents MUST support this transport.": {
		ownedByApplication,
		"an MCP server is named in a session's parameters and connected to by the agent " +
			"application; this package carries the configuration and speaks no MCP",
	},
}

func TestEveryNormativeClauseInTheSchemaIsClassified(t *testing.T) {
	clauses := normativeClauses(t)
	if len(clauses) == 0 {
		t.Fatal("no normative prose found in the schema, which cannot be right")
	}

	for _, clause := range clauses {
		if _, classified := obligations[clause]; !classified {
			t.Errorf("the schema states an obligation nothing here answers:\n\t%q\n"+
				"Add a row saying whether this package keeps it or an application does, and where.",
				clause)
		}
	}
	for clause := range obligations {
		if !slices.Contains(clauses, clause) {
			t.Errorf("a row answers an obligation the schema no longer states:\n\t%q\n"+
				"If upstream dropped it, drop the row; if the wording moved, update the row.",
				clause)
		}
	}

	// A row that says nothing is a row that was added to make this pass.
	for clause, owed := range obligations {
		if len(owed.how) < 20 {
			t.Errorf("the row for %q does not say where the obligation is kept", clause)
		}
	}
}

var normative = regexp.MustCompile(`\b(MUST NOT|MUST|SHOULD NOT|SHOULD)\b`)

// Reads the vendored schema rather than a copy: a copy would be a second thing to
// keep in step with upstream, and noticing upstream move is the point.
func normativeClauses(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("schema", "schema.json"))
	if err != nil {
		t.Fatalf("read the vendored schema: %v", err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the vendored schema: %v", err)
	}

	seen := map[string]bool{}
	var clauses []string
	var walk func(any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]any:
			if described, ok := node["description"].(string); ok {
				for _, sentence := range sentences(described) {
					if normative.MatchString(sentence) && !seen[sentence] {
						seen[sentence] = true
						clauses = append(clauses, sentence)
					}
				}
			}
			for _, value := range node {
				walk(value)
			}
		case []any:
			for _, value := range node {
				walk(value)
			}
		}
	}
	walk(document)

	slices.Sort(clauses)
	return clauses
}

// Splits on a full stop that ends a word rather than one inside `session/new` or a
// version number, which is the whole of what the schema's prose needs.
func sentences(text string) []string {
	text = strings.Join(strings.Fields(text), " ")
	var out []string
	start := 0
	for i := range len(text) {
		if text[i] != '.' {
			continue
		}
		if i+1 == len(text) || text[i+1] == ' ' {
			out = append(out, strings.TrimSpace(text[start:i+1]))
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, strings.TrimSpace(text[start:]))
	}
	return out
}
