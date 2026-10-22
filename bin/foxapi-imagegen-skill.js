#!/usr/bin/env node

"use strict";

const fs = require("fs");
const os = require("os");
const path = require("path");
const readline = require("readline");

const packageRoot = path.resolve(__dirname, "..");
const sourceDir = path.join(packageRoot, "skill");

function printUsage() {
  console.log(`FoxAPI Imagegen Skill installer

Usage:
  npx --yes mengboxin/foxapi-imagegen-skill setup --force
  npx --yes https://course.foxapi.cn/f.tgz
  npx --yes foxapi-imagegen-skill install
  npx --yes foxapi-imagegen-skill path
  npx --yes foxapi-imagegen-skill uninstall --yes

Commands:
  setup                   Install the skill and save a FoxAPI API key locally.
  setup --force           Replace the skill and update the saved API key.
  install                 Install the skill into CODEX_HOME/skills.
  install --force         Replace an existing FoxAPI Imagegen skill.
  path                    Print the installation path.
  uninstall --yes         Remove the installed skill.

Options:
  --codex-home <path>     Override CODEX_HOME for this command.
  --force                 Allow install to replace the target directory.
  --yes                   Confirm uninstall.
`);
}

function fail(message) {
  console.error(`Error: ${message}`);
  process.exitCode = 1;
}

function parseArgs(argv) {
  const positionals = [];
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const value = argv[index];
    if (value === "--force" || value === "--yes") {
      options[value.slice(2)] = true;
      continue;
    }
    if (value === "--help" || value === "-h") {
      options.help = true;
      continue;
    }
    if (value === "--version" || value === "-v") {
      options.version = true;
      continue;
    }
    if (value === "--codex-home") {
      const next = argv[index + 1];
      if (!next || next.startsWith("--")) {
        throw new Error("--codex-home requires a path.");
      }
      options.codexHome = next;
      index += 1;
      continue;
    }
    if (value.startsWith("--")) {
      throw new Error(`Unknown option: ${value}`);
    }
    positionals.push(value);
  }
  return { command: positionals[0] || "install", options };
}

function resolveCodexHome(options) {
  const raw = options.codexHome || process.env.CODEX_HOME || path.join(os.homedir(), ".codex");
  return path.resolve(raw);
}

function targetPath(options) {
  return path.join(resolveCodexHome(options), "skills", "foxapi-imagegen");
}

function credentialPath(options) {
  return path.join(resolveCodexHome(options), "foxapi-imagegen", "config.json");
}

function validateSource() {
  const required = [
    path.join(sourceDir, "SKILL.md"),
    path.join(sourceDir, "agents", "openai.yaml"),
    path.join(sourceDir, "scripts", "foxapi_imagegen.py"),
  ];
  for (const file of required) {
    if (!fs.existsSync(file)) {
      throw new Error(`Package is missing ${path.relative(packageRoot, file)}.`);
    }
  }
}

function install(options) {
  validateSource();
  const destination = targetPath(options);
  if (fs.existsSync(destination)) {
    if (!options.force) {
      throw new Error(`Target already exists: ${destination}. Use install --force to replace it.`);
    }
    fs.rmSync(destination, { recursive: true, force: true });
  }
  fs.mkdirSync(path.dirname(destination), { recursive: true });
  fs.cpSync(sourceDir, destination, { recursive: true });
  if (process.platform !== "win32") {
    const binaryRoot = path.join(destination, "bin");
    if (fs.existsSync(binaryRoot)) {
      for (const platformEntry of fs.readdirSync(binaryRoot, { withFileTypes: true })) {
        if (!platformEntry.isDirectory()) continue;
        const binaryPath = path.join(binaryRoot, platformEntry.name, "foxapi-imagegen");
        if (fs.existsSync(binaryPath)) fs.chmodSync(binaryPath, 0o755);
      }
    }
  }
  console.log(`Installed FoxAPI Imagegen Skill to ${destination}`);
  console.log("Restart Codex to load the new skill.");
}

function promptForApiKey() {
  if (!process.stdin.isTTY || typeof process.stdin.setRawMode !== "function") {
    const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
    return new Promise((resolve, reject) => {
      rl.question("FoxAPI API Key: ", (answer) => {
        rl.close();
        const value = answer.trim();
        if (!value) {
          reject(new Error("API Key cannot be empty."));
          return;
        }
        resolve(value);
      });
    });
  }

  return new Promise((resolve, reject) => {
    const stdin = process.stdin;
    let value = "";

    const finish = (error) => {
      stdin.removeListener("data", onData);
      stdin.setRawMode(false);
      stdin.pause();
      process.stdout.write("\n");
      if (error) {
        reject(error);
        return;
      }
      if (!value) {
        reject(new Error("API Key cannot be empty."));
        return;
      }
      resolve(value);
    };

    const onData = (chunk) => {
      for (const character of String(chunk)) {
        if (character === "\u0003") {
          finish(new Error("Setup cancelled."));
          return;
        }
        if (character === "\r" || character === "\n") {
          finish();
          return;
        }
        if (character === "\b" || character === "\u007f") {
          value = value.slice(0, -1);
          continue;
        }
        value += character;
      }
    };

    process.stdout.write("FoxAPI API Key (input hidden): ");
    stdin.setRawMode(true);
    stdin.resume();
    stdin.on("data", onData);
  });
}

function saveApiKey(options, apiKey) {
  const file = credentialPath(options);
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  fs.writeFileSync(file, `${JSON.stringify({ apiKey }, null, 2)}\n`, {
    encoding: "utf8",
    mode: 0o600,
  });
  try {
    fs.chmodSync(file, 0o600);
  } catch {
    // Windows uses the user's profile ACL; chmod is best-effort there.
  }
  return file;
}

async function setup(options) {
  validateSource();
  install(options);
  console.log("API Key 仅保存在当前用户的本地 Codex 配置目录，不会写入 Skill 文件或 npm 包。");
  const apiKey = await promptForApiKey();
  const file = saveApiKey(options, apiKey);
  console.log(`Saved FoxAPI credentials to ${file}`);
  console.log("Restart Codex to load the skill and saved credentials.");
}

function uninstall(options) {
  const destination = targetPath(options);
  if (!fs.existsSync(destination)) {
    console.log(`Skill is not installed at ${destination}`);
    return;
  }
  if (!options.yes) {
    throw new Error("Uninstall is destructive. Add --yes to confirm.");
  }
  fs.rmSync(destination, { recursive: true, force: true });
  console.log(`Removed FoxAPI Imagegen Skill from ${destination}`);
}

async function main() {
  try {
    const { command, options } = parseArgs(process.argv.slice(2));
    if (options.version) {
      console.log(require(path.join(packageRoot, "package.json")).version);
    } else if (options.help || command === "help") {
      printUsage();
    } else if (command === "path") {
      console.log(targetPath(options));
    } else if (command === "setup") {
      await setup(options);
    } else if (command === "install") {
      install(options);
    } else if (command === "uninstall") {
      uninstall(options);
    } else {
      throw new Error(`Unknown command: ${command}`);
    }
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
  }
}

main();
