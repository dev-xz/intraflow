#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
xcrun --find actool >/dev/null 2>&1 || die 'xcrun actool is required (install Xcode).'

wails_bin="$(command -v wails || true)"
if [[ -z "$wails_bin" ]]; then
  wails_bin="$(go env GOPATH)/bin/wails"
  [[ -x "$wails_bin" ]] || die 'wails is not in PATH or GOPATH/bin.'
fi

temp_dir="$(mktemp -d)"
trap 'rm -rf -- "$temp_dir"' EXIT
mkdir "$temp_dir/out"

xcrun actool \
  --compile "$temp_dir/out" \
  --platform macosx \
  --target-device mac \
  --minimum-deployment-target 26.0 \
  --app-icon AppIcon \
  --include-all-app-icons \
  --output-partial-info-plist "$temp_dir/partial.plist" \
  "$repo_root/build/darwin/AppIcon.icon"

assets="$temp_dir/out/Assets.car"
[[ -f "$assets" ]] || die 'actool did not produce Assets.car.'

cd "$repo_root"
"$wails_bin" build "$@"

app="$repo_root/build/bin/intraflow.app"
[[ -d "$app" ]] || die 'Wails did not produce build/bin/intraflow.app.'
resources="$app/Contents/Resources"
[[ -d "$resources" ]] || die 'Wails app bundle has no Contents/Resources directory.'
[[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIconName' "$app/Contents/Info.plist")" == 'AppIcon' ]] || die 'App bundle CFBundleIconName is not AppIcon.'

cp "$assets" "$resources/Assets.car"
codesign --force --deep --sign - "$app"
codesign --verify --deep --strict "$app"
