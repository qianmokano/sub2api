#!/bin/bash
set -euo pipefail

repository=$(cd "$(dirname "$0")/../../.." && pwd)
fixture_build=$(mktemp -d)
fixture_id="kano-updater-ci-${GITHUB_RUN_ID:-local}-$$"
fixture_volume="ku-${GITHUB_RUN_ID:-local}-$$"
fixture_root="/var/lib/docker/volumes/$fixture_volume/_data"
fixture_socket="$fixture_root/kano-updater-it-1234567890/progress/agent.sock"
if [[ ${#fixture_socket} -ge 104 ]]; then
  echo 'Docker fixture volume name exceeds the Unix socket path budget.' >&2
  exit 1
fi
cleanup() {
  docker rm -f "$fixture_id" >/dev/null 2>&1 || true
  docker volume rm "$fixture_volume" >/dev/null 2>&1 || true
  rm -rf "$fixture_build"
}
trap cleanup EXIT
architecture=$(docker info --format '{{.Architecture}}')
case "$architecture" in
  x86_64) architecture=amd64 ;;
  aarch64) architecture=arm64 ;;
  *) echo 'Unsupported Docker test architecture' >&2; exit 1 ;;
esac

cd "$repository/backend"
CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go test -c -tags=integration -o "$fixture_build/agent-integration.test" ./internal/kanoagent
for version in 1 2; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -tags=embed -trimpath -ldflags="-X main.Version=v0.2.16-$version -X main.Commit=1111111111111111111111111111111111111111 -X main.BuildType=kano-container" -o "$fixture_build/server-$version" ./cmd/server
done
cd "$repository"
cp deploy/kano-upgrade-policy.json "$fixture_build/kano-upgrade-policy.json"
docker build -f deploy/tests/kano-updater/Dockerfile.target --build-arg SERVER_FILE=server-2 -t kano-sub2api-test:target "$fixture_build"
# Migration compatibility uses the actual previously published application.
# The browser-only v1 below adds today's UI to that fixture's version number.
docker pull ghcr.io/qianmokano/sub2api:v0.2.16-1
docker tag ghcr.io/qianmokano/sub2api:v0.2.16-1 kano-sub2api-test:base
docker build -f deploy/tests/kano-updater/Dockerfile.harness -t kano-updater-test:harness "$fixture_build"
docker volume create "$fixture_volume" >/dev/null
docker run --rm --name "$fixture_id" \
  --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
  --mount "type=volume,src=$fixture_volume,dst=$fixture_root" \
  -e KANO_UPDATER_DOCKER_TEST=1 -e "KANO_TEST_HARNESS_CONTAINER=$fixture_id" \
  -e KANO_TEST_POLICY=/usr/local/share/kano-upgrade-policy.json \
  -e "KANO_DOCKER_FIXTURE_ROOT=$fixture_root" kano-updater-test:harness \
  -test.run='^TestKanoUpdaterDocker(Lifecycle|BackgroundStartup)$' -test.v -test.timeout=40m
docker build -f deploy/tests/kano-updater/Dockerfile.target --build-arg SERVER_FILE=server-1 --build-arg FIXTURE_VERSION=v0.2.16-1 -t kano-sub2api-test:base "$fixture_build"
docker run --rm --name "$fixture_id" -p 127.0.0.1:19443:19443 \
  --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
  --mount "type=volume,src=$fixture_volume,dst=$fixture_root" \
  -e KANO_UPDATER_BROWSER_TEST=1 -e "KANO_TEST_HARNESS_CONTAINER=$fixture_id" \
  -e KANO_TEST_POLICY=/usr/local/share/kano-upgrade-policy.json \
  -e "KANO_DOCKER_FIXTURE_ROOT=$fixture_root" kano-updater-test:harness \
  -test.run='^TestKanoUpdaterDockerBrowser$' -test.v -test.timeout=15m >"$fixture_build/browser-fixture.log" 2>&1 &
fixture_pid=$!
browser_result=0
node deploy/tests/kano-updater/browser.mjs || browser_result=$?
fixture_result=0
wait "$fixture_pid" || fixture_result=$?
cat "$fixture_build/browser-fixture.log"
if [[ "$browser_result" != 0 || "$fixture_result" != 0 ]]; then exit 1; fi
