#!/bin/bash
# Play-speed benchmarks, pinned: bench.sh [core] [raw-output-dir]
# Runs BenchmarkPlay and BenchmarkStart on one core with GOMAXPROCS=1,
# 5 repeats, twice: the default build (native house bots map their bucket
# files) and -tags tables_on_demand (native house bots read them on
# demand). Prints a table of the results; keeps the raw output in the dir.
# Needs the house files (go run ./sdk/cmd/house download all, in poker/engine).
set -u
cd "$(dirname "$0")" || exit 9
CORE=${1:-15}
OUT=${2:-$(mktemp -d)}
mkdir -p "$OUT"
go test -c -o "$OUT/arena.test" . || exit 9
go test -c -tags tables_on_demand -o "$OUT/arena-ondemand.test" . || exit 9
run() { # binary pattern raw
	echo "load before: $(cat /proc/loadavg)" > "$OUT/$3"
	taskset -c "$CORE" env GOMAXPROCS=1 nice -n 19 "$OUT/$1" -test.run '^$' -test.bench "$2" \
		-test.benchtime=5x -test.count=5 >> "$OUT/$3" 2>&1
	local rc=$?
	echo "load after: $(cat /proc/loadavg)" >> "$OUT/$3"
	return $rc
}
run arena.test . default.txt || { tail "$OUT/default.txt"; exit 1; }
run arena-ondemand.test '(Play|Start)/(turtle|ox)/native' ondemand.txt || { tail "$OUT/ondemand.txt"; exit 1; }
python3 - "$OUT" <<'PY'
import re, statistics, sys
out = sys.argv[1]
rows = {}  # (kind, bot, mode) -> metric -> [values]
loads = []
for fname, native in (("default.txt", "native"), ("ondemand.txt", "native-ondemand")):
    for line in open(f"{out}/{fname}"):
        if line.startswith("load"):
            loads.append(f"{fname} {line.strip()}")
        m = re.match(r"Benchmark(Play|Start)/(\w+)/([\w-]+?)(-\d+)?\s+\d+\s+(.*)", line)
        if not m:
            continue
        kind, bot, mode, _, rest = m.groups()
        if mode == "native":
            mode = native
        vals = dict((u, float(v)) for v, u in re.findall(r"([\d.]+) ([\w/-]+)", rest))
        for k, v in vals.items():
            rows.setdefault((kind, bot, mode), {}).setdefault(k, []).append(v)

def cell(vs, scale=1.0, fmt="{:.2f}"):
    med = statistics.median(vs) / scale
    lo, hi = min(vs) / scale, max(vs) / scale
    spread = (hi - lo) / med * 100 if med else 0
    return fmt.format(med) + f" ({spread:.0f}%)"

bots = ["dove", "clam", "shark", "turtle", "ox"]
print("| bot | mode | mean us | p50 us | p99 us | mean ratio | p99 ratio | hands/s | hands/s ratio |")
print("|---|---|---|---|---|---|---|---|---|")
for bot in bots:
    base = rows.get(("Play", bot, "native"))
    for mode in ["native", "native-ondemand", "wasm", "wasm-ondemand", "wasm-inmemory"]:
        r = rows.get(("Play", bot, mode))
        if not r:
            continue
        mean = statistics.median(r["ns-mean"]); p99 = statistics.median(r["ns-p99"]); hs = statistics.median(r["hands/s"])
        ratio = lambda x, k: f"{x / statistics.median(base[k]):.1f}x" if base and mode != "native" else ""
        hratio = f"{statistics.median(base['hands/s']) / hs:.1f}x" if base and mode != "native" else ""
        print(f"| {bot} | {mode} | {cell(r['ns-mean'], 1000, '{:.3f}')} | {cell(r['ns-p50'], 1000, '{:.3f}')} | {cell(r['ns-p99'], 1000, '{:.3f}')} "
              f"| {ratio(mean, 'ns-mean')} | {ratio(p99, 'ns-p99')} | {cell(r['hands/s'], 1, '{:,.0f}')} | {hratio} |")
print()
print("| bot | mode | start ms |")
print("|---|---|---|")
for bot in bots:
    for mode in ["native", "native-ondemand", "wasm", "wasm-ondemand", "wasm-inmemory"]:
        r = rows.get(("Start", bot, mode))
        if r:
            print(f"| {bot} | {mode} | {cell(r['ns/op'], 1e6, '{:.3f}')} |")
print()
for l in loads:
    print("-", l)
PY
