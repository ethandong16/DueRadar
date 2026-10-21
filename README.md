# DueRadar

把作业、考试和学习截止时间放在一处。为 小型 Linux 设备 和小型 Linux 设备开发的个人学习提醒服务，使用 Bark 向 iPhone 发送通知。

**当前为 0.1 开发版。** 普通雨课堂微信扫码、知到 APP 扫码、真实任务同步和 Bark 通知已接通。知到校内课覆盖 AI 智课和旧版翻转课；共享课、账号密码登录仍待接入验证。完整产品需求见 [REQUIREMENTS.md](REQUIREMENTS.md)。

## 已有功能

- Go 单服务，网页与时区数据内嵌，无 Node、数据库服务或常驻浏览器运行依赖。
- 原子持久化任务、账号状态、通知事件；保留完成记录和未知状态。
- 全局提前时间、跨午夜静默、考试前 1 小时／截止前 30 分钟例外。
- 首次逐项通知、时间变更、状态核验后的恢复补发、持久化去重和有上限的重试。
- 登录过期与连续三轮同步失败的故障事件，并区分采集与通知状态。
- 管理登录、来源校验、登录限速；平台会话与 Bark Key 使用 AES-256-GCM 加密保存在数据目录。
- 官方二维码绑定，登录后使用 HTTP 独立同步，设备无需常驻浏览器。
- Dockerfile、Compose、systemd 模板和 Linux amd64／arm64／armv7 构建。



## 本机启动

需要 Go 1.25 或更高版本；运行编译后的程序无需 Go。

```sh
go test ./...
go build -trimpath -ldflags="-s -w" -o dueradar ./cmd/dueradar
export DUERADAR_ADMIN_PASSWORD='替换为至少12字符的随机管理密码'
./dueradar -listen 127.0.0.1:8080 -data ./data
```

打开 `http://127.0.0.1:8080`，使用管理密码登录。PowerShell 使用 `$env:DUERADAR_ADMIN_PASSWORD='你的管理密码'` 设置环境变量。

演示界面使用独立的 `data/demo` 数据目录，且禁止保存 Bark 凭据和发送真实通知：

```sh
./dueradar -demo
```



## 小型 Linux 设备 直接运行

设备测量见 [DEVICE.md](docs/DEVICE.md)。当前设备为 ARM64，交叉编译命令：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/dueradar-linux-arm64 ./cmd/dueradar
```

将程序安装到 `/opt/dueradar/dueradar`，将 [systemd 模板](deploy/dueradar.service) 安装到 `/etc/systemd/system/dueradar.service`。创建仅 root 可读的 `/etc/dueradar.env`：

```dotenv
DUERADAR_ADMIN_PASSWORD=替换为至少12字符的随机管理密码
DUERADAR_LISTEN=127.0.0.1:8080
```

```sh
chmod 600 /etc/dueradar.env
systemctl daemon-reload
systemctl enable --now dueradar
systemctl status dueradar
```

systemd 使用独立动态用户，数据保存在 `/var/lib/dueradar`。局域网或 Tailscale 访问时，将监听地址改为相应网卡 IP；公网部署使用 HTTPS 反向代理并设置 `DUERADAR_SECURE_COOKIES=true`。反向代理保留原始 Host，应用不信任用户提交的转发地址头。

## Docker

```sh
cp .env.example .env
# 编辑 .env，设置随机管理密码
docker compose up -d --build
docker compose logs --tail 50
```

Compose 默认只发布本机 `127.0.0.1:8080`，使用持久卷保存任务与加密密钥，日志轮换上限为 3 × 2 MiB。镜像以非 root 用户和只读根文件系统运行。小型 Linux 设备 当前未安装 Docker，容器验收与直接运行验收分开记录。

## 数据与升级

停止服务后备份整个数据目录，包括 `state.json`、`secrets.enc` 和 `master.key`；密钥与密文必须一起保留。设备上的同一数据目录只允许一个服务进程使用。不要把数据目录、环境文件、平台 Cookie 或 `.local` 目录提交到 Git。

升级时保留旧二进制和完整数据目录，停止服务、替换程序、重启并检查运行状态。状态格式含版本号，不支持的版本会停止启动。恢复旧版本前同时恢复相应版本的数据备份。

## 开发与验证

```sh
go test ./...
go vet ./...
# Linux / 支持 C 工具链的环境
go test -race ./...
```

提交会触发 GitHub Actions 单元测试、竞态检查、多架构构建和 Docker 构建。提交前应核对 [验收记录](docs/ACCEPTANCE.md)，真实平台的登录、采集和 iPhone 到达情况逐项记录，不能由示例数据推断。

采用的建议默认值、恢复算法和已知局限见 [ARCHITECTURE.md](docs/ARCHITECTURE.md)。许可证尚未选定；当前仓库先私有保存，公开发布前确定许可证。
