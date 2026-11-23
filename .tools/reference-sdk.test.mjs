import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const repository = fileURLToPath(new URL("..", import.meta.url));

for (const script of ["interop.sh", "update-fixtures.sh"]) {
  test(`${script} installs only a committed archive and preserves user work on failure`, () => {
    const temporary = fs.mkdtempSync(path.join(os.tmpdir(), "acp-reference-test-"));
    try {
      const project = path.join(temporary, "project");
      const source = path.join(temporary, "source checkout");
      const commands = path.join(temporary, "commands");
      for (const directory of [path.join(project, "scripts"), path.join(project, "schema"), source, commands]) {
        fs.mkdirSync(directory, { recursive: true });
      }
      for (const name of [script, "reference-sdk.sh"]) {
        fs.copyFileSync(path.join(repository, "scripts", name), path.join(project, "scripts", name));
      }
      fs.writeFileSync(path.join(source, "package.json"), JSON.stringify({ name: "test-sdk", version: "1.0.0" }));
      fs.writeFileSync(path.join(source, "interop-agent.ts"), "committed agent\n");
      execFileSync("git", ["init", "--quiet", source]);
      execFileSync("git", ["-C", source, "add", "."]);
      execFileSync("git", ["-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "reference"]);
      const commit = execFileSync("git", ["-C", source, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      fs.writeFileSync(path.join(source, "interop-agent.ts"), "uncommitted user work\n");
      fs.writeFileSync(path.join(source, "oracle.ts"), "untracked user work\n");
      const before = execFileSync("git", ["-C", source, "status", "--porcelain"], { encoding: "utf8" });

      const contents = "{}\n";
      const hash = createHash("sha256").update(contents).digest("hex");
      for (const asset of ["schema.json", "meta.json"]) {
        fs.writeFileSync(path.join(project, "schema", asset), contents);
      }
      fs.writeFileSync(path.join(project, "schema", "provenance.json"), JSON.stringify({
        schema: { tag: "schema-test", sha256: hash, metaSha256: hash },
        typescript: { repository: source, commit },
        tools: { nodeMajor: Number(process.versions.node.split(".")[0]) },
      }));
      const recorded = path.join(temporary, "npm-working-directory.txt");
      fs.writeFileSync(path.join(commands, "npm"), `#!/bin/sh
printf '%s\\n' "$PWD" > "$ACP_TEST_RECORD"
cat interop-agent.ts >> "$ACP_TEST_RECORD"
exit 42
`, { mode: 0o700 });
      const result = spawnSync("bash", [path.join(project, "scripts", script)], {
        encoding: "utf8",
        env: { ...process.env, PATH: commands + path.delimiter + process.env.PATH, ACP_TYPESCRIPT_SDK: source, ACP_TEST_RECORD: recorded },
      });
      assert.equal(result.status, 42, result.stderr);
      const [workingDirectory, executedSource] = fs.readFileSync(recorded, "utf8").trim().split("\n");
      assert.notEqual(workingDirectory, source);
      assert.equal(executedSource, "committed agent");
      assert.equal(fs.existsSync(workingDirectory), false, "failed run left its private checkout behind");
      assert.equal(fs.readFileSync(path.join(source, "interop-agent.ts"), "utf8"), "uncommitted user work\n");
      assert.equal(fs.readFileSync(path.join(source, "oracle.ts"), "utf8"), "untracked user work\n");
      assert.equal(execFileSync("git", ["-C", source, "status", "--porcelain"], { encoding: "utf8" }), before);
    } finally {
      fs.rmSync(temporary, { recursive: true, force: true });
    }
  });
}
