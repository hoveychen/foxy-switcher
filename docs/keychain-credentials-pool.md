# Claude Code (macOS) Keychain 凭证程序化替换 — 研究笔记

研究目标：在 macOS 上做 Claude Code CLI 的"凭证池"轮换，**不**走官方推荐的 `apiKeyHelper` 机制，而是直接覆写 Keychain item。

## 1. Keychain item 元信息

`/login` 走完后，Claude Code 在 Keychain 里写**两个独立的 generic-password item**，必须一起看待：

| Item | Service (`-s`) | 内容 | 何时用 |
|---|---|---|---|
| OAuth tokens | `Claude Code-credentials` | `claudeAiOauth` JSON blob（access/refresh token、expiresAt、scopes、subscriptionType、rateLimitTier、clientId、外层 organizationUuid） | 没有「托管 API key」时回退使用 |
| 托管 API key | `Claude Code` | 单字符串，`sk-ant-api03-...` 格式的 subscription-bound API key | **运行时实际发请求拿的就是这一个**（优先级高于 OAuth tokens，详见 §4.5） |

| 公共项 | 值 |
|---|---|
| Keychain | `~/Library/Keychains/login.keychain-db` |
| Class | `genp` (generic password) |
| Account (`-a`) | macOS 用户名（本机为 `hoveychen`） |

注意：此前调研时一度误以为 service 名是 `claude-code`——实测错的，正确名字带空格、首字母大写。OAuth tokens 项有 `-credentials` 后缀，托管 API key 项没有后缀。

**Service 名拼接规则**（[claude-code-fork/src/utils/secureStorage/macOsKeychainHelpers.ts:27-41](src/utils/secureStorage/macOsKeychainHelpers.ts#L27-L41)）：

```
`Claude Code${OAUTH_FILE_SUFFIX}${serviceSuffix}${dirHash}`
```

- `OAUTH_FILE_SUFFIX`：prod 为空；staging 为 `-staging-oauth`；local dev 为 `-local-oauth`；FedStart 自定义为 `-custom-oauth`
- `serviceSuffix`：OAuth tokens 项为 `-credentials`，托管 API key 项为空
- `dirHash`：默认安装为空；设了 `CLAUDE_CONFIG_DIR` 则拼 8 位 sha256 哈希（如 `-a3f1b2c8`）

所以默认 prod 安装下两个名字就是 `Claude Code` 和 `Claude Code-credentials`。遇到 staging/自定义 config dir 的机器名字会不一样，别看见 `Claude Code-credentials-a3f1b2c8` 之类的就慌。

不相关 item（**不要动**）：
- `Claude Safe Storage` / `Claude Key`：Claude **桌面 App** 用的，与 CLI 无关。

## 2. 存储格式

### 2.1 OAuth tokens 项（`Claude Code-credentials`）

password 字段是**单个 JSON blob**（约 600 字节），结构：

```json
{
  "claudeAiOauth": {
    "accessToken":      "sk-ant-oat01-...",   // 108 字符 OAuth access token
    "refreshToken":     "sk-ant-ort01-...",   // 108 字符 OAuth refresh token
    "expiresAt":        1700000000000,         // 13 位毫秒时间戳
    "scopes":           ["...", "...", "...", "...", "..."],  // scope 字符串数组，默认含 "user:inference"
    "subscriptionType": "...",                 // 例如 "max"
    "rateLimitTier":    "...",                 // 21 字符
    "clientId":         "9d1c250a-e61b-44d9-88ed-5944d1962f5e"   // OAuth 客户端 ID，固定常量
  },
  "organizationUuid":   "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
}
```

**关键点**：
- `accessToken` 前缀 `sk-ant-oat01-` = OAuth Access Token v01。
- `refreshToken` 前缀 `sk-ant-ort01-` = OAuth Refresh Token v01。
- `organizationUuid` 在 **外层**，与 `claudeAiOauth` 平行，不在它内部。
- `clientId` 是 Anthropic 给 Claude Code 这个 OAuth 应用注册的 client ID，**全部用户共用同一个常量**（[claude-code-fork/src/constants/oauth.ts:99](src/constants/oauth.ts#L99)），prod 值为 `9d1c250a-e61b-44d9-88ed-5944d1962f5e`，staging 值为 `22422756-60c9-4084-8eb7-27705fd5cf9a`。不是个人机密，不需要轮换，但**写入时不能省略**。
- 必须保留全部字段，缺字段或类型不对会让 Claude Code 解析失败或下次 refresh 失败。
- 该 JSON 在 macOS Keychain / Linux libsecret / Linux `~/.claude/.credentials.json`（plaintext fallback，mode 0600）三种后端里**字节序列完全一致**——单看 blob 区分不出来源平台，从二进制 writer 函数 `cS8` 同一段代码可证实。

### 2.2 托管 API key 项（`Claude Code`，无后缀）

password 字段是**纯字符串**（不是 JSON），就是一把 `sk-ant-api03-...` 格式的 API key，由 Anthropic 在 OAuth 完成时通过 `https://api.anthropic.com/api/oauth/claude_cli/create_api_key` 端点凭 subscription 身份铸造给当前账号，账单计入 subscription 套餐而不是 API console 余额。

非 macOS 平台上，这把 key 落在 `~/.claude.json` 的 **`primaryApiKey`** 字段（[claude-code-fork/src/utils/config.ts:224](src/utils/config.ts#L224)），不在 `.credentials.json` 里。

## 3. 程序化替换的核心命令

两个 keychain item 都用 `genp` class，操作命令格式相同，只是 service 名不同：

```bash
SVC_OAUTH="Claude Code-credentials"   # OAuth tokens（JSON blob）
SVC_APIKEY="Claude Code"              # 托管 API key（纯字符串）
ACCT="$USER"

# 读出 OAuth tokens（明文 JSON，含 token，慎用）
security find-generic-password -s "$SVC_OAUTH" -a "$ACCT" -w

# 读出托管 API key（明文 sk-ant-api03-... 字符串）
security find-generic-password -s "$SVC_APIKEY" -a "$ACCT" -w

# 覆盖写入新 JSON / 新 API key（-U = update if exists, -w = password 来自参数）
security add-generic-password -U -s "$SVC_OAUTH"  -a "$ACCT" -w "$NEW_JSON"
security add-generic-password -U -s "$SVC_APIKEY" -a "$ACCT" -w "$NEW_API_KEY"

# 删除（清空登录态——两个都要删才彻底登出）
security delete-generic-password -s "$SVC_OAUTH"  -a "$ACCT"
security delete-generic-password -s "$SVC_APIKEY" -a "$ACCT"
```

注意 `security add-generic-password -w "$VALUE"` 会让明文进入进程命令行参数，本机 `ps`/Activity Monitor 都能看见。Claude Code 自己的实现用 `security -i` interactive 模式 + `-X`（hex 编码）写入避免这一点（见 [auth.ts:1116-1121](src/utils/auth.ts#L1116-L1121)）。如果要写入真实账号 token 建议照搬这个手法。

## 4. 已知的坎

### 4.1 自动刷新会覆盖凭证池 entry

Claude Code 启动时若发现 `expiresAt` 已过 → 用 `refreshToken` 自动刷新 → **新 token 写回 Keychain**。这意味着凭证池里那条 entry 在被使用一次后已经"漂"了，下次再用会失败（refreshToken 通常一次性）。

应对方案：每次切换前从池里取 entry → 写入 keychain → 用完后再从 keychain 读出**新的** JSON 写回池。或者每次直接走 OAuth 流程获取新一批凭证。

### 4.2 organizationUuid 必须同步切换

切换不同 organization 不能只换 `claudeAiOauth.accessToken`，外层 `organizationUuid` 也得换。每个池 entry 当作"账号 + org"二元组存。

### 4.3 进程缓存（待验证）

未确认 Claude Code 进程启动时是否缓存 keychain。验证方法：
```bash
# Terminal A：启动 claude，开一个 session
# Terminal B：备份后写个坏 token
ORIG=$(security find-generic-password -s "Claude Code-credentials" -a "$USER" -w)
echo "$ORIG" | python3 -c '
import json, sys
d = json.load(sys.stdin)
d["claudeAiOauth"]["accessToken"] = "sk-ant-oat01-INVALID"
print(json.dumps(d))
' | xargs -0 -I{} security add-generic-password -U \
       -s "Claude Code-credentials" -a "$USER" -w {}
# 回 Terminal A 继续问问题：
#   报 401 → 进程不缓存（覆写即生效）
#   仍能用 → 进程启动时缓存了（覆写后需重启 claude）
# 验证完恢复：
printf '%s' "$ORIG" | xargs -0 -I{} security add-generic-password -U \
       -s "Claude Code-credentials" -a "$USER" -w {}
```

### 4.4 优先级：环境变量会赢过 Keychain

按官方文档（https://code.claude.com/docs/en/authentication）认证优先级：

1. Cloud provider credentials (BEDROCK / VERTEX / FOUNDRY)
2. `ANTHROPIC_AUTH_TOKEN`
3. `ANTHROPIC_API_KEY`
4. `apiKeyHelper`
5. `CLAUDE_CODE_OAUTH_TOKEN`
6. Keychain (Subscription OAuth) ← 我们覆写的位置

如果 shell 里设了 `ANTHROPIC_API_KEY` 或 `CLAUDE_CODE_OAUTH_TOKEN`，凭证池会被锁死在那一个值上。轮换前先 `unset` 这两个变量，或在启动 claude 的脚本里 explicit `unset`。

### 4.5 托管 API key 优先级高于 OAuth tokens

§4.4 那个官方优先级列表把「Keychain (Subscription OAuth)」当成单一节点，其实 keychain 内部还有一层优先级：

```
托管 API key (Claude Code item / primaryApiKey)
            ↓ 没有时回退
OAuth tokens (Claude Code-credentials item / .credentials.json)
            ↓ 才轮到 refreshToken 自动 refresh
```

代码路径见 [claude-code-fork/src/utils/auth.ts:1051-1086](src/utils/auth.ts#L1051-L1086)：`getApiKeyFromConfigOrMacOSKeychain` 先查 `Claude Code` keychain item，命中就直接当作 API key 用，根本不会走到 OAuth tokens 那条分支。运行时实际 HTTP 请求的 `x-api-key` header 也是这把托管 key，不是 `accessToken`。

**对凭证池的影响**：

- 只轮换 `Claude Code-credentials`（OAuth tokens）**没用**——优先级被 `Claude Code`（托管 API key）压住了。
- 两种应对方式：
  1. **同时轮换两个 item**：每个池 entry 同时存 `(api_key_string, oauth_blob)` 二元组，写入时两个 `security add-generic-password` 都执行。这是默认推荐。
  2. **清掉托管 key，让 OAuth 接手**：`security delete-generic-password -s "Claude Code" -a "$USER"`，然后 Claude Code 启动时会拿 OAuth tokens 重新走流程（必要时调 `create_api_key` 再铸一把新的托管 key）。但这个回退路径会在重新铸造时把池 entry 弄脏，下次轮换前还得重新导出。

**怎么判断当前账号走哪条路径**：

```bash
# 有这个 item 说明走「托管 API key」路径
security find-generic-password -s "Claude Code" -a "$USER" -w 2>/dev/null | head -c 20

# 或者在 Linux/无 keychain 环境看 .claude.json
jq -r .primaryApiKey ~/.claude.json
```

如果上面两个命令任一返回了 `sk-ant-api03-...`，就是托管 API key 路径，必须一并轮换。

## 5. 凭证池轮换脚本骨架

池 entry 文件结构建议为一个 wrapper JSON，同时容纳两个 item 的内容：

```json
{
  "oauth":          { "claudeAiOauth": { ... }, "organizationUuid": "..." },
  "managed_api_key": "sk-ant-api03-..."
}
```

`managed_api_key` 在没有走过托管 key 路径的账号上可缺省（pool entry 里设为 `null`），脚本相应跳过即可。

```bash
#!/bin/bash
# rotate-claude.sh <pool-entry.json>
# 把指定 JSON 文件的两段内容分别写入 Claude Code 的两个 Keychain item。
set -euo pipefail
POOL="${1:?usage: rotate-claude.sh <pool-entry.json>}"
SVC_OAUTH="Claude Code-credentials"
SVC_APIKEY="Claude Code"
ACCT="$USER"

# 1. 校验字段齐全
python3 - <<PY
import json
d = json.load(open("$POOL"))
oauth = d["oauth"]
assert "claudeAiOauth" in oauth, "missing oauth.claudeAiOauth"
assert "organizationUuid" in oauth, "missing oauth.organizationUuid"
for k in ("accessToken", "refreshToken", "expiresAt", "scopes",
          "subscriptionType", "clientId"):
    assert k in oauth["claudeAiOauth"], f"missing oauth.claudeAiOauth.{k}"
mk = d.get("managed_api_key")
assert mk is None or (isinstance(mk, str) and mk.startswith("sk-ant-api03-")), \
    "managed_api_key must be null or sk-ant-api03-* string"
PY

# 2. 备份当前两个 item（带时间戳，权限 600）
BACKUP_DIR="$HOME/.claude/cred-backup"
mkdir -p "$BACKUP_DIR"
TS=$(date +%Y%m%d-%H%M%S)
BK_OAUTH="$BACKUP_DIR/$TS.oauth.json"
BK_APIKEY="$BACKUP_DIR/$TS.api-key.txt"
if security find-generic-password -s "$SVC_OAUTH"  -a "$ACCT" -w > "$BK_OAUTH"  2>/dev/null; then chmod 600 "$BK_OAUTH";  fi
if security find-generic-password -s "$SVC_APIKEY" -a "$ACCT" -w > "$BK_APIKEY" 2>/dev/null; then chmod 600 "$BK_APIKEY"; fi

# 3. 覆写 OAuth tokens item
NEW_OAUTH=$(jq -c .oauth "$POOL")
security add-generic-password -U -s "$SVC_OAUTH" -a "$ACCT" -w "$NEW_OAUTH"

# 4. 覆写或删除 托管 API key item
NEW_APIKEY=$(jq -r '.managed_api_key // empty' "$POOL")
if [[ -n "$NEW_APIKEY" ]]; then
    security add-generic-password -U -s "$SVC_APIKEY" -a "$ACCT" -w "$NEW_APIKEY"
else
    security delete-generic-password -s "$SVC_APIKEY" -a "$ACCT" 2>/dev/null || true
fi

echo "切换完成。如果 claude 已运行，请重启使其生效。"
```

生产化时建议把 `-w "$VALUE"` 改成 `security -i` interactive + `-X`（hex 编码）写入，避免明文进入 `ps`／命令行历史；可参考 [claude-code-fork/src/utils/auth.ts:1116-1121](src/utils/auth.ts#L1116-L1121) 的做法。

## 6. 初次种入凭证池的方法

凭证池里的 entry 必须是真实账号走完 OAuth 流程后从 keychain 里导出的，**两个 item 都要导**。流程：

```bash
# 对每个要纳入池的账号：
claude /logout
claude       # 浏览器走 OAuth 完成
# 立即导出（趁未刷新）：
ENTRY="pool/account-A.json"
OAUTH_JSON=$(security find-generic-password -s "Claude Code-credentials" -a "$USER" -w)
API_KEY=$(security find-generic-password -s "Claude Code" -a "$USER" -w 2>/dev/null || echo "")
jq -n \
  --argjson oauth "$OAUTH_JSON" \
  --arg apikey "$API_KEY" \
  '{oauth: $oauth, managed_api_key: ($apikey | select(length > 0))}' \
  > "$ENTRY"
chmod 600 "$ENTRY"
```

`managed_api_key` 字段：

- 没走过托管 key 路径的账号该字段为 `null`（OAuth-only），轮换时只覆写 OAuth tokens item，删掉 `Claude Code` item。
- 走过托管 key 路径的账号该字段是 `sk-ant-api03-...` 字符串，轮换时两个 item 都写。

注意：导出后第一次被使用就可能触发刷新并失效，参考 §4.1。

## 7. 安全注意事项

- 凭证池 JSON 里是真实 OAuth token，泄露等于账号被夺。文件权限必须 600，目录 700，不要进 git。
- 备份目录 `~/.claude/cred-backup/` 同样按 600/700 处理。
- 调试时谨慎使用 `security ... -w`（明文 token 会进入 shell history、终端 scrollback、tmux/screen 缓冲）。

## 8. 参考文档

- Claude Code Authentication: https://code.claude.com/docs/en/authentication
- Claude Code Settings: https://docs.claude.com/en/docs/claude-code/settings
- macOS `security(1)` man page: `man security`
