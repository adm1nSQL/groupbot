# V2Board Telegram 群组管理机器人

基于 Go 开发的 Telegram 群组管理机器人，通过读取 V2Board 数据库验证用户套餐状态，自动管理群组成员。

## 功能

- **邀请进群**：用户先在私聊绑定邮箱和密码，账户存在且套餐有效后，获得对应群的一次性邀请链接
- **入群拦截**：没有有效绑定的用户，即使用其他链接进群，也会被立即移出
- **发言兜底**：已在群里但绑定失效的成员，发言时会被移出
- **LRU 缓存**：每群独立的已验证用户内存缓存，正常发言 0 次 DB 查询，性能与活跃度无关
- **套餐检查**：按 `v2_plan.reset_traffic_method` 区分套餐类型：仅明确为 `2`（不重置/一次性）的套餐检查流量耗尽；其他套餐流量耗尽不移出群组，过期满三天才移出。一次性套餐若设置了到期时间，仍在到期后立即失效
- **定时巡检**：自动检查用户的套餐状态，失效用户自动踢出并在群内合并通知，附带原因（周期套餐过期满三天 / 一次性套餐流量已用尽或到期 / 账户已被封禁）
- **官网关键词**：在受管理群中只发送“官网”时，机器人回复带官网链接按钮的消息（需配置 `website_url`）
- **唯一绑定**：同一面板邮箱只能绑定一个 Telegram 账号，防止账号共享
- **强制改绑** `/forcebind`：旧 Telegram 号丢失/无法访问时，凭邮箱+密码即可把绑定迁移到新号
- **套餐详情**：管理员查询和用户状态可显示套餐名称、到期时间等完整信息
- **白名单**：支持配置例外用户，不受规则约束
- **多群多库**：一个 Bot 实例可管理多个群组，各自对应不同的 V2Board 数据库
- **数据库只读**：Bot 仅需 SELECT 权限，绑定关系完全存储在本地文件
- **历史绑定迁移**：首次启动自动把数据库中已存在的 `telegram_id` 绑定一次性导入到本地，之后以本地为权威源，面板中后续的修改不再影响 Bot
- **防永久封禁**：踢出时使用 Ban+Unban 组合，并设置兜底超时，避免用户残留在封禁列表

## 进群流程

群需要设为私密群，不要把群的永久邀请链接公开。用户只能通过机器人拿到一次性链接。

```
私聊机器人 → /bind 邮箱 密码
                ↓
        邮箱或密码错误 → 拒绝
        账户被封禁 / 无套餐 / 周期套餐过期满三天 / 一次性套餐到期或流量耗尽 → 绑定成功，但不发链接
        账户与套餐有效 → 发送一次性邀请链接（默认 30 分钟、仅 1 人可用）
                ↓
        用户点链接进群 → 再次核对绑定和套餐 → 通过则欢迎
        未绑定或套餐无效的人进群 → 立即移出，并提示先去私聊绑定
```

已经绑定过的用户，套餐仍然有效时，可以发送 `/invite` 再领一次链接。重新领取会使上一条未使用的链接失效。

### 已在群内的成员

```
群内任意发言 → 命中已验证缓存？ ─是→ 放行，0 次 DB 查询
                    ↓ 否
              白名单或群管理员 → 放行
              本地绑定 + 按 email 查 DB 套餐有效 → 写入缓存放行
              否则 → 删除消息并移出群组
```

缓存是按群独立的 LRU，容量由 `cache_size` 控制（默认 5000），重启后通过自然发言重建，不写盘。

### 绑定规则

- 每个面板邮箱**只能绑定一个 Telegram 账号**
- 正常情况下，如需转移绑定，原账号执行 `/unbind` 解绑后新账号再 `/bind`。解绑会把该账号移出对应群组
- 旧账号无法访问时，新账号在私聊里执行 `/forcebind 邮箱 密码`。密码验证通过后，旧 Telegram 账号的绑定会被解除并移出群组
- 绑定时会在已配置的数据库里查找该邮箱，邀请链接只发给使用同一数据库的群组

## 命令列表

| 命令 | 说明 | 权限 |
|------|------|------|
| `/start` | 显示帮助信息 | 所有人 |
| `/bind 邮箱 密码` | 绑定面板账户；套餐有效时附带一次性邀请链接 | 所有人 |
| `/invite` | 重新获取一次性邀请链接 | 所有人 |
| `/forcebind 邮箱 密码` | 强制接管该邮箱的绑定（旧 TG 号失联时使用） | 所有人 |
| `/unbind` | 解除绑定并移出对应群组 | 所有人 |
| `/status` | 查看套餐状态 | 所有人 |
| `/check` | 手动触发巡检 | 管理员 |
| `/cha TelegramID` | 查询指定用户信息 | 管理员 |

> 除 `/status` 外，所有命令在群里使用都会被静默拦截并提示到私聊，避免命令噪音和密码外泄。

### 数据流向（重要）

- 启动时，Bot 会从所有数据库 **一次性导入** `v2_user.telegram_id` 字段到本地 `bindings.json`，导入后写入 `bindings.json.imported` 标记文件
- 此后 Bot **完全不再读取** `v2_user.telegram_id` 字段，所有"谁绑定了哪个邮箱"完全以本地为准
- 这意味着：
  - 用户在 V2Board 面板里直接修改 `telegram_id` 不会被 Bot 感知
  - 所有绑定/改绑/解绑都通过 Bot 命令完成
  - 数据库**完全不需要写权限**，Bot 也不会改 `v2_user.telegram_id`
- 删除 `bindings.json.imported` 标记文件并重启 Bot 可触发再次种子导入（仅补充本地不存在的条目，不会覆盖）

## 部署

### 1. 下载

从 [Releases](../../releases) 页面下载对应平台的二进制文件。

### 2. 配置

复制示例配置到工作目录，编辑 `config.yaml`：

```bash
cp configs/config.example.yaml config.yaml
```

完整示例：

```yaml
telegram:
  bot_token: "YOUR_BOT_TOKEN"
  admin_ids:
    - 123456789

  # Bot 介绍卡片与命令菜单（可选，启动时自动同步到 Telegram）
  profile:
    description: |
      🤖 V2Board 群组管理机器人

      用于自动管理群组成员，只有拥有有效套餐的用户才能加入。
    short_description: "V2Board 群组管理助手"
    commands:
      - command: "start"
        description: "开始使用"
      - command: "bind"
        description: "绑定面板账户"
      - command: "invite"
        description: "获取一次性邀请链接"
      - command: "unbind"
        description: "解除绑定"
      - command: "status"
        description: "查看套餐状态"

groups:
  - chat_id: -1001234567890
    name: "用户群"
    database:
      host: "127.0.0.1"
      port: 3306
      user: "bot_readonly"
      password: "your_password"
      dbname: "v2board"
    exempt_users:
      - 111111111

website_url: "https://example.com"
invite_ttl: 1800
check_interval: 300
```

### 3. 创建数据库只读账号

Bot 需要读取 `v2_user`（用户信息）和 `v2_plan`（套餐名称）两张表：

```sql
CREATE USER 'bot_readonly'@'127.0.0.1' IDENTIFIED BY 'your_password';
GRANT SELECT ON v2board.v2_user TO 'bot_readonly'@'127.0.0.1';
GRANT SELECT ON v2board.v2_plan TO 'bot_readonly'@'127.0.0.1';
FLUSH PRIVILEGES;
```

### 4. Telegram 设置

1. 通过 [@BotFather](https://t.me/BotFather) 创建 Bot 并获取 Token
2. 将 Bot 添加到群组并设为管理员
3. 赋予权限：**邀请用户、封禁用户、删除消息**
4. 把群设为私密群，并撤销已经公开的永久邀请链接。用户应只使用机器人发出的一次性链接
5. **重要**：通过 [@BotFather](https://t.me/BotFather) 的 `/setprivacy` 命令把 Bot 的 Privacy Mode 设为 **Disabled**，否则 Bot 收不到群里普通成员的消息，无法在发言时移出未授权成员

### 5. 运行

```bash
chmod +x v2board-tg-bot
./v2board-tg-bot -config config.yaml -bindings bindings.json
```

参数说明：

- `-config` 配置文件路径，默认 `config.yaml`
- `-bindings` 本地绑定存储文件路径，默认 `bindings.json`

### 6. 使用 Supervisor 守护进程（宝塔面板）

在宝塔面板安装 Supervisor 管理器，添加守护进程：

- 名称：`tg-bot`
- 运行目录：`/opt/tg-bot`
- 启动命令：`/opt/tg-bot/v2board-tg-bot -config /opt/tg-bot/config.yaml`

## 配置说明

| 字段 | 说明 |
|------|------|
| `telegram.bot_token` | Telegram Bot Token |
| `telegram.admin_ids` | 管理员 Telegram ID 列表 |
| `telegram.profile.description` | 用户首次打开对话时的介绍卡片（≤ 512 字符） |
| `telegram.profile.short_description` | Bot 个人资料页简介（≤ 120 字符） |
| `telegram.profile.commands` | 命令菜单列表（用户点 `/` 按钮时显示） |
| `groups[].chat_id` | 群组 ID（负数） |
| `groups[].name` | 可选，邀请消息里显示的群名称 |
| `groups[].database` | V2Board 数据库连接信息 |
| `groups[].exempt_users` | 白名单用户 Telegram ID 列表 |
| `invite_ttl` | 一次性邀请链接有效期（秒），默认 1800 |
| `check_interval` | 定时巡检间隔（秒），默认 300；限时套餐过期三天后移出群组 |
| `website_url` | 官网按钮链接（http/https）；留空时不启用“官网”自动回复 |
| `cache_size` | 每群已验证用户的 LRU 缓存上限，默认 5000 |

`profile` 字段会在启动时同步到 Telegram，同时 `/start` 命令的响应会使用这些内容自动生成。头像和介绍图片仍需通过 [@BotFather](https://t.me/BotFather) 手动设置。

## 项目结构

```
v2board-tg-bot/
├── cmd/bot/                    # 程序入口
│   └── main.go
├── internal/                   # 内部包，不对外公开
│   ├── config/                 # 配置加载与类型定义
│   ├── db/                     # 数据库客户端与密码校验
│   ├── binding/                # 本地绑定存储 (JSON)
│   └── bot/                    # Telegram 业务逻辑
│       ├── handler.go          #   命令与事件处理
│       ├── checker.go          #   定时巡检
│       └── helpers.go          #   格式化工具
├── configs/
│   └── config.example.yaml     # 示例配置
├── .github/workflows/build.yml # CI 构建与发布
├── go.mod
└── README.md
```

本地开发构建：

```bash
go build -o v2board-tg-bot ./cmd/bot
```

GitHub Actions 打包：推送到 `master`/`main` 会自动运行，或在仓库 **Actions → Build and Release → Run workflow** 手动触发。运行完成后，在该次运行的 **Artifacts** 中下载三个平台的程序。仅当推送 `v*` 标签（如 `v1.0.0`）时，才会额外创建 GitHub Release；普通分支推送和手动运行不会创建 Release。测试或构建失败时不会发布。

## 技术栈

- Go 1.23+
- [telebot v3](https://github.com/tucnak/telebot) - Telegram Bot 框架
- [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) - MySQL 驱动
- [yaml.v3](https://github.com/go-yaml/yaml) - YAML 配置解析

## License

MIT
