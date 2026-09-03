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
// obligations, kept only by somebody having read them. Written down once, the way
// the capability table quotes the schema beside each row, and held against the
// schema in both directions so that a release adding an obligation fails here
// rather than waiting to be noticed, and a row cannot outlive its clause.
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
		"PeerInfo.permitsPromptContent gates image, audio and embedded resources and nothing " +
			"else, so text is never refused for want of a capability",
	},
	"All agents MUST support resource links in prompts.": {
		ownedHere,
		"the same switch: a resource link reaches no case, so it is never gated",
	},
	"As a baseline, the Agent MUST support [`ContentBlock::Text`] and [`ContentBlock::ResourceLink`], while other variants are optionally enabled via [`PromptCapabilities`].": {
		ownedHere,
		"the sentence above, stated where PromptRequest.prompt is defined",
	},
	"As a baseline, all Agents **MUST** support `session/new`, `session/prompt`, `session/cancel`, and `session/update`.": {
		ownedHere,
		"NewAgent refuses a configuration missing NewSession, Prompt or Cancel, and " +
			"session/update is an outbound operation every AgentSession has",
	},

	"When a client sends a `session/cancel` notification to cancel an ongoing prompt turn, it MUST respond to all pending `session/request_permission` requests with this `Cancelled` outcome.": {
		ownedHere,
		"ClientSession.Cancel claims the turn's pending permission requests and answers them " +
			"before the notification goes out; see cancel.go",
	},
	"If the client cancels the prompt turn via `session/cancel`, it MUST respond to this request with `RequestPermissionOutcome::Cancelled`.": {
		ownedHere,
		"the same claim, stated where RequestPermissionRequest is defined",
	},
	"This stop reason MUST be returned when the client sends a `session/cancel` notification, even if the cancellation causes exceptions in underlying operations.": {
		ownedHere,
		"the connection turns a committed cancellation into StopReasonCancelled rather than " +
			"letting a handler's context error escape as -32603",
	},
	"MUST send one of these responses for the original request: - Valid response with appropriate data (partial results or cancellation marker) - Error response with code `-32800` (Cancelled) See protocol docs: [Cancellation](https://agentclientprotocol.com/protocol/cancellation)": {
		ownedHere,
		"a request cancelled by $/cancel_request is answered by the connection, with -32800 " +
			"when the handler could not produce a valid response",
	},
	"Upon receiving this notification, the Agent SHOULD: - Stop all language model requests as soon as possible - Abort all tool call invocations in progress - Send any pending `session/update` notifications - Respond to the original `session/prompt` request with `StopReason::Cancelled` See protocol docs: [Cancellation](https://agentclientprotocol.com/protocol/prompt-turn#cancellation)": {
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

	"JSON RPC Request Id An identifier established by the Client that MUST contain a String, Number, or NULL value if included.": {
		ownedHere,
		"requestID is a union of exactly those three arms, and jsonrpcID refuses anything else",
	},
	"The value SHOULD normally not be Null \\[1\\] and Numbers SHOULD NOT contain fractional parts \\[2\\] The Server MUST reply with the same value in the Response object if included.": {
		ownedHere,
		"a response is written under the identifier its request arrived with; identifiers are " +
			"minted by internal/jsonrpc2 and never by a caller",
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
	"The Client MUST adapt its interface according to [`PromptCapabilities`].": {
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
