import * as vscode from "vscode";
import * as cp from "child_process";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import * as http from "http";

// LLMate Gate VS Code 扩展。
// 职责（执行手册任务 2.1）：激活后将 OpenAI 兼容请求的 base_url 指向本地网关
// （localhost:8400），并管理网关守护进程的启动 / 停止，状态栏显示网关状态。

let output: vscode.OutputChannel;
let statusBar: vscode.StatusBarItem;
let gatewayProc: cp.ChildProcess | undefined;
// 记录被本扩展改写过 apiBase 的 Continue 模型下标，disable 时还原。
let touchedModelIndexes = new Set<number>();

const CONFIG_SECTION = "llmateGate";

function cfg<T>(key: string, fallback: T): T {
  return vscode.workspace.getConfiguration(CONFIG_SECTION).get<T>(key, fallback);
}

function port(): number {
  return cfg<number>("gatewayPort", 8400);
}

function gatewayUrl(): string {
  return `http://127.0.0.1:${port()}`;
}

// 在 PATH 中查找网关可执行文件；若用户显式配置了路径则优先。
function findGatewayBinary(): string | undefined {
  const explicit = cfg<string>("gatewayBinaryPath", "").trim();
  if (explicit) {
    if (fs.existsSync(explicit)) {
      return explicit;
    }
    vscode.window.showWarningMessage(`LLMate Gate: 配置的二进制不存在：${explicit}`);
  }
  const binName = process.platform === "win32" ? "llmate-gate.exe" : "llmate-gate";
  const dirs = (process.env.PATH || "").split(path.delimiter).filter(Boolean);
  for (const dir of dirs) {
    const candidate = path.join(dir, binName);
    if (fs.existsSync(candidate)) {
      return candidate;
    }
  }
  return undefined;
}

function log(msg: string): void {
  output.appendLine(`[${new Date().toLocaleTimeString()}] ${msg}`);
}

function updateStatusBar(running: boolean): void {
  statusBar.text = running
    ? `$(shield) LLMate Gate: 运行中 :${port()}`
    : `$(shield) LLMate Gate: 已停止`;
  statusBar.tooltip = running
    ? "网关正在运行，请求经 localhost 隐私网关转发"
    : "点击启用 LLMate Gate";
  statusBar.command = running ? "llmateGate.openDashboard" : "llmateGate.enable";
  statusBar.show();
}

// 轮询 /healthz 判断网关存活（鉴权为 401 也算「在跑」）。
function pollHealth(): void {
  const req = http.get(`${gatewayUrl()}/healthz`, (res) => {
    updateStatusBar(res.statusCode !== undefined && res.statusCode < 500);
    res.resume();
  });
  req.on("error", () => updateStatusBar(false));
  req.setTimeout(1500, () => {
    req.destroy();
    updateStatusBar(false);
  });
}

function stopGateway(): void {
  if (!gatewayProc || gatewayProc.pid === undefined) {
    return;
  }
  const pid = gatewayProc.pid;
  try {
    if (process.platform === "win32") {
      cp.execSync(`taskkill /PID ${pid} /T /F`, { windowsHide: true });
    } else {
      process.kill(pid, "SIGTERM");
    }
  } catch (e) {
    log(`停止网关失败: ${String(e)}`);
  }
  gatewayProc = undefined;
}

function startGateway(): boolean {
  const bin = findGatewayBinary();
  if (!bin) {
    vscode.window.showErrorMessage(
      "LLMate Gate: 未找到 llmate-gate 可执行文件。请在设置中配置 llmateGate.gatewayBinaryPath，或将其加入 PATH。"
    );
    return false;
  }
  const args: string[] = ["--listen", `:${port()}`];
  const configPath = cfg<string>("configPath", "").trim();
  if (configPath) {
    args.push("--config", configPath);
  }
  const auth = cfg<string>("authToken", "").trim();
  if (auth) {
    // 通过环境变量传入，避免出现在进程参数里。
    process.env.GATEWAY_AUTH_TOKEN = auth;
  }
  log(`启动网关: ${bin} ${args.join(" ")}`);
  const child = cp.spawn(bin, args, {
    env: process.env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout?.on("data", (d) => output.append(d.toString()));
  child.stderr?.on("data", (d) => output.append(d.toString()));
  child.on("exit", (code) => {
    log(`网关进程退出，code=${code}`);
    gatewayProc = undefined;
    updateStatusBar(false);
  });
  gatewayProc = child;
  return true;
}

// 改写 Continue 配置，把 OpenAI 兼容模型的 apiBase 指向本地网关。
function pointContinueToGateway(enable: boolean): void {
  const file = path.join(os.homedir(), ".continue", "config.json");
  let config: any = {};
  try {
    if (fs.existsSync(file)) {
      config = JSON.parse(fs.readFileSync(file, "utf8"));
    }
  } catch (e) {
    log(`读取 Continue 配置失败: ${String(e)}`);
  }
  if (!Array.isArray(config.models)) {
    config.models = [];
  }
  touchedModelIndexes.clear();
  const targetBase = `${gatewayUrl()}/v1`;
  config.models.forEach((m: any, i: number) => {
    const isOpenAI = m.provider === "openai" || m.apiBase?.includes("openai") ||
      (m.apiKey === "skip" && !m.apiBase);
    if (!isOpenAI) {
      return;
    }
    if (enable) {
      m.apiBase = targetBase;
      touchedModelIndexes.add(i);
    } else if (touchedModelIndexes.has(i)) {
      delete m.apiBase;
    }
  });
  // 若没有任何 OpenAI 模型，追加一个最小可用配置，便于直接体验。
  if (enable && touchedModelIndexes.size === 0) {
    config.models.push({
      title: "LLMate Gate (OpenAI 兼容)",
      provider: "openai",
      model: "gpt-4o",
      apiBase: targetBase,
      apiKey: "sk-placeholder",
    });
  }
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify(config, null, 2));
    log(`已${enable ? "指向" : "还原"} Continue 配置: ${file}`);
  } catch (e) {
    vscode.window.showWarningMessage(`LLMate Gate: 写入 Continue 配置失败 ${file}: ${String(e)}`);
  }
}

async function enable(): Promise<void> {
  if (!gatewayProc) {
    if (!startGateway()) {
      return;
    }
    // 给网关一点启动时间。
    await new Promise((r) => setTimeout(r, 800));
  }
  pointContinueToGateway(true);
  updateStatusBar(true);
  vscode.window.showInformationMessage(
    `LLMate Gate 已启用：Continue / Cursor 请求经 localhost:${port()} 隐私网关转发。`
  );
}

function disable(): void {
  pointContinueToGateway(false);
  stopGateway();
  updateStatusBar(false);
  vscode.window.showInformationMessage("LLMate Gate 已停用。");
}

export function activate(context: vscode.ExtensionContext): void {
  output = vscode.window.createOutputChannel("LLMate Gate");
  statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  statusBar.text = "$(shield) LLMate Gate";

  context.subscriptions.push(
    vscode.commands.registerCommand("llmateGate.enable", () => void enable()),
    vscode.commands.registerCommand("llmateGate.disable", () => disable()),
    vscode.commands.registerCommand("llmateGate.openDashboard", () => {
      vscode.env.openExternal(vscode.Uri.parse(`${gatewayUrl()}/_debug`));
    }),
    statusBar
  );

  pollHealth();

  // 激活时按 autoStart 拉起网关，并提示用户一键启用（改写 Continue 配置）。
  if (cfg<boolean>("autoStart", true) && !gatewayProc) {
    startGateway();
  }
  vscode.window
    .showInformationMessage(
      "检测到 LLMate Gate 隐私网关扩展。是否启用（将 OpenAI 兼容请求指向本地网关）？",
      "启用",
      "稍后"
    )
    .then((choice) => {
      if (choice === "启用") {
        void enable();
      }
    });

  // 周期性健康检查。
  const timer = setInterval(pollHealth, 10000);
  context.subscriptions.push({ dispose: () => clearInterval(timer) });
}

export function deactivate(): void {
  stopGateway();
}
