# 生产部署

本文档描述标准 Linux systemd 部署。其它进程管理器（PM2、supervisord、OpenRC）同理。

若你更偏向容器化部署，仓库已提供：

- [Dockerfile](/Users/wuhao/Developer/Temp/Windsurf_To_API_Go/Dockerfile)
- [compose.yml](/Users/wuhao/Developer/Temp/Windsurf_To_API_Go/compose.yml)
- [docker workflow](/Users/wuhao/Developer/Temp/Windsurf_To_API_Go/.github/workflows/docker.yml)

## 推荐布局

```
/opt/windsurfapi/
├── windsurfapi                   # Linux amd64 二进制
├── bin/
│   └── language_server_linux_x64
├── .env                          # 不要 git 追踪
├── accounts.json                 # 运行时生成
├── proxy.json                    # 运行时生成（若配置了代理）
├── runtime-config.json           # 运行时生成
├── model-access.json             # 运行时生成
├── stats.json                    # 运行时生成
└── logs/                         # JSONL 日志
```

## Docker 部署

镜像发布到 GitHub Container Registry：

```text
ghcr.io/wuhao1477/windsurf-to-api-go
```

拉取并运行：

```bash
mkdir -p .docker-data

docker run -d \
  --name windsurfapi \
  --restart unless-stopped \
  --platform=linux/amd64 \
  -p 3003:3003 \
  --env-file .env \
  -v $(pwd)/.docker-data:/data \
  -v windsurf-ls-data:/opt/windsurf/data \
  -v windsurf-workspace:/tmp/windsurf-workspace \
  ghcr.io/wuhao1477/windsurf-to-api-go:latest
```

或者：

```bash
docker compose up -d
```

容器化时建议至少持久化：

- `/data`
- `/opt/windsurf/data`

## 创建专用用户

以 root 运行直接上 LS 是可以的，但建议降权。LS 不需要 root。

```bash
sudo useradd --system --home /opt/windsurfapi --shell /usr/sbin/nologin windsurfapi
sudo chown -R windsurfapi:windsurfapi /opt/windsurfapi
sudo chmod 755 /opt/windsurfapi
sudo chmod 600 /opt/windsurfapi/.env
sudo chmod +x /opt/windsurfapi/windsurfapi /opt/windsurfapi/bin/language_server_linux_x64
```

## systemd 单元

`/etc/systemd/system/windsurfapi.service`：

```ini
[Unit]
Description=WindsurfAPI (Go) — OpenAI/Anthropic 兼容的 Windsurf 反向代理
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=windsurfapi
Group=windsurfapi
WorkingDirectory=/opt/windsurfapi
ExecStart=/opt/windsurfapi/windsurfapi
EnvironmentFile=/opt/windsurfapi/.env
Restart=on-failure
RestartSec=3

# 隔离强化
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/windsurfapi /opt/windsurf /tmp/windsurf-workspace
LimitNOFILE=65536

# LS 需要 HOME 写 /opt/windsurf/data，显式指定
Environment=HOME=/opt/windsurfapi

[Install]
WantedBy=multi-user.target
```

应用并启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now windsurfapi
sudo systemctl status windsurfapi
```

## 启动自检

```bash
# 进程
pgrep -af 'windsurfapi|language_server_linux_x64'

# 日志（最近 20 行）
sudo journalctl -u windsurfapi -n 20 --no-pager

# 监听端口
ss -lntp | grep -E ':(3003|42100)'

# 健康检查
curl -s http://127.0.0.1:3003/health | python3 -m json.tool
```

## 推送新版本（SCP + systemd 重启）

1. 本机编译 Linux 二进制：
   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -trimpath -o windsurfapi-linux ./cmd/windsurfapi
   ```
2. SCP 上传：
   ```bash
   scp windsurfapi-linux root@<host>:/opt/windsurfapi/windsurfapi.new
   ```
3. 服务端切换 + 重启：
   ```bash
   sudo systemctl stop windsurfapi
   sudo mv /opt/windsurfapi/windsurfapi.new /opt/windsurfapi/windsurfapi
   sudo chown windsurfapi:windsurfapi /opt/windsurfapi/windsurfapi
   sudo chmod +x /opt/windsurfapi/windsurfapi
   sudo systemctl start windsurfapi
   ```

停服务前记得关注是否有正在跑的长连接（SSE / stream）—— systemd stop 会发 SIGTERM，Go 侧有 30 秒的 graceful drain 窗口。

## 反向代理（可选）

若要挂在 Nginx 后面做 TLS：

```nginx
upstream windsurfapi {
    server 127.0.0.1:3003;
    keepalive 64;
}

server {
    listen 443 ssl http2;
    server_name api.example.com;
    ssl_certificate     /etc/letsencrypt/live/api.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.example.com/privkey.pem;

    client_max_body_size 32m;

    location / {
        proxy_pass http://windsurfapi;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE / 长连接
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        chunked_transfer_encoding on;
    }
}
```

对应调整 `CORS_ALLOWED_ORIGINS`，见 [ENV_VARS.md](ENV_VARS.md)。

## 一键自更新

控制台仪表盘右上角 **检查更新** / **一键更新重启** 执行的是：

1. `git fetch origin <branch>`
2. 对比 commit → 有新则 `git pull`
3. 重新 `go build`（如果是 git 部署）
4. `os.Exit(0)` → systemd `Restart=on-failure` 自动拉起

**前提**：`/opt/windsurfapi/` 必须是 git 工作区（即 `git clone` 安装的部署，而不是 SCP 上传的裸二进制）。SFTP / 压缩包部署的话这条路径会提示"此实例不是 git 部署，一键更新不可用"，改为手动 SCP + 重启。

## 多实例部署

以多个独立出口 IP（不同代理）服务不同地区用户：
- 同一台机器：单实例足够，通过控制台"代理配置 → 新增账号级代理"绑定不同代理到不同账号。每个独立代理会启动**独立的** Language Server 实例，互不污染
- 多台机器：每台一份 `.env` + `accounts.json`，通过域名 / 负载均衡器按请求头分流

## 备份

真正需要备份的：
- `accounts.json`（账号池，**含 API Key**，加密或限权存储）
- `proxy.json`（代理配置，可能含账密）
- `runtime-config.json`（实验性开关 + 自定义身份提示）
- `model-access.json`（模型黑白名单）
- `.env`（密钥）

`stats.json` 是可丢弃的统计，`logs/` 根据保留策略轮转。

## 常见问题

### 服务反复重启

- `journalctl -u windsurfapi -n 100` 找错误信息
- 最常见：`bin/language_server_linux_x64` 权限不对或找不到
- 其次：`PORT` 被占用 —— `ss -lntp | grep :3003` 查

### LS 启动后立即 SIGKILL

Cgroup 内存限制太紧。LS 空载约 50–100 MB，给的 MemoryLimit 至少 256 MB。

### 日志里看到 `LS default port 42100 already in use — adopting`

说明上次 LS 退出不干净（SIGKILL 后端口未立即释放），当前进程把那端口上的 LS 认领了。v1.2.0 之后 StopAll 会**阻塞等端口真正释放**才返回，该消息会显著减少。
