# hpscan - "Scan to Computer" for HP all-in-one printers

A small self-contained service that makes the **Scan to Computer** button on an
HP Photosmart / OfficeJet / ENVY (tested target: Photosmart 6510 B211a) work
without HP's software. It registers your Mac or Synology NAS as a destination
on the printer, waits for you to press Scan on the printer's screen, pulls the
pages over the network and saves them as PDF (multi-page) or JPEG in the folder
you choose.

It speaks the printer's built-in LEDM REST interface (port 8080), the same one
HP's own utilities use. Nothing is installed on the printer.

## Quick start (Mac)

```sh
make build                      # or: go build -o hpscan ./cmd/hpscan
sudo make install-local         # copies ./hpscan to /usr/local/bin
hpscan config init              # finds the printer via Bonjour, writes ~/.config/hpscan/config.yaml
hpscan config set output_dir ~/Documents/Scans
hpscan config set name "Olivier MacBook"   # name shown on the printer screen
hpscan install                  # installs a launchd agent: starts now and at every login
hpscan status
```

On the printer: **Scan -> Computer -> pick your computer name -> pick a
shortcut (Save as PDF / Save as JPEG) -> Start Scan**. Files appear in
`output_dir` a few seconds later. Multi-page: keep answering "scan another
page" on the printer; the PDF is closed when you choose "Done" (or after
`page_timeout` of inactivity).

macOS note: the first time, macOS may ask to allow the binary to access the
**Local Network**. If scans never arrive and the log shows "no route to host",
go to System Settings -> Privacy & Security -> Local Network and enable it for
the terminal / hpscan.

## Commands

| Command | What it does |
|---|---|
| `hpscan run` | Run in the foreground (this is what the service runs) |
| `hpscan install` / `uninstall` | Register / remove the startup service (launchd on Mac, systemd on Linux) |
| `hpscan start` / `stop` / `restart` | Control the installed service |
| `hpscan status` | Is it running? |
| `hpscan config init` | Create the config file, auto-detecting the printer |
| `hpscan config show` / `path` | Print the config / its location |
| `hpscan config set <key> <value>` | Change one setting (comments are preserved). Restart to apply. |
| `hpscan discover` | List HP scanners announced on the network |
| `hpscan scan [file]` | Trigger a single scan from the computer (handy to test connectivity) |
| `hpscan probe` | Dump the printer's XML resources for troubleshooting |

Global flags: `--config <path>` (or `HPSCAN_CONFIG`), `-v` for debug logging.

## Configuration

See [config.yaml](config.yaml) for the annotated sample. Keys:

| Key | Default | Meaning |
|---|---|---|
| `printer` | `""` | Printer hostname (prefer the Bonjour name, e.g. `HP058DA0.local`, it survives IP changes) or IP. Empty = mDNS auto-discovery. Falls back to mDNS if the address stops answering |
| `port` | `8080` | LEDM port (some models use 80) |
| `name` | hostname | Destination name shown on the printer |
| `output_dir` | `~/Scans` | Where scans are written |
| `format` | `pdf` | `pdf` or `jpeg`. A PDF/JPEG shortcut chosen on the printer overrides this |
| `resolution` | `300` | DPI, 75..1200 |
| `color_mode` | `color` | `color` or `gray` |
| `paper` | `a4` | `a4` or `letter` scan area |
| `filename` | `scan_{date}_{time}` | Pattern; `{page}` is used for JPEG pages |
| `page_timeout` | `120s` | Close a multi-page PDF after this much inactivity |
| `log_level` | `info` | `debug` for protocol traces |
| `log_file` | `""` | Empty = stderr (the service redirects to `~/.config/hpscan/hpscan.log`) |

## Synology NAS

Two options.

### Option A: Docker (Container Manager) - recommended

One command from your Mac (SSH enabled on the NAS, Container Manager installed):

```sh
./deploy/synology-docker-install.sh inas.local /volume1/Dropbox/ScanDoc iNAS
```

Re-run it after code changes to rebuild and restart. Manual equivalent:
copy this whole project folder to the NAS (e.g. `/volume1/docker/hpscan`
over SMB), then either over SSH:

```sh
cd /volume1/docker/hpscan
mkdir -p config && cp config.yaml config/config.yaml
# edit config/config.yaml: name: "Synology", output_dir: "/scans", printer: "" (or the printer IP)
# edit docker-compose.yml: point the /scans volume at your shared folder
sudo docker compose up -d
sudo docker logs -f hpscan
```

or in DSM: Container Manager -> Project -> Create -> path `/volume1/docker/hpscan`,
use the existing docker-compose.yml -> Build. The compose file uses host
networking so mDNS discovery of the printer works; on a bridge network set
`printer` to the printer's IP instead.

### Option B: native binary + systemd / Task Scheduler

```sh
make dist          # on your Mac; produces dist/hpscan-linux-amd64 and -arm64
./deploy/synology-install.sh inas.local /volume1/Dropbox/ScanDoc iNAS   # does the steps below over SSH
```

Copy the matching binary (DS918+/DS920+ and most Plus models are `amd64`, J/
value models are often `arm64`) to e.g. `/volume1/apps/hpscan/hpscan`,
`chmod +x` it, create the config:

```sh
HPSCAN_CONFIG=/volume1/apps/hpscan/config.yaml ./hpscan config init
HPSCAN_CONFIG=/volume1/apps/hpscan/config.yaml ./hpscan config set output_dir /volume1/scans
```

Then in DSM: Control Panel -> Task Scheduler -> Create -> Triggered Task ->
User-defined script, event **Boot-up**, user **root** (or a user that can write
the scans folder), script:

```sh
HPSCAN_CONFIG=/volume1/apps/hpscan/config.yaml nohup /volume1/apps/hpscan/hpscan run >> /volume1/apps/hpscan/hpscan.log 2>&1 &
```

`hpscan stop` and `hpscan status` work through the pidfile written next to the
config. If your DSM has systemd (DSM 7), `hpscan install` will use it instead.

## Troubleshooting

* `hpscan -v scan` scans one page from the computer: proves network + scanner.
* `hpscan probe > probe.txt` dumps the printer's discovery tree, scan caps,
  destinations and event table. Attach it when reporting a protocol problem.
* Printer says "no computer found": the daemon must be running **before** you
  open the menu on the printer; check `hpscan status` and the log.
* After updating the binary, macOS treats it as a new program: the first
  connection attempt may fail with "no route to host" until the Local Network
  permission is re-applied (a prompt may appear). The daemon retries by itself.
* After a printer power cycle, registrations are lost; the daemon re-registers
  automatically on the next event or reconnect.

## Build from source

Go 1.26+. `make build`, `make test`, `make dist`.
