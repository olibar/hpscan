# hpscan - "Scan to Computer" for HP all-in-one printers

A small self-contained service that makes the **Scan to Computer** button on
HP Photosmart / OfficeJet / ENVY all-in-ones work without HP's software. It
registers your Mac, Windows PC, Linux box or Synology NAS as a destination on
the printer, waits for you to press Scan on the printer's screen, pulls the
pages over the network and saves them as PDF or JPEG in the folder you choose.

It speaks the printer's built-in LEDM REST interface (port 8080), the same one
HP's own utilities use. Nothing is installed on the printer.

## Features

* Single static binary, no runtime. Runs as a startup service: launchd on
  macOS, systemd on Linux and Synology DSM, Windows service via the Service
  Control Manager.
* Several printers from one instance: list them, or let mDNS find them all.
  `hpscan printer add` / `remove` manage the list interactively.
* Multi-page PDF: "scan another page" on the flatbed, or the whole stack when
  the printer has a document feeder with paper in it. JPEG per page otherwise.
* Honours the shortcut chosen on the printer (Save as Document/PDF -> PDF,
  Save as Photo/JPEG -> JPEG).
* Files are written atomically (temp file + rename), so Dropbox, Cloud Sync
  and similar pick up complete files only.
* Survives printer IP changes: Bonjour hostnames, plus mDNS re-lookup when a
  configured address stops answering. Re-registers after printer reboots.

## Install

Grab the binary for your platform from the
[Releases](https://github.com/olibar/hpscan/releases) page
(`hpscan-darwin-arm64` for Apple Silicon Macs, `hpscan-darwin-amd64` for Intel
Macs, `hpscan-linux-amd64` / `hpscan-linux-arm64` for NAS and Linux boxes,
`hpscan-windows-amd64.exe` for Windows on Intel/AMD, `hpscan-windows-arm64.exe`
for Windows on ARM such as a Parallels VM on Apple Silicon), or
build from source with Go 1.26+:

```sh
go install github.com/olibar/hpscan/cmd/hpscan@latest   # or: make build
```

## Quick start (Mac)

```sh
make build                      # skip if you downloaded a release binary (rename it to hpscan)
sudo make install-local         # copies ./hpscan to /usr/local/bin
hpscan config init              # finds the printer via Bonjour, writes ~/.config/hpscan/config.yaml
hpscan config set output_dir ~/Documents/Scans
hpscan config set name "My MacBook"   # name shown on the printer screen
hpscan install                  # installs a launchd agent: starts now and at every login
hpscan status
hpscan printer list             # which printers are served, and which are on the network
```

On the printer: **Scan -> Computer -> pick your computer name -> pick a
shortcut (Save as Document / Save as Photo) -> Start Scan**. Files appear in
`output_dir` a few seconds later. Multi-page from the flatbed: keep answering
"scan another page" on the printer; the PDF is closed when you choose "Done"
(or after `page_timeout` of inactivity). With paper in the document feeder the
whole stack becomes one PDF in a single run.

Got a second printer? `hpscan printer add` lists what it finds on the network
and lets you pick one; the service restarts by itself.

macOS note: the first time, macOS may ask to allow the binary to access the
**Local Network**. If scans never arrive and the log shows "no route to host",
go to System Settings -> Privacy & Security -> Local Network and enable it for
the terminal / hpscan.

## Commands

| Command | What it does |
|---|---|
| `hpscan run` | Run in the foreground (this is what the service runs) |
| `hpscan install [--user <name>]` / `uninstall` | Register / remove the startup service (launchd on Mac, systemd on Linux, Windows service). `--user` runs a systemd system unit as that account |
| `hpscan start` / `stop` / `restart` | Control the installed service |
| `hpscan status` | Is it running? |
| `hpscan config init` | Create the config file, auto-detecting the printer |
| `hpscan config show` / `path` | Print the config / its location |
| `hpscan config set <key> <value>` | Change one setting (comments are preserved). Restart to apply. |
| `hpscan printer list` / `add [host]` / `remove [host]` | Manage the printer list; without a host you pick from a numbered list. Restarts the service if running |
| `hpscan discover` | List HP scanners announced on the network |
| `hpscan scan [file]` | Trigger a single scan from the computer (handy to test connectivity) |
| `hpscan probe [host[:port]]` | Dump a printer's XML resources for troubleshooting, the configured one or any address |
| `hpscan help` | List all commands |

Global flags: `--config <path>` (or `HPSCAN_CONFIG`), `-v` for debug logging.

## Configuration

See [config.yaml](config.yaml) for the annotated sample. Keys:

| Key | Default | Meaning |
|---|---|---|
| `printer` | `""` | Printer hostname(s) or IP(s), comma-separated for several printers; append `:port` to an entry that does not use the default port (`HPxxxxxx.local:80`). Prefer Bonjour names (`HPxxxxxx.local`, survive IP changes). Empty = serve every HP scanner found via mDNS. Falls back to mDNS if an address stops answering |
| `port` | `8080` | Default LEDM port for entries without `:port` (some models use 80). Auto-discovered printers use the port announced over mDNS |
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

Two routes. **Use the native route if the scans folder is synced by Cloud
Sync**: Cloud Sync only notices files written from the host, not from inside
a Docker container.

### Option A: native binary as a systemd service (recommended)

Requirements: SSH enabled on the NAS, a DSM account that owns the scans folder
(e.g. `scanner`) with write permission on it.

```sh
make dist
./deploy/synology-install.sh nas.local /volume1/scans NAS scanner
```

The script picks the amd64/arm64 binary, installs it in `/volume1/apps/hpscan`,
writes the config there, removes any Docker instance, and enables a systemd
unit running as the given user. Re-run it after code changes. Logs go to
`/volume1/apps/hpscan/hpscan.log`.

```sh
ssh -t user@nas.local 'sudo systemctl status|stop|start|restart hpscan'
ssh user@nas.local 'tail -f /volume1/apps/hpscan/hpscan.log'
```

If the DSM has no systemd the script prints a Task Scheduler recipe instead.

### Option B: Docker

```sh
./deploy/synology-docker-install.sh nas.local /volume1/scans NAS
```

Builds the image on the NAS with the legacy `docker-compose`, host networking
for mDNS, and runs the container as the SSH user (files owned by that user).
Fine for a plain shared folder; not suitable for Cloud Sync folders (see above).

## Windows

Download `hpscan-windows-amd64.exe` (or `-arm64.exe` on Windows on ARM),
rename it `hpscan.exe` and put it in a
permanent folder such as `C:\Program Files\hpscan`. In a PowerShell window
opened **as Administrator**:

```powershell
cd "C:\Program Files\hpscan"
.\hpscan.exe config init
.\hpscan.exe config set output_dir "C:\Users\<you>\Documents\Scans"   # absolute path; the service runs as SYSTEM
.\hpscan.exe config set name "Office PC"
.\hpscan.exe -v scan                # test from the console first
.\hpscan.exe install                # Windows service, automatic start
.\hpscan.exe status
```

Config and log live in `C:\ProgramData\hpscan`. `install`, `uninstall`,
`start`, `stop`, `restart` and `status` all talk to the Service Control
Manager and need an Administrator window. Windows Firewall does not need
changes: the client only makes outgoing connections to the printer.

## Troubleshooting

* `hpscan -v scan` scans one page from the computer: proves network + scanner.
* `hpscan probe > probe.txt` dumps the printer's discovery tree, scan caps,
  destinations and event table. Attach it when reporting a protocol problem.
* Printer says "no computer found" or "set up Scan to Computer first": the
  daemon must be running **before** you open the menu on the printer; check
  `hpscan status` and look for a "ready" line per printer in the log. Some
  newer models also have a Scan to Computer on/off switch in their web page
  (Scan section).
* Two printers, one shows no computer: check the log has a "ready" line with
  each printer's name; `hpscan printer list` shows what is configured.
* After updating the binary, macOS treats it as a new program: the first
  connection attempt may fail with "no route to host" until the Local Network
  permission is re-applied (a prompt may appear). The daemon retries by itself.
* After a printer power cycle, registrations are lost; the daemon re-registers
  automatically on the next event or reconnect.

## Tested devices

| Printer | Protocol | Status |
|---|---|---|
| HP Photosmart 6510 e-All-in-One (B211a) | LEDM WalkupScanToComp | Works: PDF/JPEG, multi-page, Mac + Synology |
| HP OfficeJet Pro 9010 series | LEDM WalkupScanToComp | Works: flatbed + document feeder, Mac + Synology |

The protocol is shared by most HP inkjet all-in-ones from roughly 2010 to
2020 (Photosmart, ENVY, Deskjet, OfficeJet, OfficeJet Pro). If it works for yours,
please open an issue with the model and the `hpscan probe` output so it can be
added here. If it does not, the probe output is what is needed to fix it.

## Limitations

* No duplex scanning from the feeder yet.
* LEDM printers only. Models that offer scan-to-computer solely through eSCL
  (AirScan) or the HP Smart cloud are not supported.
* Windows service support is implemented but not yet verified on a real
  machine; reports welcome.
* In auto-discovery mode (empty `printer`), printers that appear after startup
  need a service restart.

## Build from source

Go 1.26+. `make build`, `make test`, `make dist` (cross-compiles all targets).
Pushing a tag `v*` builds and publishes release binaries via GitHub Actions.

## License

MIT, see [LICENSE](LICENSE).
