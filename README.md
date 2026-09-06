# hpscan - "Scan to Computer" for HP all-in-one printers

A small self-contained service that makes the **Scan to Computer** button on an
HP Photosmart / OfficeJet / ENVY (tested target: Photosmart 6510 B211a) work
without HP's software. It registers your Mac or Synology NAS as a destination
on the printer, waits for you to press Scan on the printer's screen, pulls the
pages over the network and saves them as PDF (multi-page) or JPEG in the folder
you choose.

It speaks the printer's built-in LEDM REST interface (port 8080), the same one
HP's own utilities use. Nothing is installed on the printer.

## Install

Grab the binary for your platform from the
[Releases](https://github.com/olibar/hpscan/releases) page
(`hpscan-darwin-arm64` for Apple Silicon Macs, `hpscan-darwin-amd64` for Intel
Macs, `hpscan-linux-amd64` / `hpscan-linux-arm64` for NAS and Linux boxes), or
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
| `printer` | `""` | Printer hostname (prefer the Bonjour name, e.g. `HPxxxxxx.local`, it survives IP changes) or IP. Empty = mDNS auto-discovery. Falls back to mDNS if the address stops answering |
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

## Tested devices

| Printer | Protocol | Status |
|---|---|---|
| HP Photosmart 6510 e-All-in-One (B211a) | LEDM WalkupScanToComp | Works: PDF/JPEG, multi-page, Mac + Synology |

The protocol is shared by most HP inkjet all-in-ones from roughly 2010 to
2016 (Photosmart, ENVY, Deskjet, OfficeJet 4xxx-8xxx). If it works for yours,
please open an issue with the model and the `hpscan probe` output so it can be
added here. If it does not, the probe output is what is needed to fix it.

## Limitations

* Flatbed only. No document feeder (ADF) or duplex support yet.
* LEDM printers only. Newer models that expose scan-to-computer through eSCL
  (AirScan) or HP Smart cloud are not supported.
* macOS and Linux (systemd, Synology DSM) services. No Windows service yet;
  `hpscan run` works on Windows from a terminal if built with `GOOS=windows`,
  but startup integration is not provided.
* One destination per running instance.

## Build from source

Go 1.26+. `make build`, `make test`, `make dist` (cross-compiles all targets).
Pushing a tag `v*` builds and publishes release binaries via GitHub Actions.

## License

MIT, see [LICENSE](LICENSE).
