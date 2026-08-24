# Togather 客服迁移部署手册（openclaw → WeKnora）

把 openclaw 上的 `togather-cs` WhatsApp 客服（Togather Cafe，马来西亚 13 家门店，三语 EN/中文/BM）迁移到 WeKnora 的完整步骤。适用于全新服务器部署；本地开发机已按此流程跑通并端到端验证（2026-08-24）。

## 架构对照

| openclaw 组件 | WeKnora 对应 | 位置 |
|---|---|---|
| agent 定义（openclaw.json） | CustomAgent「Togather 客服小灵」 | setup.py 自动创建 |
| SOUL/IDENTITY/AGENTS/HANDOFF.md（行为规则） | agent 的自定义 System Prompt | `integrations/togather-cs/system_prompt.md` |
| MENU/MENU_CN/STORES/FAQ/POLICY/MEMBERSHIP/TOP_10/BRAND.md（知识） | 知识库 72 个文档（菜单按分类拆、门店按分店拆） | setup.py 自动导入 |
| togather-distance 插件（查最近门店） | MCP 服务（Streamable HTTP，独立容器） | `integrations/togather-distance/` |
| WhatsApp 个人号（Baileys 扫码） | WeKnora IM 渠道（whatsmeow 扫码，同为 WhatsApp Web 协议） | UI 创建绑定 |
| 转人工（只给电话号码） | WeKnora 原生：关键词自动转人工 + 运营收件箱 + 人工接管 | 已内置 |

本次迁移涉及的代码已全部在仓库里（commit `e23415cc`、`da506b0a`），新服务器只需 `git clone` 本 fork，不需要重做代码改动：

- `internal/im/whatsapp/adapter.go` — 客人发的 WhatsApp 定位 pin 转成 `Shared location: lat,lng` 文本，agent 可直接喂给距离工具（带测试）。
- `integrations/togather-distance/` — 零依赖 MCP server（Python 标准库；stdio + HTTP 两种模式）、13 家门店坐标 `stores.json`、马来西亚调优的地理编码（Nominatim 多策略 + Google Maps 短链解析 + bbox 校验）。
- `integrations/togather-cs/` — 改写后的 system prompt + 幂等安装脚本 `setup.py`。
- `docker-compose.yml` — 新增 `togather-distance` 常驻服务；app 挂载 `./integrations`（只读）。

## 前置条件

1. Docker + Docker Compose（Linux 需 Docker 20.10+，compose 里 `host.docker.internal:host-gateway` 才生效）。
2. 本 fork 代码：`git clone git@github.com:Ailein/WeKnora.git`（或在已有部署上 `git pull`）。
3. openclaw 的知识源目录 `~/.openclaw/workspace-togather-cs/`（setup.py 从这里读 md 文件；正式服务器上已有）。
4. 模型：
   - **对话模型**：MiniMax（或任意 OpenAI 兼容 API），需要 API key。
   - **Embedding：强烈建议本地 Ollama + bge-m3**。WeKnoraCloud 免费 embedding 额度撑不住 72 个文档（本地部署时就是因“额度已用完”全量失败后切换的）。bge-m3 多语种，覆盖 EN/中文/BM。
   - Rerank：可选（有就配）。

安装 Ollama 并拉模型（宿主机上）：

```bash
curl -fsSL https://ollama.com/install.sh | sh   # Linux；mac 用 brew install ollama
ollama pull bge-m3
curl -s http://127.0.0.1:11434/api/version      # 确认服务在 11434
```

## 部署步骤

### 1. 配置 .env

在 repo 根目录 `cp .env.example .env` 后，重点确认这几项（其余按常规 WeKnora 部署）：

```bash
# app 容器访问宿主机 Ollama（compose 的 app 服务已带 extra_hosts host-gateway）
OLLAMA_BASE_URL=http://host.docker.internal:11434

# SSRF 白名单：逗号分隔。必须包含 togather-distance（MCP 容器主机名），
# 以及所有模型 API 域名——尤其当服务器 DNS 走 fake-ip 代理（Clash 等）时，
# 外部域名会被解析到 198.18.x.x 保留段而被 SSRF 校验拦截。
SSRF_WHITELIST=togather-distance,api.minimaxi.com
```

### 2. 启动服务

```bash
docker compose up -d --build   # 首次；包含新增的 togather-distance 容器
docker compose ps              # 等 app healthy
```

验证距离服务（从 app 容器内）：

```bash
docker exec WeKnora-app sh -c "wget -q -O- --post-data='{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}' \
  --header='Content-Type: application/json' http://togather-distance:9310/mcp"
# 期望: {"jsonrpc": "2.0", "id": 1, "result": {}}
```

### 3. UI 初始化：账号 + 模型

浏览器打开 WeKnora → 注册/登录 → 设置 → 模型配置，**先把模型配好再跑脚本**（知识库创建后 embedding 模型不可更换，选错只能删库重导）：

1. 对话模型：MiniMax（base_url `https://api.minimaxi.com/v1`，填 API key）。
2. Embedding 模型：来源选 **本地 (Ollama)**，模型名 `bge-m3`，维度 1024。
3. Rerank：可选。

### 4. 生成 API Key

设置页的 API Key 管理里创建一个 **full access** key（`sk-` 开头，只显示一次）。没有 UI 入口时用 SQL 兜底：

```bash
TOKEN="sk-$(python3 -c "import secrets,base64;print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip('='))")"
HASH=$(python3 -c "import hashlib;print(hashlib.sha256('$TOKEN'.encode()).hexdigest())")
# tenant_id 换成实际工作区 ID（tenants 表）
docker exec WeKnora-postgres psql -U postgres -d WeKnora -c \
  "insert into tenant_api_keys (tenant_id,name,key_hash,api_key,full_access,knowledge_base_ids,capabilities,scope_type)
   values (10000,'togather-setup','$HASH','$TOKEN',true,'[]','[]','tenant');"
echo "$TOKEN"   # 保存好
```

### 5. 一键安装（知识库 + MCP + Agent）

```bash
cd integrations/togather-cs
python3 setup.py --api-key sk-xxxx \
  --base-url http://localhost:8080 \
  --workspace ~/.openclaw/workspace-togather-cs
```

脚本幂等（按名字复用、按文件名跳过已导入文档），做四件事：

1. 建「Togather 客服知识库」（md 用本地解析引擎；chunk 2000 保证一个菜单分类不被切碎；embedding 自动优先选名字含 `bge` 的本地模型）。
2. 拆分并导入 72 个知识文档（MENU/MENU_CN 按分类、STORES 按门店、其余整档）。
3. 注册 MCP 服务 `togather-distance`（http-streamable → `http://togather-distance:9310/mcp`）并做连接测试，成功会打印发现的工具。
4. 创建/更新 agent「Togather 客服小灵」：smart-reasoning、自定义 system prompt、绑知识库、勾选 MCP、citation 关闭、工具为 thinking + 4 个知识检索工具。

等 1-2 分钟解析完成，确认全部 completed：

```bash
curl -s -H "X-API-Key: sk-xxxx" \
  "http://localhost:8080/api/v1/knowledge-bases/<脚本输出的KB_ID>/knowledge?page=1&page_size=100" \
  | python3 -c "import json,sys;from collections import Counter;print(Counter(i['parse_status'] for i in json.load(sys.stdin)['data']))"
```

### 6. 网页对话验证

UI → 智能体 → Togather 客服小灵，过一遍验收用例：

| 用例 | 期望 |
|---|---|
| `any pizza?` | 列出全部 4 款披萨 + 价格 + 菜单 URL |
| `你们有什么披萨？` | 中文菜名（MENU_CN）、价格一致 |
| `I am at Bandar Puchong Jaya 47170, which outlet is nearest?` | 调 `mcp_togather_distance_togather_nearest_store`，答 Puchong ~527m + 地址 + 地图链接 |
| `nak menu` (BM) | BM 回复，菜名保持英文 |
| `我要投诉，食物有问题` | 道歉 + 收集分店/时间/经过，给投诉热线 017-9887981 |
| 问未记载事项（如包厢最低消费） | 不编造，引导致电门店 |

### 7. 绑定 WhatsApp（先测试号）

UI → 收件箱/IM 渠道 → 新建 WhatsApp 渠道 → 绑定「Togather 客服小灵」→ 用**测试号**扫码配对 → 真机发消息验证：文字、中文、定位 pin（期望 agent 直接算出最近门店）、投诉话术。转人工与收件箱按 WeKnora 原生功能配置（关键词触发在渠道设置里）。

### 8. 正式号切换（关键）

WeKnora 和 openclaw 都是 WhatsApp Web 协议的 companion 设备，**同一个号两边同时在线会各自回复一遍**，必须先停一边：

1. openclaw 服务器上停用账号：`openclaw.json` → `channels.whatsapp.accounts.togather-cs.enabled` 改为 `false`，重启 openclaw（先备份 openclaw.json）。
2. WeKnora 渠道用正式号扫码配对。
3. 观察 1-2 天（收件箱里盯真实会话质量）。
4. **回滚**：WeKnora 渠道停用 → openclaw 里 enabled 改回 `true` 重启即可，openclaw 侧配置和会话数据都还在。

## 踩坑记录（部署前先读）

1. **stdio MCP 被硬禁用**：WeKnora 出于命令注入风险禁 stdio transport，报 "stdio transport is disabled for security reasons"。所以距离服务走独立容器 + Streamable HTTP，别尝试 stdio。
2. **SSRF 白名单**：MCP 容器主机名、所有模型 API 域名都要进 `SSRF_WHITELIST`。典型症状：`SSRF validation failed: resolves to restricted IP 198.18.x.x`（服务器 DNS 被 fake-ip 代理接管）或 `direct IP address access is not allowed`。改完 `.env` 必须 `docker compose up -d app` 重建（`restart` 不重读 env_file）。
3. **重建 app 后前端 502**：app 容器重建 IP 会变，frontend 的 nginx 缓存旧 IP，页面报"加载模型列表失败"。执行 `docker compose restart frontend`。
4. **embedding 模型建库后不可换**：换模型只能删知识库重导（setup.py 重跑即可），所以第 3 步先配好 bge-m3。
5. **WeKnoraCloud 免费 embedding 额度**：72 个文档会打爆（"额度已用完"），别用它做这个库的 embedding。
6. **fork 合并 upstream 后的迁移编号**：如果 merge 导致 `migrations/versioned/` 重编号，upstream 插入的新迁移可能被 `schema_migrations` 版本号跳过（本地曾漏掉 `000085 messages.usage`，agent 对话直接 500 `column "usage" does not exist`）。merge 后核对 `select version from schema_migrations` 与文件号，缺的手动 `psql < up.sql` 补（全新部署不受影响）。
7. **Nominatim 限流**：免费地理编码约 1 req/s。客服量大后在 `integrations/togather-distance/nearest.py` 换成 Google Geocoding / Mapbox（代码已留好扩展点）。

## 日常运维

- **改门店**（新店/换址/电话）：改 `integrations/togather-distance/stores.json`（含 lat/lng），MCP 每次调用现读，**不用重启**；同时更新 openclaw workspace 的 `STORES.md` 并重导对应知识文档。
- **改菜单/政策知识**：改 `~/.openclaw/workspace-togather-cs/*.md` 后，在 UI 删掉对应旧文档再重跑 `setup.py`（脚本按文件名跳过已存在文档，不删不会更新）。
- **改人格/规则**：改 `integrations/togather-cs/system_prompt.md` 重跑 `setup.py`（agent 是按名字更新的，直接生效）。

## 已知未迁移项

- **语音消息转写**：需在 WeKnora 配一个 ASR 模型后到 agent/渠道设置里启用（openclaw 侧原来用 agent 的 ASR）。
- **会员 App QR 码图片**：两张 QR 图未迁，system prompt 已按"发下载链接"兜底；如需发图，把 `workspace-togather-cs/assets/` 两张图配置到 IM 渠道的素材里再更新提示词。
- CRM 收集（openclaw 的 nova-crm-collector）：WeKnora 侧未对接，用收件箱 + IM 分析看板替代。
