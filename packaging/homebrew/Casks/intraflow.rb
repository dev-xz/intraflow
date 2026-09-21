cask "intraflow" do
  version "0.1.1"
  # Replace this with the real checksum after the first tagged release:
  #   shasum -a 256 intraflow-<version>-macos-universal.zip
  # A wrong value here blocks the install (which is the safe failure mode).
  sha256 "REPLACE_WITH_SHA256_AFTER_FIRST_RELEASE"

  url "https://github.com/dev-xz/intraflow/releases/download/v#{version}/intraflow-#{version}-macos-universal.zip"

  name "IntraFlow"
  desc "Redirect public tunnel domains to internal LAN addresses via hosts hijack"
  homepage "https://github.com/dev-xz/intraflow"

  livecheck do
    url :url
    strategy :github_latest
  end

  app "intraflow.app"

  # IntraFlow is distributed without Apple notarization, so a plain browser
  # download would be blocked by Gatekeeper on first launch. Homebrew already
  # marks the download as "moved" (quarantine flag bit 0x0100), which avoids
  # Gatekeeper's App Translocation, but the app would still be assessed once.
  # Clearing the quarantine attribute here means brew users get a build that
  # just launches, with no extra step and no "Open Anyway" dance.
  #
  # This intentionally bypasses a macOS security mechanism; it is the user's
  # choice to install from this tap. `xattr -d` is the documented manual
  # equivalent for users who download the zip directly.
  #
  # Keep this in the modern `postflight_steps` (declarative) form: the older
  # `postflight do ... system_command ... end` block is deprecated.
  postflight_steps do
    run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{appdir}}/intraflow.app"]
  end
end
