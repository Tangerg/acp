// Decides every fixture's expected outcome using TypeScript SDK validators
// generated from the published stable schema, and writes the answers back into
// the fixture files.
//
// It runs inside a checkout of agentclientprotocol/typescript-sdk, pinned by
// scripts/update-fixtures.sh, because the answers have to come from the
// reference implementation's deserialisation rules rather than from this Go
// implementation. The generator input is replaced with this module's pinned
// published schema by scripts/update-fixtures.sh: the SDK's normal v1 input is
// explicitly unstable and therefore cannot be a stable-protocol oracle.
//
// The published npm package cannot be used for this. Its exports map reaches
// dist/acp.js, the experimental entry points and the raw schema JSON, and
// nothing else — the generated Zod validators and the deserialisation helpers,
// which are the actual oracle, are not reachable from it.
//
// Invoked as: npx tsx oracle.ts <fixtures-directory>

import * as fs from "node:fs";
import * as path from "node:path";
import * as schemas from "./src/schema/zod.gen.js";

const fixturesDir = process.argv[2];
if (!fixturesDir) {
  console.error("usage: tsx oracle.ts <fixtures-directory>");
  process.exit(2);
}

type Case = {
  name: string;
  type?: string;
  why: string;
  input: unknown;
  accepted?: boolean;
  normalized?: unknown;
  // A case where the Go implementation deliberately disagrees, because the
  // schema and this SDK disagree and the schema wins. Recorded by hand and
  // never written here: the oracle's job is to say what this SDK does, not to
  // adjudicate.
  divergence?: unknown;
};

type Fixture = { type: string; $comment?: string; cases: Case[] };

// The generated validators are exported as z<TypeName>. The fixture names the
// schema definition, so the mapping is mechanical — and a fixture naming a
// definition the SDK does not export is a fixture that cannot be checked, which
// has to fail rather than be skipped.
function validatorFor(typeName: string) {
  const validator = (schemas as Record<string, unknown>)["z" + typeName];
  if (validator === undefined) {
    throw new Error(`the TypeScript SDK exports no validator for ${typeName}`);
  }
  return validator as { safeParse(value: unknown): { success: boolean; data?: unknown } };
}

// The fixture files spell Go's names for three definitions the schema spells
// differently, because Go's initialism convention is not the schema's. The
// oracle is asked about the schema's.
const schemaNames: Record<string, string> = {
  SessionID: "SessionId",
  McpServerHTTP: "McpServerHttp",
  HTTPHeader: "HttpHeader",
};

let changed = 0;
for (const file of fs.readdirSync(fixturesDir).sort()) {
  if (!file.endsWith(".json")) continue;
  const fixturePath = path.join(fixturesDir, file);
  const fixture: Fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));

  for (const testCase of fixture.cases) {
    const goName = testCase.type || fixture.type;
    if (!goName) {
      throw new Error(`${file}: case ${testCase.name} names no type`);
    }
    const result = validatorFor(schemaNames[goName] ?? goName).safeParse(testCase.input);

    testCase.accepted = result.success;
    if (result.success) {
      // JSON.stringify drops undefined, which is how an absent property stays
      // absent, and keeps null, which is how a present-null one stays null.
      // Round-tripping through it is what makes the answer comparable to Go's.
      testCase.normalized = JSON.parse(JSON.stringify(result.data));
    } else {
      delete testCase.normalized;
    }
    changed++;
  }

  fs.writeFileSync(fixturePath, JSON.stringify(fixture, null, 2) + "\n");
}

console.log(`oracle: recorded ${changed} expectations from the TypeScript SDK`);
