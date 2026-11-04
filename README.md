# q2-applemusic

Apple Music for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod), as [gamdl](https://github.com/glomatico/gamdl) does Apple Music: browse your Apple Music library and Apple Music's catalogue on the Q2, and what you pick is downloaded to the card as AAC 256 kbps, decrypted and tagged, lyrics included. Every song then plays from the card through the Q2's own player, as local music, with no Wi-Fi. Or switch to **Stream** mode, and a press plays the song straight away without keeping it. It needs an Apple Music subscription and a Q2 Pod build that supports card apps (its **System settings → Apps** lists Apple Music).

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **Apple Music**.

**By hand:**

1. Download `q2-applemusic-*.zip` from the [latest release](https://github.com/group-a-study/q2-applemusic/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.applemusic/q2-applemusic`, `.applemusic/run`, `.applemusic/q2app` and its icons `.applemusic/icon.png` and `.applemusic/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

Apple Music then has a row in **Streaming**, with its icon.

## Sign in

Open **Streaming → Apple Music** and press **Sign In from Your Phone**: the Q2 shows a QR code, an address and a PIN. Scan the code with your phone (or open the address on a computer on the same Wi-Fi and type the PIN). The page explains how to copy the `media-user-token` cookie from music.apple.com in a computer's browser, and has a box to paste it in (a whole `cookies.txt` exported from music.apple.com works too). It is checked with Apple (an account without a subscription is refused), then saved as `.applemusic/media-user-token` on the card. Treat it like a password. **Account → Sign Out** removes it; your downloads stay. The cookie lasts months; when Apple stops taking it, the Q2 asks you to sign in again.

**Or sign in with a cookies file:** export music.apple.com's cookies from a computer's browser as `cookies.txt` (Netscape format, as gamdl takes them: Firefox's *Export Cookies*, Chromium's *Get cookies.txt LOCALLY*), and put the file on the card: at its root, in `Apple Music/` or in `.applemusic/`, under any name with "cookie" in it ending `.txt`. Apple Music finds it when it starts and every 10 seconds while it runs, checks its `media-user-token` with Apple and signs in by itself, no phone needed. The file is then deleted from the card, as a browser's export can also hold your Apple ID's own sign-in; your token is kept as above. A file Apple refuses stays, and the menu says why. A newer file replaces an expired sign-in the same way.

## Use

Apple Music's page has **Downloaded Songs**, **Recently Added**, **Songs** (your library's), **Playlists** (Top Charts first, then yours), **Albums**, **Artists**, **Search Apple Music**, **Downloads**, **Mode** (Download or Stream), **Organize Files**, **Account** and **Stop Apple Music**.

- Press a song to download it. **Download All** downloads a whole list, and so does holding **Play/Pause** on a playlist or an album.
- **Albums → Download All Albums** downloads every album in your library, and **Playlists → Download All Playlists** every playlist. They are fetched one at a time: the next album's songs are asked for once the last one's are on the card, so the Q2 never has hundreds of lists in hand, and Apple sees one song at a time at a person's pace. **Downloads** shows how many are left and has **Stop Download All**.
- A song already on the card says so, and pressing it plays it from the card.
- Downloads carry on with the page closed, one song at a time, and wait by themselves while you're offline. The Q2 stays awake while a song downloads.
- A song that fails is tried 3 more times (5, 10 and 15 s apart), then the queue moves on without it. Each one left out is listed in `Apple Music/Not Downloaded.txt`: tab-separated (a spreadsheet opens it), with its artist, album, track, the playlist it came from, why, its music.apple.com link and when. **Downloads → Try the Failed Again** tries them again, and a song that downloads leaves the list; **Clear the Failed** empties it. Being offline, signed out or the card being away pauses the queue instead, without using up a song's tries.
- Run **Library → Update Local Music** for your downloads to show in Artists, Albums and Coverflow too.
- Each song is tagged with its lyrics when Apple has them: time-synced ones as LRC (`[01:03.01]A line`), shown in time in Now Playing, else the plain text.
- Every song is AAC-LC 256 kbps (Apple's web stream, 44.1 kHz), in a plain M4A indexed as iTunes writes them, so it also plays on an iPod. A song at 48 kHz or more, which iPods skip on, is kept apart in `Apple Music/48khz/`, again as artist, album and song (`48khz/<artist>/<year> - <album>/`, a playlist's in `48khz/Playlists/<name>/`); **Downloaded Songs** lists them under **48 kHz and Higher**. With Apple's stream at 44.1 kHz, that folder is a safety net.
- Your account's country (its storefront) decides what Apple licenses: a song it doesn't license there fails, saying so, and an album none of whose songs plays there says that.
- A compilation in your library (a soundtrack, *Various Artists*) whose songs Apple matched to their own releases, as it does where the compilation itself isn't licensed, downloads as that one album: its name, *Various Artists* as album artist, its year and cover, your library's track numbers, in `Various Artists/<year> - <album>/`, so Coverflow shows one album, not one per song. A song on it and on its own album you downloaded is on the card in both; a copy an earlier version filed alone under its own album (v1.6 and before did) is removed once the compilation's is saved.

## Playlists are kept as albums

A song downloaded from a playlist (pressed in it, its **Download All**, or **Download All Playlists**) goes into that playlist's own album, whatever Organize Files says:

    Apple Music/Playlists/Road Trip/01 - Pendulum [104].m4a
    Apple Music/Playlists/Road Trip/02 - Valerie [102].m4a
    Apple Music/Playlists/Road Trip/cover.jpg

Each song is tagged with the playlist's name as its album, `Playlists` as its album artist (so **Library → Artists** lists one *Playlists* artist holding them all), and its place in the playlist as its track number. Each song keeps its own cover, so Now Playing shows the song's; the playlist's cover is saved beside them as `cover.jpg`, which Q2 Pod shows for the album in Coverflow and Albums (group-a-study's Q2 Pod test builds from 4 November 2026 on; others show the first song's cover there). A song on a playlist and on an album you downloaded is on the card twice, once in each. A hidden `.q2-playlist` file names the playlist, so a renamed playlist keeps its folder.

## Stream or download

**Mode: Download** (the default): a press downloads the song to `Apple Music/`, as above.

**Mode: Stream**: a press plays the song, and the rest of its list after it, straight away. **Play All** plays a list from its start. Songs are fetched a couple ahead of the one playing into a hidden folder (`.applemusic/stream`) and deleted as you go on, so nothing joins your library and the card doesn't fill up. Holding **Play/Pause** on a song still downloads it. The setting is saved in `.applemusic/mode`.

## Organize Files

**On** (the default): each new song is filed by its album's artist, then its album's year and title, numbered in track order. The year comes first, so an artist's albums list oldest first wherever they are browsed by name:

    Apple Music/Broadcast/2005 - Tender Buttons/01 - Tears in the Typing Pool [101].m4a
    Apple Music/Various Artists/2019 - Some Compilation/2-07 - Song [23456].m4a    (disc 2 of 2)

**Downloaded Songs** then lists your artists, each artist's albums oldest first (with the year beside each), and each album's songs in track order. A press plays the album from that song on.

**Off:** every new song goes straight into `Apple Music/`, as `Title - Artist [id].m4a`. **Downloaded Songs** lists them newest first. Playlists still get their albums.

The setting is saved in `.applemusic/organize` and applies to new downloads.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace `.applemusic/q2-applemusic`, `.applemusic/run`, `.applemusic/q2app`, `.applemusic/icon.png` and `.applemusic/icon-40.png`. Either way, `.applemusic/media-user-token` (your sign-in), `.applemusic/organize`, `.applemusic/mode`, `.applemusic/queue.json` (songs waiting to download), `.applemusic/bulk.json` (Download All's lists to go) and `.applemusic/failed.json` (the songs in Not Downloaded.txt) are kept.

To remove it, press **Apple Music** twice in **System settings → Apps**, or delete `.applemusic`. Your songs in `Apple Music/` stay.

## How it gets the music

As gamdl's `aac-web` codec: the web player's developer token, read from music.apple.com's own script (and kept in `.applemusic/devtoken`), and your `media-user-token` sign each request. The playback service gives a song's HLS stream, a fragmented MP4 encrypted with CENC, and its Widevine key ID; a license for it comes from Apple's web player license service through a Widevine CDM (`internal/widevine`, a Go port of what pywidevine does), with gamdl's built-in device. The first seconds of every stream are encrypted with Apple's fixed prefetch key, the rest with the song's. Each sample is decrypted (AES-CTR) and written into a plain M4A with iTunes tags (`internal/mp4`). Should Apple stop taking the built-in device, put another `.wvd` file on the card as `.applemusic/device.wvd`.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.applemusic` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-applemusic/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). How it works on the Q2: [contrib/shanlingq2/README](contrib/shanlingq2/README).

Tests: `go test ./...` runs everything against a fake Apple (its streams encrypted as Apple's are, its keys from a real Widevine license exchange) and, where ffmpeg is installed, checks a real AAC encode decodes to the same audio after decryption. `AM_LIVE_COOKIES=cookies.txt go test -run TestLive ./internal/apple` downloads one song from Apple with a signed-in account's cookies.

## License

[MIT](LICENSE). It follows [gamdl](https://github.com/glomatico/gamdl) (MIT), whose built-in Widevine device it carries (`internal/widevine/android.wvd`). Mozilla's CA bundle (`internal/tlsroots/cacert.pem`) is MPL-2.0.
