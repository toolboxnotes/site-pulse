#!/bin/sh
# Builds dist/Site Pulse.exe (Windows) and dist/Site-Pulse-Mac.zip (macOS, Apple Silicon + Intel).
set -e
cd "$(dirname "$0")"
rm -rf dist && mkdir -p dist
LD="-s -w"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LD -H=windowsgui" -o "dist/Site Pulse.exe" .
APP="dist/mac/Site Pulse.app/Contents"
mkdir -p "$APP/MacOS" "$APP/Resources"
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LD" -o "$APP/MacOS/sitepulse-arm64" .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$LD" -o "$APP/MacOS/sitepulse-x86_64" .
cat > "$APP/MacOS/Site Pulse" <<'SH'
#!/bin/sh
DIR="$(cd "$(dirname "$0")" && pwd)"
ARCH="$(uname -m)"
[ "$ARCH" = "arm64" ] || ARCH="x86_64"
exec "$DIR/sitepulse-$ARCH" "$@"
SH
chmod +x "$APP/MacOS/"*
cp build/icon.icns "$APP/Resources/AppIcon.icns"
cat > "$APP/Info.plist" <<'PL'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Site Pulse</string>
<key>CFBundleDisplayName</key><string>Site Pulse</string>
<key>CFBundleIdentifier</key><string>com.toolboxnotes.sitepulse</string>
<key>CFBundleVersion</key><string>1.3.10</string>
<key>CFBundleShortVersionString</key><string>1.3.10</string>
<key>CFBundleExecutable</key><string>Site Pulse</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSMinimumSystemVersion</key><string>11.0</string>
<key>LSUIElement</key><true/>
</dict></plist>
PL
python3 - <<'PY'
import os, zipfile
root = "dist/mac"
with zipfile.ZipFile("dist/Site-Pulse-Mac.zip", "w", zipfile.ZIP_DEFLATED) as z:
    for d, dirs, files in os.walk(root):
        for f in files:
            p = os.path.join(d, f); arc = os.path.relpath(p, root)
            zi = zipfile.ZipInfo(arc); mode = os.stat(p).st_mode
            zi.external_attr = (mode & 0xFFFF) << 16; zi.compress_type = zipfile.ZIP_DEFLATED
            z.writestr(zi, open(p, "rb").read())
PY
rm -rf dist/mac
ls -la dist
