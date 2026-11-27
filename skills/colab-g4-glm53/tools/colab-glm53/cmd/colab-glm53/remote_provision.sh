set -euo pipefail
HF_REPO="$1"
MODEL_DIR="$2"
CTX="$3"
SPEC_MIN_P="$4"
TIER="$5"
IS_TTY="${6:-0}"
[ -n "${NO_COLOR:-}" ] && IS_TTY=0

fmt_duration() {
  local s="${1:-0}"
  printf '%02d:%02d' $((s / 60)) $((s % 60))
}

step_header() {
  printf '\n[%s/6] %s\n' "$1" "$2"
}

step_ok() {
  printf '  OK: %s\n' "$1"
}

fail_with_log() {
  local msg="$1"
  local logfile="$2"
  local lines="${3:-50}"
  printf '  ERROR: %s\n' "$msg" >&2
  if [ -f "$logfile" ]; then
    printf 'Last %s lines of %s:\n' "$lines" "$logfile" >&2
    tail -n "$lines" "$logfile" >&2 || true
  fi
  exit 42
}

LAST_DURATION=0
run_step() {
  local label="$1"
  local logfile="$2"
  shift 2
  local t0=$SECONDS
  printf '  -> %s\n' "$label"
  "$@" > "$logfile" 2>&1
  LAST_DURATION=$((SECONDS - t0))
}

export PATH="/usr/local/cuda/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

step_header 1 "Checking tools & Python dependencies"
install_deps() {
  if ! command -v ninja >/dev/null 2>&1 || ! command -v cmake >/dev/null 2>&1; then
    apt-get -qq update
    apt-get -qq install -y aria2 ninja-build cmake build-essential
  fi
  if ! python3 -c "import huggingface_hub, jinja2, regex, yaml, tqdm, requests, PIL, psutil, jsonschema" 2>/dev/null; then
    pip -q install -U huggingface_hub jinja2 regex pyyaml tqdm requests pillow psutil "jsonschema>=4.18,<5"
  fi
}
if command -v ninja >/dev/null 2>&1 && command -v cmake >/dev/null 2>&1 && \
   python3 -c "import huggingface_hub, jinja2, regex, yaml, tqdm, requests, PIL, psutil, jsonschema" 2>/dev/null; then
  step_ok "Cached"
else
  run_step "Installing build tools & Python packages..." /content/maya-deps.log install_deps || \
    fail_with_log "Failed to install tools/dependencies." /content/maya-deps.log 50
  step_ok "Dependencies ready in $(fmt_duration "$LAST_DURATION")"
fi

step_header 2 "Building Project Maya (CUDA sm_120)"
cd /content
if [ ! -f /content/project-maya/CMakeLists.txt ]; then
  rm -rf /content/project-maya
  run_step "Cloning Project Maya..." /content/maya-build.log \
    git clone --depth 1 https://github.com/mw00/project-maya.git /content/project-maya || \
    fail_with_log "Failed to clone Project Maya." /content/maya-build.log 50
fi
cd /content/project-maya

build_maya() {
  python3 -c 'import setup as S; S.get_llama_cpp()'
  cmake -G Ninja -S . -B build \
    -DCMAKE_BUILD_TYPE=Release \
    -DSTRATA_ENABLE_CUDA=ON \
    -DCMAKE_CUDA_ARCHITECTURES=120 \
    -DSTRATA_NATIVE_EXPERTS=ON \
    -DSTRATA_BUILD_TESTS=OFF \
    -DCMAKE_CUDA_COMPILER=/usr/local/cuda/bin/nvcc \
    -DCUDAToolkit_ROOT=/usr/local/cuda \
    -DSTRATA_GGML_DIR=/content/project-maya/third_party/llama.cpp
  cmake --build build --target maya -j 24
}

if [ -x /content/project-maya/build/maya ]; then
  step_ok "Cached"
else
  run_step "Building Maya (CUDA sm_120)..." /content/maya-build.log build_maya || \
    fail_with_log "Project Maya build failed." /content/maya-build.log 50
  step_ok "Built in $(fmt_duration "$LAST_DURATION")"
fi

step_header 3 "Downloading DFlash2 drafter (665 MB) & ${HF_REPO}"
mkdir -p /content/models/dflash2 "/content/models/${MODEL_DIR}"

need_dflash=0
need_model=0
[ ! -f /content/models/dflash2/GLM-5.3-Flash-DFlash2-Q4_K_M.gguf ] && need_dflash=1
[ ! -f "/content/models/${MODEL_DIR}/pack/.packed_ok" ] && need_model=1

download_dflash() {
  local tmp="/content/models/dflash2/GLM-5.3-Flash-DFlash2-Q4_K_M.gguf.tmp"
  rm -f "$tmp"
  curl -fSL -o "$tmp" \
    "https://huggingface.co/Anbeeld/GLM-5.3-Flash-DFlash2-GGUF/resolve/f3569acb7f9cee95e66d24442d75536eb00ab842/GLM-5.3-Flash-DFlash2-Q4_K_M.gguf"
  mv -f "$tmp" /content/models/dflash2/GLM-5.3-Flash-DFlash2-Q4_K_M.gguf
}

if [ "$need_dflash" -eq 0 ] && [ "$need_model" -eq 0 ]; then
  step_ok "Cached"
else
  if [ "$need_dflash" -eq 1 ]; then
    run_step "Downloading DFlash2 drafter (665 MB)..." /content/maya-dflash.log download_dflash || \
      fail_with_log "Failed to download DFlash2 drafter." /content/maya-dflash.log 50
    step_ok "DFlash2 drafter downloaded in $(fmt_duration "$LAST_DURATION")"
  else
    step_ok "DFlash2 drafter cached"
  fi

  if [ "$need_model" -eq 1 ]; then
    for d in /content/models/MikeRoz-*; do
      [ -d "$d" ] && [ "$(basename "$d")" != "$MODEL_DIR" ] && rm -rf "$d"
    done
    dl_t0=$SECONDS
    printf '  -> Downloading %s -> /content/models/%s\n' "$HF_REPO" "$MODEL_DIR"
    env HF_XET_HIGH_PERFORMANCE=1 HF_HUB_DISABLE_PROGRESS_BARS=1 python3 - "$HF_REPO" "/content/models/${MODEL_DIR}" <<'PY'
import sys
from huggingface_hub import snapshot_download

repo, dst = sys.argv[1], sys.argv[2]
snapshot_download(repo_id=repo, local_dir=dst, max_workers=8)
PY
    step_ok "Model weights downloaded in $(fmt_duration "$((SECONDS - dl_t0))")"
  fi
fi

step_header 4 "Packing EXL3 weights (zero-copy)"
if [ -f "/content/models/${MODEL_DIR}/pack/.packed_ok" ]; then
  step_ok "Cached"
else
  rm -rf "/content/models/${MODEL_DIR}/pack"
  run_step "Packing EXL3 weights..." /content/maya-pack.log \
    python3 /content/project-maya/tools/exl3_pack.py --model "/content/models/${MODEL_DIR}" || \
    fail_with_log "Failed to pack EXL3 weights." /content/maya-pack.log 50
  touch "/content/models/${MODEL_DIR}/pack/.packed_ok"
  step_ok "Packed in $(fmt_duration "$LAST_DURATION")"
fi

step_header 5 "Writing Maya server config"
python3 - "$MODEL_DIR" "$CTX" "$SPEC_MIN_P" "$TIER" <<'PY'
import sys, json
model_dir, ctx, spec_p, tier = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
pack = f"/content/models/{model_dir}/pack"

args = [
    "--glm-pack", pack,
    "--max-context", str(ctx),
    "--vram-reserve-mib", "200",
    "--glm-ram-resident",
    "--dflash", "/content/models/dflash2/GLM-5.3-Flash-DFlash2-Q4_K_M.gguf",
    "--spec-min-p", spec_p,
]
if ctx <= 131072:
    args += ["--kv", "k8v4"]
else:
    args += ["--kv", "int8", "--kv-resident", "20480"]

cfg = {
    "exe": "/content/project-maya/build/maya",
    "pack": pack,
    "tokenizer": f"{pack}/tokenizer",
    "model": f"glm-5.3-flash-uncensored-{tier}bpw",
    "model_name": f"glm-5.3-flash-uncensored-{tier}bpw",
    "host": "127.0.0.1",
    "port": 8090,
    "max_context": ctx,
    "args": args,
    "env": {
        "CUDA_DEVICE_ORDER": "PCI_BUS_ID",
        "CUDA_VISIBLE_DEVICES": "0",
        "MAYA_GLM_PCIE_SHARE": "0.00",
        "MAYA_GLM_CPU_LANE": "18",
        "MAYA_GLM_NGRAM_DRAFT": "1",
        "MAYA_GLM_DSA_GRAPHS": "1",
    },
}
with open("/content/maya-glm53.json", "w") as f:
    json.dump(cfg, f, indent=2)
PY
step_ok "Config ready: tier=${TIER}, ctx=${CTX}, DFlash2, spec_min_p=${SPEC_MIN_P}, CPU_LANE=18, DSA_GRAPHS=1"

step_header 6 "Loading GLM-5.3-Flash (${TIER} bpw) into 96 GB VRAM"
pkill -f "serve/server.py" 2>/dev/null || true
pkill -f "build/maya" 2>/dev/null || true
sleep 1

nohup python3 /content/project-maya/serve/server.py \
  --engine maya \
  --config /content/maya-glm53.json \
  --host 127.0.0.1 \
  --port 8090 \
  > /content/maya-server.log 2>&1 < /dev/null &
srv_pid=$!

ready=0
t0=$SECONDS
last_log=0
printf '  Waiting for model load (cold ~3-5 min first time, ~25s warm)...\n'
while true; do
  elapsed=$((SECONDS - t0))
  if ! kill -0 "$srv_pid" 2>/dev/null; then
    fail_with_log "Maya server exited unexpectedly during load." /content/maya-server.log 40
  fi
  if [ "$elapsed" -ge 540 ]; then
    break
  fi
  if curl -sf http://127.0.0.1:8090/v1/models >/dev/null 2>&1; then
    ready=1
    break
  fi
  if [ "$elapsed" -gt 0 ] && [ $((elapsed / 30)) -gt "$last_log" ]; then
    last_log=$((elapsed / 30))
    printf '  still loading... (%s)\n' "$(fmt_duration "$elapsed")"
  fi
  sleep 2
done

if [ "$ready" -ne 1 ]; then
  fail_with_log "Maya server did not become ready within 540s." /content/maya-server.log 40
fi
step_ok "Model ready in $(fmt_duration "$((SECONDS - t0))")"
