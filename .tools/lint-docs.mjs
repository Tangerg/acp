import { execFileSync } from "node:child_process";
import { lint } from "markdownlint/promise";

// Git includes tracked dot-directories without a separate glob implementation.
const files = execFileSync("git", ["ls-files", "-z", "--", "*.md"], {
  encoding: "utf8",
}).split("\0").filter(Boolean);

const results = await lint({
  files,
  config: {
    // Prose is wrapped by hand; tables may be wider than the surrounding text.
    MD013: false,
    // Each changelog version uses the same section names.
    MD024: { siblings_only: true },
    // The README banner needs linked HTML and precedes its first heading.
    MD033: false,
    MD041: false,
    // Go examples follow gofmt's tabs; prose still requires spaces.
    MD010: { code_blocks: false },
  },
});

for (const [file, failures] of Object.entries(results)) {
  for (const failure of failures) {
    const detail = failure.errorDetail ? ` [${failure.errorDetail}]` : "";
    console.error(
      `${file}:${failure.lineNumber}: ${failure.ruleNames.join("/")} ${failure.ruleDescription}${detail}`,
    );
    process.exitCode = 1;
  }
}
console.log(`Checked ${files.length} Markdown files.`);
