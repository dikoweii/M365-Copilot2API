# Production release

Production deployment uses immutable release directories, an atomic `current`
symlink, and a separate persistent state directory. Routine releases do not
modify Nginx, the systemd unit, accounts, tokens, or API keys.

## Repository and secret boundary

Never commit a host name, IP address, SSH key, API key, administrator password,
account file, proxy credential, TLS private key, or production `.env` file.
Pass target details to the deployment command and keep secrets in root-owned
files on the server.

The persistent paths are:

```text
/opt/m365-copilot2api/current -> releases/<release-id>
/opt/m365-copilot2api/previous -> releases/<previous-id>
/opt/m365-copilot2api/releases/<release-id>/
/var/lib/m365-copilot2api/
```

## One-time server setup

1. Create the `m365-copilot2api` system user and group.
2. Install `deploy/systemd/m365-copilot2api.service`, then run
   `systemctl daemon-reload` and `systemctl enable m365-copilot2api`.
3. Create `/var/lib/m365-copilot2api` owned by the service user with mode
   `0700`. Put the administrator password in `admin-password` with mode `0600`.
4. Install the Nginx location template. Define `$connection_upgrade` in the
   Nginx `http` block, validate with `nginx -t`, then reload Nginx.
5. If replacing a legacy single-binary installation, first copy its binary,
   unit file, and state directory into a root-only backup. Confirm the unit now
   starts `/opt/m365-copilot2api/current/m365-copilot2api`.
6. Record the server's SSH host key in a dedicated `known_hosts` file through a
   trusted channel. The deployment script never auto-accepts a new host key.

## Release command

Run from a clean, reviewed snapshot repository using PowerShell 7.2 or newer:

```powershell
./scripts/deploy-production.ps1 `
  -HostName <host-or-ip> `
  -Port <ssh-port> `
  -KnownHostsPath <known-hosts-file> `
  -IdentityFile <ssh-private-key-path> `
  -ReleaseId stable-vX.Y.Z
```

The script runs tests, vet, and the privacy scan; cross-compiles the Linux
binary with version metadata; writes a manifest and SHA256 files; uploads the
archive; and invokes `deploy/remote-release.sh`.

The remote helper takes an exclusive deployment lock, verifies both archive and
binary checksums, stops the service, creates a root-only state snapshot, switches
the symlink, starts the service, checks `/login`, and verifies the hash of the
running process. A failed start, health check, or hash check automatically
switches back to the previous release. State is not automatically restored,
because refreshed OAuth tokens can have one-time exchange behavior.

## Verification

After a successful release:

```bash
systemctl is-active m365-copilot2api
systemctl show m365-copilot2api --property MainPID,ExecMainStatus
curl --fail --silent http://127.0.0.1:4141/login >/dev/null
journalctl -u m365-copilot2api --since '-10 minutes' --no-pager
```

Then use an externally supplied API key for one `/v1/models` request and one
minimal chat request. Do not put the key in the repository or command history.
An upstream M365 failure should be investigated separately and should not by
itself trigger binary rollback.

## Manual rollback

```bash
systemctl stop m365-copilot2api
ln -sfn "$(readlink -f /opt/m365-copilot2api/previous)" /opt/m365-copilot2api/current
systemctl start m365-copilot2api
```

Verify the service, local login page, process checksum, and logs after rollback.
