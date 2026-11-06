// `NEXT_EXPORT=1 next build` 只在 POSIX shell 里成立；Windows 的 npm script 走
// cmd/PowerShell，前缀赋值不会被当成环境变量，`npm run build:static` 会直接失败
// （'NEXT_EXPORT' 不是内部或外部命令）。这里统一用 Node 起子进程设变量，让 Windows
// 与 Linux/CI 跑的是同一条命令、同一份 next.config.mjs 分支。
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const nextBin = path.join(root, "node_modules", "next", "dist", "bin", "next");

const result = spawnSync(process.execPath, [nextBin, "build"], {
  cwd: root,
  stdio: "inherit",
  // 继承其余环境变量，只固定导出开关，避免覆盖 CI 传入的 NODE_ENV 等设置。
  env: { ...process.env, NEXT_EXPORT: "1" },
});

if (result.error) {
  console.error(`static build failed to start: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
