#!/usr/bin/env bash
# Build before replacing the running service; preserve all host-owned settings.
set -euo pipefail
umask 077

repo=${1:?repository path required}
revision=${2:?tested commit required}
backup_root=${CPA_BACKUP_ROOT:-/home/fkh/.cpa-backups}
mkdir -p "$backup_root"
exec 9>"$backup_root/production-deploy.lock"
flock -n 9 || { echo "Another CPA deployment is active."; exit 1; }
cd "$repo"
test -f config.yaml
python3 -c 'import yaml'

# Runtime settings belong in ignored files, not tracked deployment templates.
# Refuse unexpected edits rather than discarding them with reset --hard.
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "Tracked files contain local edits. Move deployment settings to docker-compose.override.yml first."
  exit 1
fi
test "$(git branch --show-current)" = main
git fetch --prune origin main
target=$(git rev-parse --verify "$revision^{commit}")
git merge-base --is-ancestor "$target" origin/main
git merge-base --is-ancestor HEAD "$target"

backup=$(mktemp -d "$backup_root/bps-deploy-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
git rev-parse HEAD > "$backup/previous-commit"
for file in config.yaml docker-compose.yml docker-compose.override.yml .env; do
  if test -f "$file"; then cp -p "$file" "$backup/$file"; fi
done
chmod -R go-rwx "$backup"
docker compose config --format json > "$backup/previous-compose.json"
previous_image=$(docker inspect --format '{{.Image}}' cli-proxy-api)
rollback_image="local/cli-proxy-api:rollback-${target:0:12}-$(date -u +%s)"
docker tag "$previous_image" "$rollback_image"
printf '%s\n' "$rollback_image" > "$backup/rollback-image"

mkdir "$backup/source"
git archive "$target" | tar -x -C "$backup/source"
image="local/cli-proxy-api:bps-${target:0:12}"
version=$(cat "$backup/source/.github/upstream-version")
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
DOCKER_BUILDKIT=1 docker build \
  --build-arg VERSION="$version-bps" \
  --build-arg COMMIT="$target" \
  --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t "$image" "$backup/source"

artifact_container=$(docker create "$image")
docker cp "$artifact_container:/opt/cliproxy-plugins/bps.so" "$backup/bps.so"
docker rm "$artifact_container" >/dev/null

# Use resolved bind paths so environment overrides and existing mounts survive.
plugin_dir=$(python3 - "$backup/previous-compose.json" <<'PY'
import json, sys
service = json.load(open(sys.argv[1]))['services']['cli-proxy-api']
matches = [v['source'] for v in service['volumes']
           if v['type'] == 'bind' and v['target'] == '/CLIProxyAPI/plugins']
assert len(matches) == 1, 'Expected one plugin bind mount'
print(matches[0])
PY
)
architecture=$(docker image inspect "$image" --format '{{.Architecture}}')
plugin="$plugin_dir/linux/$architecture/bps-v0.5.2.so"
mkdir -p "$(dirname "$plugin")"
if test -f "$plugin"; then cp -p "$plugin" "$backup/previous-bps.so"; fi

# Resolve the new compose template with the same private override and .env.
git merge --ff-only "$target"
export CLI_PROXY_IMAGE="$image"
docker compose config -q
docker compose config --format json > "$backup/next-compose.json"
python3 - "$backup/previous-compose.json" "$backup/next-compose.json" <<'PY'
import json, sys
before, after = [json.load(open(p))['services']['cli-proxy-api'] for p in sys.argv[1:]]
for key in ('environment', 'ports', 'volumes', 'networks'):
    if before.get(key) != after.get(key):
        raise SystemExit('Deployment settings changed: ' + key + '. Review docker-compose.override.yml before retrying.')
PY
changed=0
rollback() {
  status=$?
  trap - ERR
  if test "$changed" = 1; then
    cp -p "$backup/config.yaml" config.yaml
    if test -f "$backup/previous-bps.so"; then
      cp -p "$backup/previous-bps.so" "$plugin"
    else
      rm -f "$plugin"
    fi
    python3 - "$backup/previous-compose.json" "$backup/rollback-compose.json" "$rollback_image" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
data['services']['cli-proxy-api']['image'] = sys.argv[3]
with open(sys.argv[2], 'w') as out:
    json.dump(data, out)
PY
    docker compose -f "$backup/rollback-compose.json" up -d --no-build --pull never --no-deps cli-proxy-api || true
    echo "Deployment failed; previous image/config restored. Backup: $backup"
  fi
  exit "$status"
}
trap rollback ERR
changed=1
install -m 644 "$backup/bps.so" "$plugin.new"
mv "$plugin.new" "$plugin"

python3 - config.yaml <<'PY'
import pathlib, sys, yaml
p = pathlib.Path(sys.argv[1])
raw = p.read_text()
doc = yaml.safe_load(raw)
plugins = doc.setdefault('plugins', {})
configs = plugins.setdefault('configs', {})
if not plugins.get('enabled') or not configs.get('bps', {}).get('enabled'):
    plugins['enabled'] = True
    configs.setdefault('bps', {})['enabled'] = True
    # Only use a full serialization when the necessary mapping is absent.
    lines = raw.splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if line.rstrip() == 'plugins:']
    if starts:
        start = starts[0]
        end = next((i for i in range(start + 1, len(lines))
                    if lines[i].strip() and not lines[i].startswith((' ', '\t', '#'))), len(lines))
        nodes = [i for i in range(start + 1, end) if lines[i].strip() == 'configs:']
        if nodes and not any(line.strip() == 'bps:' for line in lines[nodes[0]+1:end]) and yaml.safe_load(raw)['plugins'].get('enabled'):
            index = nodes[0]
            indent = len(lines[index]) - len(lines[index].lstrip())
            lines[index+1:index+1] = [' '*(indent*2)+'bps:\n', ' '*(indent*3)+'enabled: true\n']
            updated = ''.join(lines)
        else:
            updated = yaml.safe_dump(doc, sort_keys=False, allow_unicode=True)
    else:
        updated = yaml.safe_dump(doc, sort_keys=False, allow_unicode=True)
    assert yaml.safe_load(updated) == doc
    p.write_text(updated)
    p.chmod(0o600)
PY

docker compose up -d --no-build --pull never --no-deps cli-proxy-api
healthy=0
for _ in $(seq 1 30); do
  if curl --fail --silent --show-error --max-time 5 http://127.0.0.1:8317/healthz >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 2
done
test "$healthy" = 1
docker logs cli-proxy-api 2>&1 | grep -F 'plugin registered plugin_id=bps ' >/dev/null

# Persist the selected local image for future compose up/restart commands.
python3 - "$image" <<'PY'
import pathlib, sys
p = pathlib.Path('.env')
lines = p.read_text().splitlines() if p.exists() else []
lines = [line for line in lines if not line.startswith('CLI_PROXY_IMAGE=')]
lines.append('CLI_PROXY_IMAGE=' + sys.argv[1])
p.write_text('\n'.join(lines) + '\n')
p.chmod(0o600)
PY
trap - ERR
printf 'CPA healthy; BPS loaded. Commit: %s\nBackup: %s\n' "$target" "$backup"
