# 管理员认证与部署

升级后旧 JWT（不带认证版本）统一失效，需要重新登录。密码及原数据库数据不变。
JWT 仅通过服务器设置的 `HttpOnly; SameSite=Lax; Path=/` Cookie 交付；有效期仍为 7 天。
密码 hash 的 SHA-256 指纹作为持久化会话版本，每次鉴权从数据库读取并匹配，数据库错误
返回 503。改密使用旧 hash 条件更新，防止并发改密覆盖；改密成功清除 Cookie。
旧版本的 localStorage Token 会在页面启动时移除，不再读取或写入 JWT。

`GET /api/auth/status` 返回 initialized、authenticated。登录及初始化成功返回 `{ok:true}`，
不再返回 JWT。`POST /api/auth/logout` 清除浏览器 Cookie；注销不撤销其他设备的会话。
API 脚本可使用 curl Cookie jar，修改类请求需带 `X-Artex-CSRF: 1`。已持有带当前版本的
Bearer Token 的非浏览器客户端仍可使用 Authorization；URL token 参数不再作为凭据。

修改类请求校验 Origin（存在时）并要求自定义 CSRF 请求头。CORS 只回显受信任的完整
Origin，允许 credentials，不使用 `*`。原生同源页面无需配置；localhost/127.0.0.1:3000
的 Next 开发页面可连接同主机的 Go 后端。其他开发地址或前端来源需显式填写：

```env
ARTEX_ALLOWED_ORIGINS=https://admin.example.com,http://192.0.2.10:3000
ARTEX_TRUSTED_PROXIES=127.0.0.1/32,::1/128
ARTEX_COOKIE_SECURE=1
```

只填写实际代理地址/CIDR。服务默认不信任 X-Forwarded-For 或 X-Forwarded-Proto。
可信代理必须覆盖这些请求头，保留原始 Host；HTTPS 部署设 COOKIE_SECURE=1。
Docker 桥接代理需填写其实际源地址而非照搬回环地址。错误配置可信来源将收到 403。

SSE 使用 Cookie；开发跨端口连接使用 withCredentials 和显式可信 CORS。所有日志、
任务、问答和更新事件共享该逻辑。会话失效时重连转回登录；已经建立的事件流每秒复核
认证版本，数据库故障也会关闭。HTTPS 反代应关闭 SSE 缓冲，保留 /api 路由及 Cookie。

登录及当前密码校验按 IP 限制 15 分钟内 5 次失败，并按单管理员账户限制 15 分钟内
20 次失败；超限返回 429 与 Retry-After。内存状态受互斥锁保护并清理过期项，最多
4096 个 IP；服务重启会重置速率计数。多服务实例需在反向代理增加共享限流。

## 网络与容器权限

Compose 默认把 8787、8788 都绑定到 127.0.0.1；容器内部仍监听 :8787/:8788，内部
PostgreSQL 通信不变。本机二进制默认只监听 127.0.0.1:8787，远程监听需显式传 -addr。
推荐 HTTPS 反向代理或 VPN。确需 LAN 直接访问可使用独立 Compose override：

```yaml
services:
  artex:
    ports: !override
      - "192.0.2.10:8787:8787"
      - "127.0.0.1:8788:8788"
```

!override 需要支持该标签的 Compose；旧版应编辑本地端口映射文件。请将示例 IP 替换为
宿主机实际 LAN 地址，并配置防火墙。不要把未认证的 8788 记录代理公开到公网。

镜像以 UID/GID 10001 的 artex 用户运行，禁用提权并移除 capabilities，限制进程数、
CPU、内存；上限可由 .env 调整。不挂载 Docker Socket。Playwright 浏览器放在共享只读
工具目录，普通用户仍可运行。data/ 和 skills/ 宿主机目录需允许 UID 10001 读写：Linux
升级前由宿主机管理员调整这两个具体目录的属主/ACL；Windows Docker Desktop 通常
自动处理共享权限。容器中 /app 可写以保留现有自更新功能，不设置只读根文件系统。
需要特权原始套接字的扫描模式须部署在独立、受控的扫描执行器；普通 TCP 扫描保留。

签名密钥通过独立 authdata 卷持久化到 /app/auth（0700，密钥0600），不进入可浏览的
工作目录。非 Docker 可配置 ARTEX_AUTH_DIR。不得把该目录放进 Agent 工作区。
Bash、交互 shell、Python 工具和 stdio MCP 默认清空继承的服务器数据库、LLM、更新
凭据；MCP 专用凭据可通过其独立 env 配置显式提供。

## 仍需独立执行环境的边界

上述措施不是 Web/API 与 Agent 的进程级隔离。同一 UID 下的任意 Bash、Read 和 MCP
仍可能读取 API 进程可访问的文件、进程环境或本地数据库。独立目录和子进程环境清理
不能阻止这种访问，因此不能宣称签名密钥和管理员数据对恶意本地工具已完全隔离。

高安全部署必须把 API/认证与扫描执行器放在不同用户和容器/主机：认证卷、数据库凭据
仅挂载 API；执行器仅接收任务数据及专用短期凭据；两端通过受认证的任务接口通信，
分别限制网络出站。当前单进程架构尚无该远程执行协议；完整实现属于独立架构工作，
本次未以禁用 Bash、浏览器、扫描器或 MCP 代替隔离。
