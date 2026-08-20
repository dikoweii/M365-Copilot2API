#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 7 ]]; then
  echo "usage: remote-release.sh RELEASE_ID ARCHIVE ARCHIVE_SHA256 SERVICE INSTALL_ROOT STATE_DIR RETAIN" >&2
  exit 2
fi

release_id=$1
archive=$2
archive_sha256=$3
service_name=$4
install_root=$5
state_dir=$6
retain=$7

[[ $release_id =~ ^[A-Za-z0-9._-]+$ ]] || { echo "invalid release id" >&2; exit 2; }
[[ $service_name =~ ^[A-Za-z0-9@._-]+$ ]] || { echo "invalid service name" >&2; exit 2; }
[[ $install_root == /* && $state_dir == /* ]] || { echo "paths must be absolute" >&2; exit 2; }
[[ $retain =~ ^[2-9][0-9]*$ ]] || { echo "retain must be at least 2" >&2; exit 2; }

install -d -m 0755 "$install_root/releases"
install -d -m 0700 "$install_root/state-backups" "$state_dir"
exec 9>"$install_root/.deploy.lock"
flock -n 9 || { echo "another deployment is running" >&2; exit 1; }

actual_archive_sha256=$(sha256sum "$archive" | awk '{print $1}')
[[ $actual_archive_sha256 == "$archive_sha256" ]] || { echo "archive checksum mismatch" >&2; exit 1; }

release_dir="$install_root/releases/$release_id"
stage_dir="$install_root/.incoming-$release_id"
[[ ! -e $release_dir ]] || { echo "release already exists: $release_id" >&2; exit 1; }
rm -rf -- "$stage_dir"
install -d -m 0755 "$stage_dir"
trap 'rm -rf -- "$stage_dir"' EXIT
tar -xzf "$archive" -C "$stage_dir"
(cd "$stage_dir" && sha256sum -c SHA256SUMS)
chmod 0755 "$stage_dir/m365-copilot2api"
mv -- "$stage_dir" "$release_dir"

previous_target=''
if [[ -L "$install_root/current" ]]; then
  previous_target=$(readlink -f "$install_root/current")
fi

systemctl stop "$service_name"
backup="$install_root/state-backups/$release_id.tar.gz"
if ! tar -czf "$backup" -C "$state_dir" .; then
  systemctl start "$service_name" || true
  echo "state backup failed; old release restarted" >&2
  exit 1
fi
chmod 0600 "$backup"

ln -s "$release_dir" "$install_root/.current.new"
mv -Tf "$install_root/.current.new" "$install_root/current"

rollback() {
  if [[ -n $previous_target && -d $previous_target ]]; then
    ln -s "$previous_target" "$install_root/.current.rollback"
    mv -Tf "$install_root/.current.rollback" "$install_root/current"
    systemctl restart "$service_name" || true
  fi
}

if ! systemctl start "$service_name"; then
  rollback
  echo "service failed to start; release rolled back" >&2
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
  echo "health gate failed; release rolled back" >&2
  exit 1
fi

main_pid=$(systemctl show "$service_name" --property MainPID --value)
running_binary=$(readlink -f "/proc/$main_pid/exe")
expected_binary_sha256=$(awk '$2 == "m365-copilot2api" {print $1}' "$release_dir/SHA256SUMS")
running_binary_sha256=$(sha256sum "$running_binary" | awk '{print $1}')
if [[ -z $expected_binary_sha256 || $running_binary_sha256 != "$expected_binary_sha256" ]]; then
  rollback
  echo "running binary checksum mismatch; release rolled back" >&2
  exit 1
fi

if [[ -n $previous_target && -d $previous_target ]]; then
  ln -s "$previous_target" "$install_root/.previous.new"
  mv -Tf "$install_root/.previous.new" "$install_root/previous"
fi

mapfile -t old_releases < <(find "$install_root/releases" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' | sort -nr | tail -n +$((retain + 1)) | cut -d' ' -f2-)
for old_release in "${old_releases[@]}"; do
  [[ $(readlink -f "$install_root/current") == "$old_release" ]] && continue
  [[ -L "$install_root/previous" && $(readlink -f "$install_root/previous") == "$old_release" ]] && continue
  rm -rf -- "$old_release"
done

rm -f -- "$archive"
trap - EXIT
echo "release active: $release_id"
