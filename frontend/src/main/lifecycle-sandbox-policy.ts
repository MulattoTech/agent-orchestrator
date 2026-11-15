// Owner-local packaging policy. This is deliberately separate from the
// upstream lifecycle patch and must never be included in an official release.
import path from "node:path";

export type LifecycleSandboxPaths = {
	dataDir: string;
	runFile: string;
	userData: string;
	browserData: string;
	port: number;
};

export function resolveLifecycleSandbox(
	env: Record<string, string | undefined>,
	homeDir: string,
	platform: NodeJS.Platform = process.platform,
): LifecycleSandboxPaths {
	const paths = platform === "win32" ? path.win32 : path.posix;
	const canonical = (value: string) => platform === "win32" ? value.toLowerCase() : value;
	if (env.AO_LOCAL_LIFECYCLE_TEST !== "1") throw new Error("Launch this unofficial build with START-SANDBOX.ps1.");
	const dataDir = env.AO_DATA_DIR?.trim() ?? "";
	const runFile = env.AO_RUN_FILE?.trim() ?? "";
	if (!paths.isAbsolute(dataDir) || !paths.isAbsolute(runFile)) throw new Error("Sandbox data and run-file paths must be absolute.");
	const data = paths.resolve(dataDir);
	const production = paths.resolve(homeDir, ".ao");
	const normalized = canonical(data);
	if (normalized === canonical(paths.parse(data).root) || normalized === canonical(paths.resolve(homeDir)) ||
		normalized === canonical(production) || normalized.startsWith(canonical(production + paths.sep))) {
		throw new Error("The lifecycle test cannot use the home directory or production AO state.");
	}
	if (canonical(paths.resolve(runFile)) !== canonical(paths.join(data, "running.json"))) {
		throw new Error("The run-file must belong to the isolated sandbox directory.");
	}
	if (!paths.basename(data).startsWith("ao3921-sandbox-")) throw new Error("The sandbox needs its own named-pipe namespace.");
	if (!/^\d+$/.test(env.AO_PORT ?? "")) throw new Error("An explicit sandbox port is required.");
	const port = Number(env.AO_PORT);
	if (port < 39000 || port > 39999) throw new Error("The sandbox port must be between 39000 and 39999.");
	if (env.AO_DAEMON_COMMAND?.trim() || env.AO_DEV_DAEMON_BINARY?.trim()) throw new Error("An external daemon override is not allowed in this build.");
	return { dataDir: data, runFile: paths.resolve(runFile), userData: paths.join(data, "electron"), browserData: paths.join(data, "br"), port };
}

export function lifecycleSandboxEnvironment(paths: LifecycleSandboxPaths): Record<string, string> {
	return {
		AO_LOCAL_LIFECYCLE_TEST: "1",
		AO_DATA_DIR: paths.dataDir,
		AO_RUN_FILE: paths.runFile,
		AO_PORT: String(paths.port),
		AO_KEEP_DAEMON: "off",
		AO_TRACKER_INTAKE: "off",
		AO_HARNESS_UPDATE_CHECKS: "off",
		AO_UPDATE_WHILE_RUNNING: "off",
		AO_VERSION_REFRESH: "off",
		AO_TUI_VERSION_REFRESH: "off",
		AO_UPDATE_WHEN_IDLE: "off",
		AO_WINDOWS_UPDATE_WHEN_IDLE: "off",
		AO_MANAGED_HARNESS_INSTALLS: "off",
		AO_CLOUD_OFFERING: "off",
		AO_TELEMETRY_EVENTS: "off",
		AO_TELEMETRY_METRICS: "off",
		AO_TELEMETRY_REMOTE: "off",
		AO_TELEMETRY_RENDERER: "off",
		AO_SENTRY_DSN: "",
		AO_DAEMON_COMMAND: "",
		AO_DEV_DAEMON_BINARY: "",
	};
}
