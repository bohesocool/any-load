# Any-Load

[English](README.md) | 中文 | [日本語](README_JP.md)

[![Release](https://img.shields.io/github/v/release/bohesocool/any-load)](https://github.com/bohesocool/any-load/releases)
![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Any-Load 是 [tbphp/gpt-load](https://github.com/tbphp/gpt-load) 的二开分支。上游是一个用 Go 开发的高性能、企业级 AI 接口透明代理服务，本仓库继承了其密钥池管理、加权负载均衡、故障转移和监控能力，并在此基础上规划自己的演进方向。

> 🙏 特别感谢 [@tbphp](https://github.com/tbphp) 及 gpt-load 的所有贡献者。没有他们的工作就没有 Any-Load。上游仓库：[tbphp/gpt-load](https://github.com/tbphp/gpt-load)。

## 二开路线图

Any-Load 是一个处于早期的二开分支。在上游已有能力之外：

- **渠道亲和性 / 粘性路由** ✅ —— 将共享同一特征（会话请求头、body 中的 `session_id` / `prompt_cache_key` / 对话 ID，或首条消息哈希）的请求固定到同一（上游 + 密钥）组合，而非纯轮询——对依赖会话状态的上游、以及平滑限流行为有帮助。支持全局与分组级配置、绑定带 TTL、绑定密钥不可用时自动故障转移。
- **每密钥并发限制** ✅ —— 限制分组内每把密钥的并发在途请求数（分组级覆盖全局设置）；超限的密钥被跳过，轮询落到其它可用密钥。
- **协议转换** ✅ —— 在代理层完成各厂商原生 API 格式（OpenAI Chat / OpenAI Responses / Anthropic / Gemini）之间的互转，双向、流式与非流式均支持，含工具调用与图片。客户端用任意格式调用，代理按上游支持的格式转换。详见 [协议转换](#协议转换)。

透明透传 + 上游与密钥的加权轮询；渠道亲和性与每密钥并发均为可选，默认关闭。

## 功能特性

- **透明代理**: 完全保留原生 API 格式，支持 OpenAI、Google Gemini 和 Anthropic Claude 等多种格式
- **协议转换**: 可选的分组级转换，在任意支持的 API 格式（OpenAI Chat / Responses / Anthropic / Gemini）之间互转——双向、流式+非流式，含工具调用与图片
- **智能密钥管理**: 高性能密钥池，支持分组管理、自动轮换和故障恢复
- **负载均衡**: 支持多上游端点的加权负载均衡，提升服务可用性
- **智能故障处理**: 自动密钥黑名单管理和恢复机制，确保服务连续性
- **动态配置**: 系统设置和分组配置支持热重载，无需重启即可生效
- **企业级架构**: 分布式主从部署，支持水平扩展和高可用
- **现代化管理**: 基于 Vue 3 的 Web 管理界面，直观易用
- **全面监控**: 实时统计、健康检查、详细请求日志
- **高性能设计**: 零拷贝流式传输、连接池复用、原子操作
- **生产就绪**: 优雅关闭、错误恢复、完善的安全机制
- **双重认证体系**: 管理端与代理端认证分离，代理认证支持全局和分组级别密钥

## 支持的 AI 服务

Any-Load 作为透明代理服务，完整保留各 AI 服务商的原生 API 格式：

- **OpenAI 格式**: 官方 OpenAI API、Azure OpenAI、以及其他 OpenAI 兼容服务
- **Google Gemini 格式**: Gemini Pro、Gemini Pro Vision 等模型的原生 API
- **Anthropic Claude 格式**: Claude 系列模型，支持高质量的对话和文本生成

## 快速开始

### 环境要求

- Go 1.24+ (源码构建)
- Docker (容器化部署)
- MySQL, PostgreSQL, 或 SQLite (数据库存储)
- Redis (缓存和分布式协调，可选)

### 方式一：Docker 快速开始

```bash
docker run -d --name any-load \
    -p 3001:3001 \
    -e AUTH_KEY=your-secure-key-here \
    -v "$(pwd)/data":/app/data \
    ghcr.io/bohesocool/any-load:latest
```

> 请将 `your-secure-key-here` 改为强密码（决不能使用默认值），即可登录管理界面：<http://localhost:3001>

### 方式二：使用 Docker Compose（推荐）

**安装命令：**

```bash
# 创建目录
mkdir -p any-load && cd any-load

# 下载配置文件
wget https://raw.githubusercontent.com/bohesocool/any-load/refs/heads/main/docker-compose.yml
wget -O .env https://raw.githubusercontent.com/bohesocool/any-load/refs/heads/main/.env.example

# 编辑 .env 文件，修改AUTH_KEY为强密码，绝不使用 sk-123456 等默认或者简单密钥

# 启动服务
docker compose up -d
```

在部署之前，您必须修改默认的管理密钥 (AUTH_KEY)，建议密钥格式：sk-prod-[随机字符串32位]。

默认安装的是 SQLite 版本，适合轻量单机应用。

如需安装 MySQL, PostgreSQL 及 Redis，请在 `docker-compose.yml` 文件中取消所需服务的注释，并配置好对应的环境配置重启即可。

**其他命令：**

```bash
# 查看服务状态
docker compose ps

# 查看日志
docker compose logs -f

# 重启服务
docker compose down && docker compose up -d

# 更新到最新版本
docker compose pull && docker compose down && docker compose up -d
```

部署完成后：

- 访问 Web 管理界面：<http://localhost:3001>
- API 代理地址：<http://localhost:3001/proxy>

> 使用你修改的 AUTH_KEY 登录管理端。

### 方式三：源码构建

源码构建需要本地已安装数据库（SQLite、MySQL 或 PostgreSQL）和 Redis（可选）。

```bash
# 克隆并构建
git clone https://github.com/bohesocool/any-load.git
cd any-load
go mod tidy

# 创建配置
cp .env.example .env

# 编辑 .env 文件，修改AUTH_KEY为强密码，绝不使用 sk-123456 等默认或者简单密钥
# 修改 .env 中 DATABASE_DSN 和 REDIS_DSN 配置
# REDIS_DSN 为可选，如果不配置则启用内存存储

# 运行
make run
```

部署完成后：

- 访问 Web 管理界面：<http://localhost:3001>
- API 代理地址：<http://localhost:3001/proxy>

> 使用你修改的 AUTH_KEY 登录管理端。

### 方式四：集群部署

集群部署需要所有节点都连接同一个 MySQL（或者 PostgreSQL） 和 Redis，并且 Redis 是必须要求。建议使用统一的分布式 MySQL 和 Redis 集群。

**部署要求：**

- 所有节点必须配置相同的 `AUTH_KEY`、`DATABASE_DSN`、`REDIS_DSN`
- 一主多从架构，从节点必须配置环境变量：`IS_SLAVE=true`

详细请参考[集群部署文档](https://github.com/bohesocool/any-load#readme)

## 配置系统

### 配置架构概述

Any-Load 采用双层配置架构：

#### 1. 静态配置（环境变量）

- **特点**：应用启动时读取，运行期间不可修改，需重启应用生效
- **用途**：基础设施配置，如数据库连接、服务器端口、认证密钥等
- **管理方式**：通过 `.env` 文件或系统环境变量设置

#### 2. 动态配置（热重载）

- **系统设置**：存储在数据库中，为整个应用提供统一的行为基准
- **分组配置**：为特定分组定制的行为参数，可覆盖系统设置
- **配置优先级**：分组配置 > 系统设置 > 环境配置
- **特点**：支持热重载，修改后立即生效，无需重启应用

<details>
<summary>静态配置（环境变量）</summary>

**服务器配置：**

| 配置项       | 环境变量                           | 默认值          | 说明                       |
| ------------ | ---------------------------------- | --------------- | -------------------------- |
| 服务端口     | `PORT`                             | 3001            | HTTP 服务器监听端口        |
| 服务地址     | `HOST`                             | 0.0.0.0         | HTTP 服务器绑定地址        |
| 读取超时     | `SERVER_READ_TIMEOUT`              | 60              | HTTP 服务器读取超时（秒）  |
| 写入超时     | `SERVER_WRITE_TIMEOUT`             | 600             | HTTP 服务器写入超时（秒）  |
| 空闲超时     | `SERVER_IDLE_TIMEOUT`              | 120             | HTTP 连接空闲超时（秒）    |
| 优雅关闭超时 | `SERVER_GRACEFUL_SHUTDOWN_TIMEOUT` | 10              | 服务优雅关闭等待时间（秒） |
| 从节点模式   | `IS_SLAVE`                         | false           | 集群部署时从节点标识       |
| 时区         | `TZ`                               | `Asia/Shanghai` | 指定时区                   |

**安全配置：**

| 配置项   | 环境变量         | 默认值 | 说明                                                                               |
| -------- | ---------------- | ------ | ---------------------------------------------------------------------------------- |
| 管理密钥 | `AUTH_KEY`       | -      | **管理端**的访问认证密钥，请修改为强密码                                           |
| 加密密钥 | `ENCRYPTION_KEY` | -      | 加密存储的API密钥，支持任意字符串或留空禁用加密。参见[数据加密迁移](#数据加密迁移) |

**数据库配置：**

| 配置项     | 环境变量       | 默认值             | 说明                                 |
| ---------- | -------------- | ------------------ | ------------------------------------ |
| 数据库连接 | `DATABASE_DSN` | ./data/any-load.db | 数据库连接字符串 (DSN) 或文件路径    |
| Redis 连接 | `REDIS_DSN`    | -                  | Redis 连接字符串，为空时使用内存存储 |

**性能与跨域配置：**

| 配置项       | 环境变量                  | 默认值                        | 说明                     |
| ------------ | ------------------------- | ----------------------------- | ------------------------ |
| 最大并发请求 | `MAX_CONCURRENT_REQUESTS` | 100                           | 系统允许的最大并发请求数 |
| 启用 CORS    | `ENABLE_CORS`             | false                         | 是否启用跨域资源共享     |
| 允许的来源   | `ALLOWED_ORIGINS`         | -                             | 允许的来源，逗号分隔     |
| 允许的方法   | `ALLOWED_METHODS`         | `GET,POST,PUT,DELETE,OPTIONS` | 允许的 HTTP 方法         |
| 允许的头部   | `ALLOWED_HEADERS`         | `*`                           | 允许的请求头，逗号分隔   |
| 允许凭据     | `ALLOW_CREDENTIALS`       | false                         | 是否允许发送凭据         |

**日志配置：**

| 配置项       | 环境变量          | 默认值                | 说明                               |
| ------------ | ----------------- | --------------------- | ---------------------------------- |
| 日志级别     | `LOG_LEVEL`       | `info`                | 日志级别：debug, info, warn, error |
| 日志格式     | `LOG_FORMAT`      | `text`                | 日志格式：text, json               |
| 启用文件日志 | `LOG_ENABLE_FILE` | false                 | 是否启用文件日志输出               |
| 日志文件路径 | `LOG_FILE_PATH`   | `./data/logs/app.log` | 日志文件存储路径                   |

**代理配置：**

Any-Load 会自动从环境变量中读取代理设置，用于向上游 AI 服务商发起请求。

| 配置项     | 环境变量      | 默认值 | 说明                                     |
| ---------- | ------------- | ------ | ---------------------------------------- |
| HTTP 代理  | `HTTP_PROXY`  | -      | 用于 HTTP 请求的代理服务器地址           |
| HTTPS 代理 | `HTTPS_PROXY` | -      | 用于 HTTPS 请求的代理服务器地址          |
| 无代理     | `NO_PROXY`    | -      | 不需要通过代理访问的主机或域名，逗号分隔 |

支持的代理协议格式：

- **HTTP**: `http://user:pass@host:port`
- **HTTPS**: `https://user:pass@host:port`
- **SOCKS5**: `socks5://user:pass@host:port`
</details>

<details>
<summary>动态配置（热重载）</summary>

**基础设置：**

| 配置项       | 字段名                               | 默认值                      | 分组可覆盖 | 说明                                                           |
| ------------ | ------------------------------------ | --------------------------- | ---------- | -------------------------------------------------------------- |
| 项目地址     | `app_url`                            | `http://localhost:3001`     | ❌         | 项目基础 URL                                                   |
| 全局代理密钥 | `proxy_keys`                         | 初始值为环境配置的 AUTH_KEY | ❌         | 全局生效的代理认证密钥，多个用逗号分隔                         |
| 日志保留天数 | `request_log_retention_days`         | 7                           | ❌         | 请求日志保留天数，0 为不清理                                   |
| 日志写入间隔 | `request_log_write_interval_minutes` | 1                           | ❌         | 日志写入数据库周期（分钟）                                     |
| 启用日志详情 | `enable_request_body_logging`        | false                       | ✅         | 是否在请求日志中记录完整的请求体和上游响应内容，启用会增加内存和存储占用 |

**请求设置：**

| 配置项               | 字段名                    | 默认值 | 分组可覆盖 | 说明                                               |
| -------------------- | ------------------------- | ------ | ---------- | -------------------------------------------------- |
| 请求超时             | `request_timeout`         | 600    | ✅         | 转发请求完整生命周期超时（秒）                     |
| 连接超时             | `connect_timeout`         | 15     | ✅         | 与上游服务建立连接超时（秒）                       |
| 空闲连接超时         | `idle_conn_timeout`       | 120    | ✅         | HTTP 客户端空闲连接超时（秒）                      |
| 响应头超时           | `response_header_timeout` | 600    | ✅         | 等待上游响应头超时（秒）                           |
| 最大空闲连接数       | `max_idle_conns`          | 100    | ✅         | 连接池最大空闲连接总数                             |
| 每主机最大空闲连接数 | `max_idle_conns_per_host` | 50     | ✅         | 每个上游主机最大空闲连接数                         |
| 代理服务器地址       | `proxy_url`               | -      | ✅         | 用于转发请求的 HTTP/HTTPS 代理，为空则使用环境配置 |

**密钥配置：**

| 配置项         | 字段名                            | 默认值 | 分组可覆盖 | 说明                                             |
| -------------- | --------------------------------- | ------ | ---------- | ------------------------------------------------ |
| 最大重试次数   | `max_retries`                     | 3      | ✅         | 单个请求使用不同密钥的最大重试次数               |
| 黑名单阈值     | `blacklist_threshold`             | 3      | ✅         | 密钥累计失败多少次后进入黑名单                   |
| 不计入失败状态码 | `uncounted_status_codes`          | (空)   | ✅         | 命中这些状态码仍重试但不计失败、不禁用账号；留空表示不豁免 |
| 密钥验证间隔   | `key_validation_interval_minutes` | 60     | ✅         | 后台定时验证密钥周期（分钟）                     |
| 密钥验证并发数 | `key_validation_concurrency`      | 10     | ✅         | 后台定时验证无效 Key 时的并发数                  |
| 密钥验证超时   | `key_validation_timeout_seconds`  | 20     | ✅         | 后台定时验证单个 Key 时的 API 请求超时时间（秒） |

</details>

## 数据加密迁移

Any-Load 支持对 API 密钥进行加密存储。您可以随时启用、禁用或更换加密密钥。

<details>
<summary>查看数据加密迁移详细说明</summary>

### 迁移场景

- **启用加密**：将明文数据加密存储 - 使用 `--to <新密钥>`
- **禁用加密**：将加密数据解密为明文 - 使用 `--from <当前密钥>`
- **更换密钥**：更换加密密钥 - 使用 `--from <当前密钥> --to <新密钥>`

### 操作步骤

#### Docker Compose 部署

```bash
# 1. 更新镜像（确保使用最新版本）
docker compose pull

# 2. 停止服务
docker compose down

# 3. 备份数据库（强烈建议）
# 执行迁移前，必须手动备份数据库或者导出你的密钥，避免因操作或者异常导致的密钥丢失。

# 4. 执行迁移命令
# 启用加密（your-32-char-secret-key 为你的密钥，建议使用32位以上的随机字符串）
docker compose run --rm any-load migrate-keys --to "your-32-char-secret-key"

# 禁用加密
docker compose run --rm any-load migrate-keys --from "your-current-key"

# 更换密钥
docker compose run --rm any-load migrate-keys --from "old-key" --to "new-32-char-secret-key"

# 5. 更新配置文件
# 编辑 .env 文件，设置 ENCRYPTION_KEY 与 --to 参数一致
# 如果禁用加密，则删除 ENCRYPTION_KEY 或设置为空
vim .env
# 添加或修改: ENCRYPTION_KEY=your-32-char-secret-key

# 6. 重启服务
docker compose up -d
```

#### 源码构建部署

```bash
# 1. 停止服务
# 停止正在运行的服务进程（Ctrl+C 或 kill 进程）

# 2. 备份数据库（强烈建议）
# 执行迁移前，必须手动备份数据库或者导出你的密钥，避免因操作或者异常导致的密钥丢失。

# 3. 执行迁移命令
# 启用加密
make migrate-keys ARGS="--to your-32-char-secret-key"

# 禁用加密
make migrate-keys ARGS="--from your-current-key"

# 更换密钥
make migrate-keys ARGS="--from old-key --to new-32-char-secret-key"

# 4. 更新配置文件
# 编辑 .env 文件，设置 ENCRYPTION_KEY 与 --to 参数一致
echo "ENCRYPTION_KEY=your-32-char-secret-key" >> .env

# 5. 重启服务
make run
```

### 注意事项

⚠️ **重要提醒**：

- **ENCRYPTION_KEY 一旦丢失将无法恢复已加密的数据！** 请务必安全备份此密钥，建议使用密码管理器或安全的密钥管理系统保存
- 迁移前**必须停止服务**，避免数据不一致
- 强烈建议**备份数据库**，以防迁移失败需要恢复
- 密钥建议使用 **32 位或更长的随机字符串**，确保安全性
- 迁移后确保 `.env` 中的 `ENCRYPTION_KEY` 与 `--to` 参数一致
- 如果禁用加密，需要删除或清空 `ENCRYPTION_KEY` 配置

### 密钥生成示例

```bash
# 生成安全的随机密钥（32字符）
openssl rand -base64 32 | tr -d "=+/" | cut -c1-32
```

</details>

## Web 管理界面

访问管理控制台：<http://localhost:3001>（默认地址）

### 界面展示

<img src="screenshot/dashboard.png" alt="仪表盘" width="600" />

<br/>

<img src="screenshot/keys.png" alt="密钥管理" width="600" />

<br/>

Web 管理界面提供以下功能：

- **仪表盘**: 实时统计信息和系统状态概览
- **密钥管理**: 创建和配置 AI 服务商分组，添加、删除和监控 API 密钥
- **请求日志**: 详细的请求历史记录和调试信息
- **系统设置**: 全局配置管理和热重载

## API 使用说明

<details>
<summary>代理接口调用方式</summary>

Any-Load 通过分组名称路由请求到不同的 AI 服务。使用方式如下：

### 1. 代理端点格式

```text
http://localhost:3001/proxy/{group_name}/{原始API路径}
```

- `{group_name}`: 在管理界面创建的分组名称
- `{原始API路径}`: 保持与原始 AI 服务完全一致的路径

### 2. 认证方式

在 Web 管理界面中配置**代理密钥** (`Proxy Keys`)，可设置系统级别和分组级别的代理密钥。

- **认证方式**: 与原生 API 一致，但需将原始密钥替换为配置的代理密钥。
- **密钥作用域**: 在系统设置配置的 **全局代理密钥** 可以在所有分组使用，在分组配置的 **分组代理密钥** 仅在当前分组有效。
- **格式**: 多个密钥使用半角英文逗号分隔。

### 3. OpenAI 接口调用示例

Any-Load 当前支持两种 OpenAI 兼容分组类型：

- `openai`（OpenAI Chat Completions 格式）
- `openai-response`（OpenAI Responses 格式）

假设创建了名为 `openai` 的分组：

**原始调用方式：**

```bash
curl -X POST https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer sk-your-openai-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "messages": [{"role": "user", "content": "Hello"}]}'
```

**代理调用方式：**

```bash
curl -X POST http://localhost:3001/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer your-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "messages": [{"role": "user", "content": "Hello"}]}'
```

**变更说明：**

- 将 `https://api.openai.com` 替换为 `http://localhost:3001/proxy/openai`
- 将原始 API Key 替换为**代理密钥**

**OpenAI Responses 格式示例（`openai-response` 分组）：**

```bash
curl -X POST http://localhost:3001/proxy/openai-response/v1/responses \
  -H "Authorization: Bearer your-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "input": "Hello"}'
```

### 4. Gemini 接口调用示例

假设创建了名为 `gemini` 的分组：

**原始调用方式：**

```bash
curl -X POST https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent?key=your-gemini-key \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "Hello"}]}]}'
```

**代理调用方式：**

```bash
curl -X POST http://localhost:3001/proxy/gemini/v1beta/models/gemini-2.5-pro:generateContent?key=your-proxy-key \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "Hello"}]}]}'
```

**变更说明：**

- 将 `https://generativelanguage.googleapis.com` 替换为 `http://localhost:3001/proxy/gemini`
- 将 URL 参数中的 `key=your-gemini-key` 替换为**代理密钥**

### 5. Anthropic 接口调用示例

假设创建了名为 `anthropic` 的分组：

**原始调用方式：**

```bash
curl -X POST https://api.anthropic.com/v1/messages \
  -H "x-api-key: sk-ant-api03-your-anthropic-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model": "claude-sonnet-4-20250514", "messages": [{"role": "user", "content": "Hello"}]}'
```

**代理调用方式：**

```bash
curl -X POST http://localhost:3001/proxy/anthropic/v1/messages \
  -H "x-api-key: your-proxy-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model": "claude-sonnet-4-20250514", "messages": [{"role": "user", "content": "Hello"}]}'
```

**变更说明：**

- 将 `https://api.anthropic.com` 替换为 `http://localhost:3001/proxy/anthropic`
- 将 `x-api-key` 头部中的原始 API Key 替换为**代理密钥**

### 6. 支持的接口

**OpenAI Chat Completions 格式（`openai`）：**

- `/v1/chat/completions` - 聊天对话
- `/v1/completions` - 文本补全
- `/v1/embeddings` - 文本嵌入
- `/v1/models` - 模型列表
- 以及其他所有 OpenAI 兼容接口

**OpenAI Responses 格式（`openai-response`）：**

- `/v1/responses` - 统一响应生成
- `/v1/models` - 模型列表
- 以及其他所有 OpenAI Responses 兼容接口

**Gemini 格式：**

- `/v1beta/models/*/generateContent` - 内容生成
- `/v1beta/models` - 模型列表
- 以及其他所有 Gemini 原生接口

**Anthropic 格式：**

- `/v1/messages` - 消息对话
- `/v1/models` - 模型列表（如果可用）
- 以及其他所有 Anthropic 原生接口

### 7. 客户端 SDK 配置

**OpenAI Python SDK：**

```python
from openai import OpenAI

client = OpenAI(
    api_key="your-proxy-key",  # 使用密钥
    base_url="http://localhost:3001/proxy/openai"  # 使用代理端点
)

response = client.chat.completions.create(
    model="gpt-4.1-mini",
    messages=[{"role": "user", "content": "Hello"}]
)
```

**Google Gemini SDK (Python)：**

```python
import google.generativeai as genai

# 配置 API 密钥和基础 URL
genai.configure(
    api_key="your-proxy-key",  # 使用代理密钥
    client_options={"api_endpoint": "http://localhost:3001/proxy/gemini"}
)

model = genai.GenerativeModel('gemini-2.5-pro')
response = model.generate_content("Hello")
```

**Anthropic SDK (Python)：**

```python
from anthropic import Anthropic

client = Anthropic(
    api_key="your-proxy-key",  # 使用代理密钥
    base_url="http://localhost:3001/proxy/anthropic"  # 使用代理端点
)

response = client.messages.create(
    model="claude-sonnet-4-20250514",
    messages=[{"role": "user", "content": "Hello"}]
)
```

> **重要提示**：作为透明代理服务，Any-Load 完全保留各 AI 服务的原生 API 格式和认证方式，仅需要替换端点地址并使用在管理端配置的**代理密钥**即可无缝迁移。

</details>

## 协议转换

协议转换是一个**可选的、分组级**功能，在代理层把任意支持的 LLM API 格式互相转换。客户端可以用一种格式（如 Anthropic `/v1/messages`）调用，请求被转换成上游实际支持的格式（如 OpenAI `/v1/chat/completions`），响应再转换回客户端的格式。这与 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的行为一致。

转换是**双向**的（任意入站 → 任意上游），**流式与非流式**响应都支持，并包含**工具调用**（定义、调用、结果）与**图片**。

### 两个分组设置

| 设置 | 取值 | 作用 |
|---|---|---|
| **协议转换**（开关） | 开 / 关 | 关 = 纯透传，行为与以前完全一致 |
| **上游格式**（多选） | `openai-chat` / `openai-response` / `anthropic` / `gemini` | 声明**上游能接受哪些格式** |

入站（客户端）格式由**请求路径自动识别**，无需客户端声明：

| 请求路径 | 入站格式 |
|---|---|
| `/v1/chat/completions` | `openai-chat` |
| `/v1/responses` | `openai-response` |
| `/v1/messages` | `anthropic` |
| `/v1beta/models/<model>:generateContent` / `:streamGenerateContent` | `gemini` |

### 目标如何选择（转换开关开启时）

1. 入站格式**在上游格式列表里** → **直接透传**，不转换（智能透传）。
2. 入站格式**不在列表里** → 转换为列表里**第一个**格式（"首选"目标）。

以下情况完全不转换（纯透传）：开关关闭、列表为空、入站路径不是转换适用的端点（如 `/v1/models`）、入站格式上游已支持。

### 配置示例

**上游只支持 OpenAI Chat** —— `上游格式 = ["openai-chat"]`：

| 客户端发的 | 实际发生 |
|---|---|
| OpenAI Chat（`/v1/chat/completions`） | 入站 chat 在列表里 → **透传**到 `/v1/chat/completions`，Bearer 鉴权 |
| OpenAI Responses（`/v1/responses`） | 不在列表 → **转换**为 chat → 转发到 `/v1/chat/completions`；响应转回 Responses |
| Anthropic（`/v1/messages`） | 不在列表 → **转换**为 chat → 转发到 `/v1/chat/completions`；响应转回 Anthropic |
| Gemini（`:generateContent`） | 不在列表 → **转换**为 chat → 转发到 `/v1/chat/completions`；响应转回 Gemini |

→ 无论客户端用哪种格式，上游永远收到 OpenAI Chat。

**上游同时支持 Chat 和 Responses** —— `上游格式 = ["openai-chat", "openai-response"]`：

| 客户端发的 | 实际发生 |
|---|---|
| OpenAI Chat | 在列表里 → **透传** `/v1/chat/completions` |
| OpenAI Responses | 在列表里 → **透传** `/v1/responses` |
| Anthropic | 不在列表 → 转换为**列表第一个**格式（chat）→ `/v1/chat/completions` |
| Gemini | 不在列表 → 转换为**列表第一个**格式（chat）→ `/v1/chat/completions` |

> **列表顺序很关键**：首选目标永远是**第一项**。若想让 Anthropic 入站转换*成 Responses*，把 responses 放第一：`["openai-response", "openai-chat"]`。（此时 chat 客户端仍在列表里，仍透传。）

### 支持的转换范围

- **格式**：OpenAI Chat、OpenAI Responses、Anthropic Messages、Gemini —— 任意两两互转，双向。
- **工具调用**：工具定义（`function.parameters` ↔ `input_schema` ↔ `functionDeclarations` ↔ Responses 扁平 schema）、工具调用（`tool_calls` ↔ `tool_use` ↔ `functionCall` ↔ `function_call`）、工具结果（`role:"tool"` ↔ `tool_result` ↔ `functionResponse` ↔ `function_call_output`）。
- **图片**：`image_url`（URL 或 `data:` URI）↔ Anthropic `image` source（base64/URL）↔ Gemini `inline_data`（base64）↔ Responses `input_image`。

### 注意事项与限制

- **转换关闭** = 纯透传，与上游格式列表无关（与原始行为一致，不解析也不改写请求/响应）。
- **流式工具调用**按各格式事件类型翻译（Anthropic `input_json_delta`、OpenAI `tool_calls[].function.arguments` delta、Responses `function_call_arguments.delta`、Gemini 整块）。
- **Gemini 图片 URL**：Gemini 的 `inline_data` 需要 base64。纯 `http(s)` URL 图片（非 `data:` URI）会**返回 400 拒绝**，代理不抓取——请提供 base64 或 `data:` URI。
- **Gemini 无 call id**：解析 Gemini 时工具调用 id 合成为 `name#index`。
- 转换模式下错误响应目前原样透传（尚未翻译为入站格式的错误形状）。
- `param_overrides` 作用于**入站（客户端）请求体**，透传和转换模式均生效；转换模式下覆盖后的入站字段会被翻译到上游格式。支持旧版扁平 map（`{"temperature": 0.7}`）和 `operations` 数组（在旧版 key 之后应用，冲突时以 operations 为准）。每条操作含 `mode`（`set`/`delete`/`copy`/`move`/`append`/`prepend`）、`path`（嵌套 JSON 路径，如 `messages.0.role`；`-1` 表示数组末尾）、可选 `value`/`from`/`to`/`keep_origin`，以及可选 `conditions`（含 `mode` `full`/`prefix`/`suffix`/`contains`/`gt`/`gte`/`lt`/`lte`，以及 `invert`、`pass_missing_key`、`logic` `AND`/`OR`）——仅当条件匹配时该规则才生效。示例：
  ```json
  {
    "operations": [
      {
        "path": "temperature", "mode": "set", "value": 0.7,
        "conditions": [ { "path": "model", "mode": "prefix", "value": "gpt-" } ]
      }
    ]
  }
  ```
- 转换开启时，建议分组**渠道类型**与上游主格式一致（用于原生格式的密钥校验）。

## 相关项目

- **[gpt-load](https://github.com/tbphp/gpt-load)** — Any-Load 的上游项目，核心架构的全部功劳归于 @tbphp 及 gpt-load 贡献者。
- **[New API](https://github.com/QuantumNous/new-api)** - 优秀的AI模型聚合管理分发系统

## 贡献

感谢所有为 Any-Load 做出贡献的开发者们！

[![Contributors](https://contrib.rocks/image?repo=bohesocool/any-load)](https://github.com/bohesocool/any-load/graphs/contributors)

## 许可证

MIT 许可证 - 详情请参阅 [LICENSE](LICENSE) 文件。

## Star History

[![Stargazers over time](https://starchart.cc/bohesocool/any-load.svg?variant=adaptive)](https://starchart.cc/bohesocool/any-load)
