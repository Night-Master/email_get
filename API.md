# API

Base URL: `http://127.0.0.1:8080/`

## 服务启动

```bash
cd release
./email_get_linux_amd64 -listen 127.0.0.1:8080
```

- `-listen`:监听地址;`127.0.0.1:8080` 仅允许本机访问,`0.0.0.0:8080` 会对外开放 8080 端口
- `-auth-password`:可选,设置/重置管理密码;不填则沿用数据库中已有密码
- `-fetch-timeout`:单条 IMAP/POP3 命令超时,默认 `30s`(如 `-fetch-timeout 1m`);8 秒拉不动大邮件
- 数据目录默认是可执行文件所在目录(`release/`);`app.db` 不存在时会自动创建
- 收件/搜索默认覆盖 `INBOX` 和 `Junk` 两个文件夹(验证码常被判进垃圾邮件),API 可用 `folders` 参数覆盖

## Auth

除 `/api/health`、`/api/auth/login` 和 `/login` 外,所有页面与 API 都需要登录。
登录成功后会写入 `eg_session` cookie(HttpOnly,有效期 7 天),后续请求自动携带。
未登录访问页面会 302 到 `/login`,访问 API 会返回:

```json
{
  "error": "未登录或登录已过期"
}
```

### `POST /api/auth/login`

请求体:

```json
{
  "password": "你的管理密码"
}
```

响应:

```json
{
  "ok": true
}
```

密码错误返回 401。

### `POST /api/auth/logout`

清除登录 cookie。

```json
{
  "ok": true
}
```

管理密码通过启动参数 `-auth-password` 设置/重置;首次启动未设置且数据库中没有密码哈希时,
使用 `crypto/rand` 生成随机密码,打印到 stdout 并写入数据目录下 `auth_password.txt`。
同一数据目录重启会沿用原密码;仅删除 `auth_password.txt` 不会触发重新生成。

## Health

### `GET /api/health`

响应:

```json
{
  "ok": true
}
```

## Accounts

### `GET /api/accounts`

返回当前账号面板数据。

查询参数：

- `keyword`: 可选，按邮箱地址、服务器、client_id 模糊匹配

示例：

```text
GET /api/accounts
GET /api/accounts?keyword=outlook
GET /api/accounts?keyword=163.com
```

响应示例:

```json
{
  "accounts": [
    {
      "id": "fe92d40f7557e3ac",
      "email": "user@example.com",
      "server": "",
      "port": 0,
      "protocol": "auto",
      "hasOAuth": false,
      "hasServer": false,
      "createdAt": "2026-04-09T17:18:56.520563+08:00",
      "updatedAt": "2026-04-09T17:22:22.436422+08:00",
      "lastFetchedAt": "2026-04-09T17:22:22.436422+08:00",
      "lastError": "",
      "lastProtocol": "imap"
    }
  ]
}
```

### `POST /api/accounts/import`

导入多行账号文本。

请求体:

```json
{
  "text": "user1@example.com----app_password\nuser2@outlook.com----app_password----client_id----refresh_token",
  "protocol": "auto"
}
```

字段说明:

- `text`: 多行账号内容
- `protocol`: `auto | imap | pop3`

响应:

```json
{
  "added": 2,
  "updated": 0
}
```

### `DELETE /api/accounts/{id}`

删除账号和该账号关联的当前邮件列表记录。

响应:

```json
{
  "deleted": true
}
```

## Fetch

### `GET /api/fetch`

获取当前活跃的后台收件任务。

响应示例：

```json
{
  "job": {
    "id": "9d5e3d0d8b6b3f5a",
    "status": "running",
    "totalAccounts": 20,
    "completedAccounts": 6,
    "successCount": 5,
    "failureCount": 1,
    "currentAccountId": "fe92d40f7557e3ac",
    "currentEmail": "user@example.com",
    "message": "正在收取 user@example.com",
    "createdAt": "2026-04-15T14:30:00+08:00",
    "startedAt": "2026-04-15T14:30:01+08:00"
  }
}
```

如果当前没有运行中的任务，返回：

```json
{
  "job": null
}
```

### `POST /api/fetch`

执行接邮件。支持同步模式和后台任务模式。

请求体:

```json
{
  "accountIds": [],
  "emails": [],
  "limit": 20,
  "allMessages": true,
  "markSeen": false,
  "async": true,
  "folders": ["INBOX", "Junk"]
}
```

字段说明：

- `accountIds`: 可选，按内部账号 id 指定
- `emails`: 可选，按邮箱地址指定，更适合外部调用
- `limit`: 每个账号每个文件夹最多抓多少封
- `allMessages`: `true` 时抓最近邮件，不只未读
- `markSeen`: 只对 IMAP 有意义，POP3 下无实际效果
- `async`: 可选，`true` 时启动后台收件任务并立即返回任务信息
- `folders`: 可选,IMAP 下要抓取的文件夹,**默认 `["INBOX", "Junk"]`**(验证码常被判进垃圾邮件);POP3 无文件夹概念,忽略此参数

同步模式响应示例：

```json
{
  "results": [
    {
      "accountId": "fe92d40f7557e3ac",
      "email": "user@example.com",
      "protocol": "imap",
      "count": 3,
      "error": ""
    }
  ]
}
```

后台任务模式响应示例：

```json
{
  "started": true,
  "message": "后台收件已开始",
  "job": {
    "id": "9d5e3d0d8b6b3f5a",
    "status": "queued",
    "totalAccounts": 20,
    "completedAccounts": 0,
    "successCount": 0,
    "failureCount": 0,
    "message": "等待开始",
    "createdAt": "2026-04-15T14:30:00+08:00"
  }
}
```

如果已有后台任务在执行，会返回当前任务而不会新开一个：

```json
{
  "started": false,
  "message": "已有收件任务在执行",
  "job": {
    "id": "9d5e3d0d8b6b3f5a",
    "status": "running"
  }
}
```

### `GET /api/fetch/{id}`

获取指定后台收件任务详情。

响应示例：

```json
{
  "job": {
    "id": "9d5e3d0d8b6b3f5a",
    "status": "completed",
    "totalAccounts": 20,
    "completedAccounts": 20,
    "successCount": 18,
    "failureCount": 2,
    "results": [
      {
        "accountId": "fe92d40f7557e3ac",
        "email": "user@example.com",
        "protocol": "imap",
        "count": 3,
        "error": ""
      }
    ],
    "message": "收件完成，成功 18，失败 2",
    "createdAt": "2026-04-15T14:30:00+08:00",
    "startedAt": "2026-04-15T14:30:01+08:00",
    "finishedAt": "2026-04-15T14:31:18+08:00"
  }
}
```

## Proxy Settings

### `GET /api/proxy-settings`

获取当前代理配置。

响应示例：

```json
{
  "settings": {
    "enabled": true,
    "rawList": "user:pass@proxy.example.com:8080",
    "count": 1,
    "updatedAt": "2026-04-14T16:20:00.000000+08:00"
  }
}
```

### `PUT /api/proxy-settings`

保存代理配置。启用后，探测、收件和 OAuth token 请求都会按代理列表轮询走 HTTP CONNECT 代理。

请求体示例：

```json
{
  "enabled": true,
  "rawList": "user:pass@proxy1.example.com:8080\nhttp://user:pass@proxy2.example.com:8080"
}
```

代理格式：

- `user:pass@host:port`
- `http://user:pass@host:port`
- `https://user:pass@host:port`

## Remote Search

### `POST /api/search`

按协议层远端查询邮件。

说明：

- `IMAP` 会先用服务端 `SEARCH` 缩小范围，再在程序内存里按秒级时间和正则精确过滤
- `POP3` 没有原生服务端查询能力，只能拉取候选邮件后在程序内存里过滤
- 这个接口不走 SQLite 查询层

请求体：

```json
{
  "accountIds": [],
  "emails": [],
  "recentSeconds": 86400,
  "keyword": "Codeforces",
  "regex": "Codeforces|verification",
  "fields": ["all"],
  "limit": 20,
  "scanLimit": 20,
  "folders": ["INBOX", "Junk"]
}
```

字段说明：

- `accountIds`: 可选，按内部账号 id 指定
- `emails`: 可选，按邮箱地址指定，更适合外部调用
- `recentSeconds`: 最近多少秒内的邮件。默认 `86400`。对 IMAP 会先转成服务端 `SINCE` 做粗筛，再在内存里做秒级精筛
- `keyword`: 关键词。对 IMAP 会尽量映射成远端 `SUBJECT` / `FROM` / `BODY` / `TEXT`
- `regex`: 正则表达式。只在程序内存中做匹配，不是 IMAP 原生正则
- `fields`: 可选值 `all | subject | from | body`。默认等同于 `["all"]`
- `limit`: 最多返回多少条匹配结果
- `scanLimit`: 每个账号每个文件夹最多扫描多少封候选邮件。默认 `20`
- `folders`: 可选,IMAP 下要搜索的文件夹,**默认 `["INBOX", "Junk"]**;POP3 忽略此参数
- 响应中每封邮件带 `folder` 字段,标识来自哪个文件夹

响应示例：

```json
{
  "messages": [
    {
      "id": "302ae330652fd2daa96121690abea757e2e28548",
      "accountId": "8cb2b2425c245584",
      "accountEmail": "user@example.com",
      "remoteId": 2,
      "protocol": "imap",
      "from": "noreply@codeforces.com",
      "subject": "Codeforces Round 1091 and CodeCraft 26 (Div. 2)",
      "date": "Mon, 6 Apr 2026 15:44:58 +0300 (MSK)",
      "body": "Hello user...",
      "preview": "Hello user, I'm glad to invite you...",
      "savedDir": "",
      "attachments": [],
      "fetchedAt": "2026-04-09T18:23:32.400789+08:00"
    }
  ]
}
```

## Messages

### `GET /api/messages`

查询当前邮件列表。

查询参数:

- `accountId`: 可选，只看某个账号的邮件
- `limit`: 可选，限制返回数量

示例:

```text
GET /api/messages?limit=50
GET /api/messages?accountId=fe92d40f7557e3ac&limit=20
```

响应示例:

```json
{
  "messages": [
    {
      "id": "697e92d2b6e7f784fc21f763922e7eb8594b4ba9",
      "accountId": "fe92d40f7557e3ac",
      "accountEmail": "user@example.com",
      "remoteId": 2,
      "protocol": "imap",
      "from": "noreply@codeforces.com",
      "subject": "Codeforces Round 1091 and CodeCraft 26 (Div. 2)",
      "date": "Mon, 6 Apr 2026 15:44:58 +0300 (MSK)",
      "body": "Hello user...",
      "preview": "Hello user, I'm glad to invite you...",
      "savedDir": "",
      "attachments": [],
      "fetchedAt": "2026-04-09T17:20:00.943163+08:00"
    }
  ]
}
```



## 协议与缓存说明

- `auto` 模式默认先尝试 IMAP，失败再回退到 POP3
- 同一个邮箱后缀一旦成功探测，系统会缓存该后缀对应的服务器和端口
- 后续相同后缀的新账号会优先命中缓存，不再对每个账号做完整探测
- 当前账号和邮件内容持久化在数据目录下的 SQLite 数据库 `app.db`;原生运行默认是可执行文件所在目录,Docker 中为 `/app/data/app.db`
