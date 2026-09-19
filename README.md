# ripperX

A web interface and HTTP API for the optical drives of a machine: what they
are and what they can do, what is in them, the files on that disc, the audio
on it, an exact image of it, and — when the drive can write — a disc burned
from an image and then read back and checked.

One static binary. No libcdio, no SANE, no Python, no mount, no root if the
service account is in the `cdrom` group.

## What it does

- **Lists every drive** and asks each one what it is, in SCSI MMC:
  `GET CONFIGURATION` for the disc kinds it handles and the CD/DVD
  capabilities page for its buffer, speeds and loading mechanism. The answer
  is shown as two plain lists — what the drive **can** do and what it
  **cannot** — so a refusal is as visible as a feature.
- **Shows the disc**: its kind, whether it is blank, appendable or closed,
  its tracks, its size as an `.iso` and as an `.img`, and the ISO 9660
  volume's own label, publisher and date.
- **Rips**
  - `.iso` — the 2048-byte data sectors, exactly what `dd` would give.
  - `.img` — the full 2352 bytes of every sector as they are on the disc,
    with a `.cue` written beside it.
  - `.wav` — one file per audio track. The sectors of an audio CD are
    already PCM, so a header is put in front of them and nothing is decoded
    or re-encoded.
  - **single files**, straight off the disc's filesystem. One file is
    written as itself; several are wrapped in the archive format you pick.
- **Checks a disc's health.** Reads every sector and counts the bytes the
  drive's error correction could not fix — C2 error pointers, the only
  portable measure there is. A disc reads perfectly right up until it does
  not; a rising error rate is what shows the decline while there is still
  time to copy it. Results are graded, mapped across the disc, and **kept in
  a SQLite database**, so the same disc checked a year apart can be
  compared — it is recognised by a fingerprint of its table of contents and
  volume, not by its name.
- **Browses the disc** without mounting it, reading the directory records
  where they lie. Joliet and Rock Ridge names are used where the disc has
  them, so what you see is what the disc's author saw rather than the 8.3
  version.
- **Downloads a folder as one archive** — zip, tar, tar.gz, tar.bz2 or
  tar.xz — produced as the disc is read, so nothing is staged on the server
  first however large the folder is.
- **Plays media in the browser**: an audio track or a media file on a data
  disc, streamed from the disc with byte ranges honoured, so seeking in the
  browser seeks the laser. There is an `.m3u` for VLC, with a token in each
  URL when the server asks for a login.
- **Burns a disc** from an image, with every check that can be made before
  the laser is switched on, and — unless told not to — reads every sector
  back afterwards and compares it with the image, byte for byte and by
  SHA-256.
- **Adds files to a disc that is not closed.** A written disc usually cannot
  be burned and very often can be appended to, which is a different question
  and gets its own answer. Nothing already there is erased and the files that
  were on it stay visible, because the existing filesystem is read and added
  to rather than replaced. Each appended file is read back off the disc and
  its SHA-256 compared with what was sent.
- **Reads multi-session discs properly**, from the newest session rather than
  the first — which is the difference between seeing what is on a disc and
  seeing what was on it before anything was added.
- **Opens and closes the trays** from the page, so a machine in another room
  is still usable.
- **Keeps images** in a flat directory, either local or on an SMB share
  ripperX talks to itself. Upload to it from the browser, download from it,
  delete, and convert a raw `.img` to a burnable `.iso`.
- **Shows everything that is happening**, including downloads. A folder
  being sent to a browser holds the drive for minutes, so it is a job like
  any other: it appears in the list with its progress and an estimate, and
  Stop ends it. A rip asked for while one is running is refused with a
  reason rather than left to block.
- **Survives being watched from several places.** Every page sees the same
  drives and the same jobs over server-sent events, so a rip started on a
  laptop is visible on a phone, and two people cannot start the same rip
  twice without noticing.
- **Survives a restart.** Finished jobs and every surface scan go into the
  database; only a job that was still running is lost, and that is not
  something to pretend survived.

## Running it

```
ripperx -addr 0.0.0.0:8080 -out /var/lib/ripperx
```

Then open `http://<host>:8080`. The HTTP interface documents itself at
`/docs`.

ripperX needs read and write access to the drive nodes — membership of the
`cdrom` group, or root. Burning additionally needs `xorriso` on `PATH`
(`cdrecord` and `wodim` are used if it is not there):

```
apt install xorriso        # Debian, Ubuntu
dnf install xorriso        # Fedora
```

`examples/systemd/ripperx.service` runs it as a confined service, and
`examples/ripperx.conf` documents every setting. Anything in the settings
file can also be a command-line flag, and the flag wins; the three passwords
are the exception, because an argument is readable by every user on the
machine through `/proc`.

### A login

With `auth-user` set and `auth-password` in the settings file, ripperX serves
a login page and issues a short-lived access token and a long-lived refresh
token, both as cookies and both in the response body for scripts. The same
credentials work for the page and for `Authorization: Bearer`.

```
auth-user = ripper
auth-password = the password
```

The settings file must be mode 600 for ripperX to read a password out of it.

### An SMB share instead of a local directory

```
smb-address = //nas.lan/media/discs
smb-user = ripperx
smb-password = ...
```

ripperX speaks SMB itself, so this needs no `cifs-utils`, no mount unit and
no root. A share that cannot be reached fails at startup rather than after a
rip that took twenty minutes.

## How it works

ripperX talks to the drives directly, in SCSI MMC over Linux's `SG_IO`
ioctl — the same interface `cdrecord` and `xorriso` use underneath. That is
the only way to ask a drive what it is, to read a sector as the 2352 raw
bytes it is on the disc rather than the 2048 the kernel hands back, and to
tell a blank CD-R apart from an empty tray.

Three packages:

| | |
| --- | --- |
| [`mmc`](mmc/) | the drive itself: capabilities, disc and track information, data and raw reads, sense decoding, tray and speed control |
| [`iso9660`](iso9660/) | the filesystem on a data disc, read without mounting it: ISO 9660 with Joliet and Rock Ridge |
| [`cmd/ripperx`](cmd/ripperx/) | the server, the jobs, the health scan, the history database, the image store and the web UI |

**Writing a disc is the one thing ripperX does not do itself.** Getting
`MODE SELECT`, the write parameters page, the track descriptors and the close
sequence right for a particular drive's firmware is decades of accumulated
knowledge, and getting any of it wrong costs a disc that cannot be
un-ruined. So the write is handed to xorriso or cdrecord. What ripperX
contributes is everything either side of it: refusing a burn that cannot
work before a disc is spoiled, and proving afterwards that what is on the
disc is what was meant to be.

### What a health check measures

A CD's error correction is strong enough to hide a great deal of damage. By
the time a sector actually fails to read, the disc has been declining for
years — so "it still reads" says almost nothing about how long it will go on
reading.

C2 error pointers are what the drive knows and does not otherwise tell you:
for every byte of every sector, whether the error correction had to give up
on it. A disc with a rising C2 rate reads perfectly today and should be
copied this month.

The scan reports:

| | |
| --- | --- |
| `pristine` | not one uncorrected byte |
| `good` | under 0.01% of sectors flagged — normal for a disc that has been used |
| `worn` | 0.01% to 1% — it still reads, but it is going; copy it |
| `degraded` | over 1% — close to failing; copy it today |
| `failing` | sectors that cannot be read at all; that data is already gone |

with a map of where on the disc the damage is — left is the middle, right is
the outer edge, which is where a disc rots first and so tells manufacturing
decay apart from handling damage. The map is drawn as the scan reads, with
the part it has not reached hatched rather than shown as clean: a strip that
implies a clean bill of health for sectors nobody has looked at is a lie told
by omission.

A drive that declines a C2 read (they all do within a few sectors of the
lead-out) has that sector retried without the flags and then as plain data:
only a sector that no read will produce is counted as lost.

**On a DVD this measurement does not exist, and ripperX says so rather than
inventing one.** There is no portable equivalent of C2 for DVD: the tools
that plot PI and PIF error rates do it with vendor-specific commands that
differ per manufacturer and per firmware. What is left is what any drive
will tell you — which sectors will not read, and where the drive slowed
down, because a drive reading slowly is a drive retrying internally. Those
stretches are marked on the map, which on a DVD is the only mark it gets.

So a scratched DVD whose error correction is still coping reads back
perfectly and scans as healthy. That is not the scan being wrong; it is the
disc genuinely still being readable, with no way to see how much margin is
left. Treat a DVD's clean result as "it reads today", and a CD's as a
measurement.

Read speed is a noisy signal and is treated as one. Three rules keep it
from crying wolf, each of which was added after it did:

- It is compared against the blocks either side rather than against the
  whole disc. An optical drive spins at a constant angular velocity, so a
  read from the centre outwards gets two or three times faster on its own,
  and against one figure for the whole disc that ramp looks like damage.
- The first seconds of a scan are not timed at all. The drive is stopped
  when a scan starts and those reads wait for the motor, coming back below
  even 1x and differing from run to run. The sectors are still read and
  still checked; only their timing is discarded.
- A slow stretch has to be several blocks long. A scratch is physical and
  spans a contiguous piece of track, which at 128 kB a block is several of
  them; one slow block on its own is the host being busy.

Single-sector reads are never timed either - those happen when a failed
block is being narrowed down, and the cost of the command rather than the
disc dominates them.

Scan two years apart and `/api/discs` says which way each disc is going, in
words.

### Appending

A disc reports how much room it has left and whether anything more can be
written to it, so a DVD-R with a third of a gigabyte going spare is not a
disc to throw away. `POST /api/append` writes a further session:

```sh
curl -s -XPOST localhost:8080/api/append -H 'Content-Type: application/json' \
     -d '{"drive":"sr0","names":["notes.tar.gz"],"folder":"/extras"}'
```

This needs xorriso specifically. cdrecord will write another session, but it
writes whatever image it is handed — so unless that image was built against
the previous session, everything already on the disc becomes invisible even
though it is still there. xorriso loads the filesystem that is on the disc
and adds to it, which is what anyone means by appending.

Session overhead on a DVD-R is not small: a few megabytes of files costs
something closer to twenty once the run-in and run-out are counted. Appending
one file at a time to a DVD wastes a great deal of it.

Closing the disc is offered and is off by default. It is the one part of this
that cannot be undone.

### Damaged discs

A sector the drive cannot read does not end a rip. The failing batch is
split until the bad sectors are isolated, those are written as zeroes, and
the job reports how many there were and where — as sector ranges, because a
scratch produces a run of thousands. Everything else on the disc still comes
off.

The single most effective thing for a marginal disc is to slow the drive
down: at 4x the servo tracks damage that it skates straight over at 48x.
Every rip takes a speed, and `read-speed-kb` sets a default for the server.

### What a burn checks

Before: that this server allows burning at all, that a burner program is
installed, that the drive can write this kind of disc, that there is a disc,
that it is writable and blank, that the image is a whole number of 2048-byte
sectors, and that it fits.

After: every sector read back off the disc and compared with the source
image. Both a SHA-256 over the whole disc and the address of the first
differing sector come out of the same pass — the address is what tells you
whether the burn failed at the very end, which means it ran out of disc, or
in the middle, which is a different fault.

A rehearsal (`dummy`) runs the whole burn with the laser off, which proves
the drive keeps up with the source at that speed without spending a disc.

## The API

Everything the page does, a script can do. The document at `/docs` is
generated from the server's own route table and from the Go types its
handlers decode and encode, so it describes what the server does rather than
what someone wrote down.

```sh
# what is in the drive
curl -s localhost:8080/api/drives

# rip an .iso, slowly, because the disc is scratched
curl -s -XPOST localhost:8080/api/rip -H 'Content-Type: application/json' \
     -d '{"drive":"sr0","kind":"iso","speedKb":705}'

# watch it
curl -sN localhost:8080/api/events

# one file, straight off the disc
curl -s 'localhost:8080/api/drives/sr0/file?path=/README.TXT'

# a whole directory as one archive, produced as the disc is read
curl -s 'localhost:8080/api/drives/sr0/archive?path=/docs&format=tar.xz' | tar tJvf -

# check the disc's condition, and see how it compares with last time
curl -s -XPOST localhost:8080/api/scan -H 'Content-Type: application/json' \
     -d '{"drive":"sr0"}'
curl -s localhost:8080/api/discs | jq '.discs[] | {label, latestGrade, trend, trendNote}'

# burn, and check what was written
curl -s -XPOST localhost:8080/api/burn -H 'Content-Type: application/json' \
     -d '{"drive":"sr0","image":"debian.iso","speedX":8,"verify":true}'
```

With a login configured, get a token first and send it as a bearer:

```sh
token=$(curl -s -XPOST localhost:8080/api/login -H 'Content-Type: application/json' \
        -d '{"user":"ripper","password":"..."}' | jq -r .token)
curl -s -H "Authorization: Bearer $token" localhost:8080/api/drives
```

## Building

```
go build ./cmd/ripperx
```

Go 1.25 or newer, no cgo, no C toolchain — the SQLite behind the scan
history is `modernc.org/sqlite`, which is Go rather than a binding. The
release builds are static and cross-compiled for x86-64, x86, aarch64, armv7
and riscv64; each runs on glibc and musl systems alike. (mips and mipsel were
dropped when the history went in: that SQLite does not support them, and a
MIPS router is not a machine with a DVD writer in it.)

## Licence

MIT. See [LICENSE](LICENSE).
