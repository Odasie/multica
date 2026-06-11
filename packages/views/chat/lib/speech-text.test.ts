import { describe, expect, it } from "vitest";
import { plainTextForSpeech } from "./speech-text";

describe("plainTextForSpeech", () => {
  it("returns plain prose unchanged (whitespace-collapsed)", () => {
    expect(plainTextForSpeech("Hello there, how can I help?")).toBe(
      "Hello there, how can I help?",
    );
  });

  it("drops fenced code blocks entirely", () => {
    const md = "Run this:\n\n```ts\nconst x = 1;\n```\n\nThen reload.";
    expect(plainTextForSpeech(md)).toBe("Run this: Then reload.");
  });

  it("keeps inline code contents without the backticks", () => {
    expect(plainTextForSpeech("Use `npm install` first")).toBe(
      "Use npm install first",
    );
  });

  it("reads link labels, not URLs", () => {
    expect(
      plainTextForSpeech("See [the docs](https://example.com/very/long) now"),
    ).toBe("See the docs now");
  });

  it("reads image alt text, not the URL", () => {
    expect(plainTextForSpeech("![a cat](https://x.test/c.png)")).toBe("a cat");
  });

  it("strips headings, bullets, blockquotes and emphasis markers", () => {
    const md = "# Title\n\n> quote\n\n- **one**\n- _two_\n\nDone.";
    expect(plainTextForSpeech(md)).toBe("Title quote one two Done.");
  });

  it("returns empty string for whitespace-only input", () => {
    expect(plainTextForSpeech("   \n\n  ")).toBe("");
  });
});
