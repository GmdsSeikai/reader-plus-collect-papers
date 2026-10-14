import { describe, expect, it } from "vitest";
import { windowsProxySettings } from "./windows-proxy";
import { systemProxyEnvironment } from "./proxy";

const convert = (settings: unknown, env: NodeJS.ProcessEnv = {}) =>
  systemProxyEnvironment(env, windowsProxySettings(settings));

describe("Windows system proxy", () => {
  it("uses one manual address for HTTP and HTTPS", () => {
    const env = convert({ proxy: "127.0.0.1:7890", autoDetect: true });
    expect(env.HTTP_PROXY).toBe("http://127.0.0.1:7890");
    expect(env.HTTPS_PROXY).toBe(env.HTTP_PROXY);
    expect(env.NO_PROXY).toBe("localhost,127.0.0.1,::1");
  });
  it("supports protocol-specific proxies and bypass hosts", () => {
    const env = convert({
      proxy: "http=localhost:7890;https=[::1]:7891;ftp=unused:21",
      bypass: "localhost;*.example.org;10.0.0.1",
    });
    expect(env.HTTP_PROXY).toBe("http://localhost:7890");
    expect(env.HTTPS_PROXY).toBe("http://[::1]:7891");
    expect(env.NO_PROXY).toBe("localhost,127.0.0.1,::1,.example.org,10.0.0.1");
  });
  it("handles no configuration and ignores stale bypass rules", () => {
    const env = convert(
      { proxy: null, bypass: "<local>", autoDetect: false },
      {
        Http_Proxy: "http://stale:80",
        All_Proxy: "http://stale:80",
        NO_PROXY: "*",
      },
    );
    expect(env).toEqual({
      NO_PROXY: "localhost,127.0.0.1,::1",
      no_proxy: "localhost,127.0.0.1,::1",
    });
    expect(convert({})).toEqual(env);
  });

  it("tolerates <local> and unsupported wildcards in bypass list", () => {
    const env = convert({
      proxy: "127.0.0.1:7890",
      bypass: "*jd.com;localhost;<local>;172.2*;192.168.*;example.com",
    });
    expect(env.HTTP_PROXY).toBe("http://127.0.0.1:7890");
    expect(env.NO_PROXY).toBe("localhost,127.0.0.1,::1,example.com");
  });

  it("preserves valid bypass rules when mixed with unsupported ones", () => {
    const env = convert({
      proxy: "localhost:7890",
      bypass: "*.example.org;10.*.*;valid.com;foo*bar",
    });
    expect(env.HTTP_PROXY).toBe("http://localhost:7890");
    expect(env.NO_PROXY).toBe("localhost,127.0.0.1,::1,.example.org,valid.com");
  });
  it("does not invent a proxy for an unconfigured protocol", () => {
    const env = convert({ proxy: "https=proxy.example:8080" });
    expect(env.HTTP_PROXY).toBeUndefined();
    expect(env.HTTPS_PROXY).toBe("http://proxy.example:8080");
  });
  it("accepts an HTTP URL with the default port", () => {
    expect(convert({ proxy: "http://proxy.example" }).HTTPS_PROXY).toBe(
      "http://proxy.example",
    );
  });
  it.each([
    null,
    [],
    { proxy: 123 },
    { autoConfigUrl: "https://example.org/proxy.pac" },
    { autoDetect: true },
    { proxy: "socks=localhost:7890" },
    { proxy: "https=http://proxy.example/path" },
    { proxy: "https=user:password@proxy.example" },
    { proxy: "https=proxy.example:99999" },
  ])("rejects unreadable or unsupported settings: %j", (raw) => {
    expect(() => convert(raw)).toThrow();
  });
});
