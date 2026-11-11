package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// The task-thread forum's Discord client (hk 4040, docs/task-threads.md). It
// authenticates as a bot, so besides posting it can list the forum's threads
// and find a thread whose create answer was lost. It reads no message content.

const (
	// discordAPIBase is Discord's REST API, version 10.
	discordAPIBase = "https://discord.com/api/v10"
	// discordBotAPI names the bot in errors. The token is never quoted.
	discordBotAPI = "discord bot"
	// discordBotUserAgent is the User-Agent form Discord asks bots to send.
	discordBotUserAgent = "DiscordBot (https://github.com/mgh3326/handoffkeep, 1)"
	// discordArchivedThreadsLimit is the most archived threads one lookup
	// reads (Discord's page maximum). A lost create is minutes old, and a
	// thread archives after an hour idle at the earliest, so the newest page
	// holds it if it is archived at all.
	discordArchivedThreadsLimit = 100
)

// DiscordMessage is a message to post: its content, flags and the users it
// may ping (none pings nobody; no @everyone or role ever pings).
type DiscordMessage struct {
	Content string
	Flags   int
	Users   []string
}

// DiscordThread is one forum thread as a lookup lists it.
type DiscordThread struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	OwnerID  string `json:"owner_id"`
	Name     string `json:"name"`
}

// TaskThreadDiscord is what the task-thread poster needs from Discord.
type TaskThreadDiscord interface {
	// CreateForumThread opens a post in forum and returns the thread id.
	CreateForumThread(ctx context.Context, forumID, name string, msg DiscordMessage) (string, error)
	// PostThreadMessage adds a message to a thread.
	PostThreadMessage(ctx context.Context, threadID string, msg DiscordMessage) error
	// ForumThreads lists the forum's active and newest archived threads, and
	// returns the bot's own user id with them.
	ForumThreads(ctx context.Context, forumID string) (botUserID string, threads []DiscordThread, err error)
}

// DiscordBot calls Discord's REST API with a bot token. The token is a
// credential: no error it returns quotes it.
type DiscordBot struct {
	Token string
	// BaseURL is the API root; empty means discordAPIBase.
	BaseURL string
	HTTP    *http.Client

	mu     sync.Mutex
	userID string
	guilds map[string]string
}

// ParseDiscordSnowflake accepts a Discord id. name is the variable the
// message names; the value is never quoted.
func ParseDiscordSnowflake(name, raw string) (string, error) {
	if !discordMessageIDRE.MatchString(raw) {
		return "", errors.New(name + " must be a Discord id (digits only)")
	}
	return raw, nil
}

func (b *DiscordBot) call(ctx context.Context, method, path string, payload, out any) error {
	base := b.BaseURL
	if base == "" {
		base = discordAPIBase
	}
	return discordRequest(ctx, b.HTTP, discordBotAPI, method, strings.TrimSuffix(base, "/")+path, "Bot "+b.Token, payload, out)
}

func discordMessagePayload(msg DiscordMessage) map[string]any {
	return map[string]any{"content": msg.Content, "flags": msg.Flags, "allowed_mentions": discordAllowedMentions(msg.Users)}
}

// errDiscordNoThreadID is a create Discord accepted without returning the
// thread, so its outcome is unknown.
var errDiscordNoThreadID = errors.New("discord bot returned no thread id")

// CreateForumThread is POST /channels/{forum}/threads. The thread's id is the
// returned channel's id.
func (b *DiscordBot) CreateForumThread(ctx context.Context, forumID, name string, msg DiscordMessage) (string, error) {
	if !discordMessageIDRE.MatchString(forumID) {
		return "", errors.New("invalid discord forum id")
	}
	var channel struct {
		ID string `json:"id"`
	}
	payload := map[string]any{"name": name, "message": discordMessagePayload(msg)}
	if err := b.call(ctx, http.MethodPost, "/channels/"+forumID+"/threads", payload, &channel); err != nil {
		return "", err
	}
	if !discordMessageIDRE.MatchString(channel.ID) {
		return "", errDiscordNoThreadID
	}
	return channel.ID, nil
}

// PostThreadMessage is POST /channels/{thread}/messages. A 2xx answer
// carries the message, so it exists.
func (b *DiscordBot) PostThreadMessage(ctx context.Context, threadID string, msg DiscordMessage) error {
	if !discordMessageIDRE.MatchString(threadID) {
		return errors.New("invalid discord thread id")
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := b.call(ctx, http.MethodPost, "/channels/"+threadID+"/messages", discordMessagePayload(msg), &message); err != nil {
		return err
	}
	if !discordMessageIDRE.MatchString(message.ID) {
		return errDiscordNoMessageID
	}
	return nil
}

// ForumThreads reads the bot's user id (GET /users/@me) and the forum's
// guild (GET /channels/{forum}), both cached for the process, then the
// guild's active threads (GET /guilds/{guild}/threads/active, which lists
// every channel's, filtered here to the forum) and the forum's newest
// archived public threads.
func (b *DiscordBot) ForumThreads(ctx context.Context, forumID string) (string, []DiscordThread, error) {
	if !discordMessageIDRE.MatchString(forumID) {
		return "", nil, errors.New("invalid discord forum id")
	}
	botID, err := b.botUserID(ctx)
	if err != nil {
		return "", nil, err
	}
	guild, err := b.forumGuild(ctx, forumID)
	if err != nil {
		return "", nil, err
	}
	var active, archived struct {
		Threads []DiscordThread `json:"threads"`
	}
	if err := b.call(ctx, http.MethodGet, "/guilds/"+guild+"/threads/active", nil, &active); err != nil {
		return "", nil, err
	}
	query := url.Values{"limit": {strconv.Itoa(discordArchivedThreadsLimit)}}
	if err := b.call(ctx, http.MethodGet, "/channels/"+forumID+"/threads/archived/public?"+query.Encode(), nil, &archived); err != nil {
		return "", nil, err
	}
	var threads []DiscordThread
	for _, t := range append(active.Threads, archived.Threads...) {
		if t.ParentID == forumID {
			threads = append(threads, t)
		}
	}
	return botID, threads, nil
}

func (b *DiscordBot) botUserID(ctx context.Context) (string, error) {
	b.mu.Lock()
	id := b.userID
	b.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := b.call(ctx, http.MethodGet, "/users/@me", nil, &user); err != nil {
		return "", err
	}
	if !discordMessageIDRE.MatchString(user.ID) {
		return "", errors.New("discord bot user has no id")
	}
	b.mu.Lock()
	b.userID = user.ID
	b.mu.Unlock()
	return user.ID, nil
}

func (b *DiscordBot) forumGuild(ctx context.Context, forumID string) (string, error) {
	b.mu.Lock()
	guild := b.guilds[forumID]
	b.mu.Unlock()
	if guild != "" {
		return guild, nil
	}
	var channel struct {
		GuildID string `json:"guild_id"`
	}
	if err := b.call(ctx, http.MethodGet, "/channels/"+forumID, nil, &channel); err != nil {
		return "", err
	}
	if !discordMessageIDRE.MatchString(channel.GuildID) {
		return "", errors.New("discord forum has no guild id")
	}
	b.mu.Lock()
	if b.guilds == nil {
		b.guilds = map[string]string{}
	}
	b.guilds[forumID] = channel.GuildID
	b.mu.Unlock()
	return channel.GuildID, nil
}
