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
// schema/provenance.json, and internal/cmd/interop drives it.
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

import { agent, methods, RequestError, ndJsonStream } from "./src/v1/stable/acp.js";
import type { AgentContext } from "./src/v1/stable/acp.js";
import * as schema from "./src/v1/schema/index.js";
import { Readable, Writable } from "node:stream";

const sessionID = "interop-session";

class InteropAgent {
  private authenticated = false;
  private cancelled: (() => void) | undefined;

  initialize(_params: schema.InitializeRequest): schema.InitializeResponse {
    return {
      protocolVersion: schema.PROTOCOL_VERSION,
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
    throw RequestError.methodNotFound(methods.agent.session.load);
  }

  async cancel(_params: schema.CancelNotification): Promise<void> {
    this.cancelled?.();
  }

  async prompt(params: schema.PromptRequest, client: AgentContext): Promise<schema.PromptResponse> {
    const first = params.prompt[0];
    const scenario = first && first.type === "text" ? first.text : "turn";

    switch (scenario) {
      case "cancel":
        return await this.cancelledTurn(client, params.sessionId);
      case "workspace":
        return await this.workspaceTurn(client, params.sessionId);
      case "elicit":
        return await this.elicitingTurn(client, params.sessionId);
      default:
        return await this.ordinaryTurn(client, params.sessionId);
    }
  }

  private async chunk(client: AgentContext, session: string, text: string): Promise<void> {
    await client.notify(methods.client.session.update, {
      sessionId: session,
      update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text } },
    });
  }

  private async ordinaryTurn(client: AgentContext, session: string): Promise<schema.PromptResponse> {
    await this.chunk(client, session, "thinking");

    await client.notify(methods.client.session.update, {
      sessionId: session,
      update: {
        sessionUpdate: "tool_call",
        toolCallId: "call-1",
        title: "Edit a file",
        kind: "edit",
        status: "pending",
      },
    });

    const outcome = await client.request(methods.client.session.requestPermission, {
      sessionId: session,
      toolCall: { toolCallId: "call-1" },
      options: [
        { optionId: "allow", name: "Allow", kind: "allow_once" },
        { optionId: "reject", name: "Reject", kind: "reject_once" },
      ],
    });
    const chosen =
      outcome.outcome.outcome === "selected" ? outcome.outcome.optionId : "cancelled";

    await client.notify(methods.client.session.update, {
      sessionId: session,
      update: {
        sessionUpdate: "tool_call_update",
        toolCallId: "call-1",
        status: chosen === "allow" ? "completed" : "failed",
      },
    });
    await this.chunk(client, session, `decided: ${chosen}`);
    return { stopReason: "end_turn" };
  }

  private async cancelledTurn(client: AgentContext, session: string): Promise<schema.PromptResponse> {
    const cancelled = new Promise<void>((resolve) => {
      this.cancelled = resolve;
    });
    await this.chunk(client, session, "working");
    await cancelled;
    this.cancelled = undefined;
    // A final update after the cancellation, which the client must keep
    // accepting until the prompt is answered.
    await this.chunk(client, session, "stopping");
    return { stopReason: "cancelled" };
  }

  // Both elicitation modes, from the side this package's client serves. A form
  // asks for a value and gets one back; a URL sends the user somewhere and is
  // finished later by a notification rather than by its own response.
  private async elicitingTurn(client: AgentContext, session: string): Promise<schema.PromptResponse> {
    const form = await client.request(methods.client.elicitation.create, {
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
    await this.chunk(client, session, `form: ${answered}`);

    const page = await client.request(methods.client.elicitation.create, {
      sessionId: session,
      toolCallId: "call-1",
      message: "sign in",
      mode: "url",
      elicitationId: "elicit-1",
      url: "https://example.invalid/sign-in",
    });
    await this.chunk(client, session, `url: ${page.action}`);
    await client.notify(methods.client.elicitation.complete, { elicitationId: "elicit-1" });

    return { stopReason: "end_turn" };
  }

  private async workspaceTurn(client: AgentContext, session: string): Promise<schema.PromptResponse> {
    const read = await client.request(methods.client.fs.readTextFile, { sessionId: session, path: "/w/a.txt" });
    await this.chunk(client, session, `read: ${read.content}`);

    await client.request(methods.client.fs.writeTextFile, {
      sessionId: session,
      path: "/w/b.txt",
      content: "written by the interop agent\n",
    });

    const terminal = await client.createTerminal({
      sessionId: session,
      command: "/bin/true",
      args: ["--quiet"],
    });
    const exited = await terminal.waitForExit();
    const output = await terminal.currentOutput();
    await terminal.release();
    await this.chunk(client, session, `ran: ${output.output.trim()} exit=${exited.exitCode}`);

    return { stopReason: "end_turn" };
  }
}

const stream = ndJsonStream(
  Writable.toWeb(process.stdout) as WritableStream<Uint8Array>,
  Readable.toWeb(process.stdin) as ReadableStream<Uint8Array>,
);
const implementation = new InteropAgent();
agent({ name: "interop-agent" })
  .onRequest(methods.agent.initialize, (ctx) => implementation.initialize(ctx.params))
  .onRequest(methods.agent.authenticate, (ctx) => implementation.authenticate(ctx.params))
  .onRequest(methods.agent.session.new, (ctx) => implementation.newSession(ctx.params))
  .onRequest(methods.agent.session.load, () => implementation.loadSession())
  .onRequest(methods.agent.session.prompt, (ctx) => implementation.prompt(ctx.params, ctx.client))
  .onNotification(methods.agent.session.cancel, (ctx) => implementation.cancel(ctx.params))
  .connect(stream);
