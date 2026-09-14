# Discord music

Join a regular voice channel, then use these commands in a text channel on the same server:

- `!search song`: five YouTube results. Use `ytm:` for YouTube Music, `sc:` for SoundCloud or `bc:` for Bandcamp, e.g. `!search bc: song`.
- `!play 2`: select a result from your search in the same text channel (expires after 10 minutes).
- `!play song` or `!play https://youtu.be/...`: play a song or enqueue it. Source prefixes also work with `!play`. Links from YouTube, YouTube Music, SoundCloud and Bandcamp are accepted.
- `!queue`, `!skip`, `!pause`, `!resume`: inspect and control playback.
- `!stop` or `!leave`: clear the queue and disconnect.

Commands use `DISCORD_COMMAND_PREFIX`. Playback controls require the user to be in
 the bot's voice channel. The bot needs View Channel, Connect, Speak, and text message
permissions. There is one queue per server with up to 50 waiting tracks; queues are
not persisted. The bot disconnects when the queue ends. Stage channels are unsupported.
Playlist URLs select only the first track. Music works with the LLM disabled.

## Direct voice: no Java or Docker required

The default `DISCORD_MUSIC_BACKEND=direct` uses [DisGo](https://github.com/disgoorg/disgo)
for Discord voice and [dave-go](https://github.com/thomas-vilte/dave-go) for DAVE.
`yt-dlp` resolves media; FFmpeg streams 48 kHz stereo Opus at 64 kbit/s. There is
no Java service or native libdave dependency; the bot still builds without CGO.

Install FFmpeg with `libopus`, a current `yt-dlp[default]` (including EJS), and
Deno on the service user's PATH; see [yt-dlp dependencies](https://github.com/yt-dlp/yt-dlp#dependencies).
For example, on Debian/Ubuntu:

```bash
sudo apt-get update
sudo apt-get install -y ffmpeg python3-venv
python3 -m venv ~/.local/share/nyande-media
~/.local/share/nyande-media/bin/pip install -U 'yt-dlp[default]'
```

Configure the bot's environment (replace `USER` with the installation user):

```dotenv
DISCORD_MUSIC_BACKEND=direct
MUSIC_MAX_PLAYERS=1
YTDLP_PATH=/home/USER/.local/share/nyande-media/bin/yt-dlp
FFMPEG_PATH=/usr/bin/ffmpeg
```

`YTDLP_PATH=yt-dlp` also works if it is already on the service PATH. Existing
`YTDLP_COOKIES_FILE` / `YTDLP_COOKIES_FROM_BROWSER` settings are reused. Rebuild
with **Go 1.26+**, install at the service's `ExecStart` path, and restart:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o nyande-bot ./cmd/nyande-bot
# Install the binary at your service's ExecStart path.
sudo systemctl restart nyande-bot
sudo journalctl -u nyande-bot -f
```

Use your actual service name. On a 512 MB host, build elsewhere for the host's
OS/architecture and copy the binary.

### Migration and resource limits

Set `DISCORD_MUSIC_BACKEND=direct` and restart the updated bot. `LAVALINK_*` values
are ignored in this mode. Stop the old Java service, e.g.
`sudo systemctl disable --now lavalink` if that is its unit name, or
`docker compose --profile music stop lavalink` for the old container.

`MUSIC_MAX_PLAYERS=1` permits one concurrent voice player across all guilds.
Music searches and stream resolution share one yt-dlp process slot. Search
returns up to 20 tracks; FFmpeg uses one encoding thread and small streaming
buffers. This removes JVM overhead but does not guarantee that the OS, the bot,
media downloads, Whisper and other services fit in 512 MB. Keep
`WHISPER_ENABLED=false` on a small host. Optional `GOMEMLIMIT=128MiB` is a soft
Go GC target, not a cap on FFmpeg, yt-dlp or Deno. Inspect the whole service:

```bash
systemctl show nyande-bot -p MemoryCurrent -p MemoryPeak
systemd-cgtop
```

Pause stops frame consumption. Volume changes restart FFmpeg at the current
position and may briefly interrupt audio; live streams reconnect at the live
edge. Stop, skip and shutdown cancel and reap child processes. An external
voice disconnect or channel move clears the queue; run play again to reconnect.

### Docker

The image already includes yt-dlp, FFmpeg and Deno. Set `DOCKER_TARGET=runtime-lite`,
`WHISPER_ENABLED=false`, `DISCORD_MUSIC_BACKEND=direct` and `MUSIC_MAX_PLAYERS=1`,
then run `docker compose up -d --build bot`. The `music` profile is unnecessary.

### Sources and troubleshooting

YouTube and YouTube Music search through yt-dlp; SoundCloud uses `scsearch`;
Bandcamp uses its public track search page. Prefixes `yt:`, `ytm:`, `sc:`, `bc:`
and direct HTTPS links work. Metadata availability varies; Bandcamp search may
omit durations. Sites may restrict datacenter addresses or require cookies.
Bandcamp browser challenges produce a search error. Update yt-dlp, check cookies
and Deno, or try a different source. Read `journalctl -u nyande-bot -f` or
`docker compose logs -f bot` for diagnostics.

The optional legacy transport remains available with
`DISCORD_MUSIC_BACKEND=lavalink`. Only that mode reads `LAVALINK_ADDRESS`,
`LAVALINK_PASSWORD` and `LAVALINK_SECURE`. For Compose, start the legacy node via
`docker compose --profile music up -d lavalink`, set
`LAVALINK_ADDRESS=lavalink:2333`, and use `resources/lavalink.yml`. The direct
mode does not need this service.

## Slash-command music browser and player

The updated bot registers `/nyande` on startup. Install the app on the server
with the `applications.commands` scope and grant Send Messages and Embed Links
in the text channel.

`/nyande search` opens a modal. Supply optional `query` and `source` arguments to
search directly. Sources: YouTube Music, YouTube (default), SoundCloud, Bandcamp.
Results are private to the requester: up to 20 tracks in direct mode, 10 per page, with titles,
authors, durations and available artwork. Multi-select across pages, then press
Add Selected; tracks are queued in their original list order. Repeated clicks do
not duplicate the batch. A batch exceeding queue capacity is rejected entirely.
Back opens source selection; New Search opens another modal. Searches expire
after 10 minutes and do not survive restarts.

`/nyande play` plays or queues one result. `/nyande player` displays a public
player card; selecting tracks or using slash play also creates one. There is one
card per server in its original text channel. Progress updates roughly every
15 seconds, based on sent audio frames in direct mode, and controls refresh the card.
Buttons provide pause/resume, skip, repeat, shuffle, stop, volume ±10 and queue.
Controls are disabled after playback ends; old cards are invalid after restart.
If a card is deleted, invoke `/nyande player` again.

Additional subcommands: `queue`, `skip`, `pause`, `resume`, `stop`, `shuffle`,
`clear`, `move from:1 to:3`, `repeat mode:off|track|queue`, `volume value:50`,
and `help`. Volume ranges from 0 to 100 and starts at 50. Clear retains the
current track. Move positions refer only to queued tracks. Skip bypasses track
repeat. Playback changes and enqueueing require sharing the bot's voice channel;
searching and viewing the queue do not.

Prefix commands remain available, including `!shuffle`, `!clear`, `!move 1 3`,
`!repeat off|track|queue`, and `!volume 50`. This browser searches tracks;
artist/album/playlist collection search is not implemented.

## Validation

Tests cover queue/UI behavior, races, Ogg/Opus parsing, pause/volume, process
cancellation and local UDP transport encryption. Live YouTube, YouTube Music
and SoundCloud searches returned 20 results each; a real SoundCloud stream was
encoded into 100 Opus frames. Bandcamp returned a browser challenge. A live
Discord voice connection and DAVE negotiation were not exercised in this environment.
A separate synthetic FFmpeg encoding run peaked around 51 MiB RSS; this excludes
the Go bot, yt-dlp, Deno and the OS and is not a 512 MB host guarantee.
