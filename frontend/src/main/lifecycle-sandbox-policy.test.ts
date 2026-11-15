import { describe, expect, it } from "vitest";
import { lifecycleSandboxEnvironment, resolveLifecycleSandbox } from "./lifecycle-sandbox-policy";

const windows = {
	AO_LOCAL_LIFECYCLE_TEST: "1",
	AO_DATA_DIR: "C:\\isolated\\ao3921-sandbox-fixture",
	AO_RUN_FILE: "C:\\isolated\\ao3921-sandbox-fixture\\running.json",
	AO_PORT: "39421",
};

describe("unofficial lifecycle sandbox isolation", () => {
	it("confines profile, browser state and handshake to the Windows sandbox", () => {
		const result = resolveLifecycleSandbox(windows, "C:\\Users\\fixture", "win32");
		expect(result).toEqual({ dataDir: windows.AO_DATA_DIR, runFile: windows.AO_RUN_FILE,
			userData: windows.AO_DATA_DIR + "\\electron", browserData: windows.AO_DATA_DIR + "\\br", port: 39421 });
	});
	it.each([
		{ AO_LOCAL_LIFECYCLE_TEST: undefined }, { AO_DATA_DIR: "relative" }, { AO_RUN_FILE: "C:\\outside\\running.json" },
		{ AO_PORT: "3001" }, { AO_PORT: "4320" }, { AO_PORT: "18726" }, { AO_PORT: "39421extra" },
		{ AO_DAEMON_COMMAND: "production-ao daemon" }, { AO_DEV_DAEMON_BINARY: "C:\\production\\ao.exe" },
		{ AO_DATA_DIR: "C:\\Users\\fixture\\.AO\\ao3921-sandbox-fixture" },
		{ AO_DATA_DIR: "C:\\Users\\fixture" }, { AO_DATA_DIR: "C:\\" },
		{ AO_DATA_DIR: "C:\\isolated\\data" },
	])("refuses unsafe launcher settings %j", (override) => {
		expect(() => resolveLifecycleSandbox({ ...windows, ...override }, "C:\\Users\\fixture", "win32")).toThrow();
	});
	it("pins safety configuration after a shell environment overlay", () => {
		const paths = resolveLifecycleSandbox(windows, "C:\\Users\\fixture", "win32");
		const child = { AO_PORT: "3001", AO_TRACKER_INTAKE: "on", AO_KEEP_DAEMON: "on", AO_DAEMON_COMMAND: "unsafe", ...lifecycleSandboxEnvironment(paths) };
		expect(child.AO_PORT).toBe("39421");
		expect(child.AO_TRACKER_INTAKE).toBe("off");
		expect(child.AO_KEEP_DAEMON).toBe("off");
		expect(child.AO_DAEMON_COMMAND).toBe("");
	});
});
