# SAND — Secure Archival Network Distribution

SAND splits, encrypts, and distributes files for secure archival. Given any file, it compresses with **zstd**, splits into two equal parts (padding if odd bytes), generates a third **XOR redundancy** part, and encrypts all three with **AES-256-GCM** using an **Argon2id**-derived key. **Any 2 of 3 parts** can reconstruct the original.

Ships as a **single static Go binary** with a CLI and an embedded React web GUI.

---

## Quick Start

```bash
# 1. Build (requires Go 1.22+ and Node.js 18+)
make build

# 2. Archive a file
./sand archive passport.pdf --password "my-secret"
# → passport.pdf.media1  passport.pdf.media2  passport.pdf.media3

# 3. Restore from any two parts
./sand restore --parts passport.pdf.media1,passport.pdf.media3 --password "my-secret"
# → passport.pdf  (byte-identical to original)

# 4. Start the web GUI
./sand serve --port 8080
# Open http://127.0.0.1:8080
```

---

## Building from Source

### Prerequisites

| Tool     | Minimum version | Install                          |
|----------|-----------------|----------------------------------|
| Go       | 1.22            | https://go.dev/dl/               |
| Node.js  | 18              | https://nodejs.org / `winget install OpenJS.NodeJS.LTS` |

### Build commands

```bash
# Full build (frontend + binary)
make build

# Frontend only (outputs to internal/server/dist/)
make build-web

# Go binary only (assumes web is already built)
make build-go

# Cross-compile release binaries for all platforms → dist/
make release          # Linux/macOS host only

# Windows (PowerShell / Git Bash)
make build            # produces sand.exe
```

### Output binary names

| Platform        | Binary         |
|-----------------|----------------|
| Linux / macOS   | `sand`         |
| Windows         | `sand.exe`     |

---

## CLI Reference

### `sand archive`

Compress, split, and encrypt a file into three media parts.

```
sand archive <file> [flags]

Flags:
  --password  string   Encryption password (prompted securely if omitted)
  --output-dir string  Directory to write .media files (default: same as input)
```

**Example**

```bash
sand archive report.pdf --password "hunter2" --output-dir ~/archive/
# Produces:
#   ~/archive/report.pdf.media1
#   ~/archive/report.pdf.media2
#   ~/archive/report.pdf.media3
```

Output:
```
Archive complete. Distribute any 2 of these 3 files to restore the original:
  report.pdf.media1
  report.pdf.media2
  report.pdf.media3
```

---

### `sand restore`

Reconstruct the original file from any 2 (or all 3) media parts.

```
sand restore [flags]

Flags:
  --parts      string   Comma-separated paths to 2 or 3 .media files (required)
  --password   string   Decryption password (prompted securely if omitted)
  --output-dir string   Directory to write the restored file (default: current dir)
```

**Example — any 2-of-3 combination works**

```bash
sand restore --parts report.pdf.media1,report.pdf.media3 --password "hunter2"
# Restored: report.pdf
```

---

### `sand serve`

Start the embedded web server and GUI.

```
sand serve [flags]

Flags:
  --port  int     Port to listen on (default: 8080)
  --bind  string  IP address to bind to (default: 127.0.0.1)
```

**Example**

```bash
# Local only (default)
sand serve --port 8080

# Expose on all interfaces (use behind a reverse proxy)
sand serve --port 8080 --bind 0.0.0.0
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080) to use the GUI.

---

## Web API

The server exposes a JSON/binary REST API alongside the GUI.

### `GET /api/health`

```json
{ "status": "ok", "version": "1.0.0" }
```

### `POST /api/archive`

Multipart form:

| Field      | Type | Required | Description            |
|------------|------|----------|------------------------|
| `file`     | file | yes      | File to archive        |
| `password` | text | yes      | Encryption password    |

Returns a `application/zip` stream containing three `.media` files.

### `POST /api/restore`

Multipart form:

| Field      | Type      | Required | Description                   |
|------------|-----------|----------|-------------------------------|
| `parts[]`  | file (×2-3) | yes    | Two or three `.media` files   |
| `password` | text      | yes      | Decryption password           |

Returns the original file as `application/octet-stream`.

**Error response shape**

```json
{ "error": "human-readable message", "code": "MACHINE_CODE" }
```

Error codes: `MISSING_FILE`, `MISSING_PASSWORD`, `WRONG_PASSWORD`, `MISMATCHED_PARTS`, `INVALID_PARTS`.

---

## How It Works

```
Input → zstd compress → split into 2 halves (pad if odd)
      → XOR(half1, half2) = part3
      → AES-256-GCM encrypt each part (unique nonce, Argon2id key)
      → 3 .media files
```

### Reconstruction truth table

| Part 1 | Part 2 | Part 3 | Method                         |
|:------:|:------:|:------:|:-------------------------------|
| ✓      | ✓      | ✗      | Direct concat                  |
| ✓      | ✗      | ✓      | `part2 = XOR(part1, part3)`    |
| ✗      | ✓      | ✓      | `part1 = XOR(part2, part3)`    |

### Binary file format (`.media` files)

Each part is a self-contained binary file:

```
[magic "SAND"][version][part_num][archive_uuid][sha256][sizes][argon2_params][nonce][ciphertext]
```

All three parts carry the same archive UUID and SHA-256 hash of the original, enabling mismatch detection before decryption.

---

## Security

| Property           | Algorithm / Parameter                               |
|--------------------|-----------------------------------------------------|
| Key derivation     | Argon2id — 3 iterations, 64 MB memory, 4 threads   |
| Encryption         | AES-256-GCM with a random 12-byte nonce per part    |
| Integrity          | GCM auth tag (per-part) + SHA-256 of original file  |
| Header binding     | File header bytes used as GCM associated data (AD)  |
| Mismatch detection | Archive UUID checked before decryption              |

Tampering with any part (even a single flipped bit) causes GCM authentication to fail and the restore to abort.

---

## Testing

### Go unit tests

```bash
make test           # run all internal package tests
make test-cover     # with HTML coverage report (coverage.html)
```

### End-to-end tests (CLI + HTTP API + browser GUI)

```bash
# Install Python dependencies (once)
make test-deps       # installs pytest, playwright, requests + Chromium

# Run all e2e tests (skips slow large-file tests)
make test-e2e

# Run everything including slow tests
make test-e2e-slow

# Run Go unit tests + e2e tests together
make test-all
```

The e2e suite is in `tests/` and covers:

| File              | Coverage                                              |
|-------------------|-------------------------------------------------------|
| `test_cli.py`     | CLI subprocesses: archive, restore, edge cases, security |
| `test_api.py`     | HTTP endpoints: health, archive, restore, error codes |
| `test_gui.py`     | Playwright/Chromium: page load, API-via-JS, full GUI workflow |

GUI tests (`TestGUILayout`, `TestArchiveWorkflow`, `TestRestoreWorkflow`) require the React frontend to be built (`make build-web`) and automatically skip otherwise.

---

## Deployment

### Linux — systemd service

```bash
# Build the binary first
make build

# Install as a service (run as root)
sudo ./scripts/deploy-linux.sh ./sand 8080 127.0.0.1

# Check status
systemctl status sand
journalctl -u sand -f
```

The script creates a `sand` system user, installs the binary to `/usr/local/bin/sand`, and registers a hardened systemd unit.

### Windows — background service (NSSM)

```powershell
# Install NSSM once
winget install nssm

# Build the binary
make build

# Install as a service (run as Administrator)
.\scripts\deploy-windows.ps1 -Binary .\sand.exe -Port 8080 -Bind 127.0.0.1

# Check status
Get-Service sand
```

### Reverse proxy (nginx + TLS)

See `scripts/nginx-sand.conf` for a ready-to-use nginx config with HTTPS termination and Let's Encrypt. Edit the `server_name` line to match your domain, then:

```bash
sudo cp scripts/nginx-sand.conf /etc/nginx/sites-available/sand
sudo ln -s /etc/nginx/sites-available/sand /etc/nginx/sites-enabled/
sudo certbot --nginx -d sand.example.com
sudo nginx -t && sudo systemctl reload nginx
```

### Release builds (all platforms)

```bash
# Produces dist/ with Linux/macOS/Windows binaries + SHA256SUMS
./scripts/build-release.sh 1.0.0
```

---

## Project Structure

```
sand/
├── cmd/sand/main.go             # CLI entry point (cobra)
├── internal/
│   ├── archive/                 # Archive + Restore orchestrators
│   ├── crypto/                  # Argon2id key derivation + AES-256-GCM
│   ├── compress/                # zstd compress/decompress wrapper
│   ├── splitter/                # Split, XOR, Reconstruct
│   ├── mediafile/               # Binary .media file format
│   └── server/                  # HTTP server + embedded SPA
│       └── dist/                # Built React assets (git-ignored)
├── web/                         # React + Vite frontend source
│   └── src/App.jsx              # Single-file React app
├── tests/                       # Python e2e test suite
│   ├── conftest.py              # Shared fixtures (server, sand_bin, …)
│   ├── test_cli.py              # CLI subprocess tests
│   ├── test_api.py              # HTTP API tests (requests)
│   └── test_gui.py              # Headless browser tests (Playwright)
├── scripts/
│   ├── build-release.sh         # Cross-compile all platforms
│   ├── deploy-linux.sh          # Install as systemd service
│   ├── deploy-windows.ps1       # Install as Windows service (NSSM)
│   └── nginx-sand.conf          # nginx reverse-proxy template
├── Makefile
├── SAND_ARCHITECTURE.md         # Detailed architecture document
└── README.md
```

---

## Development

```bash
# Hot-reload frontend (proxies /api/* to localhost:8080)
cd web && npm run dev

# In another terminal — run the Go server
make build-go && ./sand serve --port 8080
```

The Vite dev server at `http://localhost:5173` proxies all `/api/` requests to the Go server, so you get instant React reloads without rebuilding.
