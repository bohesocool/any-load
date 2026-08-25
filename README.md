# Any-Load

English | [中文](README_CN.md) | [日本語](README_JP.md)

[![Release](https://img.shields.io/github/v/release/bohesocool/any-load)](https://github.com/bohesocool/any-load/releases)
![Go Version](https://img.shields.io/badge/Go-1.24+-blue.svg)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Any-Load is a fork of [tbphp/gpt-load](https://github.com/tbphp/gpt-load) — a high-performance, enterprise-grade AI API transparent proxy service built with Go. It inherits the upstream's key pool management, weighted load balancing, failover, and monitoring, and adds its own roadmap on top.

> 🙏 Special thanks to [@tbphp](https://github.com/tbphp) and the gpt-load contributors. Any-Load would not exist without their work. Upstream: [tbphp/gpt-load](https://github.com/tbphp/gpt-load).

## Roadmap

Any-Load is an early-stage fork. Beyond the upstream feature set:

- **Channel affinity / sticky routing** ✅ — bind requests that share a trait (session header, `session_id` / `prompt_cache_key` / conversation id in body, or first-message hash) to a consistent (upstream, key) pair instead of pure round-robin — useful for upstream session state and smoother rate-limit behavior. Configurable globally and per-group, with a TTL on bindings and automatic failover when a bound key is unavailable.
- **Per-key concurrency limiting** ✅ — cap concurrent in-flight requests per key within a group (group-level override of the global setting); keys at capacity are skipped and rotation falls through to others.
- **Protocol conversion** ✅ — translate between any supported LLM API format (OpenAI Chat / OpenAI Responses / Anthropic / Gemini) at the proxy, in both directions and for streaming + non-streaming, including tool calls and images. Clients can call with any format and the proxy converts it to a format the upstream supports. See [Protocol Conversion](#protocol-conversion).

Transparent pass-through with weighted round-robin across upstreams and keys; channel affinity and per-key concurrency are opt-in (disabled by default).

## Features

- **Transparent Proxy**: Complete preservation of native API formats, supporting OpenAI, Google Gemini, and Anthropic Claude among other formats
- **Protocol Conversion**: Optional per-group conversion between any supported API format (OpenAI Chat / Responses / Anthropic / Gemini) — bidirectional, streaming + non-streaming, with tool-call and image support
- **Intelligent Key Management**: High-performance key pool with group-based management, automatic rotation, and failure recovery
- **Load Balancing**: Weighted load balancing across multiple upstream endpoints to enhance service availability
- **Smart Failure Handling**: Automatic key blacklist management and recovery mechanisms to ensure service continuity
- **Dynamic Configuration**: System settings and group configurations support hot-reload without requiring restarts
- **Enterprise Architecture**: Distributed leader-follower deployment supporting horizontal scaling and high availability
- **Modern Management**: Vue 3-based web management interface that is intuitive and user-friendly
- **Comprehensive Monitoring**: Real-time statistics, health checks, and detailed request logging
- **High-Performance Design**: Zero-copy streaming, connection pool reuse, and atomic operations
- **Production Ready**: Graceful shutdown, error recovery, and comprehensive security mechanisms
- **Dual Authentication**: Separate authentication for management and proxy, with proxy authentication supporting global and group-level keys

## Supported AI Services

Any-Load serves as a transparent proxy service, completely preserving the native API formats of various AI service providers:

- **OpenAI Format**: Official OpenAI API, Azure OpenAI, and other OpenAI-compatible services
- **Google Gemini Format**: Native APIs for Gemini Pro, Gemini Pro Vision, and other models
- **Anthropic Claude Format**: Claude series models, supporting high-quality conversations and text generation

## Quick Start

### System Requirements

- Go 1.24+ (for source builds)
- Docker (for containerized deployment)
- MySQL, PostgreSQL, or SQLite (for database storage)
- Redis (for caching and distributed coordination, optional)

### Method 1: Docker Quick Start

```bash
docker run -d --name any-load \
    -p 3001:3001 \
    -e AUTH_KEY=your-secure-key-here \
    -v "$(pwd)/data":/app/data \
    ghcr.io/bohesocool/any-load:latest
```

> Please change `your-secure-key-here` to a strong password (never use the default value), then you can log in to the management interface: <http://localhost:3001>

### Method 2: Using Docker Compose (Recommended)

**Installation Commands:**

```bash
# Create Directory
mkdir -p any-load && cd any-load

# Download configuration files
wget https://raw.githubusercontent.com/bohesocool/any-load/refs/heads/main/docker-compose.yml
wget -O .env https://raw.githubusercontent.com/bohesocool/any-load/refs/heads/main/.env.example

# Edit the .env file and change AUTH_KEY to a strong password. Never use default or simple keys like sk-123456.

# Start services
docker compose up -d
```

Before deployment, you must change the default admin key (AUTH_KEY). A recommended format is: sk-prod-[32-character random string].

The default installation uses the SQLite version, which is suitable for lightweight, single-instance applications.

If you need to install MySQL, PostgreSQL, and Redis, please uncomment the required services in the `docker-compose.yml` file, configure the corresponding environment variables, and restart.

**Other Commands:**

```bash
# Check service status
docker compose ps

# View logs
docker compose logs -f

# Restart Service
docker compose down && docker compose up -d

# Update to latest version
docker compose pull && docker compose down && docker compose up -d
```

After deployment:

- Access Web Management Interface: <http://localhost:3001>
- API Proxy Address: <http://localhost:3001/proxy>

> Use your modified AUTH_KEY to log in to the management interface.

### Method 3: Source Build

Source build requires a locally installed database (SQLite, MySQL, or PostgreSQL) and Redis (optional).

```bash
# Clone and build
git clone https://github.com/bohesocool/any-load.git
cd any-load
go mod tidy

# Create configuration
cp .env.example .env

# Edit the .env file and change AUTH_KEY to a strong password. Never use default or simple keys like sk-123456.
# Modify DATABASE_DSN and REDIS_DSN configurations in .env
# REDIS_DSN is optional; if not configured, memory storage will be enabled

# Run
make run
```

After deployment:

- Access Web Management Interface: <http://localhost:3001>
- API Proxy Address: <http://localhost:3001/proxy>

> Use your modified AUTH_KEY to log in to the management interface.

### Method 4: Cluster Deployment

Cluster deployment requires all nodes to connect to the same MySQL (or PostgreSQL) and Redis, with Redis being mandatory. It's recommended to use unified distributed MySQL and Redis clusters.

**Deployment Requirements:**

- All nodes must configure identical `AUTH_KEY`, `DATABASE_DSN`, `REDIS_DSN`
- Leader-follower architecture where follower nodes must configure environment variable: `IS_SLAVE=true`

For details, please refer to [Cluster Deployment Documentation](https://github.com/bohesocool/any-load#readme)

## Configuration System

### Configuration Architecture Overview

Any-Load adopts a dual-layer configuration architecture:

#### 1. Static Configuration (Environment Variables)

- **Characteristics**: Read at application startup, immutable during runtime, requires application restart to take effect
- **Purpose**: Infrastructure configuration such as database connections, server ports, authentication keys, etc.
- **Management**: Set via `.env` files or system environment variables

#### 2. Dynamic Configuration (Hot-Reload)

- **System Settings**: Stored in database, providing unified behavioral standards for the entire application
- **Group Configuration**: Behavior parameters customized for specific groups, can override system settings
- **Configuration Priority**: Group Configuration > System Settings > Environment Configuration
- **Characteristics**: Supports hot-reload, takes effect immediately after modification without application restart

<details>
<summary>Static Configuration (Environment Variables)</summary>

**Server Configuration:**

| Setting                   | Environment Variable               | Default         | Description                                     |
| ------------------------- | ---------------------------------- | --------------- | ----------------------------------------------- |
| Service Port              | `PORT`                             | 3001            | HTTP server listening port                      |
| Service Address           | `HOST`                             | 0.0.0.0         | HTTP server binding address                     |
| Read Timeout              | `SERVER_READ_TIMEOUT`              | 60              | HTTP server read timeout (seconds)              |
| Write Timeout             | `SERVER_WRITE_TIMEOUT`             | 600             | HTTP server write timeout (seconds)             |
| Idle Timeout              | `SERVER_IDLE_TIMEOUT`              | 120             | HTTP connection idle timeout (seconds)          |
| Graceful Shutdown Timeout | `SERVER_GRACEFUL_SHUTDOWN_TIMEOUT` | 10              | Service graceful shutdown wait time (seconds)   |
| Follower Mode             | `IS_SLAVE`                         | false           | Follower node identifier for cluster deployment |
| Timezone                  | `TZ`                               | `Asia/Shanghai` | Specify timezone                                |

**Security Configuration:**

| Setting        | Environment Variable | Default | Description                                                                                                                                      |
| -------------- | -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| Admin Key      | `AUTH_KEY`           | -       | Access authentication key for the **management end**, please change it to a strong password                                                      |
| Encryption Key | `ENCRYPTION_KEY`     | -       | Encrypts API keys at rest. Supports any string or leave empty to disable encryption. See [Data Encryption Migration](#data-encryption-migration) |

**Database Configuration:**

| Setting             | Environment Variable | Default              | Description                                             |
| ------------------- | -------------------- | -------------------- | ------------------------------------------------------- |
| Database Connection | `DATABASE_DSN`       | `./data/any-load.db` | Database connection string (DSN) or file path           |
| Redis Connection    | `REDIS_DSN`          | -                    | Redis connection string, uses memory storage when empty |

**Performance & CORS Configuration:**

| Setting                 | Environment Variable      | Default                       | Description                                     |
| ----------------------- | ------------------------- | ----------------------------- | ----------------------------------------------- |
| Max Concurrent Requests | `MAX_CONCURRENT_REQUESTS` | 100                           | Maximum concurrent requests allowed by system   |
| Enable CORS             | `ENABLE_CORS`             | false                         | Whether to enable Cross-Origin Resource Sharing |
| Allowed Origins         | `ALLOWED_ORIGINS`         | -                             | Allowed origins, comma-separated                |
| Allowed Methods         | `ALLOWED_METHODS`         | `GET,POST,PUT,DELETE,OPTIONS` | Allowed HTTP methods                            |
| Allowed Headers         | `ALLOWED_HEADERS`         | `*`                           | Allowed request headers, comma-separated        |
| Allow Credentials       | `ALLOW_CREDENTIALS`       | false                         | Whether to allow sending credentials            |

**Logging Configuration:**

| Setting             | Environment Variable | Default               | Description                         |
| ------------------- | -------------------- | --------------------- | ----------------------------------- |
| Log Level           | `LOG_LEVEL`          | `info`                | Log level: debug, info, warn, error |
| Log Format          | `LOG_FORMAT`         | `text`                | Log format: text, json              |
| Enable File Logging | `LOG_ENABLE_FILE`    | false                 | Whether to enable file log output   |
| Log File Path       | `LOG_FILE_PATH`      | `./data/logs/app.log` | Log file storage path               |

**Proxy Configuration:**

Any-Load automatically reads proxy settings from environment variables to make requests to upstream AI providers.

| Setting     | Environment Variable | Default | Description                                                  |
| ----------- | -------------------- | ------- | ------------------------------------------------------------ |
| HTTP Proxy  | `HTTP_PROXY`         | -       | Proxy server address for HTTP requests                       |
| HTTPS Proxy | `HTTPS_PROXY`        | -       | Proxy server address for HTTPS requests                      |
| No Proxy    | `NO_PROXY`           | -       | Comma-separated list of hosts or domains to bypass the proxy |

Supported Proxy Protocol Formats:

- **HTTP**: `http://user:pass@host:port`
- **HTTPS**: `https://user:pass@host:port`
- **SOCKS5**: `socks5://user:pass@host:port`
</details>

<details>
<summary>Dynamic Configuration (Hot-Reload)</summary>

**Basic Settings:**

| Setting                     | Field Name                           | Default                       | Group Override | Description                                                  |
| --------------------------- | ------------------------------------ | ----------------------------- | -------------- | ------------------------------------------------------------ |
| Project URL                 | `app_url`                            | `http://localhost:3001`       | ❌             | Project base URL                                             |
| Global Proxy Keys           | `proxy_keys`                         | Initial value from `AUTH_KEY` | ❌             | Globally effective proxy keys, comma-separated               |
| Log Retention Days          | `request_log_retention_days`         | 7                             | ❌             | Request log retention days, 0 for no cleanup                 |
| Log Write Interval          | `request_log_write_interval_minutes` | 1                             | ❌             | Log write to database cycle (minutes)                        |
| Enable Request Body Logging | `enable_request_body_logging`        | false                         | ✅             | Whether to log complete request body and upstream response content in request logs |

**Request Settings:**

| Setting                       | Field Name                | Default | Group Override | Description                                                         |
| ----------------------------- | ------------------------- | ------- | -------------- | ------------------------------------------------------------------- |
| Request Timeout               | `request_timeout`         | 600     | ✅             | Forward request complete lifecycle timeout (seconds)                |
| Connection Timeout            | `connect_timeout`         | 15      | ✅             | Timeout for establishing connection with upstream service (seconds) |
| Idle Connection Timeout       | `idle_conn_timeout`       | 120     | ✅             | HTTP client idle connection timeout (seconds)                       |
| Response Header Timeout       | `response_header_timeout` | 600     | ✅             | Timeout for waiting upstream response headers (seconds)             |
| Max Idle Connections          | `max_idle_conns`          | 100     | ✅             | Connection pool maximum total idle connections                      |
| Max Idle Connections Per Host | `max_idle_conns_per_host` | 50      | ✅             | Maximum idle connections per upstream host                          |
| Proxy URL                     | `proxy_url`               | -       | ✅             | HTTP/HTTPS proxy for forwarding requests, uses environment if empty |

**Key Configuration:**

| Setting                    | Field Name                        | Default | Group Override | Description                                                                |
| -------------------------- | --------------------------------- | ------- | -------------- | -------------------------------------------------------------------------- |
| Max Retries                | `max_retries`                     | 3       | ✅             | Maximum retry count using different keys for single request                |
| Blacklist Threshold        | `blacklist_threshold`             | 3       | ✅             | After how many cumulative failures does the key get blacklisted            |
| Uncounted Status Codes     | `uncounted_status_codes`          | (empty) | ✅             | Status codes that still retry but skip failure accounting; never disables account. Empty = no exemption |
| Key Validation Interval    | `key_validation_interval_minutes` | 60      | ✅             | Background scheduled key validation cycle (minutes)                        |
| Key Validation Concurrency | `key_validation_concurrency`      | 10      | ✅             | Concurrency for background validation of invalid keys                      |
| Key Validation Timeout     | `key_validation_timeout_seconds`  | 20      | ✅             | API request timeout for validating individual keys in background (seconds) |

</details>

## Data Encryption Migration

Any-Load supports encrypted storage of API keys. You can enable, disable, or change the encryption key at any time.

<details>
<summary>View Data Encryption Migration Details</summary>

### Migration Scenarios

- **Enable Encryption**: Encrypt plaintext data for storage - Use `--to <new-key>`
- **Disable Encryption**: Decrypt encrypted data to plaintext - Use `--from <current-key>`
- **Change Encryption Key**: Replace the encryption key - Use `--from <current-key> --to <new-key>`

### Operation Steps

#### Docker Compose Deployment

```bash
# 1. Update the image (ensure using the latest version)
docker compose pull

# 2. Stop the service
docker compose down

# 3. Backup the database (strongly recommended)
# Before migration, you must manually backup the database or export your keys to avoid key loss due to operations or exceptions.

# 4. Execute migration command
# Enable encryption (your-32-char-secret-key is your key, recommend using 32+ character random string)
docker compose run --rm any-load migrate-keys --to "your-32-char-secret-key"

# Disable encryption
docker compose run --rm any-load migrate-keys --from "your-current-key"

# Change encryption key
docker compose run --rm any-load migrate-keys --from "old-key" --to "new-32-char-secret-key"

# 5. Update configuration file
# Edit .env file, set ENCRYPTION_KEY to match the --to parameter
# If disabling encryption, remove ENCRYPTION_KEY or set it to empty
vim .env
# Add or modify: ENCRYPTION_KEY=your-32-char-secret-key

# 6. Restart the service
docker compose up -d
```

#### Source Build Deployment

```bash
# 1. Stop the service
# Stop the running service process (Ctrl+C or kill process)

# 2. Backup the database (strongly recommended)
# Before migration, you must manually backup the database or export your keys to avoid key loss due to operations or exceptions.

# 3. Execute migration command
# Enable encryption
make migrate-keys ARGS="--to your-32-char-secret-key"

# Disable encryption
make migrate-keys ARGS="--from your-current-key"

# Change encryption key
make migrate-keys ARGS="--from old-key --to new-32-char-secret-key"

# 4. Update configuration file
# Edit .env file, set ENCRYPTION_KEY to match the --to parameter
echo "ENCRYPTION_KEY=your-32-char-secret-key" >> .env

# 5. Restart the service
make run
```

### Important Notes

⚠️ **Important Reminders**:

- **Once ENCRYPTION_KEY is lost, encrypted data CANNOT be recovered!** Please securely backup this key. Consider using a password manager or secure key management system
- **Service must be stopped** before migration to avoid data inconsistency
- Strongly recommended to **backup the database** in case migration fails and recovery is needed
- Keys should use **32 characters or longer random strings** for security
- Ensure `ENCRYPTION_KEY` in `.env` matches the `--to` parameter after migration
- If disabling encryption, remove or clear the `ENCRYPTION_KEY` configuration

### Key Generation Examples

```bash
# Generate secure random key (32 characters)
openssl rand -base64 32 | tr -d "=+/" | cut -c1-32
```

</details>

## Web Management Interface

Access the management console at: <http://localhost:3001> (default address)

### Interface Overview

<img src="screenshot/dashboard.png" alt="Dashboard" width="600"/>

<br/>

<img src="screenshot/keys.png" alt="Key Management" width="600"/>

<br/>

The web management interface provides the following features:

- **Dashboard**: Real-time statistics and system status overview
- **Key Management**: Create and configure AI service provider groups, add, delete, and monitor API keys
- **Request Logs**: Detailed request history and debugging information
- **System Settings**: Global configuration management and hot-reload

## API Usage Guide

<details>
<summary>Proxy Interface Invocation</summary>

Any-Load routes requests to different AI services through group names. Usage is as follows:

### 1. Proxy Endpoint Format

```text
http://localhost:3001/proxy/{group_name}/{original_api_path}
```

- `{group_name}`: Group name created in the management interface
- `{original_api_path}`: Maintain complete consistency with original AI service paths

### 2. Authentication Methods

Configure **Proxy Keys** in the web management interface, which supports system-level and group-level proxy keys.

- **Authentication Method**: Consistent with the native API, but replace the original key with the configured proxy key.
- **Key Scope**: **Global Proxy Keys** configured in system settings can be used in all groups. **Group Proxy Keys** configured in a group are only valid for the current group.
- **Format**: Multiple keys are separated by commas.

### 3. OpenAI Interface Example

Any-Load currently supports two OpenAI-compatible group types:

- `openai` (OpenAI Chat Completions format)
- `openai-response` (OpenAI Responses format)

Assuming a group named `openai` was created:

**Original invocation:**

```bash
curl -X POST https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer sk-your-openai-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "messages": [{"role": "user", "content": "Hello"}]}'
```

**Proxy invocation:**

```bash
curl -X POST http://localhost:3001/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer your-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "messages": [{"role": "user", "content": "Hello"}]}'
```

**Changes required:**

- Replace `https://api.openai.com` with `http://localhost:3001/proxy/openai`
- Replace original API Key with the **Proxy Key**

**OpenAI Responses format example (`openai-response` group):**

```bash
curl -X POST http://localhost:3001/proxy/openai-response/v1/responses \
  -H "Authorization: Bearer your-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4.1-mini", "input": "Hello"}'
```

### 4. Gemini Interface Example

Assuming a group named `gemini` was created:

**Original invocation:**

```bash
curl -X POST https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent?key=your-gemini-key \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "Hello"}]}]}'
```

**Proxy invocation:**

```bash
curl -X POST http://localhost:3001/proxy/gemini/v1beta/models/gemini-2.5-pro:generateContent?key=your-proxy-key \
  -H "Content-Type: application/json" \
  -d '{"contents": [{"parts": [{"text": "Hello"}]}]}'
```

**Changes required:**

- Replace `https://generativelanguage.googleapis.com` with `http://localhost:3001/proxy/gemini`
- Replace `key=your-gemini-key` in URL parameter with the **Proxy Key**

### 5. Anthropic Interface Example

Assuming a group named `anthropic` was created:

**Original invocation:**

```bash
curl -X POST https://api.anthropic.com/v1/messages \
  -H "x-api-key: sk-ant-api03-your-anthropic-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model": "claude-sonnet-4-20250514", "messages": [{"role": "user", "content": "Hello"}]}'
```

**Proxy invocation:**

```bash
curl -X POST http://localhost:3001/proxy/anthropic/v1/messages \
  -H "x-api-key: your-proxy-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model": "claude-sonnet-4-20250514", "messages": [{"role": "user", "content": "Hello"}]}'
```

**Changes required:**

- Replace `https://api.anthropic.com` with `http://localhost:3001/proxy/anthropic`
- Replace the original API Key in `x-api-key` header with the **Proxy Key**

### 6. Supported Interfaces

**OpenAI Chat Completions Format (`openai`):**

- `/v1/chat/completions` - Chat conversations
- `/v1/completions` - Text completion
- `/v1/embeddings` - Text embeddings
- `/v1/models` - Model list
- And all other OpenAI-compatible interfaces

**OpenAI Responses Format (`openai-response`):**

- `/v1/responses` - Unified response generation
- `/v1/models` - Model list
- And all other OpenAI Responses-compatible interfaces

**Gemini Format:**

- `/v1beta/models/*/generateContent` - Content generation
- `/v1beta/models` - Model list
- And all other Gemini native interfaces

**Anthropic Format:**

- `/v1/messages` - Message conversations
- `/v1/models` - Model list (if available)
- And all other Anthropic native interfaces

### 7. Client SDK Configuration

**OpenAI Python SDK:**

```python
from openai import OpenAI

client = OpenAI(
    api_key="your-proxy-key",  # Use the proxy key
    base_url="http://localhost:3001/proxy/openai"  # Use proxy endpoint
)

response = client.chat.completions.create(
    model="gpt-4.1-mini",
    messages=[{"role": "user", "content": "Hello"}]
)
```

**Google Gemini SDK (Python):**

```python
import google.generativeai as genai

# Configure API key and base URL
genai.configure(
    api_key="your-proxy-key",  # Use the proxy key
    client_options={"api_endpoint": "http://localhost:3001/proxy/gemini"}
)

model = genai.GenerativeModel('gemini-2.5-pro')
response = model.generate_content("Hello")
```

**Anthropic SDK (Python):**

```python
from anthropic import Anthropic

client = Anthropic(
    api_key="your-proxy-key",  # Use the proxy key
    base_url="http://localhost:3001/proxy/anthropic"  # Use proxy endpoint
)

response = client.messages.create(
    model="claude-sonnet-4-20250514",
    messages=[{"role": "user", "content": "Hello"}]
)
```

> **Important Note**: As a transparent proxy service, Any-Load completely preserves the native API formats and authentication methods of various AI services. You only need to replace the endpoint address and use the **Proxy Key** configured in the management interface for seamless migration.

</details>

## Protocol Conversion

Protocol Conversion is an **opt-in, per-group** feature that translates between any supported LLM API format at the proxy. A client can call with one format (e.g. Anthropic `/v1/messages`) and have the request converted to a format the upstream actually supports (e.g. OpenAI `/v1/chat/completions`), with the response converted back to the client's format. This mirrors the behavior of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

Conversion is **bidirectional** (any inbound → any upstream), works for **both streaming and non-streaming** responses, and includes **tool calls** (definitions, calls, and results) and **images**.

### Two group settings

| Setting | Values | Effect |
|---|---|---|
| **Protocol Conversion** (开关) | on / off | off = pure pass-through, identical to before |
| **Upstream Formats** (上游格式, multi-select) | `openai-chat` / `openai-response` / `anthropic` / `gemini` | declares **which formats the upstream can accept** |

The inbound (client) format is **auto-detected from the request path** — the client does not declare it:

| Request path | Inbound format |
|---|---|
| `/v1/chat/completions` | `openai-chat` |
| `/v1/responses` | `openai-response` |
| `/v1/messages` | `anthropic` |
| `/v1beta/models/<model>:generateContent` / `:streamGenerateContent` | `gemini` |

### How a target is chosen (when conversion is ON)

1. If the inbound format **is in** the upstream-formats list → **pass through** unchanged (smart passthrough — no conversion).
2. If the inbound format **is not** in the list → convert to the **first** format in the list (the "preferred" target).

Conversion is skipped entirely (pure pass-through) when: the switch is off, the list is empty, the inbound path is not a conversion-eligible endpoint (e.g. `/v1/models`), or the inbound format is already supported upstream.

### Configuration examples

**Upstream supports only OpenAI Chat** — `Upstream Formats = ["openai-chat"]`:

| Client sends | What happens |
|---|---|
| OpenAI Chat (`/v1/chat/completions`) | Inbound chat is in the list → **passthrough** to `/v1/chat/completions`, Bearer auth |
| OpenAI Responses (`/v1/responses`) | Not in list → **converted** to chat → forwarded to `/v1/chat/completions`; response converted back to Responses |
| Anthropic (`/v1/messages`) | Not in list → **converted** to chat → forwarded to `/v1/chat/completions`; response converted back to Anthropic |
| Gemini (`:generateContent`) | Not in list → **converted** to chat → forwarded to `/v1/chat/completions`; response converted back to Gemini |

→ No matter which format the client uses, the upstream always receives OpenAI Chat.

**Upstream supports Chat and Responses** — `Upstream Formats = ["openai-chat", "openai-response"]`:

| Client sends | What happens |
|---|---|
| OpenAI Chat | In the list → **passthrough** `/v1/chat/completions` |
| OpenAI Responses | In the list → **passthrough** `/v1/responses` |
| Anthropic | Not in list → converted to the **first** listed format (chat) → `/v1/chat/completions` |
| Gemini | Not in list → converted to the **first** listed format (chat) → `/v1/chat/completions` |

> **List order matters**: the "preferred" target is always the **first** item. To make Anthropic-in convert *to Responses*, put responses first: `["openai-response", "openai-chat"]`. (A chat client still passes through, since chat remains in the list.)

### Supported conversion coverage

- **Formats**: OpenAI Chat, OpenAI Responses, Anthropic Messages, Gemini — any-to-any, both directions.
- **Tool calls**: tool definitions (`function.parameters` ↔ `input_schema` ↔ `functionDeclarations` ↔ Responses flat schema), tool calls (`tool_calls` ↔ `tool_use` ↔ `functionCall` ↔ `function_call`), and tool results (`role:"tool"` ↔ `tool_result` ↔ `functionResponse` ↔ `function_call_output`).
- **Images**: `image_url` (URL or `data:` URI) ↔ Anthropic `image` source (base64/URL) ↔ Gemini `inline_data` (base64) ↔ Responses `input_image`.

### Notes & limitations

- **Conversion OFF** = pure pass-through regardless of the format list (identical to the original behavior; no request/response is parsed or rewritten).
- **Streaming tool calls** are translated per-format (Anthropic `input_json_delta`, OpenAI `tool_calls[].function.arguments` deltas, Responses `function_call_arguments.delta`, Gemini whole-call).
- **Gemini image URLs**: Gemini's `inline_data` requires base64. A plain `http(s)` URL image (not a `data:` URI) is **rejected with HTTP 400** rather than fetched — provide base64 or a `data:` URI.
- **Gemini has no call IDs**: tool-call IDs are synthesized (`name#index`) when parsing Gemini.
- Error responses in conversion mode are currently passed through as-is (the upstream error body is not yet translated to the inbound format's error shape).
- `param_overrides` are not applied in conversion mode (they target OpenAI-body keys; their semantics differ across formats).
- When conversion is ON, keep the group's **Channel Type** matching the primary upstream format (used for native-format key validation).

## Related Projects

- **[gpt-load](https://github.com/tbphp/gpt-load)** — The upstream project Any-Load is forked from. All credit for the core architecture goes to @tbphp and the gpt-load contributors.
- **[New API](https://github.com/QuantumNous/new-api)** - Excellent AI model aggregation management and distribution system

## Contributing

Thanks to all the developers who have contributed to Any-Load!

[![Contributors](https://contrib.rocks/image?repo=bohesocool/any-load)](https://github.com/bohesocool/any-load/graphs/contributors)

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Star History

[![Stargazers over time](https://starchart.cc/bohesocool/any-load.svg?variant=adaptive)](https://starchart.cc/bohesocool/any-load)
