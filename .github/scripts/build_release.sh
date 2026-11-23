#!/bin/sh
# Build the release artifacts used by GitHub Actions.
#
# The zip whitelist below must stay in sync with build.sh:73-75.
# A WebUI install/build failure is fatal; never fall back to the committed
# module/webroot bundle.
set -eu
cd "$(dirname "$0")/../.."

if [ ! -f webui/pnpm-lock.yaml ]; then
    echo "FATAL: webui/pnpm-lock.yaml is required for the frozen-lockfile install." >&2
    exit 1
fi

echo "==> building the WebUI (hard failure, no committed-webroot fallback)"
(cd webui && pnpm install --frozen-lockfile && pnpm --config.verify-deps-before-run=false run build)

echo "==> running the release test gate"
go test ./...

OUT=phoneserver-arm64
echo "==> building $OUT (linux/arm64, static)"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$OUT" .

if ! readelf -l "$OUT" >/dev/null 2>&1; then
    echo "FATAL: unable to inspect $OUT with readelf." >&2
    exit 1
fi
if readelf -l "$OUT" | grep -q INTERP; then
    echo "FATAL: $OUT has a PT_INTERP segment; Android cannot execute it." >&2
    exit 1
fi

echo "==> staging into the module tree"
mkdir -p module/bin
cp "$OUT" module/bin/phoneserver
chmod 0755 module/bin/phoneserver

cd module
chmod 0755 ./*.sh bin/phoneserver
rm -f ../phoneserver-module.zip
zip -r -X ../phoneserver-module.zip \
    module.prop customize.sh service.sh watchdog.sh action.sh uninstall.sh \
    webui.sh README.md bin webroot META-INF >/dev/null
cd ..

sha256sum phoneserver-module.zip > phoneserver-module.zip.sha256

echo "==> release artifacts ready"
ls -l "$OUT" phoneserver-module.zip phoneserver-module.zip.sha256
cat phoneserver-module.zip.sha256
