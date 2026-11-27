# `colab-g4-keeper` — Standalone Curator Profile for Google Colab G4 (GLM-5.3-Flash)

Self-contained [Curator](https://github.com/relux-works/curator) profile and
skill (`skills/colab-g4-glm53`) for running **GLM-5.3-Flash Uncensored** on
**Google Colab G4** (**NVIDIA RTX PRO 6000 Blackwell 96 GB**, ~10.05 CU/hr) on
**macOS** and **Ubuntu Linux**.

No pre-installed Go, Python, `uv`, or `google-colab-cli` is required — Curator
provisions and verifies everything into `~/.curator/tools/store/` automatically
without touching your global `PATH`.

---

## Quickstart — Variant A (1 binary: `curator` only, lowest barrier)

### 1. Install `curator` (`v0.15.1`)

**macOS (Homebrew):**
```bash
brew install relux-works/tap/curator
```

**Ubuntu Linux (`.deb`, zero extra dependencies):**
```bash
# amd64 (use linux_arm64 on ARM64)
curl -fsSLO https://github.com/relux-works/curator/releases/download/v0.15.1/curator_0.15.1_linux_amd64.deb
sudo dpkg -i curator_0.15.1_linux_amd64.deb
```
*(Alternatively, on macOS or Linux with `gh` or `cosign` installed: `curl -fsSL https://raw.githubusercontent.com/relux-works/curator/main/install.sh | sh`.)*

### 2. Install this profile (no `bootstrap` needed)

```bash
curator profile install https://github.com/relux-works/curator-profile-colab-g4.git --as colab-g4-keeper
```

Curator automatically:
1. Provisions a verified Go toolchain into `~/.curator/tools/store/go/` if Go is absent ([CIP-0040](https://github.com/relux-works/curator-spec/blob/main/cips/CIP-0040-manager-provisioned-go-toolchain.md)).
2. Builds `colab-glm53` from `skills/colab-g4-glm53/tools/colab-glm53` via the closed `go-v1` driver.
3. Provisions `astral-sh/uv` (Aqua backend) and `google-colab-cli` (`colab`, curated PyPI backend) into `~/.curator/tools/store/` ([CIP-0007 v1](https://github.com/relux-works/curator-spec/blob/main/protocol/tool-provisioning.md)).
4. Materializes the profile's verified command root (`colab` + `colab-glm53`) under `~/.curator/environments/colab-g4-keeper/commands/`.

### 3. Sign in to Google Colab (once, in your own terminal)

```bash
curator profile exec colab-g4-keeper -- colab usage
```

Sign in with your Google AI Ultra / Colab Pro account and paste the verification
code into that terminal (OAuth codes never enter the agent's context).

### 4. Launch your agent with the profile

```bash
curator profile exec colab-g4-keeper --env codex -- codex
# or
curator profile exec colab-g4-keeper --env claude -- claude
```

*(Or load the profile into your current shell session: `eval "$(curator env resolve codex --profile colab-g4-keeper --repair --format shell)" && codex`.)*

---

## Variant B — Full `curator run` (adds `curator-run` `v0.8.0`)

If you want `curator run` (managed model/effort lineup, session-manager hosting,
and typed `--permissions native|yolo` / `--yolo` with a consistent `native`
default across all harnesses), also install `curator-run`:

**macOS (Homebrew):**
```bash
brew install relux-works/tap/curator-run
```

**Ubuntu Linux (`.deb`):**
```bash
# amd64 (use linux_arm64 on ARM64)
curl -fsSLO https://github.com/relux-works/curator-agent-launcher/releases/download/v0.8.0/curator-run_0.8.0_linux_amd64.deb
sudo dpkg -i curator-run_0.8.0_linux_amd64.deb
```

Then launch with:
```bash
curator run codex --profile colab-g4-keeper          # native permissions (default)
curator run codex --yolo --profile colab-g4-keeper   # opt-in bypass
curator run claude --profile colab-g4-keeper
```

---

## Direct Operator Commands (via `curator profile exec`)

```bash
# Check auth and Compute Unit balance
curator profile exec colab-g4-keeper -- colab-glm53 auth-status

# Start GLM-5.3-Flash 2.51 bpw (131K ctx) with background SSH tunnel on :18100
curator profile exec colab-g4-keeper -- colab-glm53 start --tier 2.51 --ctx 131072 --port 18100 --bg

# Reconnect tunnel if the laptop slept
curator profile exec colab-g4-keeper -- colab-glm53 tunnel --session glm53 --port 18100 --bg

# Stop billing when finished (verify Usage rate: 0.00/hr)
curator profile exec colab-g4-keeper -- colab-glm53 stop --session glm53 --port 18100
```
