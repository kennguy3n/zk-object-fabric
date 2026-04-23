import { describe, expect, it } from "vitest";

import { formatBytes } from "./format";
import { summarizeYaml } from "./pages/PlacementPolicyPage";

describe("formatBytes", () => {
  it("handles zero", () => {
    expect(formatBytes(0)).toBe("0 B");
  });

  it("handles bytes up to KiB", () => {
    expect(formatBytes(512)).toBe("512 B");
  });

  it("handles GiB scale", () => {
    expect(formatBytes(5 * 1024 ** 3)).toBe("5.00 GiB");
  });

  it("handles negative", () => {
    expect(formatBytes(-1)).toBe("—");
  });
});

describe("summarizeYaml", () => {
  it("extracts country, provider-count (as replication), and cache_location from JSON policies", () => {
    // Mirrors what backendToFrontendPolicy emits into the textarea
    // (frontend/src/api/client.ts): canonical JSON matching
    // placement_policy.Policy.
    const json = JSON.stringify({
      tenant: "acme",
      bucket: "",
      policy: {
        encryption: { mode: "managed" },
        placement: {
          provider: ["wasabi", "r2", "b2"],
          country: ["DE", "NL"],
          cache_location: "cloudflare-r2",
        },
      },
    });
    const summary = summarizeYaml(json);
    expect(summary.countries).toEqual(["DE", "NL"]);
    expect(summary.replication).toBe(3);
    expect(summary.cache).toBe("cloudflare-r2");
  });

  it("falls back to YAML regex extraction when the buffer is hand-edited YAML", () => {
    const yaml = `
placement:
  allowed_countries: ['DE', 'NL']
  replication_factor: 3
  cache: cloudflare-r2
`;
    const summary = summarizeYaml(yaml);
    expect(summary.countries).toEqual(["DE", "NL"]);
    expect(summary.replication).toBe(3);
    expect(summary.cache).toBe("cloudflare-r2");
  });

  it("returns empty defaults when fields are missing", () => {
    const summary = summarizeYaml("placement: {}");
    expect(summary.countries).toEqual([]);
    expect(summary.replication).toBeNull();
    expect(summary.cache).toBeNull();
  });
});
