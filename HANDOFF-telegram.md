# Telegram 集成交接文档

> 生成时间：2026-07-21 · 分支 `feat/telegram-integration`（已推送）
> 上一环境：macOS + 本地 postgres(docker) + 本地 daemon，真机联调已全部通过

## 一、任务与已确认的需求决策

实现 Telegram AI Bot 集成，架构完全对齐既有飞书/Slack 的 channel engine
（`channel.Channel` / `engine.Supervisor`，PR #4512 / #4517）。

联调前逐项确认过的决策（不要推翻，除非产品要求变更）：

| 决策点 | 结论 |
|---|---|
| 收消息方式 | getUpdates long polling（Telegram 无 WebSocket；无需公网地址） |
| 流式回复 | 占位消息 + 节流 editMessageText（2.5s/次，429 按 retry_after 退避），EventChatDone 定稿 |
| 绑定粒度 | 一个 bot = 一个智能体（(workspace, agent, channel_type) 唯一，对齐飞书/Slack） |
| 会话映射 | 一个 chat = 一个持续会话；群 forum topic 按 chat:thread 隔离；/fresh 开新会话 |
| 群聊 | 支持；仅 @提及或回复 bot 触发；privacy mode 保持开启 |
| 访问控制 | 一次性绑定验证码（15 分钟、只存哈希），每个发送者都要绑定（注意：engine 强制 per-sender 身份，之前讨论的「绑群不绑人」与 engine 模型冲突，最终按 engine 语义实现） |
| 消息类型 | v1 纯文本；私聊里非文本回「暂不支持」提示，群里静默丢弃 |
| 离线/失败 | 立即回中文提示（离线/归档/失败三种文案在 sender.go / outbound.go） |
| 解绑语义 | 停 polling + 撤销 installation（行保留 audit），会话历史保留 |
| Token 存储 | secretbox 加密（`MULTICA_TELEGRAM_SECRET_KEY`），API 永不回显 |
| 多实例冲突 | getUpdates 409 → 明确日志「bot 已被另一实例轮询」+ Supervisor 退避 |
| UI 文案 | i18n 四语种（en/zh-Hans/ja/ko），locale parity 测试强制对齐 |

## 二、代码结构（两个提交）

### 提交 1 后端 `118f3fb4`
```
server/internal/integrations/telegram/
├── config.go            TypeTelegram、installConfig(app_id=bot数字id)、secretbox 解密、parseBotID
├── api.go               手写 Bot API 客户端(net/http)：getMe/getUpdates/sendMessage/editMessageText/sendChatAction；ErrConflict(409)、retryAfter(429)
├── telegram_channel.go  channel.Channel 实现：Connect=long poll 循环；Factory RegisterTelegram
├── inbound.go           Update→InboundMessage：群 @提及/回复触发、/fresh、MessageID="chat:msg"(去重键)、forum topic ThreadID
├── resolvers.go         engine.ResolverSet 五件套 + sendChatAction typing；会话路由 telegramSessionRouting
├── sender.go            出站发送：Markdown→HTML、4096 分片(3500 rune 留 HTML 余量)、HTML 被拒时纯文本降级；中文文案常量
├── markdown.go          轻量 Markdown→Telegram HTML（代码块/行内码/粗斜/链接/标题/列表）
├── outbound.go          事件总线订阅者：EventTaskMessage(流式节流编辑) + EventChatDone(定稿) + EventTaskFailed(失败提示)；ChatInputTaskID 判定 web 任务不回流
├── binding.go           绑定验证码服务（mint/redeem，事务性）
├── install.go           安装服务：getMe 校验→加密→persistInstall（reclaim 死行、冲突分类三种 sentinel）
└── telegram_test.go     单测：409/429/offset/群触发/fresh/HTML/会话路由/分片

server/internal/handler/telegram.go   list/install/revoke/redeem 四组端点
server/cmd/server/router.go           MULTICA_TELEGRAM_SECRET_KEY 门控 wiring + 路由
server/internal/handler/handler.go    TelegramInstall / TelegramBindingTokens 字段
server/pkg/protocol/events.go         telegram_installation:created/revoked
```

**零数据库迁移**：全部复用通用 `channel_*` 表（`channel_type='telegram'`），无 sqlc 变更，未改 engine 任何代码。

### 提交 2 前端 `c741e090`
```
packages/core/types/telegram.ts        类型（wire shape 对齐 handler）
packages/core/telegram/{queries,index}.ts  telegramKeys(wsId 第二段)/telegramInstallationsOptions
packages/core/api/client.ts            listTelegramInstallations/registerTelegramBot/deleteTelegramInstallation/redeemTelegramBindingToken
packages/core/realtime/use-realtime-sync.ts  telegram_installation:* 失效
packages/views/settings/components/telegram-tab.tsx(+test)  TelegramTab + TelegramAgentBindButton(粘贴 token 对话框/已连接徽章/解绑)
packages/views/settings/components/integrations-tab.tsx     挂 Telegram section
packages/views/agents/components/tabs/integrations-tab.tsx  智能体集成 tab 挂 Telegram section(admin-only)
packages/views/telegram/bind-page.tsx(+index)               绑定 redeem 页
apps/web/app/telegram/bind/page.tsx                          web 路由
packages/views/locales/{en,zh-Hans,ja,ko}/{settings,common}.json  telegram.* / telegram_bind.* 全量文案
```

## 三、新机器环境搭建（真机联调复现步骤）

```bash
git clone git@github.com:leonzone/multica.git && cd multica
git checkout feat/telegram-integration

# 1. env：.env 里加（32 字节 base64，自己生成新的即可）
#    MULTICA_TELEGRAM_SECRET_KEY=$(python3 -c "import base64,os;print(base64.b64encode(os.urandom(32)).decode())")
cp .env.example .env  # 再补上面这行

# 2. 网络：api.telegram.org 在大陆被墙。后端进程需要代理：
#    HTTPS_PROXY=http://127.0.0.1:<代理端口> NO_PROXY=localhost,127.0.0.1 make server
#    Go 客户端用默认 Transport，自动读 HTTPS_PROXY，无需改代码。

# 3. 起库+迁移+服务
make setup && HTTPS_PROXY=... NO_PROXY=localhost,127.0.0.1 make server
# 日志应出现: "telegram integration enabled (per-installation long polling)"

# 4. daemon（要真实回复必须用编译产物，不能 make daemon 的 go run —
#    go run 临时二进制被回收后 daemon fork 辅助进程会失败，教训!）
cd server && go build -o /tmp/multica-cli ./cmd/multica && cd ..
mkdir -p ~/.multica/profiles/local   # config.json: server_url/app_url/workspace_id/token
/tmp/multica-cli daemon restart --profile local

# 5. 账号/JWT（开发环境无邮件，验证码从库里读）
curl -X POST localhost:8080/auth/send-code -d '{"email":"you@test.local"}' -H 'Content-Type: application/json'
# psql: SELECT code FROM verification_code WHERE email='you@test.local' ...
curl -X POST localhost:8080/auth/verify-code -d '{"email":"...","code":"..."}' ...  # 拿 token

# 6. 绑定 bot（或直接用 web UI 的智能体集成 tab）
curl -X POST "localhost:8080/api/workspaces/$WS/telegram/install?agent_id=$AG" \
  -H "Authorization: Bearer $TOKEN" -d '{"bot_token":"<BotFather token>"}' -H 'Content-Type: application/json'
```

联调判定技巧：
- 轮询是否活着：手动 curl getUpdates，收到 **409 = 服务器正在轮询**（正常）。
- 首条消息 → needs_binding → bot 回绑定链接；本地没起 web 时可直接
  `POST /api/telegram/binding/redeem {"token":"..."}` 用 JWT 代兑。
- 观察日志：`channel router: dispatch outcome channel_type=telegram outcome=...`

## 四、验证状态

- `go build/vet/test ./...` 全绿；`pnpm typecheck / test / lint` 全绿
  （desktop 有 1 个与本改动无关的预存失败：electron 二进制未装）
- 真机联调已通过全链路：绑定→409 单消费者→needs_binding→验证码兑换→
  ingested→任务入队→daemon(claude runtime) 执行→流式编辑回复→定稿；
  失败通知路径也真实触发过一次。

## 五、遗留事项 / 下一步

1. **安全（必须做）**：联调用的 bot token 已在聊天记录中明文出现过，
   上生产前在 @BotFather `/revoke`，重新绑定即可（历史会话不丢）。
2. PR 未创建：https://github.com/leonzone/multica/pull/new/feat/telegram-integration
3. docs 站缺 `telegram-bot-integration` 指南页（连接对话框里的链接已预留，四语种）。
4. `telegram` 未加入 reserved_slugs（`slack`/`lark` 也没加，属既有先例；
   若要修，按 CLAUDE.md 流程改 json + `pnpm generate:reserved-slugs` 一起提交）。
5. 可选增强（本期明确不做）：media 消息（图片/语音）、/issue 命令的
   Telegram slash command 版、频道(channel)支持、代理配置 UI 化。
6. 已知取舍：engine 要求 per-sender 绑定，群里每个成员首次触发都会收到
   绑定链接（与飞书语义一致）；流式粒度是 EventTaskMessage 的消息级
   （非 token 级），短回复可能一次成型——这是总线现状，飞书也如此。

## 六、旧环境清理（原机器）

```bash
make stop                                    # 停 server
/tmp/multica-cli daemon stop --profile local # 停 daemon
docker stop $(docker ps -qf name=postgres)   # 可选：停库
# ~/.multica/profiles/local/ 是联调建的本地 profile，可删
# ~/.multica/config.json 是你的生产配置，未动过
```
