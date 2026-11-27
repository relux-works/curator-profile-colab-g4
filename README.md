# `colab-g4-keeper` — Standalone Curator Profile for Google Colab G4 (GLM-5.3-Flash)

Self-contained [Curator](https://github.com/relux-works/curator) profile and
skill (`skills/colab-g4-glm53`) for running **GLM-5.3-Flash Uncensored** on
**Google Colab G4** (**NVIDIA RTX PRO 6000 Blackwell 96 GB**, ~10.05 CU/hr) on
**macOS** and **Ubuntu Linux**.

## Zero-Prerequisite Quickstart (macOS & Ubuntu)

After installing `curator` (e.g. via Homebrew), install this standalone profile:

```bash
curator profile install https://github.com/relux-works/curator-profile-colab-g4.git --as colab-g4-keeper
```

Curator automatically:
1. Provisions a verified Go toolchain into `~/.curator/tools/store/go/` if Go is not installed on the host ([CIP-0040](https://github.com/relux-works/curator-spec/blob/main/cips/CIP-0040-manager-provisioned-go-toolchain.md)).
2. Compiles `colab-glm53` from `skills/colab-g4-glm53/tools/colab-glm53` via the closed `go-v1` build driver.
3. Provisions `astral-sh/uv` (Aqua backend) and `google-colab-cli` (`colab`, curated PyPI backend) into `~/.curator/tools/store/` ([CIP-0007 v1](https://github.com/relux-works/curator-spec/blob/main/protocol/tool-provisioning.md)).
4. Materializes the profile's Slice B-a command root (`colab` + `colab-glm53`) under `~/.curator/environments/colab-g4-keeper/commands/<generation>/<platform>/bin/`.

### Launch the Agent

```bash
curator run claude --profile colab-g4-keeper
# or
curator run codex --profile colab-g4-keeper
```

### First-Time Google Colab Sign-In

If you have not signed in to Colab yet, the agent detects it non-interactively
via `colab-glm53 auth-status --json` and instructs you to run one command in a
separate terminal window (so OAuth codes never touch the LLM context):

```bash
curator profile exec colab-g4-keeper -- colab usage
```

Sign in with your Google AI Ultra / Colab Pro account, paste the verification
code into that terminal, and tell the agent you are ready.

### Direct Operator Commands (via `curator profile exec`)

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
