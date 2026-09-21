# Homebrew tap

The cask in `Casks/intraflow.rb` is the source of truth for the published tap.
It is versioned here so the install instructions in the main README stay honest,
and so changes to the cask go through review like any other code.

## One-time setup

Create a **separate** GitHub repository named `homebrew-tap` under the same
account:

```
dev-xz/homebrew-tap
```

The `homebrew-` prefix is what makes `brew tap dev-xz/tap` resolve to it (Homebrew
maps `user/tap` → `user/homebrew-tap`). Then add this cask to it:

```bash
git clone git@github.com:dev-xz/homebrew-tap.git
mkdir -p homebrew-tap/Casks
cp packaging/homebrew/Casks/intraflow.rb homebrew-tap/Casks/
cd homebrew-tap && git add -A && git commit -m "intraflow: add cask" && git push
```

Users then install with:

```bash
brew install dev-xz/tap/intraflow
```

## Each release is automatic

Once the tap repo is seeded (below) and the `HOMEBREW_TAP_TOKEN` secret exists, the
`homebrew-tap` job in `.github/workflows/release.yml` updates the tap on every tag:
it downloads the macOS zip from the release, computes its sha256, rewrites
`version` + `sha256`, validates the Ruby, and pushes to `dev-xz/homebrew-tap`.

No manual bump needed. The job is skipped (with a notice, not a failure) when the
secret is absent.

### Manual fallback

If you need to update the tap by hand:

```bash
VERSION=0.1.0
curl -L -o /tmp/intraflow.zip \
  "https://github.com/dev-xz/intraflow/releases/download/v${VERSION}/intraflow-${VERSION}-macos-universal.zip"
shasum -a 256 /tmp/intraflow.zip
```

Then set `version "0.1.0"` / `sha256 "<output>"` in the tap's cask and push.
Homebrew's own tooling also works once the tap is published:

```bash
brew bump-cask-pr --version 0.1.0 intraflow
```

## Why the install command must be fully qualified

Homebrew 7 introduced a trust model for third-party taps: a cask from an
untrusted tap is refused with `these taps are not trusted`.

`brew install dev-xz/tap/intraflow` works directly, because naming the full tap
path counts as explicit consent and records the cask in
`~/.homebrew/trust.json`. The two-step form below does **not** work on its own:

```bash
brew tap dev-xz/tap
brew install intraflow            # refused: tap not trusted
```

If you prefer that flow, users need one extra command:

```bash
brew tap dev-xz/tap
brew trust --cask dev-xz/tap/intraflow
brew install intraflow
```

The main README therefore documents the single fully-qualified command.

## Quarantine

The `postflight_steps` block clears `com.apple.quarantine` from the installed
bundle. Without it, brew users would hit Gatekeeper on first launch even though
Homebrew's own download never goes through a browser — Homebrew deliberately
applies a quarantine attribute to what it installs.

Note this is a deliberate bypass of a macOS security check, which Homebrew
discourages for first-party taps. For a third-party tap it is common practice
(e.g. goreleaser-generated casks) and `brew audit` does not block it, but it is
worth knowing that the tradeoff is explicit.
