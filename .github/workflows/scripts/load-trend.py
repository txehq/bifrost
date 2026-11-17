#!/usr/bin/env python3
"""Compares a release's load-test overhead with the releases before it (TXE-7762).

The load test's absolute limits (mean 5 ms, p99 100 ms) only fail once the overhead is
already bad, and the release recorded one sample with nothing comparing one release to the
next. Its non-streaming mean read 0.71 ms (sync.10), 1.21 ms (sync.11) and 2.31 ms
(sync.12), and nobody could tell a creep from noise. This script is that comparison.

Each release records its result as the release asset load-performance.json: the
load-test-results.json that load-test.sh writes, whose latency_us is the median over
OVERHEAD_SAMPLES runs. The next release's load job fetches the assets of the last
BASELINE_RELEASES releases that have a complete one and compares:

    baseline  = median of those releases' recorded values, per mode and metric
    regressed = current > max(baseline * ratio, baseline + floor)

Both sides are over more than one sample: the current value is a median of
OVERHEAD_SAMPLES runs in this job, the baseline a median over releases. The floor stops a
ratio of two sub-millisecond numbers firing on runner noise. BOUNDS holds the bounds and the
measured noise they were set from.

Usage:
  load-trend.py fetch --repo OWNER/NAME --exclude-tag TAG --out DIR
  load-trend.py compare --current FILE [--baseline FILE ...] [--summary FILE] [--out FILE] [--warn-only]
  load-trend.py check-result FILE      # exit 0 when FILE has every mode and metric compared
  load-trend.py --self-test
Exit codes: 0 within bounds (or nothing to compare against, or --warn-only), 1 regressed,
2 usage/input.
"""
import argparse
import json
import statistics
import subprocess
import sys
from pathlib import Path

ASSET = "load-performance.json"
TAG_PREFIX = "transports/v"
BASELINE_RELEASES = 3
MODES = ("non_streaming", "streaming")
METRICS = ("mean", "p50", "p95", "p99")
# (ratio, floor in µs) per mode and metric. Set from the TXE-7762 bisect: 11 builds of
# sync.10..sync.12 x 12 runs each on ubuntu-24.04 hosted runners, 132 single samples per
# mode, with no code difference between the builds that the runs could separate. Simulating
# "median of 3 this run" against "median of 3 releases' medians" from those samples:
#   mean x1.4/+0.3 ms: 0.5% false alarms (non-streaming), detects +50% 72%, +100% 100%
#   p50, p95 x1.3/+0.15 ms: under 0.05% false alarms, detects +50% over 95%
#   non-streaming p99 x3/+30 ms: 0.1% false alarms. Its single samples ran 3.7-110 ms, so
#     only a large tail regression shows (x2/+20 ms would false-alarm 5.7%)
#   streaming p99 x1.5/+1 ms: its single samples ran 1.2-3.0 ms
# Those rates hold within one day's runners. Release runs on other days were noisier
# (sync.12's two release runs had non-streaming p99 74 and 127 ms), which is why the
# release job runs this with --warn-only.
BOUNDS = {
    ("non_streaming", "mean"): (1.4, 300.0),
    ("non_streaming", "p50"): (1.3, 150.0),
    ("non_streaming", "p95"): (1.3, 150.0),
    ("non_streaming", "p99"): (3.0, 30000.0),
    ("streaming", "mean"): (1.4, 300.0),
    ("streaming", "p50"): (1.3, 150.0),
    ("streaming", "p95"): (1.3, 150.0),
    ("streaming", "p99"): (1.5, 1000.0),
}


def metric(result, mode, name):
    value = (((result.get("overhead") or {}).get(mode) or {}).get("latency_us") or {}).get(name)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return float(value)


def complete(result):
    return isinstance(result, dict) and all(metric(result, m, k) is not None for m in MODES for k in METRICS)


def compare(current, baselines):
    """Returns (rows, regressions). Each row: mode, metric, current, baseline, limit, verdict."""
    rows, regressions = [], []
    for mode in MODES:
        for name in METRICS:
            cur = metric(current, mode, name)
            past = [v for v in (metric(b, mode, name) for b in baselines) if v is not None]
            if cur is None:
                rows.append((mode, name, None, None, None, "missing in this run"))
                continue
            if not past:
                rows.append((mode, name, cur, None, None, "no baseline"))
                continue
            base = statistics.median(past)
            ratio, floor = BOUNDS[(mode, name)]
            limit = max(base * ratio, base + floor)
            verdict = "REGRESSED" if cur > limit else "ok"
            rows.append((mode, name, cur, base, limit, verdict))
            if verdict == "REGRESSED":
                regressions.append((mode, name, cur, base, limit))
    return rows, regressions


def fmt(v):
    return "-" if v is None else f"{v / 1000:.2f} ms"


def render(rows, labels):
    out = [
        f"Baseline: median of {', '.join(labels) if labels else 'no recorded release'}",
        "",
        "| Mode | Metric | This run | Baseline | Limit | Verdict |",
        "|------|--------|----------|----------|-------|---------|",
    ]
    for mode, name, cur, base, limit, verdict in rows:
        out.append(f"| {mode} | {name} | {fmt(cur)} | {fmt(base)} | {fmt(limit)} | {verdict} |")
    out.append("")
    out.append("Regressed when this run > max(baseline x ratio, baseline + floor): "
               + ", ".join(f"{m} {k} x{r}/+{f / 1000:g} ms" for (m, k), (r, f) in BOUNDS.items()) + ".")
    return "\n".join(out)


def fetch(repo, exclude_tag, out_dir, count=BASELINE_RELEASES, scan=15):
    """Downloads the asset of the newest `count` published transports releases that have a
    complete one. A listing failure warns and fetches nothing: the comparison then reports
    no baseline, and the release still records this run's result."""
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        listing = subprocess.run(["gh", "release", "list", "-R", repo, "--limit", "100",
                                  "--json", "tagName,isDraft,createdAt"],
                                 check=True, capture_output=True, text=True)
        releases = json.loads(listing.stdout)
    except (subprocess.CalledProcessError, ValueError, OSError) as err:
        print(f"::warning::cannot list the releases of {repo}: {err}")
        return []
    tags = [r["tagName"] for r in sorted(releases, key=lambda r: r["createdAt"], reverse=True)
            if r["tagName"].startswith(TAG_PREFIX) and not r["isDraft"] and r["tagName"] != exclude_tag]
    got = []
    for tag in tags[:scan]:
        dest = out_dir / (tag.replace("/", "_") + ".json")
        proc = subprocess.run(["gh", "release", "download", tag, "-R", repo, "-p", ASSET,
                               "-O", str(dest), "--clobber"], capture_output=True, text=True)
        if proc.returncode != 0 or not dest.exists():
            continue
        try:
            ok = complete(json.loads(dest.read_text()))
        except ValueError:
            ok = False
        if not ok:
            print(f"skipping {tag}: its {ASSET} is incomplete")
            dest.unlink()
            continue
        got.append(tag)
        if len(got) == count:
            break
    print(f"fetched {len(got)} recorded result(s): {', '.join(got) or 'none'}")
    return got


def load(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as err:
        print(f"cannot read {path}: {err}", file=sys.stderr)
        sys.exit(2)


def main(argv):
    if argv == ["--self-test"]:
        return self_test()
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("--repo", required=True)
    f.add_argument("--exclude-tag", default="")
    f.add_argument("--out", required=True, type=Path)
    c = sub.add_parser("compare")
    c.add_argument("--current", required=True)
    c.add_argument("--baseline", action="append", default=[])
    c.add_argument("--summary")
    c.add_argument("--out")
    c.add_argument("--warn-only", action="store_true")
    r = sub.add_parser("check-result")
    r.add_argument("file")
    args = parser.parse_args(argv)

    if args.cmd == "fetch":
        fetch(args.repo, args.exclude_tag, args.out)
        return 0
    if args.cmd == "check-result":
        try:
            return 0 if complete(json.loads(Path(args.file).read_text())) else 1
        except (OSError, ValueError):
            return 1

    current = load(args.current)
    baselines = [load(p) for p in args.baseline]
    rows, regressions = compare(current, baselines)
    text = render(rows, [Path(p).stem for p in args.baseline])
    print(text)
    if args.out:
        Path(args.out).write_text(text + "\n")
    if args.summary:
        with open(args.summary, "a") as fh:
            fh.write("## Load overhead against recorded releases\n\n" + text + "\n")
    if not baselines:
        print("::warning::no recorded release result to compare against; this run's result becomes the first")
        return 0
    level = "warning" if args.warn_only else "error"
    for mode, name, cur, base, limit in regressions:
        print(f"::{level}::{mode} {name} overhead {fmt(cur)} is past its trend bound {fmt(limit)} "
              f"(baseline {fmt(base)}, median of the recorded releases)")
    return 1 if regressions and not args.warn_only else 0


# Recorded results: the release / test-load-performance artifacts of runs 43597428233
# (sync.10), 43772530151 (sync.11) and 44144252869 attempt 2 (sync.12). One sample each,
# calibration-subtracted µs.
RECORDED = {
    "sync.10": {"non_streaming": {"mean": 713.67, "p50": 425.08, "p95": 515.90, "p99": 2033.15},
                "streaming": {"mean": 608.18, "p50": 601.39, "p95": 707.60, "p99": 1984.59}},
    "sync.11": {"non_streaming": {"mean": 1214.73, "p50": 590.12, "p95": 710.55, "p99": 12178.25},
                "streaming": {"mean": 724.96, "p50": 691.20, "p95": 902.21, "p99": 2253.72}},
    "sync.12": {"non_streaming": {"mean": 2314.84, "p50": 675.95, "p95": 1067.66, "p99": 74469.34},
                "streaming": {"mean": 907.39, "p50": 788.37, "p95": 1341.10, "p99": 3261.92}},
}


def as_result(modes):
    return {"overhead": {m: {"latency_us": v} for m, v in modes.items()}}


def self_test():
    import tempfile
    import unittest

    script = str(Path(__file__).resolve())

    def run_cli(*args, stdin=None):
        return subprocess.run([sys.executable, script, *args], input=stdin, capture_output=True, text=True)

    class LoadTrendTest(unittest.TestCase):
        def test_sync12_against_sync10_and_sync11_fires_on_the_non_streaming_mean(self):
            # The ticket's numbers: every one under the absolute limits, sync.12 past the trend bound.
            _, regressions = compare(as_result(RECORDED["sync.12"]),
                                     [as_result(RECORDED["sync.10"]), as_result(RECORDED["sync.11"])])
            fired = {(m, n) for m, n, *_ in regressions}
            self.assertIn(("non_streaming", "mean"), fired)
            self.assertIn(("non_streaming", "p99"), fired)

        def test_a_release_against_itself_is_within_bounds(self):
            for name, modes in RECORDED.items():
                with self.subTest(release=name):
                    _, regressions = compare(as_result(modes), [as_result(modes)])
                    self.assertEqual(regressions, [])

        def test_the_floor_stops_sub_millisecond_ratios_firing(self):
            base = as_result({"non_streaming": {"mean": 100.0, "p50": 50.0, "p95": 100.0, "p99": 500.0}})
            cur = as_result({"non_streaming": {"mean": 390.0, "p50": 190.0, "p95": 240.0, "p99": 25000.0}})
            _, regressions = compare(cur, [base])
            self.assertEqual(regressions, [])

        def test_baseline_is_the_median_so_one_noisy_release_does_not_move_it(self):
            quiet = as_result({"non_streaming": {"mean": 700.0}})
            noisy = as_result({"non_streaming": {"mean": 5000.0}})
            rows, regressions = compare(as_result({"non_streaming": {"mean": 1100.0}}), [quiet, quiet, noisy])
            row = next(r for r in rows if r[0] == "non_streaming" and r[1] == "mean")
            self.assertEqual(row[3], 700.0)
            self.assertEqual([(m, n) for m, n, *_ in regressions], [("non_streaming", "mean")])

        def test_a_missing_metric_is_reported_not_compared(self):
            rows, regressions = compare(as_result({"streaming": RECORDED["sync.10"]["streaming"]}),
                                        [as_result(RECORDED["sync.10"])])
            self.assertEqual(regressions, [])
            self.assertIn("missing in this run", {r[5] for r in rows if r[0] == "non_streaming"})

        def test_check_result_accepts_only_a_complete_result(self):
            with tempfile.TemporaryDirectory() as d:
                full, part = Path(d) / "full.json", Path(d) / "part.json"
                full.write_text(json.dumps(as_result(RECORDED["sync.12"])))
                part.write_text(json.dumps(as_result({"streaming": RECORDED["sync.12"]["streaming"]})))
                self.assertEqual(run_cli("check-result", str(full)).returncode, 0)
                self.assertEqual(run_cli("check-result", str(part)).returncode, 1)
                self.assertEqual(run_cli("check-result", str(Path(d) / "absent.json")).returncode, 1)

        def test_no_baseline_passes_with_a_warning(self):
            proc = run_cli("compare", "--current", "/dev/stdin", stdin=json.dumps(as_result(RECORDED["sync.12"])))
            self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
            self.assertIn("::warning::no recorded release result", proc.stdout)

        def test_cli_fires_on_the_recorded_sync12_result(self):
            with tempfile.TemporaryDirectory() as d:
                paths = {}
                for name, modes in RECORDED.items():
                    paths[name] = Path(d) / f"{name}.json"
                    paths[name].write_text(json.dumps(as_result(modes)))
                args = ["compare", "--current", str(paths["sync.12"]),
                        "--baseline", str(paths["sync.10"]), "--baseline", str(paths["sync.11"])]
                gating = run_cli(*args)
                warning = run_cli(*args, "--warn-only", "--out", str(Path(d) / "trend.md"))
                table = (Path(d) / "trend.md").read_text()
            self.assertEqual(gating.returncode, 1, gating.stdout + gating.stderr)
            self.assertIn("::error::non_streaming mean overhead 2.31 ms is past its trend bound 1.35 ms",
                          gating.stdout)
            self.assertEqual(warning.returncode, 0, warning.stdout + warning.stderr)
            self.assertIn("::warning::non_streaming mean overhead 2.31 ms", warning.stdout)
            self.assertNotIn("::error::", warning.stdout)
            self.assertIn("| non_streaming | mean | 2.31 ms | 0.96 ms | 1.35 ms | REGRESSED |", table)

        def test_fetch_takes_the_newest_complete_results_but_not_this_release(self):
            import os
            with tempfile.TemporaryDirectory() as d:
                assets = Path(d) / "assets"
                assets.mkdir()
                listing = [{"tagName": f"transports/v2.4.0-sync.{n}", "isDraft": False,
                            "createdAt": f"2026-11-{n:02d}T00:00:00Z"} for n in range(7, 14)]
                listing += [{"tagName": "helm-chart-v2.1.52-sync.13", "isDraft": False,
                             "createdAt": "2026-11-30T00:00:00Z"},
                            {"tagName": "transports/v2.4.0-sync.14", "isDraft": True,
                             "createdAt": "2026-11-29T00:00:00Z"}]
                (Path(d) / "listing.json").write_text(json.dumps(listing))
                for n in (8, 9, 10, 12, 13):
                    (assets / f"transports_v2.4.0-sync.{n}").write_text(json.dumps(as_result(RECORDED["sync.10"])))
                (assets / "transports_v2.4.0-sync.11").write_text(json.dumps({"overhead": {}}))  # incomplete
                gh = Path(d) / "gh"
                gh.write_text(f"""#!/usr/bin/env python3
import shutil, sys
a = sys.argv[1:]
if a[:2] == ["release", "list"]:
    print(open({str(Path(d) / "listing.json")!r}).read()); sys.exit(0)
if a[:2] == ["release", "download"]:
    src = {str(assets)!r} + "/" + a[2].replace("/", "_")
    try:
        shutil.copy(src, a[a.index("-O") + 1])
    except FileNotFoundError:
        sys.exit(1)
    sys.exit(0)
sys.exit(2)
""")
                gh.chmod(0o755)
                env = dict(os.environ, PATH=f"{d}:{os.environ['PATH']}")
                out = Path(d) / "out"
                proc = subprocess.run([sys.executable, script, "fetch", "--repo", "o/r",
                                       "--exclude-tag", "transports/v2.4.0-sync.13", "--out", str(out)],
                                      env=env, capture_output=True, text=True)
                self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
                self.assertEqual(sorted(p.name for p in out.iterdir()),
                                 ["transports_v2.4.0-sync.10.json", "transports_v2.4.0-sync.12.json",
                                  "transports_v2.4.0-sync.9.json"])
                self.assertIn("skipping transports/v2.4.0-sync.11", proc.stdout)

                (Path(d) / "listing.json").write_text("not json")
                proc = subprocess.run([sys.executable, script, "fetch", "--repo", "o/r", "--out", str(Path(d) / "o2")],
                                      env=env, capture_output=True, text=True)
                self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
                self.assertIn("::warning::cannot list the releases", proc.stdout)

        def test_the_release_pipeline_records_and_compares(self):
            import yaml
            jobs = yaml.safe_load((Path(script).parent.parent / "release-pipeline.yml").read_text())["jobs"]
            load_job = jobs["test-load-performance"]
            run_step = next(s for s in load_job["steps"] if s.get("name") == "Run load performance test")
            self.assertGreaterEqual(int(run_step["env"]["OVERHEAD_SAMPLES"]), 3)
            trend = next(s for s in load_job["steps"] if "load-trend.py compare" in s.get("run", ""))
            self.assertIn("load-trend.py fetch", trend["run"])
            self.assertIn("--warn-only", trend["run"])
            release = jobs["bifrost-http-release"]
            self.assertIn("test-load-performance", release["needs"])
            # Here for its result only: the release's own condition never reads it.
            self.assertNotIn("test-load-performance", release["if"])
            record = next(s for s in release["steps"] if "gh release upload" in s.get("run", ""))
            self.assertIn(ASSET, record["run"])
            self.assertIn("load-trend.py check-result", record["run"])
            self.assertTrue(record.get("continue-on-error"))
            egress = release["steps"][0]["with"]["allowed-endpoints"]
            self.assertIn("uploads.github.com:443", egress)
            # No publishing job admits on the load result.
            for name, job in jobs.items():
                with self.subTest(job=name):
                    self.assertNotIn("needs.test-load-performance", job.get("if", "") or "")

    result = unittest.TextTestRunner(verbosity=2).run(
        unittest.defaultTestLoader.loadTestsFromTestCase(LoadTrendTest))
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
