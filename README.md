# nyande-bot — Go edition

**English** | [Русский](README.ru.md)

`nyande-bot` is a Telegram and Discord media downloader with an optional
LLM assistant on both platforms. This Go edition incorporates useful additions from the
original Python and Rust versions.

## Features

- downloads photos, videos, and carousels from TikTok, Instagram, X/Twitter,
  Xiaohongshu (RedNote), Pinterest, YouTube, and Reddit;
- includes the source post caption or description for every supported platform;
- supports direct image and video links;
- sends carousels as Telegram albums or Discord attachment batches of up to 10
  files and validates file-size limits;
- works with OpenRouter and other OpenAI-compatible APIs;
- sends photos, selected video frames, and audio/video transcripts to the LLM;
- restores the entire cached album when a user replies to one of the files
  previously sent by the bot;
- provides the LLM with `current_time` and optional `web_search` tools;
- optionally deletes unsupported links posted by non-administrators in groups;
- stores extra allowed domains added through `/allowlink example.com`;
- persists each group's link-deletion setting across restarts;
- accepts Telegram Stars and can display an optional Ko-fi button;
- keeps process statistics and separate LLM history for every chat.

## Chat games

The `/ttt` and `/checkers` commands also run games directly through Telegram
inline keyboards. Reply to another user's message with the command to challenge
that user.

`/wordle` starts a five-letter English Wordle directly in chat. Everyone gets
the same daily word, while attempts are tracked per user.
The bot sends a PNG progress card after every guess and adds the player's avatar
to the final card.

## Technology

- Go 1.26+ — Telegram and Discord bots, downloaders, and LLM client;
- `yt-dlp`, Deno, FFmpeg, and FFprobe — YouTube challenge solving, media
  processing, and downloader fallbacks;
- optional OpenAI Whisper CLI — local audio and video transcription;
- Docker Compose — service orchestration.

## Quick start

You need Podman or Docker with a Compose provider, plus a Telegram bot from
[@BotFather](https://t.me/BotFather), a Discord application, or both.

1. Copy the configuration template:

   ```bash
   cp .env.example .env
   ```

2. Set at least one bot token in `.env`. Both transports can run at once:

   ```dotenv
   BOT_TOKEN=123456789:telegram_bot_token
   DISCORD_BOT_TOKEN=your_discord_bot_token
   ```

3. Build and start every service:

   ```bash
   docker compose up -d --build
   ```

   The equivalent Podman command is:

   ```bash
   podman compose up -d --build
   ```

4. Check service status and logs:

   ```bash
   docker compose ps
   docker compose logs -f bot
   ```

   Replace `docker compose` with `podman compose` in these commands when using
   Podman.

Python dependencies are installed with `uv` and a download cache. The Whisper
layer is independent of the bot binary, so Go code changes reuse it during
normal cached builds. The first PyTorch/CUDA dependency download can still
take time.

The default `runtime` image includes local Whisper. If transcription is not
needed, use the smaller image:

```dotenv
DOCKER_TARGET=runtime-lite
WHISPER_ENABLED=false
```

## Discord setup

Create an application and bot in the
[Discord Developer Portal](https://discord.com/developers/applications), copy
its token to `DISCORD_BOT_TOKEN`, and enable **Message Content Intent** on the
Bot page.
Invite it to a server with permission to view channels, send messages, attach
files, and read message history. The bot processes the first supported media
link in each message and replies with the downloaded files.

Discord commands use `!` by default: `!help`, `!ping`, `!stats`, `!gif`, and `!reset`. Change the
prefix with `DISCORD_COMMAND_PREFIX`.

Set `LLM_ENABLED=true`, `LLM_API_KEY`, and `LLM_MODEL` to enable text conversations
using the same `LLM_*` configuration as Telegram. Discord replies in DMs and,
on servers, when mentioned, replied to, or invoked with `LLM_TRIGGER_WORDS`.
Media links and commands take priority. History and cooldown are scoped to each
channel; `!reset` clears that channel's conversation history. Discord history is
stored in `LLM_HISTORY_FILE` with `.discord` appended, separately from Telegram.
Long replies are split into messages. Media analysis, long-term memory,
application tools, moderation, payments, and games remain Telegram features.

## LLM and media analysis

Example OpenRouter configuration:

```dotenv
LLM_ENABLED=true
LLM_BASE_URL=https://openrouter.ai/api/v1
LLM_API_KEY=your_api_key
LLM_MODEL=perplexity/sonar-pro
LLM_TRIGGER_WORDS=meow,nyande
LLM_WEB_SEARCH_ENABLED=false
LLM_VISION_ENABLED=true
LLM_TIMEZONE=Asia/Almaty
LLM_VIDEO_FRAME_COUNT=3
WHISPER_ENABLED=true
```

Perplexity is available through the same OpenRouter key: set the model to
`perplexity/sonar-pro`. It has native search, so `LLM_WEB_SEARCH_ENABLED=false`
is sufficient for that model. For other OpenRouter models, setting it to `true`
enables OpenRouter's server-side web search with the Perplexity engine. Search
results inform the answer, but the bot does not append a separate source list.

Per-chat history is persisted in `LLM_HISTORY_FILE` across container restarts;
`/reset` removes it from memory and disk. Long answers are split across multiple
Telegram messages instead of being truncated.

Long-term memory is stored separately in the SQLite database at
`LLM_MEMORY_FILE`. Records are scoped to both the Telegram chat and user, so
participants in a group do not share profiles. The model receives up to
`LLM_MEMORY_RECALL_LIMIT` relevant or recent records. During a conversation, the
bot can proactively save useful, lasting facts the user states about themselves,
such as their name, preferences, goals, and ongoing projects, without a
`/remember` command. It briefly acknowledges successful saves. It is instructed
to respect requests not to remember something and skip guesses, other people's
messages, and transient requests. Use `/memory` to inspect records; ask it to
forget a fact or use `/forget` and `/forget_all` to remove them. Obvious passwords,
API keys, and tokens are rejected. Set `LLM_MEMORY_ENABLED=false` to disable the
feature.


In private chats, the bot responds to ordinary text and media messages. In a
group, it can be invoked by mentioning its `@username`, replying to one of its
messages, or using one of the comma-separated `LLM_TRIGGER_WORDS` (matched
case-insensitively). Replies to media downloaded and sent by the bot are handled
only when they contain one of those trigger words.

`LLM_VIDEO_FRAME_COUNT` controls how many evenly spaced video frames are sent
to the model and accepts values from 1 to 6. `LLM_TIMEZONE` accepts an IANA
timezone such as `Asia/Almaty` or a UTC offset such as `+05:00`.

Chat messages are recorded in an SQLite database with FTS5 search at `CHAT_LOG_FILE`.
The LLM can access historical messages via the `search_chat_messages` tool and inspect
recent chat context via `get_recent_messages`. To enable the bot to receive all group
messages (even when unaddressed), disable Privacy Mode in `@BotFather` (`/setprivacy -> Disable`)
or make the bot an administrator in the group. Set `CHAT_LOG_ENABLED=false` to disable chat logging.

See [`.env.example`](.env.example) for every available setting. The `.env` file
is ignored by Git; never publish bot tokens or API keys.

Group administrators can use `/linkdelete on` or `/linkdelete off` to control
whether unsupported links are deleted. The setting is stored in
`LINK_MODERATION_FILE` and is disabled by default.


For permanent silent moderation, a group administrator sends a standalone `blahajblahajblahaj`
message (without a slash). The bot silently enables deletion of all links and
stops replying, calling the LLM, downloading media, handling ordinary commands or buttons,
and sending reminders. Edited messages and media captions are checked too.
Administrators remain exempt. Links to supported sites and allowlisted sites are
deleted too.
This mode is per group, persists in `LINK_MODERATION_FILE` across restarts, and
cannot be disabled through the bot, including `/linkdelete off` or another `blahajblahajblahaj`.
The bot needs permission to delete messages. Successful activation is logged as
`[moderation] silent_enabled` with the chat, user, and message IDs.

Automatically forwarded posts from the linked channel are exempt in its discussion
group, including captions and edits. This uses Telegram's
[`is_automatic_forward`](https://core.telegram.org/bots/api#message) flag; ordinary
manual forwards and replies to a channel post remain subject to moderation.
Anonymous administrators sending as the group itself are also exempt.

An administrator can grant a participant permission **before** they post links:

- Reply to any message from the participant with `/permitlinks`.
- Reply with `/revokelinks` to revoke that permission.
- Alternatively supply a numeric Telegram user ID: `/permitlinks 123456789` or
  `/revokelinks 123456789`. Usernames are not supported.

Permission lasts until revoked, applies only to that user in that group, and
persists in `LINK_MODERATION_FILE`. It covers links in messages, captions and
edits without granting admin rights. After revocation, subsequent messages/edits
are checked again. Deleted messages cannot be restored; revocation does not
rescan chat history.

These two special silent-mode commands produce **no chat replies**, including
when issued by an anonymous group administrator. Grants/revocations are logged
as `[moderation] link_permission` with chat, admin and target IDs plus
`allowed=true` / `allowed=false`. Invalid commands and storage failures appear
in technical logs. In other chats these commands do not enable or change moderation.
For a service named `nyande-bot`:

```bash
journalctl -u nyande-bot -f | rg '\[moderation\]|update .* failed'
```


## Bot commands

| Command | Description |
|---|---|
| `/start` | start the bot |
| `/help` | show help |
| `/ttt` | play tic-tac-toe in Telegram messages |
| `/checkers` | play checkers in Telegram messages |
| `/wordle` | play a five-letter English Wordle in Telegram messages |
| `/donate` | support the bot through Telegram Stars or Ko-fi |
| `/paysupport` | show payment support information |
| `/ping` | check whether the bot is available |
| `/stats` | show statistics for the current process |
| `/reset` | clear LLM history for the current chat |
| `/remember fact` | save a long-term memory for the current user and chat |
| `/memory` | list long-term memories and their ids |
| `/forget id` | delete one long-term memory by id |
| `/forget_all` | delete all long-term memories for the current user and chat |
| `/round` | convert video into a square Telegram Video Note |
| `/voice` | convert audio or video into a Telegram Voice message |
| `/gif` | convert video into a looping Telegram GIF animation |
| `/mediainfo` | show technical specs (resolution, FPS, duration, codecs) of photo/video/audio |
| `/allowlink example.com` | allow a domain in a private chat or as an administrator |
| `/linkdelete [on\|off]` | configure unsupported-link deletion for this group |
| `/permitlinks` / `/revokelinks` | reply to a participant to grant/revoke links in silent mode (administrators only) |

Use `!gif` with an attached video, a supported link, or as a reply to a video
message. The bot sends a silent GIF at 15 fps and up to 480 pixels wide,
subject to `MAX_FILE_SIZE`.

Discord provides `!help`, `!ping`, `!stats`, `!gif`, and `!reset` (or the prefix configured in
`DISCORD_COMMAND_PREFIX`). Media links do not need a command.

## Local validation

```bash
gofmt -w cmd internal resources
go test ./...
go vet ./...
```

The bot requires `yt-dlp` with its EJS component, Deno, `ffmpeg`, and `ffprobe`
at runtime. The Docker image includes all of them. Local transcription
additionally requires the `whisper` CLI.

## Downloads without manually exported cookies

Leave `YTDLP_COOKIES_FILE` and `YTDLP_COOKIES_FROM_BROWSER` empty for public
videos. If the normal Instagram download fails, the bot tries vxinstagram
for Reels and IGTV. This fallback sends the video URL to that service and
depends on its availability; carousels keep using the normal downloaders.

For YouTube, [bgutil PO Token Provider](https://github.com/Brainicism/bgutil-ytdlp-pot-provider)
automatically obtains verification tokens without an account login. The Docker
image includes it. For native installations, run the following **as the bot's
service user**, with Deno 2.4.3+ on the service's `PATH`:

```bash
git clone --depth 1 --branch 2.0.1 https://github.com/Brainicism/bgutil-ytdlp-pot-provider.git ~/bgutil-ytdlp-pot-provider
cd ~/bgutil-ytdlp-pot-provider/server
deno install --prod --allow-scripts=npm:canvas --frozen
mkdir -p ~/.config/yt-dlp/plugins
curl -fL --retry 3 https://github.com/Brainicism/bgutil-ytdlp-pot-provider/releases/download/2.0.1/bgutil-ytdlp-pot-provider.zip -o ~/.config/yt-dlp/plugins/bgutil-ytdlp-pot-provider.zip
```

If `XDG_CONFIG_HOME` is set, use it instead of `~/.config`.
yt-dlp discovers the plugin and provider automatically for both media downloads
and Discord music. No extra daemon or bot settings are needed. Keep plugin and
provider versions matched when updating. Script startup adds latency to token
generation; upstream offers an HTTP service for higher concurrency. These routes
do not remove login requirements caused by IP restrictions or private content.

## Repository structure

```text
cmd/nyande-bot/     application entry point
internal/bot/         commands, games, moderation, media, and state
internal/telegram/    Telegram Bot API client
internal/discordbot/  Discord gateway, commands, and media replies
internal/downloader/  platform downloaders and yt-dlp fallback
internal/llm/         OpenAI-compatible client and tools
resources/            shared strings.json
```

Only download and share content that you own or have permission to use.

## Quote collection

Reply with `/quote` to a text message or media caption to save it and receive a
PNG card with the author's name, available profile photo, and date. Forwarded
messages use the original author when Telegram provides it. Quotes support up to
1,200 characters; rich formatting and animated emoji are not preserved in cards.

- `/quote random` — a random quote from this chat.
- `/quote list` — the latest 10 quotes and their IDs.
- `/quote 12` — show quote #12.
- `/quote delete 12` — delete a quote as its author, saver, or a chat administrator.

Collections are isolated by chat and saving the same message twice is idempotent.
SQLite storage is configured with `QUOTE_DB_FILE`; Compose keeps it in the
persistent volume at `/app/data/quotes.db`. This Telegram feature does not require an LLM.

## Find and send media with the LLM

Ask “Find a short cat video and send it here” or “Find a capybara photo and send
it as an image.” With `LLM_WEB_SEARCH_ENABLED=true`, the model can find a URL
and call `download_media` to download and send files to the current Telegram chat.
The tool accepts supported platform post URLs and direct image/video URLs, not
ordinary articles or search result pages.

Existing `MAX_FILE_SIZE` and `MAX_MEDIA_ITEMS` limits apply. Each attempt has a
three-minute timeout; repeated calls for the same URL in one request do not send
duplicate files. Failures and partial deliveries are reported back to the model.
Availability still depends on the source site and downloader configuration.

## Activity logs

`docker compose logs -f bot` shows `[command]` events (command name, chat, user,
message and duration), `[media]` events (download source, file type/name/size,
and delivery outcome), `[tool]` events (LLM tool execution), and `[search]`
events (local search results). Commands and media are logged for Telegram and
Discord. Command arguments and tool response contents are not included in these
events; the download URL field omits credentials, query parameters and fragments.
`[tool] returned` means the tool returned; actual file delivery is recorded by
`[media] sent` or `[media] send_failed`.

TikTok requests can use `search_tiktok` when web search is enabled. It keeps only unique individual video URLs, using OpenRouter search citations with a scoped DuckDuckGo fallback. `download_media` can then download and send a selected result. This searches indexed pages; freshness, popularity and download availability are not guaranteed. Hosted search adds an LLM request; no new API keys are required.

Use `!llm off` / `!llm on` to disable or enable the LLM across a Discord server (Manage Server or Administrator permission required), or for your DM conversation. `!llm` shows the status. Media downloads remain available. Settings persist in `DISCORD_LLM_SETTINGS_FILE` across restarts. Commands use `DISCORD_COMMAND_PREFIX`; enabling requires the LLM to be enabled in the bot configuration.

[Discord voice music: commands and setup](docs/discord-music.md).

Discord music UI: `/nyande search` opens a private browser with source selection and multi-select results. `/nyande player` shows playback controls. Pagination, repeat, volume, shuffle and queue reordering are supported. See the [setup guide](docs/discord-music.md).

Music now defaults to direct DisGo + DAVE voice with `yt-dlp` and FFmpeg; Java/Lavalink is not required. `MUSIC_MAX_PLAYERS=1` limits concurrent voice players across all servers. See [native installation and migration](docs/discord-music.md).


### Discord command picker

After installing the updated binary and restarting, the bot registers individual
slash commands with descriptions and typed options. Type `/` and select nyande:
`/help`, `/ping`, `/stats`, `/reset`, `/llm`, `/gif`, `/search`, `/play`, `/player`,
`/queue`, `/skip`, `/pause`, `/resume`, `/stop`, `/shuffle`, `/clear`, `/move`,
`/repeat`, `/volume`. `/llm mode:off` requires Manage Server; no mode shows status.
`/gif` accepts a `url` or `video` attachment and returns the result privately.
Existing prefix commands and `/nyande` remain available.

The installation needs `applications.commands` and channel permission to use
application commands. If the picker is stale, check startup logs for
`[discord] slash command registration failed` and reopen Discord.
