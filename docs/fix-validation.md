# 定向修复验证报告（2026-11-05）

## 结果与范围

任务 A 使用 PostgreSQL 超时 Outbox、持久化 fanout、带租约的运行和会话去重；
详情见 [timeout-trigger-reliability.md](timeout-trigger-reliability.md)。
任务 B 使用 HttpOnly Cookie、持久化密码版本校验、密码条件更新、服务端失败限流、
Origin/CSRF/CORS 校验及凭据化 SSE；部署与剩余隔离限制见 [admin-security.md](admin-security.md)。

核对源码后保留了已经存在的初始化失败关闭行为、首次设密 INSERT 冲突保护、
密钥移出文件浏览目录逻辑和任务终态 first-writer-wins 守卫。未改动 engine_timeout、
manager 的超时/完成终态竞争流程；数据库行触发器覆盖这些现有状态写入路径。

## 实际验证

- Go 1.26.3，Windows，临时 PostgreSQL 16.9，仅监听 127.0.0.1:55439；不使用业务库。
- `go test ./... -json`：903 项通过、14 项失败、11 项跳过（含子测试）。退出码 1。
- 修复前源码快照在另一独立测试库运行全量测试：896 项通过、14 项失败、11 项跳过。
- 两次全量运行均存在 Windows 目录 Sync 拒绝、skillRelPath 分隔符差异、任务范围/目标初始化
  依赖共享设置的问题。llmrec 消费取消、会话 pin 状态和图查询死锁在不同运行间有波动。
- 最新运行额外出现 TestBlackboardToolsReadDirectSources 的 PostgreSQL 40P01 死锁；
  修复前、修复后分别在隔离库单跑均通过。没有删除、跳过或放宽失败断言。
- `go test ./db ./server ./agent -run 'Test(Timeout|Merge|Auth|ValidatePassword|ProxyEnv|BlackboardToolsReadDirectSources)' -count=1 -json`：25 项通过，无失败/跳过。
- `go vet ./...`：退出码 0。
- `npm run build`：退出码 0。
- `NEXT_EXPORT=1 npm run build`：退出码 0（Docker 静态前端）。
- `NEXT_PUBLIC_MOCK=1 npm run build`：退出码 0。
- `npm run check`：退出码 1；最终统计见下文。基线也失败（233 错误、136 警告），
  未全库格式化、关闭规则或修复无关模块。核心 auth/api/proxy 文件的定向 Biome 检查通过。
- `docker compose config`：未运行成功，主机未安装 docker；用 YAML 解析器核对端口、
  capabilities 和资源限制结构通过。未宣称容器构建或部署已经验证。
- 真实本机 Chrome + Playwright：初始化、刷新、HttpOnly、localStorage 无 JWT、同源 SSE、
  Next 开发跨端口 SSE、改密撤销、新密码登录、注销，共 9 个检查通过。
- `node --check web/scripts/auth-browser-check.mjs` 与该脚本的 Biome 检查通过。

浏览器复测需新建空白临时数据库、启动 Go :8787 与 Next dev :3000，另行安装 Playwright，
然后运行 `node web/scripts/auth-browser-check.mjs`。可配置 PLAYWRIGHT_MODULE（模块入口文件
绝对路径）、PLAYWRIGHT_EXECUTABLE（浏览器路径）、ARTEX_TEST_WEB。脚本只用于可丢弃环境，
会初始化并修改测试管理员密码。

## 兼容性

数据库迁移自动、幂等，不回填历史 timeout，不改动现有触发配置和其他触发条件。
升级需重新登录，登录/初始化响应从 token 改为 ok；脚本客户端需使用 Cookie jar。
改密后所有旧 JWT 立即无法通过新请求认证，活动 SSE 最迟下一秒复核时关闭。
Compose 与本机二进制默认仅本地监听；HTTPS 代理需配置可信代理和 Secure Cookie。
镜像需要重新构建；Linux 的 data/skills 绑定目录需给 UID 10001 相应写权限。

## 未完成的安全边界

当前 API 与本地 Agent 仍共享用户身份，不能阻止任意本地工具读取同 UID 文件/进程环境。
子进程环境清理和独立密钥卷只降低误暴露，不是强隔离。完整隔离需要独立执行器协议和
不同容器/用户，未在本次进行大规模改造。非特权镜像中的原始套接字扫描模式需要独立
受控执行器。登录限流是单实例内存状态，重启会重置；多实例需额外共享限流。
超时工具副作用采用至少一次语义，不能承诺严格 Exactly-Once。

## 修改文件与目的

最终前端全量检查：231 错误、135 警告、734 提示。

- `.env.example`：新增可信代理、来源与资源参数示例。
- `Dockerfile`：非 root 身份及共享浏览器目录。
- `agent/proxyenv_test.go`：验证直连仍清理凭据，并保留代理/CA 检查。
- `agent/worker.go`：所有 Bash/shell 入口的继承环境清理。
- `cmd/artex/main.go`：默认本机 HTTP 监听。
- `db/settings.go`：密码原子条件更新。
- `docker-compose.yml`：回环端口、密钥卷、capabilities 与资源限制。
- `server/auth.go`：Cookie 签发、登录/改密及密钥读失败关闭。
- `server/auth_test.go`：原失败关闭测试保留，并提供真实版本测试凭据。
- `server/core_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/customtool.go`：Python 子进程凭据清理。
- `server/finding_traffic_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/inheritance_api_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/intercept_detail_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/intercept_filter_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/mcpdiscover.go`：stdio MCP 继承凭据清理。
- `server/mgmt_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/server.go`：接入注销路由和认证目录。
- `server/side_questions_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/task_archives_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/task_categories_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/task_control_routes_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/task_metadata_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `server/task_templates_test.go`：现有 HTTP 测试凭据接入新的持久化版本校验，断言保留。
- `web/src/app/(auth)/login/page.tsx`：通过服务器状态检查和 Cookie 登录。
- `web/src/app/(auth)/setup/page.tsx`：Cookie 初始化登录。
- `web/src/app/(main)/_components/sidebar/account-switcher.tsx`：服务端注销或改密后的重新登录流程。
- `web/src/app/(main)/_components/sidebar/change-password-dialog.tsx`：服务端注销或改密后的重新登录流程。
- `web/src/app/(main)/_components/sidebar/nav-user.tsx`：服务端注销或改密后的重新登录流程。
- `web/src/app/(main)/function/tasks/detail/_tabs/sessions-tab.tsx`：统一 Cookie SSE 与会话失效处理。
- `web/src/app/(main)/layout.tsx`：服务器认证状态路由守卫。
- `web/src/app/(main)/system/logs/page.tsx`：统一 Cookie SSE 与会话失效处理。
- `web/src/app/(main)/system/settings/_components/update-card.tsx`：统一 Cookie SSE 与会话失效处理。
- `web/src/hooks/use-side-questions.ts`：统一 Cookie SSE 与会话失效处理。
- `web/src/lib/api.ts`：统一 Cookie 请求、上传下载、SSE 和 401 跳转。
- `web/src/lib/auth.ts`：清除旧存储，服务端注销，移除 JWT 读写。
- `web/src/lib/mock/handler.ts`：保持演示模式的新认证响应。
- `web/src/proxy.ts`：避免过期 Cookie 导致重定向循环。
- `agent/child_environment.go`：集中列出服务器专用环境凭据。
- `server/auth_security.go`：版本鉴权、限流、CSRF、CORS、可信代理及 SSE 撤销。
- `server/auth_security_test.go`：认证安全、并发和故障回归。
- `web/scripts/auth-browser-check.mjs`：可重复浏览器流程验证。
- `docs/admin-security.md`：部署参数、升级行为与隔离限制。

任务 A 文件：db/schema.sql、db/triggers.go、db/timeout_outbox.go、db/timeout_outbox_test.go、server/scheduler.go、server/conversations.go、server/timeout_delivery.go、server/trigger_merge_test.go、server/server.go，以及超时投递说明。

此前域名修改以及用户自己的 README 改动不纳入这两个提交。
