// Imported before the rest of Electron main so even imported services see the
// isolated profile. Missing launcher configuration fails before app startup.
import { app } from "electron";
import { lstatSync, readFileSync, realpathSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { lifecycleSandboxEnvironment, resolveLifecycleSandbox } from "./lifecycle-sandbox-policy";

export const lifecycleSandbox = resolveLifecycleSandbox(process.env, os.homedir());
const actualDirectory = realpathSync(lifecycleSandbox.dataDir);
const equalPath = (left: string, right: string) => process.platform === "win32" ? left.toLowerCase() === right.toLowerCase() : left === right;
if (!equalPath(actualDirectory, lifecycleSandbox.dataDir)) throw new Error("The sandbox directory may not redirect through a symlink or junction.");
for (const target of [lifecycleSandbox.userData, lifecycleSandbox.browserData, lifecycleSandbox.runFile]) {
	try {
		if (lstatSync(target).isSymbolicLink() || !equalPath(realpathSync(target), target)) throw new Error("Sandbox state may not redirect through a symlink or junction.");
	} catch (error) {
		if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
	}
}
const marker = JSON.parse(readFileSync(path.join(actualDirectory, "lifecycle-sandbox.json"), "utf8"));
if (marker.schema !== 1 || marker.purpose !== "ao-3921-lifecycle-test") throw new Error("Missing lifecycle sandbox ownership marker.");
Object.assign(process.env, lifecycleSandboxEnvironment(lifecycleSandbox));
app.setPath("userData", lifecycleSandbox.userData);
