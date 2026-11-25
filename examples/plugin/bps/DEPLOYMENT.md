# Production deployment

This fork builds the BPS shared library alongside CPA in the Docker image. Its
artifact is `/opt/cliproxy-plugins/bps.so`, outside the host-mounted `plugins`
directory. Both use the same Go version, architecture, and Debian runtime.

The `test-and-deploy` workflow tests a pushed `main` commit, then runs
`.github/scripts/deploy-production.sh` on the server. The script:

1. Locks deployment, rejects tracked local edits, and requires a fast-forward.
2. Backs up config, Compose files, `.env`, the previous BPS binary, and image ID.
3. Verifies the customized panel SHA256 and builds an immutable image from the tested commit before replacing the service.
4. Checks that resolved ports, mounts, environment, and networks are unchanged.
5. Installs `bps-v0.5.5.so` and enables it in the existing config. Older versioned
   libraries remain available for rollback; the loader selects the newest version.
6. Sets `management.disable-auto-update-panel` so upstream updates cannot replace the customized panel.
7. Recreates only CPA, checks `/healthz`, BPS registration and the served panel SHA256, then saves the new
   image name in `.env`. Failure restores the previous config, BPS binary, and image.

Host-specific settings must live in ignored `docker-compose.override.yml` and
`.env`; do not edit tracked `docker-compose.yml`. Keep credentials in the existing
mounted auth directory. The deployment does not replace them or change management
keys. Backups contain private configuration and are created with private permissions.

Requirements: Bash, Git, Docker Compose v2.24.4+ (or v5), Python 3 with PyYAML,
`flock`, and `curl`. The server is Linux; plugin architecture comes from the image.
The current production path is `/home/fkh/code/CLIProxyAPI`.

For manual deployment of a tested commit already on `origin/main`:

```sh
bash management-ui/build.sh /tmp/management.html
sha256sum /tmp/management.html
# Copy the verified HTML to the server, then pass its path and SHA256.
bash .github/scripts/deploy-production.sh /home/fkh/code/CLIProxyAPI COMMIT_SHA /path/to/management.html PANEL_SHA256
```

Backups are stored in `/home/fkh/.cpa-backups/bps-deploy-*`. To roll back an image
manually, read its `rollback-image`, restore its `config.yaml` and (when present)
`previous-bps.so`, then run Compose with `CLI_PROXY_IMAGE` set to that saved image.
Restore `.env` from the same backup to keep the rollback across future Compose runs.
The source checkout remains at the newer commit for diagnosis.
