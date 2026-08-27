#!/usr/bin/env bash
# Builds the cockpit and compiles it into the zerg binary.
set -euo pipefail
cd "$(dirname "$0")"

# Vite 8 and the shadcn-vue CLI require ^22.18.0 || >=24.12.0. Rather than
# activate a version manager, check what is on PATH and say what is wrong —
# a build that silently runs on the wrong Node fails much further downstream.
for tool in node pnpm go; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "$tool is not on PATH; see the README for what this project needs" >&2
    exit 1
  }
done

need=$(cat .nvmrc)
have=$(node -v 2>/dev/null | sed 's/^v//') || true
if [ -z "$have" ]; then
  echo "node is not on PATH; this project needs $need" >&2
  exit 1
fi
major=${have%%.*}
if [ "$major" -lt 24 ]; then
  echo "node $have is too old; this project needs $need (found $(command -v node))" >&2
  exit 1
fi

echo "==> cockpit"
(cd web && pnpm install --frozen-lockfile && pnpm build)

# //go:embed cannot reach outside its own package directory, so the built
# assets are copied next to the code that serves them.
#
# The assets themselves are generated and not committed; the .gitkeep is, and
# it is what keeps `go build` working in a clone that has never run this script.
#
# They are also removed again on the way out, which is not tidiness. Left in
# place, every later `go build` embeds them, the daemon believes it has a
# cockpit, and `zerg up` stops starting the dev server: hot reload would
# disappear after one release build, and the only cure would be knowing to
# delete a directory nobody told you about. A trap, so a failed build cleans up
# too.
clean_embedded() {
  rm -rf internal/api/dist
  mkdir -p internal/api/dist
  touch internal/api/dist/.gitkeep
}
trap clean_embedded EXIT

echo "==> embedding"
clean_embedded
cp -R web/dist/. internal/api/dist/
touch internal/api/dist/.gitkeep

echo "==> zerg"
go build -o zerg ./cmd/zerg
echo "built ./zerg"
