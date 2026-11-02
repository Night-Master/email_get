# Easy Email 项目简介

## 项目概述

Easy Email 是一个对标心蓝邮箱助手的本地化多邮箱管理与收件工具，后端使用 Go，前端使用 Vue，最终以单个可执行文件的形式交付。它支持 QQ 邮箱、Foxmail、163 邮箱和 Outlook，主要用于统一管理邮箱账号、自动探测协议与服务器、批量收取邮件，并通过 HTTP API 对外提供账号管理、收件、搜索和消息查询能力。

项目特点：

- 支持 QQ 邮箱和 Foxmail 的 IMAP / POP3 接入，Foxmail 自动匹配 QQ 邮箱服务器
- 支持 163、126、Yeah 邮箱的 IMAP / POP3 接入
- 支持 Outlook / Hotmail / Live 自动服务器匹配及 OAuth 四段格式
- 支持 `auto / imap / pop3` 三种收件模式
- 支持普通账号授权码或客户端专用密码格式
- 支持自动探测 MX / IMAP / POP 服务端点，用于辅助收件协议选择
- 支持批量导入账号、批量收件、查看邮件列表与邮件正文预览
- 支持通过 HTTP API 与其他系统或脚本联动
- 支持自行安装部署
- 适合注册网站、平台或业务系统时需要通过邮箱接收验证码、激活链接、通知邮件的场景

## 自行安装方式

参考项目中的 `README.md`，支持两种方式：

- 原生运行：构建后执行 `release/` 下对应操作系统的二进制文件
- Docker 运行：准备 Linux AMD64 二进制后，使用 `Dockerfile` 和 `docker-compose.yml` 构建镜像并启动服务

自行安装时，建议重点关注：

- 源码仓库不跟踪二进制，需按 `README.md` 的构建说明生成
- 数据库不存在时会自动创建，不需要随源码分发真实 `app.db`
- 默认服务监听端口为 `80`

## 支持的账号导入格式

支持以下三种格式：

```text
邮箱----密码
邮箱----密码----服务器----端口
邮箱----密码----client_id----refresh_token
```

说明：

- 两段格式适合普通邮箱账号
- 四段 `服务器----端口` 在 `imap` 模式下按 IMAP 处理，在 `pop3` 模式下按 POP3 处理
- Outlook / Hotmail / Live 的四段 OAuth 格式会自动换取 IMAP / POP3 access token

## 核心能力

Web 界面主要支持：

- 粘贴多行账号后批量导入
- 在 Web 界面中添加和删除邮箱账号
- 选择 `auto / imap / pop3` 协议
- 查看账号列表、最近成功收件时间、错误信息、上次使用协议
- 对单个账号执行收件
- 对全部或选中账号批量收件
- 查看当前邮件列表、正文预览和附件名称

协议层主要支持：

- `auto` 模式优先 IMAP，失败后回退到 POP3
- 同域名的探测结果会缓存，减少后续重复探测
- API 搜索支持关键字、正则、最近时间窗口等条件

当前不提供 SMTP 发信能力，核心能力聚焦在账号管理、协议探测、收件和邮件查询。

## Skill 能力说明

项目内置了一个 `mail-query` skill，位于 `skills/mail-query/`，用于通过服务 HTTP API 做脚本化查询和自动化调用，不需要直接连接 IMAP 或 POP3。

这个 skill 适合以下场景：

- 列出当前服务中已经存在的邮箱账号
- 触发某个邮箱或一组邮箱执行收件
- 按关键字、正则、最近时间窗口远端搜索邮件
- 在自动化流程里绕过 Web UI，直接调用服务接口

skill 内包含这些脚本：

- `skills/mail-query/scripts/list_mailboxes.py`：列出当前邮箱账号
- `skills/mail-query/scripts/fetch_mail.py`：触发指定邮箱收件
- `skills/mail-query/scripts/search_mail.py`：按条件远端搜索邮件

默认服务地址示例：

```text
http://127.0.0.1:8080
```

示例命令：

```bash
python3 skills/mail-query/scripts/list_mailboxes.py --base-url http://127.0.0.1:8080 --pretty
python3 skills/mail-query/scripts/fetch_mail.py --base-url http://127.0.0.1:8080 --email 'user@example.com' --pretty
python3 skills/mail-query/scripts/search_mail.py --base-url http://127.0.0.1:8080 --email 'user@example.com' --keyword '验证码' --pretty
```

说明：

- skill 通过服务 API 工作，不直接操作邮箱协议连接
- `auto` 模式下服务会优先尝试 IMAP，失败后再回退到 POP3
- 搜索能力依赖服务端接口 `POST /api/search`

## API 简介

当前服务基于 HTTP 提供接口，默认基地址：

```text
http://127.0.0.1:8080/
```

常用接口如下。

### 1. 健康检查

```http
GET /api/health
```

返回：

```json
{
  "ok": true
}
```

### 2. 获取账号列表

```http
GET /api/accounts
```

支持查询参数：

- `keyword`：按邮箱、服务器、client_id 模糊过滤

### 3. 导入账号

```http
POST /api/accounts/import
```

请求体示例：

```json
{
  "text": "user1@example.com----password\nuser2@outlook.com----password----client_id----refresh_token",
  "protocol": "auto"
}
```

### 4. 删除账号

```http
DELETE /api/accounts/{id}
```

### 5. 触发收件

```http
POST /api/fetch
```

请求体示例：

```json
{
  "accountIds": [],
  "emails": [],
  "limit": 20,
  "allMessages": true,
  "markSeen": false
}
```

说明：

- `accountIds` 可按系统内部账号 ID 指定
- `emails` 可按邮箱地址指定，适合外部系统对接
- `allMessages=true` 表示抓最近邮件，不只未读
- `markSeen` 只对 IMAP 有意义

### 6. 远端搜索邮件

```http
POST /api/search
```

请求体示例：

```json
{
  "accountIds": [],
  "emails": [],
  "recentSeconds": 86400,
  "keyword": "Codeforces",
  "regex": "Codeforces|verification",
  "fields": ["all"],
  "limit": 20,
  "scanLimit": 20
}
```

说明：

- 支持按关键词、正则、最近时间窗口搜索
- IMAP 会优先使用服务端能力筛选，POP3 则在程序内存中筛选
- 该接口面向远端协议层查询，不走本地 SQLite 查询层

### 7. 查询当前邮件列表

```http
GET /api/messages
```

支持查询参数：

- `accountId`
- `limit`

## 适用场景

这个项目适合以下场景：

- 注册网站、平台或业务系统时，需要通过邮箱接收验证码、注册链接或激活邮件
- 批量导入邮箱账号并统一拉取邮件
- 需要快速部署一个带 Web UI 的邮箱接码与收件系统
- 需要对接 HTTP API 做自动化调用
- 需要对不同邮箱类型自动探测协议与服务器配置

## 相关文档

- 详细安装与运行说明：`README.md`
- 详细接口说明：`API.md`
