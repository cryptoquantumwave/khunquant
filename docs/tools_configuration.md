# Tools Configuration

KhunQuant's tools configuration is located in the `tools` field of `config.json`.

## Directory Structure

```json
{
  "tools": {
    "web": {
      ...
    },
    "mcp": {
      ...
    },
    "exec": {
      ...
    },
    "cron": {
      ...
    },
    "skills": {
      ...
    }
  }
}
```

## Web Tools

Web tools are used for web search and fetching.

### Brave

| Config        | Type   | Default | Description               |
|---------------|--------|---------|---------------------------|
| `enabled`     | bool   | false   | Enable Brave search       |
| `api_key`     | string | -       | Brave Search API key      |
| `max_results` | int    | 5       | Maximum number of results |

### DuckDuckGo

| Config        | Type | Default | Description               |
|---------------|------|---------|---------------------------|
| `enabled`     | bool | true    | Enable DuckDuckGo search  |
| `max_results` | int  | 5       | Maximum number of results |

### Perplexity

| Config        | Type   | Default | Description               |
|---------------|--------|---------|---------------------------|
| `enabled`     | bool   | false   | Enable Perplexity search  |
| `api_key`     | string | -       | Perplexity API key        |
| `max_results` | int    | 5       | Maximum number of results |

## Exec Tool

The exec tool is used to execute shell commands.

| Config                 | Type  | Default | Description                                |
|------------------------|-------|---------|--------------------------------------------|
| `enable_deny_patterns` | bool  | true    | Enable default dangerous command blocking  |
| `custom_deny_patterns` | array | []      | Custom deny patterns (regular expressions) |
| `env_passthrough`      | array | []      | Extra environment variables children inherit |

### Functionality

- **`enable_deny_patterns`**: Set to `false` to completely disable the default dangerous command blocking patterns
- **`custom_deny_patterns`**: Add custom deny regex patterns; commands matching these will be blocked
- **`env_passthrough`**: Names additional environment variables that executed commands may inherit

### Child Process Environment

Commands started by the exec tool do **not** inherit KhunQuant's environment. That environment holds
the LLM and exchange API keys (see `.env.example`), and a child that inherits it hands every one of
them to any command the agent runs — `printenv`, or any program that reads credentials from the
environment on its own.

Children instead receive an allowlist: `PATH`, `HOME`, `TMPDIR`/`TMP`/`TEMP`, `LANG`, `LC_*`, `TZ`,
`USER`, `LOGNAME`, `SHELL`, `TERM` (plus `SystemRoot`, `ComSpec`, `PATHEXT`, the profile directories
and similar on Windows), and anything named in `env_passthrough`. An entry ending in `*` passes a
prefix group; a bare `"*"` is ignored, since it would restore the full environment and the leak with it.

```json
{
  "tools": {
    "exec": {
      "env_passthrough": ["GOFLAGS", "CI_*"]
    }
  }
}
```

Note that the workspace guard expands `~`, `$HOME`, and *unset* variables when checking paths, but
leaves other set variables literal. A passed-through variable holding an absolute path is therefore
not resolved during the check: with `env_passthrough: ["DATA_DIR"]` and `DATA_DIR=/etc`, the command
`cat $DATA_DIR/passwd` is checked as written. Pass through variables that name directories only if
the agent is meant to reach them.

When `restrict_to_workspace` is enabled, `HOME` is additionally set to the workspace directory so that
`~` means the same path to the safety guard and to the shell. Tools that keep user-level configuration
in `$HOME` (git, npm, pip) therefore look for it inside the workspace while the restriction is active.

### Default Blocked Command Patterns

By default, KhunQuant blocks the following dangerous commands:

- Delete commands: `rm -rf`, `del /f/q`, `rmdir /s`
- Disk operations: `format`, `mkfs`, `diskpart`, `dd if=`, writing to `/dev/sd*`
- System operations: `shutdown`, `reboot`, `poweroff`
- Command substitution: `$()`, `${}`, backticks
- Pipe to shell: `| sh`, `| bash`
- Privilege escalation: `sudo`, `chmod`, `chown`
- Process control: `pkill`, `killall`, `kill -9`
- Remote operations: `curl | sh`, `wget | sh`, `ssh`
- Package management: `apt`, `yum`, `dnf`, `npm install -g`, `pip install --user`
- Containers: `docker run`, `docker exec`
- Git: `git push`, `git force`
- Other: `eval`, `source *.sh`

### Configuration Example

```json
{
  "tools": {
    "exec": {
      "enable_deny_patterns": true,
      "custom_deny_patterns": [
        "\\brm\\s+-r\\b",
        "\\bkillall\\s+python"
      ]
    }
  }
}
```

## Cron Tool

The cron tool is used for scheduling periodic tasks.

| Config                  | Type | Default | Description                                                                                                                              |
|-------------------------|------|---------|------------------------------------------------------------------------------------------------------------------------------------------|
| `exec_timeout_minutes`  | int  | 5       | Timeout of the shell command of a command job, in minutes, 0 means no limit                                                              |
| `workers`               | int  | 4       | How many scheduled jobs may run at the same time. At most one LLM-backed job runs at a time whatever this is                             |
| `job_timeout_minutes`   | int  | 10      | Timeout of a single scheduled run of any kind, in minutes, 0 means no limit. A timed-out run is recorded as an error                     |
| `misfire_grace_minutes` | int  | 60      | A one-shot job that was due while the assistant was offline still runs at startup if late by less than this; otherwise it is reported once and disabled |

Cron expressions are evaluated in the job's `tz` (an IANA name such as `Asia/Bangkok`); without one they use the host's local time zone.

Scheduled agent jobs run in their own session (`agent:<default agent>:cron:<job id>`) without loading history; the session is emptied before each run, so it only ever holds the most recent run. A cron-expression job whose slot was missed while the assistant was offline runs once at startup if it is late by less than `misfire_grace_minutes`; otherwise it skips to the next slot and records `missed` in its last status.

Known limitation: a config reload that lands while a scheduled agent job is running waits up to 10 seconds for it and then cancels that run; it is not retried until its next slot.

## MCP Tool

The MCP tool enables integration with external Model Context Protocol servers.

### Tool Discovery (Lazy Loading)

When connecting to multiple MCP servers, exposing hundreds of tools simultaneously can exhaust the LLM's context window
and increase API costs. The **Discovery** feature solves this by keeping MCP tools *hidden* by default.

Instead of loading all tools, the LLM is provided with a lightweight search tool (using BM25 keyword matching or Regex).
When the LLM needs a specific capability, it searches the hidden library. Matching tools are then temporarily "unlocked"
and injected into the context for a configured number of turns (`ttl`).

### Global Config

| Config      | Type   | Default | Description                                  |
|-------------|--------|---------|----------------------------------------------|
| `enabled`   | bool   | false   | Enable MCP integration globally              |
| `discovery` | object | `{}`    | Configuration for Tool Discovery (see below) |
| `servers`   | object | `{}`    | Map of server name to server config          |

### Discovery Config (`discovery`)

| Config               | Type | Default | Description                                                                                                                       |
|----------------------|------|---------|-----------------------------------------------------------------------------------------------------------------------------------|
| `enabled`            | bool | false   | If true, MCP tools are hidden and loaded on-demand via search. If false, all tools are loaded                                     |
| `ttl`                | int  | 5       | Number of conversational turns a discovered tool remains unlocked                                                                 |
| `max_search_results` | int  | 5       | Maximum number of tools returned per search query                                                                                 |
| `use_bm25`           | bool | true    | Enable the natural language/keyword search tool (`tool_search_tool_bm25`). **Warning**: consumes more resources than regex search |
| `use_regex`          | bool | false   | Enable the regex pattern search tool (`tool_search_tool_regex`)                                                                   |

> **Note:** If `discovery.enabled` is `true`, you MUST enable at least one search engine (`use_bm25` or `use_regex`),
> otherwise the application will fail to start.

### Per-Server Config

| Config     | Type   | Required | Description                                |
|------------|--------|----------|--------------------------------------------|
| `enabled`  | bool   | yes      | Enable this MCP server                     |
| `type`     | string | no       | Transport type: `stdio`, `sse`, `http`     |
| `command`  | string | stdio    | Executable command for stdio transport     |
| `args`     | array  | no       | Command arguments for stdio transport      |
| `env`      | object | no       | Environment variables for stdio process    |
| `env_file` | string | no       | Path to environment file for stdio process |
| `url`      | string | sse/http | Endpoint URL for `sse`/`http` transport    |
| `headers`  | object | no       | HTTP headers for `sse`/`http` transport    |

### Transport Behavior

- If `type` is omitted, transport is auto-detected:
    - `url` is set → `sse`
    - `command` is set → `stdio`
- `http` and `sse` both use `url` + optional `headers`.
- `env` and `env_file` are only applied to `stdio` servers.

### Configuration Examples

#### 1) Stdio MCP server

```json
{
  "tools": {
    "mcp": {
      "enabled": true,
      "servers": {
        "filesystem": {
          "enabled": true,
          "command": "npx",
          "args": [
            "-y",
            "@modelcontextprotocol/server-filesystem",
            "/tmp"
          ]
        }
      }
    }
  }
}
```

#### 2) Remote SSE/HTTP MCP server

```json
{
  "tools": {
    "mcp": {
      "enabled": true,
      "servers": {
        "remote-mcp": {
          "enabled": true,
          "type": "sse",
          "url": "https://example.com/mcp",
          "headers": {
            "Authorization": "Bearer YOUR_TOKEN"
          }
        }
      }
    }
  }
}
```

#### 3) Massive MCP setup with Tool Discovery enabled

*In this example, the LLM will only see the `tool_search_tool_bm25`. It will search and unlock Github or Postgres tools
dynamically only when requested by the user.*

```json
{
  "tools": {
    "mcp": {
      "enabled": true,
      "discovery": {
        "enabled": true,
        "ttl": 5,
        "max_search_results": 5,
        "use_bm25": true,
        "use_regex": false
      },
      "servers": {
        "github": {
          "enabled": true,
          "command": "npx",
          "args": [
            "-y",
            "@modelcontextprotocol/server-github"
          ],
          "env": {
            "GITHUB_PERSONAL_ACCESS_TOKEN": "YOUR_GITHUB_TOKEN"
          }
        },
        "postgres": {
          "enabled": true,
          "command": "npx",
          "args": [
            "-y",
            "@modelcontextprotocol/server-postgres",
            "postgresql://user:password@localhost/dbname"
          ]
        },
        "slack": {
          "enabled": true,
          "command": "npx",
          "args": [
            "-y",
            "@modelcontextprotocol/server-slack"
          ],
          "env": {
            "SLACK_BOT_TOKEN": "YOUR_SLACK_BOT_TOKEN",
            "SLACK_TEAM_ID": "YOUR_SLACK_TEAM_ID"
          }
        }
      }
    }
  }
}
```

## Skills Tool

The skills tool configures skill discovery and installation via registries like ClawHub.

### Registries

| Config                             | Type   | Default              | Description                                  |
|------------------------------------|--------|----------------------|----------------------------------------------|
| `registries.clawhub.enabled`       | bool   | true                 | Enable ClawHub registry                      |
| `registries.clawhub.base_url`      | string | `https://clawhub.ai` | ClawHub base URL                             |
| `registries.clawhub.auth_token`    | string | `""`                 | Optional Bearer token for higher rate limits |
| `registries.clawhub.search_path`   | string | `/api/v1/search`     | Search API path                              |
| `registries.clawhub.skills_path`   | string | `/api/v1/skills`     | Skills API path                              |
| `registries.clawhub.download_path` | string | `/api/v1/download`   | Download API path                            |

### Configuration Example

```json
{
  "tools": {
    "skills": {
      "registries": {
        "clawhub": {
          "enabled": true,
          "base_url": "https://clawhub.ai",
          "auth_token": "",
          "search_path": "/api/v1/search",
          "skills_path": "/api/v1/skills",
          "download_path": "/api/v1/download"
        }
      }
    }
  }
}
```

## Environment Variables

All configuration options can be overridden via environment variables with the format `KHUNQUANT_TOOLS_<SECTION>_<KEY>`:

For example:

- `KHUNQUANT_TOOLS_WEB_BRAVE_ENABLED=true`
- `KHUNQUANT_TOOLS_EXEC_ENABLE_DENY_PATTERNS=false`
- `KHUNQUANT_TOOLS_CRON_EXEC_TIMEOUT_MINUTES=10`
- `KHUNQUANT_TOOLS_CRON_WORKERS=4`
- `KHUNQUANT_TOOLS_CRON_JOB_TIMEOUT_MINUTES=10`
- `KHUNQUANT_TOOLS_CRON_MISFIRE_GRACE_MINUTES=60`
- `KHUNQUANT_TOOLS_MCP_ENABLED=true`

Note: Nested map-style config (for example `tools.mcp.servers.<name>.*`) is configured in `config.json` rather than
environment variables.
