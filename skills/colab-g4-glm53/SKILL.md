---
name: colab-g4-glm53
description: Provision, monitor, tunnel, and tear down Google Colab G4 (RTX PRO 6000 Blackwell 96 GB) instances running GLM-5.3-Flash Uncensored via Project Maya with automatic auth preflight and Compute Unit cost controls.
triggers:
  - colab g4
  - glm-5.3-flash
  - colab-glm53
  - start colab gpu
  - stop colab
  - colab ultra
---

# Google Colab G4 — GLM-5.3-Flash Uncensored Operator Skill

This skill provides two profile-provisioned commands on `PATH`:
- `colab-glm53`: compiled Go orchestrator (`start`, `tunnel`, `stop`, `status`, `usage`, `sessions`, `auth-status`, `doctor`)
- `colab`: official `google-colab-cli` provisioned automatically by Curator

## Mandatory First Step: Non-Interactive Auth Preflight

Before running any Colab operation (`start`, `sessions`, `usage`, `status`, `stop`), ALWAYS run:

```bash
colab-glm53 auth-status --json
```

If `"authenticated": false`:
1. **NEVER** run interactive `colab usage` or `colab new` yourself (it blocks waiting for an OAuth verification code).
2. **NEVER** ask the user to paste an OAuth code or token into this chat.
3. Show the user the exact `"login_command"` from the JSON output and ask them to run it in a separate terminal window:
   ```bash
   curator profile exec colab-g4-keeper -- colab usage
   ```
   Wait until they confirm sign-in completed, then re-run `colab-glm53 auth-status --json` to verify `"authenticated": true` and inspect the Compute Unit balance.

## Provisioning & Starting a Colab G4 Instance

Hardware: **NVIDIA RTX PRO 6000 Blackwell (96 GB VRAM)**, 176 GiB RAM, 48 vCPU, 236 GB ephemeral disk.  
Billing rate: **~10.05 CU/hr** (~99.5 hours per 1000 Compute Units on Google AI Ultra).

Available EXL3 quant tiers (`MikeRoz/GLM-5.3-Flash-Uncensored-*-h6-exl3` + DFlash2 drafter):
- `2.51` (default, ~102.3 tok/s, 98.1 GiB, ~10–12 min cold start)
- `3.05` (balanced quality/speed, ~89–95 tok/s, 117.5 GiB, ~12–13 min cold start)
- `4.05` (Q4-level quality, ~71.8 tok/s, 153.8 GiB, ~18–20 min cold start)

Recommended start command (detached tunnel on local `127.0.0.1:18100`):
```bash
colab-glm53 start --tier 2.51 --ctx 131072 --port 18100 --session glm53 --bg
```

Long context (262K, uses `int8` KV streaming automatically since `k8v4` cannot be combined with `--kv-resident`):
```bash
colab-glm53 start --tier 2.51 --ctx 262144 --port 18100 --session glm53 --bg
```

Local endpoints once `http://127.0.0.1:18100/v1/models` returns `200 OK`:
- OpenAI API: `http://127.0.0.1:18100/v1` (any non-empty API key, e.g. `sk-local`)
- Anthropic API: `http://127.0.0.1:18100` (`/v1/messages` with thinking + tool calls)
- Web UI / Maya monitor: `http://127.0.0.1:18100/`

## Reconnecting a Dropped Tunnel

If the host slept or the WebSocket disconnected while the Colab VM is still alive:
```bash
colab-glm53 tunnel --session glm53 --port 18100 --bg
```

## Checking Status & Compute Unit Usage

```bash
colab-glm53 status --session glm53 --port 18100
colab-glm53 sessions
colab-glm53 usage
```

## Mandatory Teardown (Stop Billing!)

Whenever work finishes or the user asks to shut down:
```bash
colab-glm53 stop --session glm53 --port 18100
```
Verify in the output that no unintended sessions remain and `Usage rate: 0.00/hr`.

## Colab Guardrails (Already Enforced by `colab-glm53`)
- Never pass `--high-mem` with `--gpu G4` (Colab rejects `shape=hm`; G4 already allocates 176 GiB RAM).
- Remote port `8080` is occupied on Colab; Maya serves on remote `127.0.0.1:8090`, tunneled locally to `127.0.0.1:18100`.
- SSH keys for Colab must be `ed25519` or `ecdsa` (RSA is rejected); `colab-glm53` automatically generates `~/.ssh/id_ed25519_colab`.
- The 236 GB disk is ephemeral: stopping the VM deletes downloaded weights and built binaries.
- Never retry HTTP `401`/`403`/`429` or `TooManyAssignmentsError`; only transient `503`/`502`/`504` capacity responses are retried with jittered backoff.

Reference tuned Maya configs live under `references/configs/`.

## Command Resolution

When launched via `curator run <env> --profile colab-g4-keeper`, Curator prepends the verified profile command root to `PATH` so `colab-glm53` and `colab` resolve directly. In shells without an active profile command root, resolve commands in order: project `.agents/bin/colab-glm53` (`.cmd` on Windows), manager `~/.curator/global/bin/colab-glm53`, or a validated bare command (`command -v colab-glm53` on POSIX / `Get-Command colab-glm53` in PowerShell).
