# Web UI Embedded Assets

This directory contains the Web UI static assets embedded into the Go binary.

## Why this directory exists

- `cli/api` stores HTTP API code only.
- `cli/webui` stores frontend build artifacts only.
- Embedding is handled by `cli/webui/embed.go`.

## Build flow

1. Edit frontend source in `web/src`.
2. Run frontend build from `web`:

   ```bash
   npm run build
   ```

3. Vite outputs directly to `cli/webui/dist`.
4. Build backend binary:

   ```bash
   go build ./cli
   ```
