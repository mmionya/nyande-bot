# nyande-bot

**English** | [Русский](README.ru.md)

`nyande-bot` is a Telegram media downloader with an optional LLM assistant.

## Features

- downloads photos, videos, and carousels from TikTok, Instagram, X/Twitter,
  Xiaohongshu (RedNote), Pinterest, YouTube, and Reddit;
- includes the source post caption or description for every supported platform;
- supports direct image and video links;
- sends carousels as Telegram albums of up to 10 files and validates file-size limits;
- compresses oversized downloaded videos when possible;
- works with OpenRouter and other OpenAI-compatible APIs;
- sends photos, selected video frames, and audio/video transcripts to the LLM;
- reads Telegram stickers, including their visible lettering, and answers voice messages;
- analyzes the selected attachment when a user replies to a file previously sent
  by the bot, including a single file selected from an album;
- provides the LLM with `current_time` and optional `web_search` tools;
- lets the LLM add emoji reactions to messages in Telegram;
- optionally deletes unsupported links posted by non-administrators in groups;
- stores extra allowed domains added through `/allowlink example.com`;
- persists each group's link-deletion setting across restarts;
- accepts Telegram Stars and can display an optional Ko-fi button;
- keeps process statistics and separate LLM history for every chat.

Downloaded media stays in memory for up to 30 minutes, with a shared 64 MiB
cache budget. Files larger than the budget are not retained. Replies to an album
use only the selected attachment. After eviction, the bot fetches that file from
Telegram when it is available.

## Twitter/X text posts and translation in Telegram

Send a public X/Twitter post URL to receive the post even when it has no photos
or videos. Text posts include the full text, author and source URL; long posts
are split into messages. Literal asterisks and code in the original are preserved.

Use `/translate en URL`, reply to a post or text message with `/translate en`,
or supply text directly: `/translate en Привет, мир!`. `/translate` defaults to
Russian. Supported language codes: `ru`, `en`, `kk`, `uk`, `de`, `fr`, `es`, `ja`,
`zh`.

You can also ask the assistant to translate, for example `переведи на английский
URL`, or reply to a message with your request. In Telegram, the LLM uses the
`translate_text` tool for X/Twitter URLs, replied messages and supplied text.
Normal group LLM triggers still apply: mention the bot or use a configured trigger
word when needed.

Translation uses the configured LLM and respects the chat's LLM setting.
The translation model request is separate from conversation history; translation
requests in normal chat remain part of that conversation. `/agent` can also use
the tool, subject to its task limits, including shortened results for very long
text. Use `/translate` when you need a complete long translation. Translating a
URL fetches only the post text, without downloading its media again. Translations
are separate messages; original posts remain unchanged.

## Telegram agent tasks

Use `/agent <task>` for multi-step research, comparison and file preparation,
for example `/agent Compare these three sources and send a Markdown report`.
Attach a supported document or reply to one to include it in the task.
The agent can search the web when `LLM_WEB_SEARCH_ENABLED=true`, read public
webpages, search this chat's archive when enabled, and send UTF-8 text files
(such as Markdown, CSV, JSON or source code; up to 10 files of 1 MiB each).
It does not execute code or create binary Office/PDF documents.

Tasks run in the background. A progress message shows the current step and
tool; `/agentstatus` checks it, and `/agentstop` lets the task's author cancel.
The limits are 12 work rounds plus a final summary, 36 local tool calls and
10 minutes, with one active
task per chat and three across the bot. A final summary identifies when a
step/call limit was reached. Normal chat remains available during a task.
OpenRouter's hosted search has its own limit of one use per model request.
`/llm off`, bans and silent moderation also prevent further agent output.

Agent tasks have separate context from normal conversation history. They use
the explicit request, its reply/attachments and any chat search they perform.
In-progress tasks are not persisted or resumed after a restart; files already
sent remain in Telegram. Existing text formatting also applies to agent replies,
including preserving literal `*single-star text*`.

## Chat games

The `/ttt` and `/checkers` commands also run games directly through Telegram
inline keyboards. Reply to another user's message with the command to challenge
that user.

`/wordle` starts a five-letter English Wordle directly in chat. Everyone gets
the same daily word, while attempts are tracked per user.
The bot sends a PNG progress card after every guess and adds the player's avatar
to the final card.

## Technology

- Go 1.26+ — Telegram bot, downloaders, and LLM client;
- `yt-dlp`, Deno, FFmpeg, and FFprobe — YouTube challenge solving, media
  processing, and downloader fallbacks;
- optional OpenAI Whisper CLI — local audio and video transcription;
- Docker Compose — service orchestration.

## Quick start

You need Podman or Docker with a Compose provider, plus a Telegram bot from
[@BotFather](https://t.me/BotFather).

1. Copy the configuration template:

   ```bash
   cp .env.example .env
   ```

2. Set your Telegram bot token in `.env`:

   ```dotenv
   BOT_TOKEN=123456789:telegram_bot_token
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

### Updating an existing systemd installation

To update the binary of a running Linux x86_64 service, run the command in the
[GitHub release notes](https://github.com/mmionya/nyande-bot/releases/latest)
as root. The installer downloads the prebuilt binary, verifies its SHA-256
checksum and saves the previous binary for rollback.

For the root-owned `nyande-bot.service` installation with working directory
`/root/nyande-bot` and binary `/usr/local/bin/nyande-bot`, run
`bash scripts/update-systemd.sh` as root on Debian/Ubuntu x86_64. An optional
full commit SHA pins the source revision; the default is `main`.

The updater builds in a temporary checkout with one compiler worker, installs
Poppler/FFmpeg and an isolated Python environment for animated stickers, and
keeps the existing working directory, `.env`, data and memory limits intact.
It disables Whisper through an additional systemd environment file; it does
not install Whisper or PyTorch. Existing vision/model settings still apply.
Allow at least 2 GiB of free space for the temporary build.

The existing bot stays running until the replacement is ready. The updater
backs up the binary and its managed systemd settings, checks the new process
for 30 seconds, and rolls back on startup failure. It prints a command for
manual rollback under `/var/backups/nyande-bot/` as well. OS dependency packages
and the isolated sticker environment remain installed after rollback.

## LLM reply formatting

Telegram LLM replies render double-asterisk bold text, underscore italics,
strikethrough, links, quotes, inline code, and fenced code blocks with a language.
Text between single asterisks stays literal, including the asterisks.
Long replies keep their formatting across messages; code indentation is preserved.

## LLM reactions

The `react_to_message` tool lets the LLM choose a standard emoji reaction during
an ordinary conversation, or when asked to react. For example, reply to a message
with “мяу, поставь сердечко на это сообщение”. The tool can target the current
user message or its direct reply parent in the same chat/channel. It allows one
attempt per target per LLM request and reports platform failures to the model.
Custom and paid reactions are not supported.

Existing LLM triggers, cooldowns and per-chat enable/disable settings still apply;
this does not start extra LLM requests for every group message. Telegram must allow
the selected reaction in the chat. No extra API key or model is needed.

## LLM and media analysis

### Telegram music search

Use `/search artist or title` (also `/music`) to browse up to 20 tracks, five
per page. Choose YouTube, YouTube Music, SoundCloud or Bandcamp with the source
buttons, then tap a track to receive an MP3 with Telegram's audio player.
When artwork is available, the bot downloads it, embeds a JPEG cover into the
MP3 without re-encoding the audio, and attaches a thumbnail for Telegram's player.
A missing or invalid cover does not prevent the song from being sent.

`/play title` sends the first result; `/play https://…` accepts a supported track
link, and `/play 2` sends the second result from your latest search in this chat.
Source prefixes are `yt:`, `ytm:`, `sc:` and `bc:`.
For example: `/search sc: ambient`. Search buttons belong to their requester
and expire after ten minutes.

This uses the existing `YTDLP_PATH`, cookie settings and `FFMPEG_PATH`; it works
with the LLM and Whisper disabled. Individual tracks must be at most 30 minutes;
live streams are excluded. MP3 uploads respect `MAX_FILE_SIZE` and Telegram's
50 MiB cap. Music searches and downloads share one extraction slot; temporary
files are removed after downloading or failure.

### Telegram stickers and voice messages

In a private chat, send a sticker or voice message directly. In groups, send it
as a reply to the bot. Sticker replies to downloaded media or quote cards are
ignored, following the same trigger rules as text replies. Voice replies to the
bot's media still work. To ask about someone else's sticker or voice message,
reply to it with your question and a configured trigger word. Unaddressed group
messages keep the existing trigger rules.

With `LLM_VISION_ENABLED=true`, the same `LLM_MODEL` receives the actual sticker
image and reads visible lettering from it. The associated Telegram emoji is not
used to infer the sticker's meaning. Static WebP stickers are converted to PNG;
TGS animations and WebM video stickers are sampled using `LLM_VIDEO_FRAME_COUNT`
(default 3, maximum 6 frames). If full rendering fails, the bot tries the sticker
thumbnail and explicitly tells the model that it only has a preview. No separate
OCR service is required; the configured model must support images.

Voice messages and audio files are transcribed locally by Whisper when
`WHISPER_ENABLED=true`, then the transcript is sent to the LLM. The updated
`.env.example` enables transcription; existing `.env` files must be updated
explicitly. `WHISPER_LANGUAGE=auto` detects the language. Processing has a
three-minute timeout; disabled transcription, empty speech, download failures
and recognition errors now produce a useful reply instead of silence.

Both Docker images include the TGS renderer. Whisper is included in `runtime`;
keep `WHISPER_ENABLED=false` with `runtime-lite` unless you install it separately.
For native installation, install FFmpeg, Python 3, `rlottie-python[full]==1.3.8`
and `openai-whisper` in the Python environment used by the bot. Use
`STICKER_PYTHON_PATH=/path/to/venv/bin/python3` for a dedicated TGS renderer
environment without changing the service's `PATH`. `WHISPER_PATH`
selects the Whisper executable. The first transcription downloads the selected
Whisper model if it is not cached.

### Documents

Telegram accepts **PDF, DOCX, PPTX, XLSX, ODT, ODS, ODP and ODF**
attachments, with a caption or a question replying to the document. In DMs, a
document without a caption gets a brief summary. Group trigger rules still
apply. The current message's attachments take precedence over replied attachments.

The model receives extracted text, including tables, sheet names and slide
boundaries. With `LLM_VISION_ENABLED=true`, embedded raster pictures in Office and
OpenDocument files are also sent as images in the **same request to `LLM_MODEL`**.
PDF pages are rendered to images so the model can see embedded pictures, diagrams
and scans. The configured model must accept image input; a separate vision model
is not required. With vision disabled, only the text layer is available.

Limits: 20 MiB per file (or a lower `MAX_FILE_SIZE`), 40,000 extracted characters
per document and 80,000 per request. Up to
6 pictures or the first 6 PDF pages per document are sent, with 12 images total
per request, resized to at most 1600 pixels on the longest side. The model is told
when text or images are omitted. Office vector drawings/charts and SVG/EMF/WMF
pictures are not rendered; embedded PNG/JPEG/GIF/WebP/BMP/TIFF pictures are supported.
Spreadsheet cells include raw stored values and formulas, not recalculated or
formatted values. Document text/images are request-only; reply to the original
file to ask another question about it. Corrupt, encrypted and oversized files
produce an error instead of being silently ignored.

PDF support requires `poppler-utils` (`pdftotext`, `pdfinfo`, `pdftoppm`), included
in both Docker runtime images. Native installations can use
`sudo apt-get install poppler-utils`. Office/OpenDocument extraction runs in Go;
LibreOffice and additional Python packages are not needed.

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
case-insensitively). Replies to media downloaded and sent by the bot, or to its
quote cards, are handled only when they contain one of those trigger words.

When replying to a message, the LLM also receives that message's text or caption
and author as quoted context, up to 4000 Unicode characters. Only the immediate
parent is included. It is marked as untrusted context rather than a new user
instruction. The quoted context itself is not appended to saved LLM history;
the model's final answer may still refer to it.

`LLM_VIDEO_FRAME_COUNT` controls how many evenly spaced video frames are sent
to the model and accepts values from 1 to 6. `LLM_TIMEZONE` accepts an IANA
timezone such as `Asia/Almaty` or a UTC offset such as `+05:00`.

Chat messages are recorded in an SQLite database at `CHAT_LOG_FILE`.
The LLM can access historical messages via the `search_chat_messages` tool and inspect
recent chat context via `get_recent_messages`. To enable the bot to receive all group
messages (even when unaddressed), disable Privacy Mode in `@BotFather` (`/setprivacy -> Disable`)
or make the bot an administrator in the group. Set `CHAT_LOG_ENABLED=false` to disable chat logging.

In Telegram, `/llm off` disables LLM replies and collection of new messages
in the current chat, including edited messages and media captions. `/llm on`
resumes them; `/llm` shows the current state. Group administrators control the
setting in groups, and private-chat users control their own chat. Previously
stored history is retained. The per-chat choice survives restarts in
`TELEGRAM_LLM_SETTINGS_FILE` (default `.nyande-telegram-llm.json`); Compose
keeps it at `/app/data/telegram-llm.json` in the persistent volume. Existing
reminders and media download commands keep working when the LLM is disabled.

Archive lookup is available to participants only within the current chat. The
chat ID comes from the Telegram message and cannot be selected by tool arguments.
Search returns recent matches for words or a phrase. It scans one chat's stored
messages (normally capped at `CHAT_LOG_MAX_PER_CHAT`) instead of keeping a
plaintext full-text index, so very large archives may take longer to search.

Set `DATA_ENCRYPTION_KEY_FILE` to a separately stored, random **32-byte binary key**
to enable AES-256-GCM encryption of archive content and LLM conversation history.
On Unix, the key must have owner-only permissions (`0600` or `0400`). On Windows,
its ACL must grant access only to the bot account, SYSTEM, and Administrators.
It is never generated or replaced automatically. Keep a protected
backup of the key separately from database backups: losing it makes the encrypted
data unreadable.

Existing plaintext archives and histories are migrated at startup. Archive texts,
author names, usernames and user IDs are encrypted; chat IDs, message IDs and dates
remain SQLite metadata. The old plaintext search index is removed and SQLite is
compacted. History files keep their names but contain encrypted binary data;
preserve their basenames when restoring backups. Missing or incorrect keys and
damaged encrypted records are rejected rather than overwritten with empty data.
On Unix, use private data directories (`0700`) and owner-only data files (`0600`).
On Windows, newly created archive/history files and directories receive a protected
ACL for the bot account, SYSTEM, and Administrators. Existing data directories are
checked; their ACLs are never changed automatically. If the working directory is
shared, set `CHAT_LOG_FILE=.nyande-data/chatlog.db` and
`LLM_HISTORY_FILE=.nyande-data/llm-history.json` and let the bot create the new
`.nyande-data` directory on first start. This does not change the working
directory permissions. History replacement flushes the new file before publishing
it on both platforms. Existing plaintext backups need separate encryption or removal; migration cannot erase
copies in external snapshots or previously freed disk blocks.

This option covers the message archive and short LLM history. Long-term facts,
saved quotes and reminders use separate databases; protect their file permissions
too. Encryption at rest does not hide data from the running bot or a server
administrator with the key. Context needed to answer a request is still sent to
the configured LLM provider.

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

These two special silent-mode commands produce **no chat replies** and run
only when issued from an identified administrator account. Commands sent as a group
or channel are ignored because Telegram hides the sender ID needed for ban checks.
Grants/revocations are logged
as `[moderation] link_permission` with chat, admin and target IDs plus
`allowed=true` / `allowed=false`. Invalid commands and storage failures appear
in technical logs. In other chats these commands do not enable or change moderation.
For a service named `nyande-bot`:

```bash
journalctl -u nyande-bot -f | rg '\[moderation\]|update .* failed'
```


### Allowed-domain scope and migration

`/allowlink example.com` applies only to the current chat. In groups it requires
administrator permission; a private-chat exception does not change any group.
After upgrading, old global entries in `ALLOWED_LINKS_FILE` remain stored but no
longer grant access. The startup log reports this migration. Add each required
domain again with `/allowlink` in the chat where it should be allowed.

## Blocking access to the bot

Set numeric Telegram owner IDs in `.env`: `BOT_OWNER_IDS=123456789`. Separate
multiple IDs with commas and restart after configuration changes. Only these
owners can manage global bans; group administrator permissions do not grant this ability.
Without configured owners, ban management commands are unavailable.

Use `/botban 123456789` and `/botunban 123456789`, or reply to a user's message
with either command without an ID. Usernames are not supported.

Bans apply to an account in every Telegram chat, including DMs. New
messages cannot invoke commands, media downloads, LLM calls, games or music;
buttons and forms are blocked too. Telegram rejects new payments and discards
pending reminders for banned users when they become due. Owners and the bot
itself cannot be banned.

To prevent identity bypasses, Telegram ignores requests sent as a channel or
anonymous administrator: use your own account for commands. Link moderation
still applies to banned users. Automatically forwarded linked-channel posts
remain exempt from deletion, but do not trigger downloads. In silent groups,
ban commands are ignored; use the bot's DM with a numeric target ID. Operations
already started before a ban may finish. A bot ban does not remove someone from
a group or hide the bot's shared messages from them.

Bans persist in `BANNED_USERS_FILE` (default `.nyande-banned-users.json`).
Compose keeps the file in its persistent `/app/data` volume.
Failed saves leave access unchanged and never report success;
an invalid ban file stops startup instead of silently losing the bans.

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
| `/stats` | show a Material 3 Expressive PNG card with statistics for the current process |
| `/reset` | clear LLM history for the current chat |
| `/translate [language] [URL or text]` | translate an X/Twitter post, supplied text or replied message in Telegram |
| `/remember fact` | save a long-term memory for the current user and chat |
| `/memory` | list long-term memories and their ids |
| `/forget id` | delete one long-term memory by id |
| `/forget_all` | delete all long-term memories for the current user and chat |
| `/round` | convert video into a square Telegram Video Note |
| `/voice` | convert audio or video into a Telegram Voice message |
| `/audio` | extract audio from video, audio, a voice message, or a video note as MP3 |
| `/gif` | convert video into a looping Telegram GIF animation |
| `/video` | send a GIF, animation or attached video as a regular MP4 video |
| `/mediainfo` | show technical specs (resolution, FPS, duration, codecs) of photo/video/audio |
| `/allowlink example.com` | allow a domain only in this chat; group administrators only |
| `/linkdelete [on\|off]` | configure unsupported-link deletion for this group |
| `/permitlinks` / `/revokelinks` | reply to a participant to grant/revoke links in silent mode (administrators only) |

In Telegram, attach media with `/audio` in its caption or reply with `/audio`
to a video, audio file, voice message, or video note. The bot sends an MP3 file.
The command uses FFmpeg from `FFMPEG_PATH` and applies `MAX_FILE_SIZE` to both
the source and the result. URLs are not accepted by this command.

## Local validation

```bash
gofmt -w cmd internal resources
go test ./...
go vet ./...
```

With the Python `yt-dlp` package installed, check Instagram photo extraction with
`python3 internal/downloader/instagram_photos_test.py`.

The bot requires `yt-dlp` with its EJS component, Deno, `ffmpeg`, and `ffprobe`
at runtime. The Docker image includes all of them. Local transcription
additionally requires the `whisper` CLI.

## Oversized videos

`MAX_FILE_SIZE` is the outgoing file-size limit in bytes (default 50 MiB).
Videos that already fit are sent unchanged. YouTube still tries smaller download
formats first. If a downloaded video is too large, the bot can compress it to
MP4 with H.264 video and AAC audio, reducing resolution when needed.

Compression is enabled by default (`VIDEO_COMPRESSION_ENABLED=true`). Source
videos are stored in temporary files on disk, with a separate
`VIDEO_MAX_SOURCE_SIZE` limit of 300 MiB (configurable from 1 KiB to 2 GiB).
Only one compression runs at a time across the bot process. By default, videos
longer than 10 minutes are not compressed (`VIDEO_MAX_DURATION_SECONDS=600`),
and compression has a five-minute timeout (`VIDEO_COMPRESSION_TIMEOUT_SECONDS=300`).
These time limits must be positive; invalid values fall back to the defaults.
The bot reports an error if it cannot produce a suitable file within the limits;
it does not split videos into parts. Compression can reduce quality and delay
delivery. `FFMPEG_PATH` and `FFPROBE_PATH` select the required executables.

## Downloads without manually exported cookies

Leave `YTDLP_COOKIES_FILE` and `YTDLP_COOKIES_FROM_BROWSER` empty for public
videos. If the normal Instagram download fails, the bot tries vxinstagram
for Reels and IGTV. This fallback sends the video URL to that service and
depends on its availability; carousels keep using the normal downloaders.
Instagram posts use an embedded yt-dlp plugin to download photos and videos
in carousel order. The plugin needs no separate installation or cookie file.

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
yt-dlp discovers the plugin and provider automatically for video and music
downloads. No extra daemon or bot settings are needed. Keep plugin and
provider versions matched when updating. Script startup adds latency to token
generation; upstream offers an HTTP service for higher concurrency. These routes
do not remove login requirements caused by IP restrictions or private content.

## Repository structure

```text
cmd/nyande-bot/     application entry point
internal/bot/         commands, games, moderation, media, and state
internal/telegram/    Telegram Bot API client
internal/music/       music search and metadata
internal/downloader/  platform downloaders and yt-dlp fallback
internal/llm/         OpenAI-compatible client and tools
resources/            shared strings.json
```

Only download and share content that you own or have permission to use.

## Quote collection

Reply with `/quote` to a text message or media caption to save it and receive a
PNG card with the author's avatar filling the background, softly blurred and
darkened behind white Nunito SemiBold text with a black outline and a `© Author` signature.
Without an available avatar, the card uses a dark background. Forwarded
messages use the original author when Telegram provides it. Quotes support up to
1,200 characters. Standard Unicode emoji are rendered in color, including flags,
skin tones and joined sequences, in both the quote and the author's name.
Rich formatting, animation and Telegram custom emoji artwork are not preserved.
Emoji images are bundled locally, so rendering does not need a CDN connection.

Emoji artwork: [Twemoji 17.0.3](https://github.com/jdecked/twemoji/releases/tag/v17.0.3)
by Twitter, Inc. and Twemoji contributors, licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). The embedded archive
includes the original graphics license and attribution; PNG artwork is unmodified.

- `/quote random` — a random quote from this chat.
- `/quote list` — the latest 10 quotes and their IDs.
- `/quote 12` — show quote #12.
- `/quote delete 12` — delete a quote as its author, saver, or a chat administrator.
- `/quote trigger цитата` — set a word that creates a quote when sent as a reply.
- `/quote trigger` — show the current trigger; `/quote trigger off` disables it.

Send only the configured word in reply to the message you want to quote;
matching ignores case and surrounding spaces. Triggers are separate for each
chat and survive restarts. Group administrators can configure them; in private
chats you can configure your own. Triggers start disabled and accept up to 32
letters, digits, dots, underscores or hyphens (for example, `/quote trigger .й`).
The bot must receive ordinary group
messages (administrator access or disabled Privacy Mode in BotFather).

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
three-minute timeout plus `VIDEO_COMPRESSION_TIMEOUT_SECONDS` when compression
is enabled (eight minutes with the defaults); an earlier request cancellation
still stops it. Repeated calls for the same URL in one request do not send
duplicate files. Failures and partial deliveries are reported back to the model.
Availability still depends on the source site and downloader configuration.

Telegram automatically sends downloaded videos without an audio track and
X/Twitter GIF posts as looping animations. Videos with an audio track remain
videos, even if the track is silent. Compatible H.264 MP4 files are uploaded
without re-encoding; other silent formats are converted when possible. If media
inspection or conversion fails, the original media is sent. Animations are sent
separately from photo/video albums, preserving the post's order and caption.

Reply to an animation with `/video` to receive a regular MP4 video, or ask the
assistant “Send this as a video.” The `convert_to_video` tool uses the same
conversion. It preserves the duration and copies compatible video streams
without re-encoding. The original download is used while it remains cached;
otherwise the bot fetches the attached/replied file from Telegram, subject to
Telegram's file download limit. `/video` also accepts attached GIFs and videos.
Both FFmpeg and FFprobe are required, configurable through `FFMPEG_PATH` and
`FFPROBE_PATH`. Conversion respects `MAX_FILE_SIZE` and Telegram's upload limit.

To convert an attached video or a video you reply to, ask “Make this into a GIF.”
The Telegram LLM can call `convert_to_gif`, which uses the same conversion as
`/gif` and sends a silent MP4 animation. It reuses the current request's collected
attachments and attempts conversion once per request, including failed deliveries.
In groups, include an `LLM_TRIGGER_WORDS` trigger when replying to media the bot sent.

## Create files with the LLM

In Telegram, ask “Write a Python script to sort a CSV and send it as a file”
or “Create an HTML page with a timer.” The model writes the content and calls
`create_file`; the bot delivers the file as an attachment replying to the message.
Any extension is supported for UTF-8 text, including `.java`, `.cpp`, `.cs`,
`.kt`, and `.kts`, as well as Markdown, JSON, and CSV. Specify an exact filename
such as `Main.java`; otherwise the model chooses a suitable name and extension.
Ask explicitly if you want only inline code or an explanation.

Each request supports up to 10 files of at most 1 MiB each. Indentation and
newlines are preserved. Files are built in memory from the model's output; the
tool cannot read server files or execute code. Larger source files may require
increasing `LLM_MAX_TOKENS`. Each filename is attempted once per request, including
failed deliveries, so repeated tool calls do not resend the same file.

## Activity logs

`docker compose logs -f bot` shows `[command]` events (command name, chat, user,
message and duration), `[media]` events (download source, file type/name/size,
and delivery outcome), `[tool]` events (LLM tool execution), and `[search]`
events (local search results). Command arguments and tool response contents are not included in these
events; the download URL field omits credentials, query parameters and fragments.
`[tool] returned` means the tool returned; actual file delivery is recorded by
`[media] sent` or `[media] send_failed`.

TikTok requests can use `search_tiktok` when web search is enabled. It keeps only unique individual video URLs, using OpenRouter search citations with a scoped DuckDuckGo fallback. `download_media` can then download and send a selected result. This searches indexed pages; freshness, popularity and download availability are not guaranteed. Hosted search adds an LLM request; no new API keys are required.
