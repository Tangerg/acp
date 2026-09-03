// An agent built on the TypeScript SDK, for this module's interoperability
// evidence.
//
// Two Go endpoints talking to each other share any wire bug they have, so they
// are not release evidence. This is the other implementation: a real process,
// speaking newline-delimited JSON over its stdin and stdout, built on the
// reference SDK's own agent-side connection rather than on anything this
// repository wrote.
//
// It runs inside a checkout of agentclientprotocol/typescript-sdk pinned by
// scripts/interop.sh, and internal/cmd/interop drives it.
//
// The scenarios are chosen for what they exercise, not for realism:
//
//   - "turn" streams updates, asks permission in the middle, and answers with
//     end_turn. That is the shape of a turn, and it is the only shape where a
//     peer is a caller and a callee at once.
//   - "cancel" streams an update, then waits to be cancelled, then sends one more
//     update before answering with the cancelled stop reason — which is what the
//     protocol requires and what a client must keep accepting.
//   - "auth" refuses session/new with -32000 until authenticate has been called,
//     which is control flow rather than failure.
//   - "workspace" reads a file, writes one, and runs a terminal to its exit,
//     which is every capability-gated method a client may be asked for.

import { AgentSideConnection, RequestError, ndJsonStream } from "./src/acp.js";
import type { Agent } from "./src/acp.js";
import * as schema from "./src/schema/index.js";
import { Readable, Writable } from "node:stream";

const sessionID = "interop-session";

class InteropAgent implements Agent {
  private conn: AgentSideConnection;
  private authenticated = false;
  private cancelled: (() => void) | undefined;

  constructor(conn: AgentSideConnection) {
    this.conn = conn;
  }

  initialize(params: schema.InitializeRequest): schema.InitializeResponse {
    return {
      protocolVersion: Math.min(params.protocolVersion, 1),
      agentCapabilities: { loadSession: false },
      authMethods: [{ id: "interop", name: "Interoperability" }],
      agentInfo: { name: "interop-agent", version: "0.0.0" },
    };
  }

  authenticate(_params: schema.AuthenticateRequest): schema.AuthenticateResponse {
    this.authenticated = true;
    return {};
  }

  newSession(params: schema.NewSessionRequest): schema.NewSessionResponse {
    if (params.cwd === "/auth" && !this.authenticated) {
      // -32000: authenticate first. The client is expected to recognise this and
      // retry, which is the whole point of the scenario.
      throw RequestError.authRequired();
    }
    return { sessionId: sessionID };
  }

  loadSession(): schema.LoadSessionResponse {
    throw RequestError.methodNotFound("session/load");
  }

  async cancel(_params: schema.CancelNotification): Promise<void> {
    this.cancelled?.();
  }

  async prompt(params: schema.PromptRequest): Promise<schema.PromptResponse> {
    const first = params.prompt[0];
    const scenario = first && first.type === "text" ? first.text : "turn";

    switch (scenario) {
      case "cancel":
        return await this.cancelledTurn(params.sessionId);
      case "workspace":
        return await this.workspaceTurn(params.sessionId);
      case "elicit":
        return await this.elicitingTurn(params.sessionId);
      default:
        return await this.ordinaryTurn(params.sessionId);
    }
  }

  private async chunk(session: string, text: string): Promise<void> {
    await this.conn.sessionUpdate({
      sessionId: session,
      update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text } },
    });
  }

  private async ordinaryTurn(session: string): Promise<schema.PromptResponse> {
    await this.chunk(session, "thinking");

    await this.conn.sessionUpdate({
      sessionId: session,
      update: {
        sessionUpdate: "tool_call",
        toolCallId: "call-1",
        title: "Edit a file",
        kind: "edit",
        status: "pending",
      },
    });

    const outcome = await this.conn.requestPermission({
      sessionId: session,
      toolCall: { toolCallId: "call-1" },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "reject", name: "Reject", kind: "reject_once" },
      ],
    });
    const chosen =
      outcome.outcome.outcome === "selected" ? outcome.outcome.optionId : "cancelled";

    await this.conn.sessionUpdate({
      sessionId: session,
      update: {
        sessionUpdate: "tool_call_update",
        toolCallId: "call-1",
        status: chosen === "allow" ? "completed" : "failed",
      },
    });
    await this.chunk(session, `decided: ${chosen}`);
    return { stopReason: "end_turn" };
  }

  private async cancelledTurn(session: string): Promise<schema.PromptResponse> {
    await this.chunk(session, "working");
    await new Promise<void>((resolve) => {
      this.cancelled = resolve;
    });
    // A final update after the cancellation, which the client must keep
    // accepting until the prompt is answered.
    await this.chunk(session, "stopping");
    return { stopReason: "cancelled" };
  }

  // Both elicitation modes, from the side this package's client serves. A form
  // asks for a value and gets one back; a URL sends the user somewhere and is
  // finished later by a notification rather than by its own response.
  private async elicitingTurn(session: string): Promise<schema.PromptResponse> {
    const form = await this.conn.createElicitation({
      sessionId: session,
      message: "which branch?",
      mode: "form",
      requestedSchema: {
        type: "object",
        properties: { branch: { type: "string" } },
        required: ["branch"],
      },
    });
    const answered =
      form.action === "accept" && form.content ? JSON.stringify(form.content) : form.action;
    await this.chunk(session, `form: ${answered}`);

    const page = await this.conn.createElicitation({
      sessionId: session,
      toolCallId: "call-1",
      message: "sign in",
      mode: "url",
      elicitationId: "elicit-1",
      url: "https://example.invalid/sign-in",
    });
    await this.chunk(session, `url: ${page.action}`);
    await this.conn.completeElicitation({ elicitationId: "elicit-1" });

    return { stopReason: "end_turn" };
  }

  private async workspaceTurn(session: string): Promise<schema.PromptResponse> {
    const read = await this.conn.readTextFile({ sessionId: session, path: "/w/a.txt" });
    await this.chunk(session, `read: ${read.content}`);

    await this.conn.writeTextFile({
      sessionId: session,
      path: "/w/b.txt",
      content: "written by the interop agent\n",
    });

    const terminal = await this.conn.createTerminal({
      sessionId: session,
      command: "/bin/true",
      args: ["--quiet"],
    });
    const exited = await terminal.waitForExit();
    const output = await terminal.currentOutput();
    await terminal.release();
    await this.chunk(session, `ran: ${output.output.trim()} exit=${exited.exitCode}`);

    return { stopReason: "end_turn" };
  }
}

const stream = ndJsonStream(
  Writable.toWeb(process.stdout) as WritableStream<Uint8Array>,
  Readable.toWeb(process.stdin) as ReadableStream<Uint8Array>,
);
new AgentSideConnection((conn) => new InteropAgent(conn), stream);
