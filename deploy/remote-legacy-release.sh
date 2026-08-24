#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 7 ]]; then
  echo "usage: remote-legacy-release.sh RELEASE_ID BINARY BINARY_SHA256 SERVICE INSTALL_ROOT STATE_DIR RETAIN" >&2
  exit 2
fi

release_id=$1
uploaded_binary=$2
binary_sha256=$3
service_name=$4
install_root=$5
state_dir=$6
retain=$7

[[ $release_id =~ ^[A-Za-z0-9._-]+$ ]] || { echo "invalid release id" >&2; exit 2; }
[[ $service_name =~ ^[A-Za-z0-9@._-]+$ ]] || { echo "invalid service name" >&2; exit 2; }
[[ $install_root == /* && $state_dir == /* ]] || { echo "paths must be absolute" >&2; exit 2; }
[[ $retain =~ ^[2-9][0-9]*$ ]] || { echo "retain must be at least 2" >&2; exit 2; }

current_binary="$install_root/m365-copilot2api"
version_file="$install_root/VERSION"
stage_binary="$install_root/.m365-copilot2api.incoming-$release_id"
backup_binary="$install_root/m365-copilot2api.previous-$release_id"
backup_version="$install_root/VERSION.previous-$release_id"
state_backup_dir="$install_root/state-backups"
state_backup="$state_backup_dir/$release_id.tar.gz"

[[ -f $uploaded_binary && -f $current_binary ]] || { echo "binary path missing" >&2; exit 1; }
install -d -m 0700 "$state_backup_dir"
exec 9>"$install_root/.deploy.lock"
flock -n 9 || { echo "another deployment is running" >&2; exit 1; }

actual_sha256=$(sha256sum "$uploaded_binary" | awk '{print $1}')
[[ $actual_sha256 == "$binary_sha256" ]] || { echo "binary checksum mismatch" >&2; exit 1; }
[[ ! -e $backup_binary ]] || { echo "release already exists: $release_id" >&2; exit 1; }

install -o root -g root -m 0755 "$uploaded_binary" "$stage_binary"
cp -a -- "$current_binary" "$backup_binary"
if [[ -f $version_file ]]; then
  cp -a -- "$version_file" "$backup_version"
fi

rollback() {
  if [[ -f $backup_binary ]]; then
    install -o root -g root -m 0755 "$backup_binary" "$stage_binary.rollback"
    mv -Tf -- "$stage_binary.rollback" "$current_binary"
  fi
  if [[ -f $backup_version ]]; then
    cp -a -- "$backup_version" "$version_file"
  fi
  systemctl restart "$service_name" || true
}

systemctl stop "$service_name"
if ! tar -czf "$state_backup" -C "$state_dir" .; then
  systemctl start "$service_name" || true
  rm -f -- "$stage_binary"
  echo "state backup failed; old binary restarted" >&2
  exit 1
fi
chmod 0600 "$state_backup"

if ! mv -Tf -- "$stage_binary" "$current_binary"; then
  systemctl start "$service_name" || true
  echo "binary replacement failed; old binary restarted" >&2
  exit 1
fi
if ! {
  printf '%s\n' "$release_id" >"$version_file.new"
  chmod 0644 "$version_file.new"
  mv -Tf -- "$version_file.new" "$version_file"
}; then
  rollback
  echo "version metadata update failed; binary rolled back" >&2
  exit 1
fi

if ! systemctl start "$service_name"; then
  rollback
  echo "service failed to start; binary rolled back" >&2
  exit 1
fi

healthy=false
for _ in $(seq 1 30); do
  if systemctl is-active --quiet "$service_name" && curl --fail --silent --show-error --max-time 3 http://127.0.0.1:4141/login >/dev/null; then
    healthy=true
    break
  fi
  sleep 1
done

if [[ $healthy != true ]]; then
  rollback
  echo "health gate failed; binary rolled back" >&2
  exit 1
fi

main_pid=$(systemctl show "$service_name" --property MainPID --value)
running_binary=$(readlink -f "/proc/$main_pid/exe")
running_sha256=$(sha256sum "$running_binary" | awk '{print $1}')
if [[ $running_sha256 != "$binary_sha256" ]]; then
  rollback
  echo "running binary checksum mismatch; binary rolled back" >&2
  exit 1
fi

mapfile -t old_binaries < <(find "$install_root" -mindepth 1 -maxdepth 1 -type f -name 'm365-copilot2api.previous-*' -printf '%T@ %p\n' | sort -nr | tail -n +$((retain + 1)) | cut -d' ' -f2-)
for old_binary in "${old_binaries[@]}"; do
  rm -f -- "$old_binary"
done

mapfile -t old_state_backups < <(find "$state_backup_dir" -mindepth 1 -maxdepth 1 -type f -name '*.tar.gz' -printf '%T@ %p\n' | sort -nr | tail -n +$((retain + 1)) | cut -d' ' -f2-)
for old_backup in "${old_state_backups[@]}"; do
  rm -f -- "$old_backup"
done

rm -f -- "$uploaded_binary"
echo "legacy release active: $release_id"
