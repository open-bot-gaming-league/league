#!/bin/bash
# Leduc storage and regret-floor experiment (EVAL.md "Storage").
# Usage: scripts/leduc_storage.sh <train-binary> <out-dir> [parallel runs, default 12]
set -u
train=$1; out=$2; parallel=${3:-12}
mkdir -p "$out"
runs=()
for seed in 1 2 3; do
  for combo in float32:float32 float16:float32 float32:float16 float16:float16 \
               bfloat16:float32 float32:bfloat16 bfloat16:bfloat16; do
    runs+=("$seed ${combo%%:*} ${combo##*:} -Inf")
  done
  for floor in -300 0; do
    runs+=("$seed float32 float32 $floor")
  done
done
printf '%s\n' "${runs[@]}" | xargs -P "$parallel" -L 1 bash -c '
  "$0" -game leduc -iters 3e8 -checkpoints 3e5,3e6,3e7 -exploit average \
    -seed "$2" -storage "$3" -sum-storage "$4" -floor "$5" \
    > "$1/regret-$3_sum-$4_floor$5_seed$2.tsv" 2> /dev/null' "$train" "$out"
