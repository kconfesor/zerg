#!/usr/bin/env bash
# Builds the cockpit and compiles it into the zerg binary.
#
# The frontend needs Node 24.19.0 (see .nvmrc): create-vue and Vite 8 require
# ^22.18.0 || >=24.12.0, and this machine's shell resolves to 22.12.0.
set -euo pipefail
cd "$(dirname "$0")"

if [ -s "$HOME/.nvm/nvm.sh" ]; then
  # shellcheck disable=SC1091
  . "$HOME/.nvm/nvm.sh"
  nvm use "$(cat .nvmrc)" >/dev/null
fi

echo "==> cockpit"
(cd web && npm run build)

# The Go embed directive cannot reach outside its own package directory, so the
# built assets are copied next to the code that serves them.
echo "==> embedding"
rm -rf internal/api/dist
cp -R web/dist internal/api/dist

echo "==> zerg"
go build -o zerg ./cmd/zerg
echo "built ./zerg"
