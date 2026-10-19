package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/binding"
	"v2board-tg-bot/internal/config"
	"v2board-tg-bot/internal/db"
)

// Handler 处理所有 Telegram 事件与命令
type Handler struct {
	bot       *tele.Bot
	config    *config.Config
	dbClients map[int64]*db.Client
	groups    map[int64]*config.GroupConfig
	bindings  *binding.Store
	check     func()

	mu sync.Mutex
	// issuedLinks: telegram_id -> chat_id -> 尚未作废的一次性邀请链接
	issuedLinks map[int64]map[int64]string
	// joinSeen 防止 OnUserJoined 与 chat_member 对同一次入群重复处理
	joinSeen map[string]time.Time

	// verifyCache: chatID -> LRU(userID -> struct{})
	// 已通过验证的用户白名单缓存，避免每条群消息都查 DB
	verifyCache map[int64]*lru.Cache[int64, struct{}]
}

// NewHandler 构造一个 Handler
func NewHandler(b *tele.Bot, cfg *config.Config, dbClients map[int64]*db.Client, bindings *binding.Store) *Handler {
	groups := make(map[int64]*config.GroupConfig)
	verifyCache := make(map[int64]*lru.Cache[int64, struct{}])
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		groups[g.ChatID] = g
		c, err := lru.New[int64, struct{}](cfg.CacheSize)
		if err != nil {
			// LRU 创建失败时降级为最小容量，保证不 panic
			c, _ = lru.New[int64, struct{}](128)
		}
		verifyCache[g.ChatID] = c
	}
	return &Handler{
		bot:         b,
		config:      cfg,
		dbClients:   dbClients,
		groups:      groups,
		bindings:    bindings,
		issuedLinks: make(map[int64]map[int64]string),
		joinSeen:    make(map[string]time.Time),
		verifyCache: verifyCache,
	}
}

// SetChecker 注册手动巡检入口。
func (h *Handler) SetChecker(run func()) {
	h.check = run
}

// Register 注册所有命令、事件处理器与中间件
func (h *Handler) Register() {
	// 全局中间件：未绑定或套餐失效的成员发言时移出群组
	h.bot.Use(h.groupVerifyMiddleware())

	h.bot.Handle(tele.OnUserJoined, h.onUserJoined)
	h.bot.Handle(tele.OnChatMember, h.onChatMemberUpdate)
	h.bot.Handle("/start", h.onStart)
	h.bot.Handle("/bind", h.onBind)
	h.bot.Handle("/forcebind", h.onForceBind)
	h.bot.Handle("/invite", h.onInvite)
	h.bot.Handle("/unbind", h.onUnbind)
	h.bot.Handle("/status", h.onStatus)
	h.bot.Handle("/check", h.onCheck)
	h.bot.Handle("/cha", h.onCha)

	// 普通群文本经过身份校验后，才允许触发官网关键词回复。
	h.bot.Handle(tele.OnText, h.onGroupText)

	// 给其他可能在群里出现的消息事件注册 noop，
	// 让中间件能拿到事件做验证；命令则走各自 handler。
	noop := func(c tele.Context) error { return nil }
	for _, ev := range []string{
		tele.OnPhoto, tele.OnVideo, tele.OnAnimation,
		tele.OnAudio, tele.OnVoice, tele.OnVideoNote, tele.OnDocument,
		tele.OnSticker, tele.OnContact, tele.OnLocation, tele.OnVenue,
		tele.OnDice, tele.OnPoll, tele.OnMedia,
	} {
		h.bot.Handle(ev, noop)
	}
}

// onGroupText 仅在受管理群内、消息全文恰好是“官网”时回复链接按钮。
func (h *Handler) onGroupText(c tele.Context) error {
	if !isGroupChat(c) || h.groups[c.Chat().ID] == nil || c.Message() == nil ||
		c.Sender() == nil || c.Sender().IsBot ||
		c.Message().Text != "官网" || h.config.WebsiteURL == "" {
		return nil
	}
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.URL("官网", h.config.WebsiteURL)))
	return c.Send("点击下方按钮访问官网：", markup)
}

// isGroupChat 判断当前是否在群组中
func isGroupChat(c tele.Context) bool {
	return c.Chat() != nil &&
		(c.Chat().Type == tele.ChatGroup || c.Chat().Type == tele.ChatSuperGroup)
}

// rejectInGroup 群内触发非 /status 命令时：删除命令消息，发临时提示
func (h *Handler) rejectInGroup(c tele.Context) error {
	_ = h.bot.Delete(c.Message())
	tip, _ := h.bot.Send(c.Chat(), "💡 该命令仅限私聊使用，请私聊我执行。")
	if tip != nil {
		go func() {
			time.Sleep(5 * time.Second)
			_ = h.bot.Delete(tip)
		}()
	}
	return nil
}

// Invalidate 让缓存中的 verified 状态失效（巡检踢人时调用）
func (h *Handler) Invalidate(chatID, userID int64) {
	if c, ok := h.verifyCache[chatID]; ok {
		c.Remove(userID)
	}
}

// markVerified 把用户加入指定群的"已验证"缓存
func (h *Handler) markVerified(chatID, userID int64) {
	if c, ok := h.verifyCache[chatID]; ok {
		c.Add(userID, struct{}{})
	}
}

// isCachedVerified 命中说明该用户最近已被验证有效，可直接放行
func (h *Handler) isCachedVerified(chatID, userID int64) bool {
	c, ok := h.verifyCache[chatID]
	if !ok {
		return false
	}
	_, hit := c.Get(userID)
	return hit
}

// groupVerifyMiddleware 群消息中间件：保证发言者已绑定且套餐有效，否则移出群组
func (h *Handler) groupVerifyMiddleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			chat := c.Chat()
			if chat == nil || (chat.Type != tele.ChatGroup && chat.Type != tele.ChatSuperGroup) {
				return next(c)
			}
			group, ok := h.groups[chat.ID]
			if !ok {
				return next(c)
			}
			user := c.Sender()
			if user == nil || user.IsBot {
				return next(c)
			}
			msg := c.Message()
			// service messages（加群/退群/置顶等）不走验证逻辑
			if msg != nil && (msg.UserJoined != nil || msg.UserLeft != nil ||
				msg.NewGroupTitle != "" || msg.NewGroupPhoto != nil ||
				msg.GroupPhotoDeleted || msg.PinnedMessage != nil ||
				msg.GroupCreated || msg.MigrateTo != 0 || msg.MigrateFrom != 0) {
				return next(c)
			}

			slog.Debug("收到群消息",
				"chat_id", chat.ID, "user_id", user.ID,
				"username", user.Username, "text_len", len(c.Text()),
			)

			if h.ensureVerified(chat, user, group, msg) {
				return next(c)
			}
			// 未通过：消息已删除 / 验证流程已启动，本次事件不再继续
			return nil
		}
	}
}

// ensureVerified 检查发言者是否已绑定且套餐有效。未通过则删除消息并移出群组。
func (h *Handler) ensureVerified(chat *tele.Chat, user *tele.User, group *config.GroupConfig, msg *tele.Message) bool {
	chatID := chat.ID
	userID := user.ID

	if group.IsExempt(userID) || h.isGroupAdmin(chatID, userID) {
		return true
	}

	if h.isCachedVerified(chatID, userID) {
		return true
	}

	valid, err := h.accountValid(chatID, userID)
	if err != nil {
		slog.Error("验证群成员时数据库查询失败，暂不移出", "chat_id", chatID, "user_id", userID, "error", err)
		return true
	}
	if valid {
		h.markVerified(chatID, userID)
		return true
	}

	slog.Info("未授权用户在群组发言，移出群组",
		"chat_id", chatID, "user_id", userID, "username", user.Username,
	)
	if msg != nil {
		_ = h.bot.Delete(msg)
	}
	h.removeUnauthorized(chat, user, "未绑定有效账户")
	return false
}

// onChatMemberUpdate 通过 chat_member 更新检测用户加入（兜底 OnUserJoined 失效的情况）
func (h *Handler) onChatMemberUpdate(c tele.Context) error {
	cm := c.ChatMember()
	if cm == nil || cm.NewChatMember == nil || cm.OldChatMember == nil {
		return nil
	}

	newRole := cm.NewChatMember.Role
	oldRole := cm.OldChatMember.Role
	user := cm.NewChatMember.User
	chat := cm.Chat

	if user == nil || chat == nil || user.IsBot {
		return nil
	}

	isJoin := (oldRole == tele.Left || oldRole == tele.Kicked) &&
		(newRole == tele.Member || newRole == tele.Administrator || newRole == tele.Creator)
	if !isJoin {
		return nil
	}

	slog.Info("检测到用户加入(chat_member)",
		"chat_id", chat.ID, "user_id", user.ID,
		"username", user.Username, "old_role", oldRole, "new_role", newRole,
	)
	h.handleJoin(chat, user)
	return nil
}

// onUserJoined 用户加入群组时触发（service message 方式，部分群组可能不触发）
func (h *Handler) onUserJoined(c tele.Context) error {
	user := c.Sender()
	if msg := c.Message(); msg != nil && msg.UserJoined != nil {
		user = msg.UserJoined
	}
	if user == nil || user.IsBot || c.Chat() == nil {
		return nil
	}

	slog.Info("新用户加入群组",
		"chat_id", c.Chat().ID,
		"user_id", user.ID,
		"username", user.Username,
	)
	h.handleJoin(c.Chat(), user)
	return nil
}

// handleJoin 入群只接受白名单、管理员，或已绑定且套餐有效的用户。
func (h *Handler) handleJoin(chat *tele.Chat, user *tele.User) {
	group, ok := h.groups[chat.ID]
	if !ok || user == nil {
		return
	}
	if !h.markJoinOnce(chat.ID, user.ID) {
		return
	}
	h.Invalidate(chat.ID, user.ID)

	if group.IsExempt(user.ID) || h.isGroupAdmin(chat.ID, user.ID) {
		slog.Info("白名单或管理员加入，跳过校验", "user_id", user.ID, "chat_id", chat.ID)
		h.sendWelcome(chat, user, "")
		return
	}

	valid, err := h.accountValid(chat.ID, user.ID)
	if err != nil {
		slog.Error("入群验证时数据库查询失败，暂不移出", "chat_id", chat.ID, "user_id", user.ID, "error", err)
		return
	}
	if valid {
		h.revokeIssued(chat.ID, user.ID)
		h.markVerified(chat.ID, user.ID)
		plan := ""
		if client, u, ok := h.boundUser(chat.ID, user.ID); ok {
			plan = planNameOf(client, u)
		}
		h.sendWelcome(chat, user, plan)
		slog.Info("已授权用户入群", "user_id", user.ID, "chat_id", chat.ID)
		return
	}

	slog.Info("未授权用户入群，移出", "user_id", user.ID, "chat_id", chat.ID)
	h.removeUnauthorized(chat, user, "未通过机器人绑定有效账户")
}

// markJoinOnce 同一用户短时间内的重复入群事件只处理一次。
func (h *Handler) markJoinOnce(chatID, userID int64) bool {
	key := fmt.Sprintf("%d:%d", chatID, userID)
	now := time.Now()

	h.mu.Lock()
	defer h.mu.Unlock()
	for k, t := range h.joinSeen {
		if now.Sub(t) > time.Minute {
			delete(h.joinSeen, k)
		}
	}
	if t, ok := h.joinSeen[key]; ok && now.Sub(t) < 2*time.Second {
		return false
	}
	h.joinSeen[key] = now
	return true
}

func (h *Handler) sendWelcome(chat *tele.Chat, user *tele.User, planName string) {
	safeName := mdEscape(displayName(user))
	var text string
	if planName != "" {
		text = fmt.Sprintf("👏 欢迎 尊贵的 %s 用户 [%s](tg://user?id=%d)", mdEscape(planName), safeName, user.ID)
	} else {
		text = fmt.Sprintf("👋 欢迎 [%s](tg://user?id=%d) 加入！", safeName, user.ID)
	}
	msg, err := h.bot.Send(chat, text, tele.ModeMarkdown)
	if err != nil || msg == nil {
		return
	}
	go func() {
		time.Sleep(60 * time.Second)
		_ = h.bot.Delete(msg)
	}()
}

// removeUnauthorized 踢出未授权用户，并私聊说明如何通过机器人进群。
func (h *Handler) removeUnauthorized(chat *tele.Chat, user *tele.User, reason string) {
	if user == nil || h.isGroupAdmin(chat.ID, user.ID) {
		return
	}
	if err := h.kick(chat.ID, user.ID); err != nil {
		slog.Error("移出未授权用户失败", "user_id", user.ID, "chat_id", chat.ID, "error", err)
		return
	}

	notice := fmt.Sprintf("⚠️ 您已被移出群组\n原因：%s\n\n请先私聊本机器人，发送 /bind 邮箱 密码。账户和套餐确认有效后，会发给你一次性邀请链接。", reason)
	if _, err := h.bot.Send(user, notice); err != nil {
		tip, _ := h.bot.Send(chat, "⚠️ 有用户未绑定有效账户，已被移出。请先私聊机器人完成绑定后再进群。")
		if tip != nil {
			go func() {
				time.Sleep(10 * time.Second)
				_ = h.bot.Delete(tip)
			}()
		}
	}
}

func (h *Handler) kick(chatID, userID int64) error {
	chat := &tele.Chat{ID: chatID}
	user := &tele.User{ID: userID}

	member, mErr := h.bot.ChatMemberOf(chat, user)
	if mErr == nil && member != nil && (member.Role == tele.Left || member.Role == tele.Kicked) {
		h.Invalidate(chatID, userID)
		return nil
	}
	if mErr == nil && member != nil && (member.Role == tele.Creator || member.Role == tele.Administrator) {
		return fmt.Errorf("跳过管理员")
	}

	err := h.bot.Ban(chat, &tele.ChatMember{
		User:            user,
		RestrictedUntil: time.Now().Add(60 * time.Second).Unix(),
	})
	if err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if err := h.bot.Unban(chat, user); err != nil {
		slog.Warn("Unban失败，重试一次", "user_id", userID, "error", err)
		time.Sleep(time.Second)
		if err := h.bot.Unban(chat, user); err != nil {
			slog.Error("Unban再次失败，将在60秒后自动解除", "user_id", userID, "error", err)
		}
	}
	h.Invalidate(chatID, userID)
	return nil
}

// onStart 处理 /start 命令
func (h *Handler) onStart(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	payload := ""
	if c.Message() != nil {
		payload = c.Message().Payload
	}
	if strings.HasPrefix(payload, "verify_") {
		return c.Send("入群方式已更新。请先发送 /bind 邮箱 密码，账户和套餐确认有效后，我会发给你一次性邀请链接。")
	}
	return c.Send(h.buildStartMessage(), tele.ModeMarkdown)
}

// buildStartMessage 基于 config 中的 profile 构建 /start 响应
func (h *Handler) buildStartMessage() string {
	var sb strings.Builder

	p := h.config.Telegram.Profile
	if p.Description != "" {
		sb.WriteString(p.Description)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("👋 欢迎使用群组管理机器人\n\n")
		sb.WriteString("请先绑定面板账户。账户和套餐确认有效后，我会发给你一次性群邀请链接。\n\n")
	}

	if len(p.Commands) > 0 {
		sb.WriteString("📝 可用命令：\n")
		for _, cmd := range p.Commands {
			sb.WriteString(fmt.Sprintf("`/%s` - %s\n", cmd.Command, cmd.Description))
		}
	} else {
		sb.WriteString("📝 可用命令：\n")
		sb.WriteString("`/bind 邮箱 密码` - 绑定面板账户并获取邀请链接\n")
		sb.WriteString("`/invite` - 重新获取一次性邀请链接\n")
		sb.WriteString("`/unbind` - 解除绑定\n")
		sb.WriteString("`/status` - 查看套餐状态\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}

// onBind 处理 /bind 命令
func (h *Handler) onBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	args := strings.Fields(c.Message().Text)
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/bind 邮箱 密码`\n\n示例：`/bind user@example.com mypassword`",
			tele.ModeMarkdown,
		)
	}

	email := args[1]
	password := args[2]
	userID := c.Sender().ID

	var foundUser *db.V2User
	var foundDBName string

	for chatID, client := range h.dbClients {
		u, err := client.FindUserByEmail(email)
		if err != nil {
			slog.Error("查询用户失败", "email", email, "error", err)
			continue
		}
		if u != nil {
			foundUser = u
			foundDBName = h.groups[chatID].Database.DBName
			break
		}
	}

	if foundUser == nil {
		return c.Send("❌ 未找到该邮箱对应的账户，请检查邮箱是否正确。")
	}

	algo := ""
	if foundUser.PasswordAlgo.Valid {
		algo = foundUser.PasswordAlgo.String
	}
	salt := ""
	if foundUser.PasswordSalt.Valid {
		salt = foundUser.PasswordSalt.String
	}

	if !db.VerifyPassword(algo, salt, password, foundUser.Password) {
		return c.Send("❌ 密码错误，请重试。")
	}

	if foundUser.Banned != 0 {
		return c.Send("❌ 您的账户已被封禁，无法绑定。")
	}

	previous, hadPrevious := h.bindings.Get(userID)
	if err := h.bindings.Set(userID, email, foundDBName); err != nil {
		if errors.Is(err, binding.ErrEmailBound) {
			return c.Send("❌ 该邮箱已被其他 Telegram 账号绑定。\n如需更换，请先在原账号执行 /unbind 解绑。")
		}
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return c.Send("❌ 绑定失败，请稍后重试。")
	}
	if hadPrevious && (previous.Email != email || previous.DBName != foundDBName) {
		h.dropIssued(userID)
		h.removeFromBoundGroups(userID, previous.DBName)
	}

	slog.Info("用户绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)
	return c.Send(h.bindResultText("✅ 绑定成功！", email, userID))
}

// onForceBind 强制绑定：通过邮箱+密码验证身份后，解除该邮箱的旧绑定并绑定到当前 TG 号
func (h *Handler) onForceBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}

	args := strings.Fields(c.Message().Text)
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/forcebind 邮箱 密码`\n\n"+
				"用于强制将邮箱绑定到当前 Telegram 账号。\n"+
				"如果该邮箱已被其他 TG 号绑定，验证密码后会自动解除旧绑定。",
			tele.ModeMarkdown,
		)
	}

	email := args[1]
	password := args[2]
	userID := c.Sender().ID

	var foundUser *db.V2User
	var foundDBName string

	for chatID, client := range h.dbClients {
		u, err := client.FindUserByEmail(email)
		if err != nil {
			continue
		}
		if u != nil {
			foundUser = u
			foundDBName = h.groups[chatID].Database.DBName
			break
		}
	}

	if foundUser == nil {
		return c.Send("❌ 未找到该邮箱对应的账户，请检查邮箱是否正确。")
	}

	algo := ""
	if foundUser.PasswordAlgo.Valid {
		algo = foundUser.PasswordAlgo.String
	}
	salt := ""
	if foundUser.PasswordSalt.Valid {
		salt = foundUser.PasswordSalt.String
	}

	if !db.VerifyPassword(algo, salt, password, foundUser.Password) {
		return c.Send("❌ 密码错误，请重试。")
	}

	if foundUser.Banned != 0 {
		return c.Send("❌ 您的账户已被封禁，无法绑定。")
	}

	previous, hadPrevious := h.bindings.Get(userID)
	oldTG, err := h.bindings.Transfer(userID, email, foundDBName)
	if err != nil {
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return c.Send("❌ 绑定失败，请稍后重试。")
	}
	if hadPrevious && (previous.Email != email || previous.DBName != foundDBName) {
		h.dropIssued(userID)
		h.removeFromBoundGroups(userID, previous.DBName)
	}
	if oldTG != 0 {
		h.dropIssued(oldTG)
		h.removeFromBoundGroups(oldTG, foundDBName)
		_, _ = h.bot.Send(&tele.User{ID: oldTG}, "⚠️ 您的面板账户已绑定到其他 Telegram 账号，当前账号已被移出群组。")
		slog.Info("强制解除旧绑定", "email", email, "old_tg", oldTG, "new_tg", userID)
	}

	slog.Info("用户强制绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)

	title := "✅ 绑定成功！"
	if oldTG != 0 && oldTG != userID {
		title = "✅ 已解除旧绑定并重新绑定成功！"
	}
	return c.Send(h.bindResultText(title, email, userID))
}

// onInvite 已绑定用户重新领取一次性邀请链接
func (h *Handler) onInvite(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	userID := c.Sender().ID
	if _, ok := h.bindings.Get(userID); !ok {
		return c.Send("请先绑定面板账户：`/bind 邮箱 密码`", tele.ModeMarkdown)
	}
	return c.Send(h.inviteSection(userID))
}

// onUnbind 解除绑定
func (h *Handler) onUnbind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	userID := c.Sender().ID
	b, ok := h.bindings.Get(userID)
	if !ok {
		return c.Send("未找到您的绑定记录。")
	}
	if err := h.bindings.Delete(userID); err != nil {
		return c.Send("❌ 解绑失败，请稍后重试。")
	}
	h.dropIssued(userID)
	h.removeFromBoundGroups(userID, b.DBName)
	return c.Send("✅ 已解除绑定，并已移出对应群组。需要重新进群时，请再次 /bind。")
}

func (h *Handler) removeFromBoundGroups(userID int64, dbName string) {
	for chatID, group := range h.groups {
		h.Invalidate(chatID, userID)
		if group.Database.DBName != dbName || group.IsExempt(userID) || h.isGroupAdmin(chatID, userID) {
			continue
		}
		if err := h.kick(chatID, userID); err != nil {
			slog.Warn("移出旧绑定账号失败", "user_id", userID, "chat_id", chatID, "error", err)
		}
	}
}

// onStatus 查询套餐状态
func (h *Handler) onStatus(c tele.Context) error {
	sender := c.Sender()
	if sender == nil || sender.IsBot {
		// 匿名管理员发送，sender 是 GroupAnonymousBot，无法查询
		if isGroupChat(c) {
			_ = h.bot.Delete(c.Message())
		}
		return nil
	}
	userID := sender.ID
	b, hasBind := h.bindings.Get(userID)

	inGroup := isGroupChat(c)

	var results []string
	for chatID, client := range h.dbClients {
		group := h.groups[chatID]
		dbName := group.Database.DBName

		if group.IsExempt(userID) {
			results = append(results, "⭐ 白名单用户，免验证")
			continue
		}

		if hasBind && b.DBName == dbName {
			user, err := client.FindUserByEmail(b.Email)
			if err != nil || user == nil {
				results = append(results, "📦 ❓ 查询失败")
				continue
			}
			results = append(results, formatStatusLine(client, user, inGroup))
		}
	}

	if len(results) == 0 {
		return c.Send("未找到您的账户信息，请先使用 `/bind 邮箱 密码` 绑定。", tele.ModeMarkdown)
	}

	text := "━━━━━ 📊 套餐状态 ━━━━━\n\n" + strings.Join(results, "\n\n━━━━━━━━━━━━━━━━━\n\n")

	if inGroup {
		// 群内：删除用户的命令消息，回复 30 秒后自动删除
		_ = h.bot.Delete(c.Message())
		reply, _ := h.bot.Send(c.Chat(), text, tele.ModeMarkdown)
		if reply != nil {
			go func() {
				time.Sleep(30 * time.Second)
				_ = h.bot.Delete(reply)
			}()
		}
		return nil
	}
	return c.Send(text, tele.ModeMarkdown)
}

// onCheck 管理员手动触发巡检
func (h *Handler) onCheck(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	if !h.config.IsAdmin(c.Sender().ID) {
		return c.Send("❓ 无效命令，请输入 /start 查看帮助。")
	}
	if h.check == nil {
		return c.Send("❌ 巡检任务尚未就绪。")
	}
	go h.check()
	return c.Send("✅ 巡检任务已触发，请稍候查看日志。")
}

// onCha 管理员通过 Telegram ID 查询用户信息
func (h *Handler) onCha(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	if !h.config.IsAdmin(c.Sender().ID) {
		return c.Send("❓ 无效命令，请输入 /start 查看帮助。")
	}

	args := strings.Fields(c.Message().Text)
	if len(args) != 2 {
		return c.Send("📝 用法：`/cha TelegramID`\n\n示例：`/cha 123456789`", tele.ModeMarkdown)
	}

	tgID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || tgID <= 0 {
		return c.Send("❌ 请输入有效的数字 ID")
	}

	b, bound := h.bindings.Get(tgID)
	if !bound {
		return c.Send(fmt.Sprintf("未找到 Telegram ID `%d` 的绑定记录。", tgID), tele.ModeMarkdown)
	}

	for chatID, client := range h.dbClients {
		if h.groups[chatID].Database.DBName != b.DBName {
			continue
		}
		user, err := client.FindUserByEmail(b.Email)
		if err != nil || user == nil {
			continue
		}
		return c.Send(
			fmt.Sprintf("🔍 查询结果（TG ID: `%d`）\n\n%s", tgID, formatUserInfo(client, user)),
			tele.ModeMarkdown,
		)
	}

	return c.Send(
		fmt.Sprintf("🔍 查询结果（TG ID: `%d`）\n\n📧 邮箱：`%s`\n⚠️ 数据库中未找到该用户", tgID, b.Email),
		tele.ModeMarkdown,
	)
}

func (h *Handler) bindResultText(title, email string, userID int64) string {
	return fmt.Sprintf("%s\n\n邮箱：%s\n\n%s", title, email, h.inviteSection(userID))
}

// inviteSection 为已绑定用户生成对应群组的一次性邀请链接说明
func (h *Handler) inviteSection(userID int64) string {
	b, ok := h.bindings.Get(userID)
	if !ok {
		return "请先绑定面板账户：/bind 邮箱 密码"
	}

	var sb strings.Builder
	matched := false
	for i := range h.config.Groups {
		g := &h.config.Groups[i]
		if g.Database.DBName != b.DBName {
			continue
		}
		matched = true
		title := h.groupTitle(g.ChatID)
		client := h.dbClients[g.ChatID]
		if client == nil {
			sb.WriteString(fmt.Sprintf("「%s」：系统错误，请联系管理员。\n\n", title))
			continue
		}
		user, err := client.FindUserByEmail(b.Email)
		if err != nil || user == nil || !db.IsUserValid(user) {
			if err == nil {
				h.revokeIssued(g.ChatID, userID)
			}
			reason := describeInvalid(user)
			if err != nil {
				reason = "查询账户失败"
			}
			sb.WriteString(fmt.Sprintf("「%s」暂时无法进群：%s\n请处理后再发送 /invite。\n\n", title, reason))
			continue
		}
		if h.memberInside(g.ChatID, userID) {
			sb.WriteString(fmt.Sprintf("「%s」：您已在群内。\n\n", title))
			continue
		}
		link, err := h.createOneTimeLink(g.ChatID, userID)
		if err != nil {
			slog.Error("生成邀请链接失败", "chat_id", g.ChatID, "user_id", userID, "error", err)
			sb.WriteString(fmt.Sprintf("「%s」：生成邀请链接失败。请确认机器人是管理员，并拥有「邀请用户」权限。\n\n", title))
			continue
		}
		sb.WriteString(fmt.Sprintf("「%s」一次性邀请链接（%s内有效，仅可使用 1 次，请勿转发）：\n%s\n\n", title, h.ttlText(), link))
	}
	if !matched {
		return "没有找到与该账户对应的群组，请联系管理员。"
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (h *Handler) ttlText() string {
	sec := h.config.InviteTTL
	if sec <= 0 {
		sec = 1800
	}
	if sec%60 == 0 {
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

func (h *Handler) groupTitle(chatID int64) string {
	if g := h.groups[chatID]; g != nil && g.Name != "" {
		return g.Name
	}
	chat, err := h.bot.ChatByID(chatID)
	if err == nil && chat != nil && chat.Title != "" {
		return chat.Title
	}
	return strconv.FormatInt(chatID, 10)
}

func (h *Handler) boundUser(chatID, userID int64) (*db.Client, *db.V2User, bool) {
	client, user, ok, _ := h.lookupBoundUser(chatID, userID)
	return client, user, ok
}

func (h *Handler) lookupBoundUser(chatID, userID int64) (*db.Client, *db.V2User, bool, error) {
	group := h.groups[chatID]
	client := h.dbClients[chatID]
	if group == nil || client == nil {
		return nil, nil, false, nil
	}
	b, bound := h.bindings.Get(userID)
	if !bound || b.DBName != group.Database.DBName {
		return client, nil, false, nil
	}
	user, err := client.FindUserByEmail(b.Email)
	if err != nil {
		return client, nil, false, err
	}
	return client, user, user != nil, nil
}

func (h *Handler) accountValid(chatID, userID int64) (bool, error) {
	_, user, ok, err := h.lookupBoundUser(chatID, userID)
	return ok && db.IsUserValid(user), err
}

func (h *Handler) isGroupAdmin(chatID, userID int64) bool {
	member, err := h.bot.ChatMemberOf(&tele.Chat{ID: chatID}, &tele.User{ID: userID})
	if err != nil || member == nil {
		return false
	}
	return member.Role == tele.Creator || member.Role == tele.Administrator
}

func (h *Handler) memberInside(chatID, userID int64) bool {
	member, err := h.bot.ChatMemberOf(&tele.Chat{ID: chatID}, &tele.User{ID: userID})
	if err != nil || member == nil {
		return false
	}
	switch member.Role {
	case tele.Creator, tele.Administrator, tele.Member, tele.Restricted:
		return true
	default:
		return false
	}
}

func (h *Handler) createOneTimeLink(chatID, userID int64) (string, error) {
	h.revokeIssued(chatID, userID)

	name := fmt.Sprintf("tg%d", userID)
	if len(name) > 32 {
		name = name[:32]
	}
	data, err := h.bot.Raw("createChatInviteLink", map[string]interface{}{
		"chat_id":      chatID,
		"name":         name,
		"member_limit": 1,
		"expire_date":  time.Now().Add(time.Duration(h.config.InviteTTL) * time.Second).Unix(),
	})
	if err != nil {
		return "", err
	}

	var resp struct {
		Result struct {
			InviteLink string `json:"invite_link"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Result.InviteLink == "" {
		return "", fmt.Errorf("empty invite link")
	}
	h.rememberIssued(chatID, userID, resp.Result.InviteLink)
	return resp.Result.InviteLink, nil
}

func (h *Handler) rememberIssued(chatID, userID int64, link string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.issuedLinks[userID] == nil {
		h.issuedLinks[userID] = make(map[int64]string)
	}
	h.issuedLinks[userID][chatID] = link
}

func (h *Handler) revokeIssued(chatID, userID int64) {
	h.mu.Lock()
	link := ""
	if chats, ok := h.issuedLinks[userID]; ok {
		link = chats[chatID]
		delete(chats, chatID)
	}
	h.mu.Unlock()
	if link == "" {
		return
	}
	if _, err := h.bot.Raw("revokeChatInviteLink", map[string]interface{}{
		"chat_id":     chatID,
		"invite_link": link,
	}); err != nil {
		slog.Debug("作废旧邀请链接失败", "user_id", userID, "chat_id", chatID, "error", err)
	}
}

func (h *Handler) dropIssued(userID int64) {
	h.mu.Lock()
	chats := h.issuedLinks[userID]
	delete(h.issuedLinks, userID)
	h.mu.Unlock()
	for chatID, link := range chats {
		if _, err := h.bot.Raw("revokeChatInviteLink", map[string]interface{}{
			"chat_id":     chatID,
			"invite_link": link,
		}); err != nil {
			slog.Debug("作废邀请链接失败", "user_id", userID, "chat_id", chatID, "error", err)
		}
	}
}
