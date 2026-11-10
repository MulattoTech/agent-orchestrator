#!/usr/bin/env bash
# Local thread-transfer benchmark: baseline revision vs this working tree.
#
#   scripts/transfer-benchmark.sh [--baseline-ref REF] [--out-dir DIR]
#       [--ceilings FILE] [--allow-client-change] [--keep-work]
#
# The baseline is REF's committed tree (default: origin/main, else main),
# exported with `git archive` into a private temporary directory. Nothing is
# fetched and no worktree is registered. Only the benchmark harness files are
# copied into it, add-only, and the exported tree is verified blob by blob
# against REF. The candidate is this working tree, including uncommitted
# changes, which are recorded as a digest. Both sides run the same harness,
# fixtures, toolchain and Go settings; the comparator then checks both runs
# against the reviewed ceilings file.
#
# Exit codes: 0 pass, 1 budget or semantic failure, 2 invalid, incomparable or
# setup failure. See docs/performance/thread-transfer-benchmark.md.
set -euo pipefail

usage() {
	sed -n '2,6p' "$0" >&2
	exit 2
}

baseline_ref=""
out_dir=""
ceilings=""
allow_client_change=""
keep_work=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--baseline-ref) baseline_ref="${2:?}"; shift 2 ;;
	--out-dir) out_dir="${2:?}"; shift 2 ;;
	--ceilings) ceilings="${2:?}"; shift 2 ;;
	--allow-client-change) allow_client_change="--allow-client-change"; shift ;;
	--keep-work) keep_work=1; shift ;;
	*) usage ;;
	esac
done

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi
}

fail() {
	echo "transfer-benchmark: $*" >&2
	exit 2
}

if [[ -z "$baseline_ref" ]]; then
	if git rev-parse --verify --quiet origin/main^{commit} >/dev/null; then baseline_ref=origin/main; else baseline_ref=main; fi
fi
base_sha="$(git rev-parse --verify --quiet "${baseline_ref}^{commit}")" || fail "cannot resolve baseline ref $baseline_ref"
head_sha="$(git rev-parse --verify HEAD^{commit})"
ceilings="${ceilings:-$repo/backend/internal/transferbench/ceilings.json}"
[[ -f "$ceilings" && ! -L "$ceilings" ]] || fail "ceilings file $ceilings is missing or not a regular file"
ceilings_sha="$(sha256 <"$ceilings")"

# Paths the benchmark compiles or reads. Dirty state elsewhere cannot change a
# measurement and is not part of the candidate identity.
measured=(backend frontend/src/renderer/hooks/useConversation.ts frontend/src/renderer/lib/event-transport.ts)
dirty_sha=""
if [[ -n "$(git status --porcelain --untracked-files=all -- "${measured[@]}")" ]]; then
	dirty_sha="$({
		git diff --binary HEAD -- "${measured[@]}"
		git ls-files --others --exclude-standard -z -- "${measured[@]}" | while IFS= read -r -d '' f; do
			printf 'untracked %s %s\n' "$f" "$(sha256 <"$f")"
		done
	} | sha256)"
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/ao-transferbench.XXXXXX")"
if [[ -z "$keep_work" ]]; then
	trap 'rm -rf "$work"' EXIT
fi
out_dir="${out_dir:-${TMPDIR:-/tmp}/ao-transferbench-results/$(date -u +%Y%m%dT%H%M%SZ)}"
# The baseline tree lives at a stable path per commit so Go's build cache,
# whose keys include each package directory, is reused across runs. It is
# still deleted and exported afresh every run, then verified.
baseline_root="${TMPDIR:-/tmp}/ao-transferbench-baseline/$base_sha"
rm -rf "$baseline_root"
mkdir -p "$out_dir" "$baseline_root" "$work/bin" "$work/gotmp"

# One Go configuration for every step: no workspace file, no toolchain or
# module downloads, read-only module graph, private temporary build output.
go_env=(env GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off GOMAXPROCS=2 GOTMPDIR="$work/gotmp")

echo "baseline  $baseline_ref = $base_sha"
echo "candidate working tree on $head_sha${dirty_sha:+ (uncommitted changes $dirty_sha)}"
echo "ceilings  $ceilings ($ceilings_sha)"

(cd backend && "${go_env[@]}" go build -o "$work/bin/transferbench" ./internal/transferbench/cmd/transferbench)
bench="$work/bin/transferbench"

git archive --format=tar "$base_sha" -- "${measured[@]}" | tar -x -C "$baseline_root"
git ls-tree -r -z --full-tree "$base_sha" -- "${measured[@]}" >"$work/ls-tree.bin"
"$bench" overlay --candidate-root "$repo" --baseline-root "$baseline_root" --manifest "$work/overlay.json"
"$bench" verify-export --export-root "$baseline_root" --ls-tree "$work/ls-tree.bin" --manifest "$work/overlay.json"

run_side() {
	local side="$1" root="$2" ref="$3" dirty="$4"
	local module_dir
	module_dir="$(cd "$root/backend" && "${go_env[@]}" go list -m -f '{{.Dir}}')"
	[[ "$module_dir" == "$(cd "$root/backend" && pwd -P)" ]] || fail "$side compiles module $module_dir, not $root/backend"
	echo "running $side harness in $root/backend"
	local status=0
	(cd "$root/backend" && "${go_env[@]}" \
		AO_TRANSFERBENCH_OUT="$out_dir/$side.json" AO_TRANSFERBENCH_SIDE="$side" \
		AO_TRANSFERBENCH_SOURCE_REF="$ref" AO_TRANSFERBENCH_DIRTY_DIFF_SHA256="$dirty" \
		AO_TRANSFERBENCH_CEILINGS_SHA256="$ceilings_sha" \
		go test -count=1 -p 1 -run '^TestTransferBenchmark$' ./internal/httpd/ >"$out_dir/$side.log" 2>&1) || status=$?
	if [[ ! -s "$out_dir/$side.json" ]]; then
		tail -n 40 "$out_dir/$side.log" >&2
		fail "$side harness wrote no run artifact"
	fi
	if [[ $status -ne 0 ]]; then
		echo "$side harness reported failures (see $out_dir/$side.log); comparing its artifact anyway" >&2
	fi
}

run_side baseline "$baseline_root" "$base_sha" ""
run_side candidate "$repo" "$head_sha" "$dirty_sha"

status=0
"$bench" compare --baseline "$out_dir/baseline.json" --candidate "$out_dir/candidate.json" --ceilings "$ceilings" \
	--expect-baseline-ref "$base_sha" --expect-candidate-ref "$head_sha" $allow_client_change \
	--out-json "$out_dir/report.json" --out-md "$out_dir/report.md" || status=$?
echo
echo "report: $out_dir/report.md and $out_dir/report.json (exit $status)"
exit "$status"
