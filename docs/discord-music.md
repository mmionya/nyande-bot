# Discord music

Join a regular voice channel, then use these commands in a text channel on the same server:

- `!search song`: five YouTube results; `!search sc: song` searches SoundCloud.
- `!play 2`: select a result from your search in the same text channel (expires after 10 minutes).
- `!play song` or `!play https://youtu.be/...`: play a song or enqueue it.
- `!queue`, `!skip`, `!pause`, `!resume`: inspect and control playback.
- `!stop` or `!leave`: clear the queue and disconnect.

Commands use `DISCORD_COMMAND_PREFIX`. Playback controls require the user to be in
 the bot's voice channel. The bot needs View Channel, Connect, Speak, and text message
permissions. There is one queue per server with up to 50 waiting tracks; queues are
not persisted. The bot disconnects when the queue ends. Stage channels are unsupported.
Playlist URLs select only the first track. Music works with the LLM disabled.

Run the bot and audio service with:

```bash
docker compose --profile music up -d --build
```

For a bot managed by systemd, start only the audio service:

```bash
docker compose --profile music up -d lavalink
```

Set `LAVALINK_ADDRESS=localhost:2333`, `LAVALINK_PASSWORD=youshallnotpass` and
`LAVALINK_SECURE=false` in the bot's environment, rebuild with Go 1.25+, replace the
service binary and restart it. The password must match on both sides. Compose uses
`lavalink:2333` by default and binds the host port only to localhost. Remote nodes
use a `host:port` address and `LAVALINK_SECURE=true` for HTTPS/WSS. Without Compose,
an empty address leaves music unconfigured. Connections are established lazily.

The included Lavalink 4.2.2 configuration uses YouTube plugin 1.18.2. Lavalink 4.2+
provides the [DAVE support](https://lavalink.dev/changelog/v4) required by
[Discord voice](https://discord.com/blog/bringing-dave-to-all-discord-platforms).
See `resources/lavalink.yml`. YouTube may restrict datacenter addresses; inspect
`docker compose logs -f lavalink` and the
[plugin configuration](https://github.com/lavalink-devs/youtube-source#plugin), or
try SoundCloud via `!play sc: song`. Connection failures may clear the queue; start
a track again after recovery. Playback errors are reported in the original text channel.
