import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import katex from "katex";
import { lint } from "markdownlint/promise";

test("the renderer does not inherit permission to render trusted commands", () => {
  const html = katex.renderToString(
    String.raw`\href{https://example.invalid/}{link}`,
    Object.create({ trust: true }),
  );
  assert.doesNotMatch(html, /<a\s/);
});

test("the renderer and Markdown math parser retain their ordinary behavior", async () => {
  const html = katex.renderToString(String.raw`\frac{1}{2}`, { displayMode: true });
  assert.match(html, /class="katex-display"/);
  assert.match(html, /<mfrac>/);

  const results = await lint({
    strings: { math: "# Math\n\n$x^2$\n\n$$\nx^2\n$$\n" },
  });
  assert.deepEqual(results.math, []);
});

test("the checker rejects violations in tracked hidden paths with spaces", (t) => {
  const root = mkdtempSync(join(tmpdir(), "acp-docs-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, ".github"));
  const file = join(root, ".github", "review template.md");
  writeFileSync(file, "# Review\n\n### Skipped heading\n");
  execFileSync("git", ["init", "--quiet"], { cwd: root });
  execFileSync("git", ["add", ".github/review template.md"], { cwd: root });

  const script = fileURLToPath(new URL("./lint-docs.mjs", import.meta.url));
  const check = () => spawnSync(process.execPath, [script], { cwd: root, encoding: "utf8" });
  const invalid = check();
  assert.equal(invalid.status, 1, invalid.stderr);
  assert.match(invalid.stderr, /\.github\/review template\.md:3: MD001/);

  writeFileSync(file, "# Review\n\n## Next heading\n");
  const valid = check();
  assert.equal(valid.status, 0, valid.stderr);
  assert.match(valid.stdout, /Checked 1 Markdown files\./);
});
