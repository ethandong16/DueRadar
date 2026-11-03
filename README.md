# DueRadar

把作业、考试和学习截止时间放在一处。为小型 Linux 设备和自托管服务器开发的学习提醒服务，使用 Bark 向移动设备发送通知。

**当前为 0.3.3-dev 开发版。** 多用户功能正在实现，账号隔离、旧版数据迁移与多用户运行仍待验收，下文相关说明是当前设计和使用约定，并非生产就绪或迁移已验证的声明。本版加强运行安全与可访问性：收紧 systemd 沙箱、补齐浏览器安全响应头、规范化凭据文件权限，并避免通知错误泄露服务地址；登录限速在多次失败后限制同一来源的验证频率，正确密码也可能需要短暂等待，但不会被永久锁住。同时改进登录和主题控件的键盘操作、字号可读性，后台刷新后保持焦点，任务总览新增下一次提醒预览，已结束通知记录保留 180 天。重新扫码失败会立即复核原有绑定，不再暂停提醒到下一轮同步。同时修复二月上旬任务被误判为历史学期、短暂同步失败丢失课上答题提醒、空时区静默退回 UTC 等问题；首次密钥写入改为原子提交，容器健康检查跟随监听地址。移动端继续使用静态资源预压缩与版本化缓存，认证和敏感接口禁止缓存。普通雨课堂微信扫码、知到 APP 扫码、真实任务同步和 Bark 通知已接通。普通雨课堂另提供电脑辅助登录程序和一次性会话配对，真实密码／验证码流程仍待验收。知到校内课覆盖 AI 智课和旧版翻转课；共享课待接入验证。完整产品需求见 [REQUIREMENTS.md](REQUIREMENTS.md)。

## 已有功能

- Go 单服务，网页与时区数据内嵌，无 Node、数据库服务或常驻浏览器运行依赖。
- 原子持久化任务、账号状态、通知事件；保留完成记录和未知状态。
- 任务清单默认只显示本学期；上学期课程及其课程视频可在筛选中展开，且不再参与提醒。
- 全局提前时间、跨午夜静默、考试前 1 小时／截止前 30 分钟例外。
- 雨课堂课上答题监测：上课期间按秒级间隔检查随堂答题，出现新的答题立即通过 Bark 提醒，静默时段照常发送，同一题只提醒一次。
- 首次逐项通知、时间变更、状态核验后的恢复补发、持久化去重和有上限的重试。
- 登录过期与连续三轮同步失败的故障事件，并区分采集与通知状态。
- 个人账号用户名／密码注册与登录；每人独立的任务状态、平台会话和 Bark 配置，单实例最多 16 个账号（开发中，待验收）。
- 登录来源校验、失败尝试限速；平台会话与 Bark Key 使用 AES-256-GCM 加密保存在各自的数据目录。
- 官方二维码绑定，登录后使用 HTTP 独立同步，设备无需常驻浏览器。
- 普通雨课堂电脑辅助登录：官方页面输入密码，限时一次性配对，设备独立验证后保存会话，见 [使用说明](docs/DESKTOP-LOGIN.md)。
- Dockerfile、Compose、systemd 模板和 Linux amd64／arm64／armv7 构建。



## 本机启动

需要 Go 1.25 或更高版本；运行编译后的程序无需 Go。

```sh
go test ./...
go build -trimpath -ldflags="-s -w" -o dueradar ./cmd/dueradar
export DUERADAR_ALLOW_REGISTRATION=true
./dueradar -listen 127.0.0.1:8080 -data ./data
```

打开 `http://127.0.0.1:8080`，选择「创建账号」，再用个人用户名和密码登录。班级共用时每人创建自己的账号、绑定自己的平台并配置自己的 Bark；用户名为 3—32 个字符，密码至少 12 字节。每次平台重新绑定成功后，该平台此前的任务、通知历史与课上答题去重记录都会清除，再以新的平台会话重新同步；即使扫回同一账号也是如此。最多 16 个账号，暂不提供删除账号或修改账号凭据的界面，请妥善保管登录信息。PowerShell 使用 `$env:DUERADAR_ALLOW_REGISTRATION='true'` 设置环境变量。

`DUERADAR_ALLOW_REGISTRATION=false` 在已有第一个账号后关闭新注册，不影响已有账号登录；没有账号时仍可注册第一个账号。当前开发版未设置或留空时允许注册，但默认策略仍在讨论，部署时不要依赖该默认值：示例显式设置 `true` 供班级使用；不需要开放注册时请显式设置 `false`。注册开放时能访问服务的人都可以创建账号，达到上限后无法继续注册，部署者应按使用范围控制访问。

新数据目录无需设置 `DUERADAR_ADMIN_PASSWORD`；若在注册表不存在时非空设置它，则会创建初始 `owner` 账号。使用旧数据目录升级时，若尚无用户注册表，必须在首次启动前设置原管理密码，以创建 `owner` 账号并保留旧数据；注册表一旦存在，修改或移除该环境变量不会重置已有账号密码。详见「数据与升级」。

演示界面使用独立的 `data/demo` 数据目录，也需创建或登录个人账号，且禁止保存 Bark 凭据和发送真实通知：

```sh
./dueradar -demo
```



课上答题提醒默认开启，可在「通知设置」中关闭或把检查间隔改为 10—300 秒。它只在雨课堂报告有课进行中时才请求平台：先用在课接口确认班级正在上课，再读取该班级最新一页活动，发现新的可作答活动立即生成即时通知；课后自动停止，不增加常驻请求。

## Linux 设备直接运行

在目标架构上交叉编译，以下示例适用于 ARM64：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/dueradar-linux-arm64 ./cmd/dueradar
```

将程序安装到 `/opt/dueradar/dueradar`，将 [systemd 模板](deploy/dueradar.service) 安装到 `/etc/systemd/system/dueradar.service`。创建仅 root 可读的 `/etc/dueradar.env`：

```dotenv
# 新安装留空；旧数据目录首次迁移时填入原管理密码
DUERADAR_ADMIN_PASSWORD=
DUERADAR_ALLOW_REGISTRATION=true
DUERADAR_SECURE_COOKIES=false
DUERADAR_LISTEN=127.0.0.1:8080
```

```sh
chmod 600 /etc/dueradar.env
systemctl daemon-reload
systemctl enable --now dueradar
systemctl status dueradar
```

systemd 使用独立动态用户；`StateDirectory=dueradar` 把数据放在 `/var/lib/private/dueradar`，并以 `/var/lib/dueradar` 符号链接暴露。局域网或 Tailscale 访问时，将监听地址改为相应网卡 IP；公网部署使用 HTTPS 反向代理并设置 `DUERADAR_SECURE_COOKIES=true`，并在可信代理配置按真实客户端地址的登录限速及访问控制。反向代理保留原始 Host；当请求来自本机、私有网段或 Tailscale 地址（例如本机运行的 cloudflared 隧道）时，应用以 Cloudflare 的 `CF-Connecting-IP` 作为限速来源，来自公网直连的请求一律使用实际对端地址，因此无法伪造。

## Docker

```sh
cp .env.example .env
# 编辑 .env：确认注册策略；旧数据首次迁移时填写原管理密码
docker compose up -d --build
docker compose logs --tail 50
```

Compose 默认只发布本机 `127.0.0.1:8080`，使用持久卷保存任务与加密密钥，日志轮换上限为 3 × 2 MiB。镜像以非 root 用户和只读根文件系统运行；健康检查会跟随 `DUERADAR_LISTEN` 的端口，因此修改监听地址后容器仍能正确报告健康状态。容器验收与直接运行验收应分开记录。

## 数据与升级

停止服务后备份整个数据目录，包括 `state.json`、`secrets.enc` 和 `master.key`；密钥与密文必须一起保留。使用 systemd 模板时，请备份解析后的 `/var/lib/private/dueradar`，或让备份工具跟随符号链接（如 `tar -h`、`rsync -L`），否则可能只备份到 `/var/lib/dueradar` 链接本身。设备上的同一数据目录只允许一个服务进程使用。不要把数据目录、环境文件、平台 Cookie 或 `.local` 目录提交到 Git。

程序启动时先取得数据目录的系统锁，再打开任务和凭据。重复启动会以 `data directory is already in use` 退出；即使改用其他端口也不会同时写同一目录。进程退出或崩溃后系统自动释放锁，留下的 `.dueradar.lock` 文件可以继续复用，无须删除。不同数据目录可分别运行；请将数据放在支持文件锁的本地持久卷。

从单用户版本升级前，先停止服务，备份整个数据目录和旧二进制；不要只备份 `state.json`。对 Compose，请备份完整 `dueradar-data` 持久卷。多用户迁移仍待实测，应先在备份副本上验证登录、任务、平台绑定和 Bark，确认结果后再升级正式数据。

旧数据目录尚无 `users.json` 时，首次启动前把 `DUERADAR_ADMIN_PASSWORD` 设为原管理密码（至少 12 字节）。程序以用户名 `owner` 创建初始账号，旧 `state.json`、`secrets.enc` 和 `master.key` 继续留在数据目录根部，由 `owner` 使用；不迁入旧版 DueRadar 网页登录会话，需以 `owner` 和该密码重新登录。其他新注册用户的数据放在 `data/users/<随机用户ID>/`；用户注册表、网页登录会话和各用户目录都属于完整备份的一部分。已有注册表时环境变量不再更改账号密码。

升级后检查账号、任务、平台会话和 Bark；状态格式含版本号，不支持的版本会停止启动。若 `users.json` 丢失而 `users/` 仍有内容，服务会拒绝启动，需从同一时间点的备份恢复匹配的注册表和用户数据；首次注册中断也可能留下未登记目录，先检查并备份，不要直接覆盖。旧版不理解多用户数据：回滚前需停止服务并同时恢复匹配旧版本的完整数据备份；新建账号及其数据不会出现在升级前的备份中，会随回滚丢失。回滚前另行保存升级后的完整数据目录，避免覆盖新数据。

## 开发与验证

```sh
go test ./...
go vet ./...
node --test web/*.test.cjs
python -m unittest discover -s scripts -p '*_test.py'
# Linux / 支持 C 工具链的环境
go test -race ./...
```

提交会触发 GitHub Actions 单元测试、竞态检查、多架构构建和 Docker 构建。提交前应核对 [验收记录](docs/ACCEPTANCE.md)，真实平台的登录、采集和 iPhone 到达情况逐项记录，不能由示例数据推断。

采用的建议默认值、恢复算法和已知局限见 [ARCHITECTURE.md](docs/ARCHITECTURE.md)。许可证尚未选定；当前仓库先私有保存，公开发布前确定许可证。
