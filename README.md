# email_get

本项目现在是一个 Go Web + Vue 的本地桌面式 Web 应用。

- Go 后端负责账号解析、MX / IMAP / POP / SMTP 自动探测、IMAP / POP3 收件、API 和静态资源嵌入
- Vue 前端负责账号导入、协议选择、批量接邮件、当前邮件列表展示
- 最终产物是单个 Go 可执行文件，前端资源通过 `go:embed` 打进程序里

## 安装与运行

提供两种部署方式：

1. 原生运行：直接执行 `release/` 下对应操作系统的二进制文件
2. Docker 运行：在目标机器上用 Docker 构建并启动 Linux AMD64 版本

### 方式 1：原生运行

源码仓库不跟踪编译后的二进制。如果使用独立发布包，直接把发布包中的 `release/` 目录拷到目标机器即可；如果从源码仓库获取项目，先按下文「跨平台构建」生成对应平台的二进制。

不需要随发布包携带真实 `app.db`。程序默认在可执行文件所在目录自动创建数据库；也可用 `--data-dir` 指定其他数据目录。已有数据库如需迁移，可放到同一数据目录中。

macOS Intel:

```bash
chmod +x ./release/email_get_darwin_amd64
./release/email_get_darwin_amd64
```

macOS Apple Silicon:

```bash
chmod +x ./release/email_get_darwin_arm64
./release/email_get_darwin_arm64
```

Linux x86_64:

```bash
chmod +x ./release/email_get_linux_amd64
./release/email_get_linux_amd64
```

Linux arm64:

```bash
chmod +x ./release/email_get_linux_arm64
./release/email_get_linux_arm64
```

Windows x86_64:

```powershell
.\release\email_get_windows_amd64.exe
```

Windows arm64:

```powershell
.\release\email_get_windows_arm64.exe
```

启动后默认监听本机 `127.0.0.1:80`，浏览器打开 `http://127.0.0.1` 即可。

如果要让局域网内其他机器访问，可以加参数：

```bash
./release/email_get_linux_amd64 --listen 0.0.0.0:80
```

### 方式 2：Docker 运行

适合目标机器已经安装 Docker 的场景。当前 Docker 镜像使用 `release/email_get_linux_amd64`，因此目标环境应为 `linux/amd64`，或具备对应的模拟运行能力。

目标机器需要准备这些文件：

- `Dockerfile`
- `docker-compose.yml`
- `release/email_get_linux_amd64`

源码仓库不附带该二进制。在安装 Go、Node.js 的开发环境中先执行：

```bash
TARGETS=linux/amd64 ./build_release.sh
```

不需要准备 `app.db`。首次启动会在 `/app/data` 自动创建空数据库，并生成初始管理密码。

直接使用 Docker：

```bash
docker build -t email_get .
docker run -d --name email_get -p 80:80 -v email_get_data:/app/data email_get
```

使用 Compose：

```bash
docker compose up -d --build
docker compose logs -f
```

如果本机使用旧版独立 Compose，可将上述命令替换为 `docker-compose up -d --build` 和 `docker-compose logs -f`。

查看初始管理密码：

```bash
docker compose exec email_get cat /app/data/auth_password.txt
```

也可以从首次启动日志中查看：

```bash
docker compose logs email_get
```

启动后访问 `http://目标机器IP`。公网部署时建议在服务前配置 HTTPS 反向代理，不要直接暴露明文 HTTP。

`email_get_data` 命名卷会持久化 `/app/data` 中的数据库和密码哈希。同一数据卷重启或重建容器会沿用原管理密码；删除该卷后再次启动会创建新数据库并生成新的随机密码。执行 `docker compose down` 默认保留命名卷，`docker compose down -v` 会删除它。

## 支持的账号格式

```text
邮箱----密码
邮箱----密码----服务器----端口
邮箱----密码----client_id----refresh_token
```

说明：

- 两段格式适合普通邮箱，协议由界面上选择 `auto / imap / pop3`
- 四段 `服务器----端口` 在 `imap` 模式下按 IMAP 处理，在 `pop3` 模式下按 POP3 处理
- Outlook / Hotmail / Live 的四段 OAuth 格式会自动按 `client_id + refresh_token` 换取 IMAP / POP3 access token

## 源码启动（开发）

先构建前端：

```bash
cd web
npm install
npm run build
cd ..
```

再编译 Go 应用：

```bash
go build -o email_get_app .
```

启动：

```bash
./email_get_app --listen 127.0.0.1:80 --data-dir ./app_data --open
```

参数：

- `--listen` Web 服务监听地址
- `--data-dir` SQLite 数据库和应用数据目录。默认是可执行文件所在目录
- `--auth-password` 设置/重置 Web 界面管理密码(见下文「登录认证」)
- `--fetch-timeout` 单条 IMAP/POP3 命令超时,默认 `30s`(大邮件用 8s 拉不下来)
- `--open` 启动后自动打开浏览器

收件与远端搜索默认覆盖 `INBOX` + `Junk` 两个文件夹(验证码常被判进垃圾邮件),
API 可通过 `folders` 参数自定义;返回的邮件带 `folder` 字段标识来源文件夹。
POP3 协议本身没有文件夹概念,会忽略该参数。

## 登录认证

Web 界面和 API 默认要求登录(单租户,只有一个管理密码):

- 数据库中没有密码哈希且未传 `--auth-password` 时,首次启动会使用 `crypto/rand` 生成 9 字节随机数,并编码为 12 字符管理密码
- 随机密码会打印到首次启动日志,并写入数据目录下的 `auth_password.txt`(权限 0600)
- 同一数据目录重启会沿用数据库中的密码,**不是每次启动都重新生成**
- 换用空数据目录、删除 Docker 数据卷后重建,才会再次随机生成;仅删除 `auth_password.txt` 而保留数据库不会重新生成
- 用 `--auth-password 你的密码` 启动可以主动设置或重置密码
- 浏览器打开任意页面会先跳到 `/login`,输入密码后进入主界面;登录态保存在 cookie 里,有效期 7 天
- 未登录调用 API 会返回 401

## 前端资源压缩与缓存

服务端对 HTML / JS / CSS / JSON 等文本响应自动启用 gzip 压缩(客户端支持时),
`assets/` 下带内容 hash 的静态文件带 `Cache-Control: immutable` 长期缓存,HTML 不缓存。

## 前端功能

- 粘贴多行账号，自动解析你现在这种 `----` 分隔格式
- 导入时可选 `自动探测 / IMAP / POP3`
- 可在页面中维护代理列表，并通过开关控制是否让探测 / 收件 / OAuth 请求走代理
- 查看账号列表、最近成功收件时间、最近使用协议、错误信息
- 每个账号都可以单独发起后台“收件”任务
- 批量收件走后台任务，页面顶部实时显示进度，避免长时间同步请求把界面卡住
- 查看当前邮件列表、正文预览、附件名称
- 同一邮箱后缀探测成功后会缓存服务器配置，后续同后缀账号优先走缓存

## API

### `GET /api/accounts`

返回当前账号列表。

### `POST /api/accounts/import`

请求体：

```json
{
  "text": "邮箱----密码\n邮箱----密码----client_id----refresh_token",
  "protocol": "auto"
}
```

### `DELETE /api/accounts/{id}`

删除账号及其当前消息索引。

### `GET /api/messages?limit=200`

返回当前邮件列表。

### `GET /api/fetch`

返回当前活跃的后台收件任务；如果当前没有任务，返回 `{"job": null}`。

### `GET /api/fetch/{id}`

返回指定后台收件任务的详细状态。

### `POST /api/fetch`

请求体：

```json
{
  "accountIds": [],
  "emails": [],
  "limit": 20,
  "allMessages": true,
  "markSeen": false,
  "async": true
}
```

说明：

- `accountIds` 和 `emails` 都为空时表示抓全部账号
- `emails` 适合外部调用时直接按邮箱地址指定
- `allMessages=true` 表示抓最近邮件，不只未读
- `markSeen` 只对 IMAP 有意义，POP3 下没有实际意义
- `async=true` 时会启动后台收件任务并立即返回任务信息，前端默认使用这个模式
- 同一时间只允许一个后台收件任务运行；重复发起会直接返回当前任务
- `async` 省略或为 `false` 时，仍保留原来的同步返回 `results` 行为，兼容旧调用方

### `GET /api/proxy-settings`

返回当前代理开关和代理列表。

### `PUT /api/proxy-settings`

保存代理开关和代理列表。启用后，探测、收件和 OAuth token 请求都会按代理列表轮询使用代理。

## 跨平台构建

项目自带 [build_release.sh](./build_release.sh)：

```bash
./build_release.sh
```

它会：

1. 构建 Vue 前端
2. 编译 Go 嵌入版应用
3. 输出这些平台的二进制到 `release/`

- `darwin/amd64`
- `darwin/arm64`
- `linux/amd64`
- `linux/arm64`
- `windows/amd64`
- `windows/arm64`

## 当前目录说明

- [main.go](./main.go): 邮件协议与收件逻辑
- [app.go](./app.go): Web API、状态管理、静态资源嵌入
- [server_main.go](./server_main.go): Web 服务入口
- [web](./web): Vue 前端源码
- [email_receiver.py](./email_receiver.py): 之前的 Python 版本

## 注意

- `auto` 模式默认优先 IMAP，IMAP 不通时回退到 POP3
- 同一个邮箱后缀一旦探测成功，会缓存该后缀对应的服务器和端口，不再为每个账号重复完整探测
- 自定义域名邮箱如果自动探测不到，仍然可以手动指定服务器和端口
- POP3 不支持 IMAP 那种文件夹选择和未读搜索
- 账号、邮件列表和探测缓存现在保存在 SQLite 数据库 `app.db`
