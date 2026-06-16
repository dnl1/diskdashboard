# diskdashboard

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Linux-only.** Disk monitoring and analysis — **CLI** + **Web Dashboard**. Discover disk usage, largest files, SMART health, and manage files across all your mounts.

- **CLI** — Rust, fast, rich terminal output, regex filtering, duplicate detection
- **Web** — Go server, zero-config Docker, auto-discovers all host mounts, real-time disk insights

---

## Web Dashboard

### Features

- **Auto-discovery** — reads `/proc/1/mounts` from the host, no need to list mounts manually
- **Disk cards** — usage bars, SMART status, model info, click to focus a disk
- **Largest files** — top 50 files > 100 MB per mount point, with size ratio bars
- **Hard link detection** — groups duplicate inodes and shows all paths
- **SMART details** — full attribute table with human-readable interpretation, color-coded (green/red)
- **File deletion** — select files via checkboxes, confirm, delete in batch with path validation
- **Auto-refresh** — disk data refreshes every 30s; manual refresh via button
- **Filter by mount** — dropdown to focus a single mount point
- **LVM support** — resolves LVM logical volumes to the underlying physical disk for SMART monitoring

> **Platform:** Linux only. The web server uses Linux-specific kernel interfaces (`/proc/mounts`, `syscall.Statfs`, `/sys/block`).

### Quick Start (Docker)

```bash
git clone https://github.com/dnl1/diskdashboard.git
cd diskdashboard
docker compose up -d
```

Open http://localhost:3005

#### docker-compose.yml

```yaml
services:
  diskdashboard:
    build: .
    container_name: diskdashboard
    ports:
      - "3005:3000"
    pid: host
    privileged: true
    volumes:
      - /:/host/root:ro           # entire host filesystem
      - /dev:/dev:ro              # SMART monitoring
    environment:
      - HOST_ROOT=/host/root
      - PORT=3000
    restart: unless-stopped
```

No need to list mounts manually — `pid: host` lets the container read the host's real mount table via `/proc/1/mounts`. `smartctl` is pre-installed in the Docker image.

### Quick Start (Bare Metal)

**Requirements:** Go 1.26+, [smartctl](https://www.smartmontools.org/) (for SMART monitoring).

```bash
# Debian/Ubuntu
sudo apt install smartmontools

# Arch
sudo pacman -S smartmontools

# Fedora/RHEL
sudo dnf install smartmontools
```

```bash
cd server
go build -o diskdashboard-server .
sudo ./diskdashboard-server
# Listening on :3000
```

Then open http://localhost:3000

### API Endpoints

| Endpoint | Description |
|---|---|
| `GET  /api/disks` | All mounted disk partitions with usage, model, SMART status, base device |
| `GET  /api/largest-files?dir=/path` | Top 50 files > 100 MB (multiple `dir=` params accepted) |
| `GET  /api/smart?dev=sda` | Full SMART attribute table for a device |
| `POST /api/delete` | Delete one or more files — `{"paths": ["/a", "/b"]}` |

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3000` | HTTP listen port |
| `HOST_ROOT` | `""` | Path to host root (set `/host/root` inside Docker) |

---

## CLI Tool

A fast, colorful terminal tool for file analysis, written in Rust.

### Install

```bash
# Cargo
cargo install diskdashboard

# Manual build
git clone https://github.com/dnl1/diskdashboard.git
cd diskdashboard
cargo build --release
```

### Usage

```bash
# List files (shows permissions, dates, sizes)
diskdashboard
diskdashboard /some/path

# Show sizes
diskdashboard -s
diskdashboard -s mb

# Filter with regex
diskdashboard --search "\.rs$"
diskdashboard --excluding "^\."

# Detailed file analysis
diskdashboard -p
diskdashboard -f important.txt

# Disk info
diskdashboard --disk list
diskdashboard --disk /dev/sda1

# Duplicates
diskdashboard --duplicates

# Tree view
diskdashboard --tree

# Export
diskdashboard --export results.json

# Interactive menu
diskdashboard -i
```

### Full Options

| Option | Description |
|---|---|
| `-v`, `--version` | Show version |
| `-s <UNIT>` | Show sizes (b, kb, mb, gb, tb, auto) |
| `-t`, `--tree` | Directory tree |
| `-p`, `--properties` | Detailed file/dir analysis |
| `--no-color` | Disable colors |
| `--disk <DISK>` | Disk operations (`list` or name) |
| `--search <PATTERN>` | Regex search |
| `--excluding <PATTERN>` | Regex exclude |
| `--sort-by <CRITERIA>` | Sort (name, size, date) |
| `--duplicates` | Find duplicates |
| `--export <FILE>` | Export to JSON/CSV |
| `-f <FILE>` | Analyze a file |
| `-d <DIR>` | Analyze a directory |
| `-r`, `--recursive` | Recurse into subdirectories |
| `-i`, `--interactive` | Interactive menu |

---

## Project Structure

```
diskdashboard/
├── src/                    # Rust CLI
│   ├── main.rs
│   ├── types.rs
│   ├── collect.rs
│   ├── display.rs
│   ├── analysis.rs
│   ├── disk.rs
│   ├── tree.rs
│   └── utils.rs
├── server/                 # Go web server
│   ├── main.go             # HTTP handlers + all logic
│   ├── index.html          # TailwindCSS frontend (vanilla JS)
│   ├── go.mod
│   └── diskdashboard.service
├── Dockerfile
├── docker-compose.yml
├── install.sh
└── uninstall.sh
```

---

## Security

- **Path validation** — all file deletions are validated against the real mount table; system mounts (`/`, `/boot`, `/boot/efi`) are blocked
- **Device validation** — SMART endpoints accept only `sd[a-z]+`, `nvme\d+n\d+`, `mmcblk\d+` device names
- **Read-only by default** — Docker mounts are `:ro`; write access is only needed for explicit file deletion
- **No shell injection** — all external commands (`smartctl`, `find`) use `exec.Command` with explicit arguments, no shell interpretation

---

## Platform

**Linux only.** The web dashboard depends on:

- `/proc/1/mounts` — real-time mount table from the host PID namespace
- `syscall.Statfs` — filesystem statistics (specific to Linux's `statfs64`)
- `/sys/block` — kernel device topology and model info
- `/dev/sdX` naming convention and `smartctl` for S.M.A.R.T.

macOS, BSD, and Windows are not supported.

## License

MIT
