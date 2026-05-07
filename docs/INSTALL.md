# 安装

两种路径：**从源码编译**（推荐）或 **预编译二进制**。

另提供 **Docker / Docker Compose** 部署方式，适合直接跑 Linux amd64 容器。

## 从源码编译

### 前置

- Go ≥ 1.26.2（`go.mod` 里的 `go` directive 值）
- Node.js ≥ 20 + `pnpm`（仅当你要改前端时才需要；仓库已提交 `internal/web/dist/` 构建产物）
- Git

### 步骤

```bash
git clone https://cnb.cool/Neko_Kernel/Windsurf_To_API_Go.git
cd Windsurf_To_API_Go

# （可选）如果你改了 web/src/ 下的前端代码，重新构建 dist：
pnpm --dir web install
pnpm --dir web build

# 编译服务端
CGO_ENABLED=0 go build -ldflags='-s -w' -trimpath -o windsurfapi ./cmd/windsurfapi
```

交叉编译示例：

```bash
# Windows 下生成 Linux 二进制
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags='-s -w' -trimpath \
  -o windsurfapi-linux ./cmd/windsurfapi
```

产物约 9 MB，包含嵌入的 Vue SPA（约 1.1 MB 资源）和所有 Go 逻辑。无需携带任何附加文件。

## Language Server 二进制

程序依赖 Windsurf 官方的 `language_server_linux_x64`（178 MB），仓库 `bin/` 下已带。查找优先级：

1. `LS_BINARY_PATH` 环境变量（如果显式指定）
2. `<可执行文件目录>/bin/language_server_linux_x64`（相对 windsurfapi 所在目录）
3. `/opt/windsurf/language_server_linux_x64`（旧版兼容路径）

生产部署时，把 windsurfapi 二进制和 `bin/language_server_linux_x64` 放在同一目录即可，无需配置。

## 最小运行

```bash
cp .env.example .env
# 按需修改 .env（见 docs/ENV_VARS.md）

# 首次启动
./windsurfapi
```

日志输出类似：

```
[INFO] Starting LS instance key=default port=42100 proxy=
[INFO] LS instance default ready on port 42100
[INFO] Loaded 0 account(s) from disk
[INFO] Auth pool: 0 active, 0 error, 0 total
[WARN] No accounts configured. POST /auth/login to add accounts.
[INFO] Server on http://0.0.0.0:3003
```

看到 `Server on http://0.0.0.0:3003` 后，浏览器访问 `http://<主机 IP>:3003/dashboard` 完成首次登录与取号。

## Docker 安装

### 前置

- Docker Engine / Docker Desktop
- 推荐 `docker compose`
- 镜像目标架构：**linux/amd64**

### 本地构建

```bash
docker build -t windsurf-to-api-go .
```

### 直接运行

```bash
mkdir -p .docker-data

docker run -d \
  --name windsurfapi \
  --platform=linux/amd64 \
  -p 3003:3003 \
  --env-file .env \
  -v "$(pwd)/.docker-data:/data" \
  -v windsurf-ls-data:/opt/windsurf/data \
  -v windsurf-workspace:/tmp/windsurf-workspace \
  ghcr.io/wuhao1477/windsurf-to-api-go:latest
```

### Compose 运行

仓库根目录已提供 [compose.yml](/Users/wuhao/Developer/Temp/Windsurf_To_API_Go/compose.yml)：

```bash
mkdir -p .docker-data
docker compose up -d --build
```

### 容器内路径

- `/data`：账号池、代理配置、运行时配置、统计、日志
- `/opt/windsurf/data`：Windsurf Language Server 运行数据
- `/tmp/windsurf-workspace`：临时工作区

## 首次添加账号

方式一 · 控制台：进入 **登录取号** 页，填邮箱密码登录，或用 Google / GitHub OAuth（仅 localhost / windsurf.com 域名可用）。

方式二 · 命令行：

```bash
# 用 Windsurf 的 Auth Token（推荐，从 windsurf.com/show-auth-token 复制）
curl -s -X POST http://127.0.0.1:3003/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"token":"<你的 auth token>"}'

# 或邮箱密码
curl -s -X POST http://127.0.0.1:3003/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"..."}'
```

## 验证

```bash
# 健康检查（无需认证）
curl http://127.0.0.1:3003/health

# OpenAI 兼容聊天
curl -X POST http://127.0.0.1:3003/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"claude-4.5-sonnet-thinking",
    "messages":[{"role":"user","content":"你好"}]
  }'
```

无 `API_KEY` 环境变量时，上面的 Authorization 头可省略（开放模式）。正式生产**强烈建议**设置。
