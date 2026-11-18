#!/usr/bin/env python3
"""Compares a release's load-test overhead with an earlier release's, on the same runner (TXE-7762).

The load test's absolute limits (mean 5 ms, p99 100 ms) only fail once the overhead is
already bad, and the release recorded one sample with nothing comparing one release to the
next. Its non-streaming mean read 0.71, 1.21 and 2.31 ms at sync.10, sync.11 and sync.12,
and nobody could tell a creep from noise.

TXE-7762 measured it. 11 builds across sync.10..sync.12, 12 runs each, interleaved on 6
hosted runners, showed no code regression: sync.12 minus sync.10, paired on one runner, was
+36 µs (95% CI -91 to +92 µs). What moved the numbers was the runner. One runner put every
build, sync.10 included, at a 2.4-2.9 ms mean and a 73-108 ms p99; another put them all at
0.65-0.94 ms and 2-6 ms. Numbers recorded by different releases' runs therefore cannot be
compared with each other, however many samples each takes.

So the release job also builds an earlier release (the control, CONTROL_DEPTH releases back,
so a creep over several releases shows) and load-test.sh measures both on the same runner,
alternating, OVERHEAD_SAMPLES times each. This script compares the medians:

    ratio     = candidate / control, per mode and metric
    regressed = candidate > max(control * ratio bound, control + floor)

Each release's result is recorded as the release asset load-performance.json. The release
that later uses it as a control shows those recorded numbers next to the control's numbers
measured on its own runner, so the runner's effect is visible on every report.

Usage:
  load-trend.py pick-control --repo OWNER/NAME --exclude-tag TAG [--depth N] [--recorded-out FILE]
  load-trend.py compare --current FILE [--control-recorded FILE] [--summary FILE] [--out FILE] [--warn-only]
  load-trend.py check-result FILE      # exit 0 when FILE has every candidate mode and metric
  load-trend.py --self-test
Exit codes: 0 within bounds (or no control, or --warn-only), 1 regressed, 2 usage/input.
"""
import argparse
import json
import subprocess
import sys
from pathlib import Path

ASSET = "load-performance.json"
TAG_PREFIX = "transports/v"
CONTROL_DEPTH = 3
MODES = ("non_streaming", "streaming")
METRICS = ("mean", "p50", "p95", "p99")
SUCCESS_DROP = 1.0
# (ratio, floor in µs) per mode and metric, for candidate / control on one runner. Set from
# TXE-7762's second run: 4 binaries with no code difference the runs could separate (sync.10,
# sync.11 and sync.12 builds, and sync.12's release binary), each measured as a median of 3
# on 6 runners x 2 rounds, giving 144 same-runner pairs. The largest pair seen, as ratio and
# as difference:
#   non-streaming mean 1.45 / +408 µs   p50 1.38 / +132 µs   p95 1.18 / +86 µs   p99 5.04 / +26 ms
#   streaming     mean 1.20 / +88 µs    p50 1.27 / +105 µs   p95 1.20 / +258 µs  p99 2.10 / +1.3 ms
# Each bound sits above both, so none of those 144 pairs fires. The non-streaming p99 is the
# first phase after a cold start and is mostly runner noise; its bound only catches a gross
# tail regression, and the absolute 100 ms limit in load-test.sh still applies to it.
BOUNDS = {
    ("non_streaming", "mean"): (1.5, 450.0),
    ("non_streaming", "p50"): (1.4, 150.0),
    ("non_streaming", "p95"): (1.25, 100.0),
    ("non_streaming", "p99"): (6.0, 30000.0),
    ("streaming", "mean"): (1.25, 100.0),
    ("streaming", "p50"): (1.3, 120.0),
    ("streaming", "p95"): (1.3, 300.0),
    ("streaming", "p99"): (2.5, 1500.0),
}


def metric(result, root, mode, name):
    value = (((result.get(root) or {}).get(mode) or {}).get("latency_us") or {}).get(name)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return float(value)


def complete(result, root="overhead"):
    return isinstance(result, dict) and all(metric(result, root, m, k) is not None for m in MODES for k in METRICS)


def success(result, root, mode):
    value = ((result.get(root) or {}).get(mode) or {}).get("success_rate")
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return float(value)


def compare(current):
    """Returns (rows, regressions). Each row: mode, metric, candidate, control, limit, verdict.
    A success rate (worst sample, %) more than SUCCESS_DROP points under the control's is a
    regression too: an overloaded gateway times requests out, and the overhead phase
    asserts no success rate of its own."""
    rows, regressions = [], []
    for mode in MODES:
        cand_ok, ctrl_ok = success(current, "overhead", mode), success(current, "control_overhead", mode)
        if cand_ok is not None and ctrl_ok is not None and cand_ok < ctrl_ok - SUCCESS_DROP:
            regressions.append((mode, "success_rate", cand_ok, ctrl_ok, ctrl_ok - SUCCESS_DROP))
            rows.append((mode, "success_rate", cand_ok, ctrl_ok, ctrl_ok - SUCCESS_DROP, "REGRESSED"))
        for name in METRICS:
            cand = metric(current, "overhead", mode, name)
            ctrl = metric(current, "control_overhead", mode, name)
            if cand is None or ctrl is None:
                rows.append((mode, name, cand, ctrl, None, "not measured"))
                continue
            ratio, floor = BOUNDS[(mode, name)]
            limit = max(ctrl * ratio, ctrl + floor)
            verdict = "REGRESSED" if cand > limit else "ok"
            rows.append((mode, name, cand, ctrl, limit, verdict))
            if verdict == "REGRESSED":
                regressions.append((mode, name, cand, ctrl, limit))
    return rows, regressions


def fmt(v, name="mean"):
    if v is None:
        return "-"
    return f"{v:.2f}%" if name == "success_rate" else f"{v / 1000:.2f} ms"


def render(current, rows, recorded):
    label = current.get("control_label") or "the control"
    out = [
        f"Candidate against {label}, both measured on this runner (median of each build's samples).",
        "",
        "| Mode | Metric | Candidate | Control | Ratio | Limit | Verdict | Control as recorded on its release |",
        "|------|--------|-----------|---------|-------|-------|---------|------------------------------------|",
    ]
    for mode, name, cand, ctrl, limit, verdict in rows:
        ratio = f"{cand / ctrl:.2f}" if cand is not None and ctrl and name != "success_rate" else "-"
        if name == "success_rate":
            rec = success(recorded, "overhead", mode) if recorded else None
        else:
            rec = metric(recorded, "overhead", mode, name) if recorded else None
        out.append(f"| {mode} | {name} | {fmt(cand, name)} | {fmt(ctrl, name)} | {ratio} | {fmt(limit, name)}"
                   f" | {verdict} | {fmt(rec, name)} |")
    out.append("")
    out.append("Regressed when candidate > max(control x ratio, control + floor): "
               + ", ".join(f"{m} {k} x{r}/+{f / 1000:g} ms" for (m, k), (r, f) in BOUNDS.items()) + ".")
    if recorded:
        out.append("The last column is the control release's own run, on a different runner; "
                   "its gap to the Control column is the runner, not the code.")
    return "\n".join(out)


def pick_control(repo, exclude_tag, depth, recorded_out):
    """Prints the tag of the release `depth` back (or the oldest there is), and downloads its
    recorded result to recorded_out when it has one."""
    try:
        listing = subprocess.run(["gh", "release", "list", "-R", repo, "--limit", "100",
                                  "--json", "tagName,isDraft,createdAt"],
                                 check=True, capture_output=True, text=True)
        releases = json.loads(listing.stdout)
    except (subprocess.CalledProcessError, ValueError, OSError) as err:
        print(f"::warning::cannot list the releases of {repo}: {err}", file=sys.stderr)
        return 1
    tags = [r["tagName"] for r in sorted(releases, key=lambda r: r["createdAt"], reverse=True)
            if r["tagName"].startswith(TAG_PREFIX) and not r["isDraft"] and r["tagName"] != exclude_tag]
    if not tags:
        print(f"::warning::{repo} has no earlier {TAG_PREFIX}* release to use as the control", file=sys.stderr)
        return 1
    tag = tags[min(depth, len(tags)) - 1]
    if recorded_out:
        proc = subprocess.run(["gh", "release", "download", tag, "-R", repo, "-p", ASSET,
                               "-O", recorded_out, "--clobber"], capture_output=True, text=True)
        if proc.returncode != 0:
            print(f"{tag} has no recorded {ASSET}", file=sys.stderr)
            Path(recorded_out).unlink(missing_ok=True)
    print(tag)
    return 0


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
    p = sub.add_parser("pick-control")
    p.add_argument("--repo", required=True)
    p.add_argument("--exclude-tag", default="")
    p.add_argument("--depth", type=int, default=CONTROL_DEPTH)
    p.add_argument("--recorded-out")
    c = sub.add_parser("compare")
    c.add_argument("--current", required=True)
    c.add_argument("--control-recorded")
    c.add_argument("--summary")
    c.add_argument("--out")
    c.add_argument("--warn-only", action="store_true")
    r = sub.add_parser("check-result")
    r.add_argument("file")
    args = parser.parse_args(argv)

    if args.cmd == "pick-control":
        if args.depth < 1:
            parser.error("--depth must be at least 1")
        return pick_control(args.repo, args.exclude_tag, args.depth, args.recorded_out)
    if args.cmd == "check-result":
        try:
            return 0 if complete(json.loads(Path(args.file).read_text())) else 1
        except (OSError, ValueError):
            return 1

    current = load(args.current)
    recorded = None
    if args.control_recorded and Path(args.control_recorded).is_file():
        recorded = load(args.control_recorded)
    rows, regressions = compare(current)
    text = render(current, rows, recorded)
    print(text)
    if args.out:
        Path(args.out).write_text(text + "\n")
    if args.summary:
        with open(args.summary, "a") as fh:
            fh.write("## Load overhead against an earlier release\n\n" + text + "\n")
    if not complete(current, "control_overhead"):
        print("::warning::no control build was measured, so nothing was compared")
        return 0
    level = "warning" if args.warn_only else "error"
    for mode, name, cand, ctrl, limit in regressions:
        print(f"::{level}::{mode} {name} {fmt(cand, name)} is past its bound {fmt(limit, name)} "
              f"against {current.get('control_label') or 'the control'} at {fmt(ctrl, name)} on the same runner")
    return 1 if regressions and not args.warn_only else 0


# Recorded results: load-test.sh with a control, OVERHEAD_SAMPLES=3, on one ubuntu-24.04 runner
# (TXE-7762 scratch run 44202780997, shard 0; medians in µs, success rates in %).
#   release: the release path exactly. pick-control for a release after sync.12 chose sync.10,
#     load-control.sh built it, and the candidate was sync.12's own release binary.
#   unoptimised: sync.12 with bifrost's packages built -gcflags=-N, against a normal build. A
#     real slowdown of 21-23% in streaming on all 4 runners, which is below every bound: the
#     bounds sit above the measured same-runner noise (streaming mean up to 1.20x), so they
#     catch roughly 30% and more in streaming and 50% in the non-streaming mean.
#   race: sync.12 built -race, against a normal build. It overloads: requests time out at 15 s
#     and about half fail. The absolute limits in load-test.sh fail it too.
RECORDED = {
    "release": {
        "label": "transports/v2.4.0-sync.10",
        "candidate": {"non_streaming": ({"mean": 1127.19, "p50": 596.76, "p95": 668.94, "p99": 9426.88}, 100.0),
                      "streaming": ({"mean": 546.03, "p50": 543.17, "p95": 601.83, "p99": 1770.91}, 100.0)},
        "control": {"non_streaming": ({"mean": 1083.79, "p50": 577.34, "p95": 619.80, "p99": 8300.87}, 100.0),
                    "streaming": ({"mean": 550.61, "p50": 550.09, "p95": 589.08, "p99": 1715.46}, 100.0)},
    },
    "unoptimised": {
        "label": "transports/v2.4.0-sync.12 (optimised build)",
        "candidate": {"non_streaming": ({"mean": 1096.25, "p50": 687.38, "p95": 788.40, "p99": 4943.14}, 100.0),
                      "streaming": ({"mean": 674.14, "p50": 683.33, "p95": 766.84, "p99": 1772.85}, 100.0)},
        "control": {"non_streaming": ({"mean": 1022.87, "p50": 519.74, "p95": 666.24, "p99": 5514.13}, 100.0),
                    "streaming": ({"mean": 558.99, "p50": 556.96, "p95": 595.98, "p99": 1617.10}, 100.0)},
    },
    "race": {
        "label": "transports/v2.4.0-sync.12 (build without -race)",
        "candidate": {"non_streaming": ({"mean": 7180431.33, "p50": 6724236.05, "p95": 12432791.16,
                                         "p99": 14499616.03}, 46.77),
                      "streaming": ({"mean": 10783167.81, "p50": 14499733.03, "p95": 14500034.84,
                                     "p99": 14500641.07}, 45.97)},
        "control": {"non_streaming": ({"mean": 1140.23, "p50": 570.78, "p95": 689.36, "p99": 9518.86}, 100.0),
                    "streaming": ({"mean": 555.21, "p50": 530.30, "p95": 622.21, "p99": 1643.33}, 100.0)},
    },
}


def recorded_result(name):
    rec = RECORDED[name]

    def side(modes):
        return {m: {"latency_us": lat, "success_rate": ok} for m, (lat, ok) in modes.items()}

    return {"overhead": side(rec["candidate"]), "control_overhead": side(rec["control"]),
            "control_label": rec["label"]}


def self_test():
    import os
    import tempfile
    import unittest

    script = str(Path(__file__).resolve())

    def run_cli(*args, stdin=None, env=None):
        return subprocess.run([sys.executable, script, *args], input=stdin, capture_output=True,
                              text=True, env=env)

    def result(cand, ctrl, label="transports/v2.4.0-sync.10"):
        out: dict = {"overhead": {m: {"latency_us": v} for m, v in cand.items()}}
        if ctrl is not None:
            out["control_overhead"] = {m: {"latency_us": v} for m, v in ctrl.items()}
            out["control_label"] = label
        return out

    flat = {"non_streaming": {"mean": 1000.0, "p50": 550.0, "p95": 650.0, "p99": 8000.0},
            "streaming": {"mean": 560.0, "p50": 560.0, "p95": 610.0, "p99": 1700.0}}

    class LoadTrendTest(unittest.TestCase):
        def test_the_recorded_race_build_fires_on_every_metric_and_on_success(self):
            _, regressions = compare(recorded_result("race"))
            fired = {(m, n) for m, n, *_ in regressions}
            for mode in MODES:
                for name in (*METRICS, "success_rate"):
                    self.assertIn((mode, name), fired)

        def test_the_recorded_release_path_is_within_bounds(self):
            _, regressions = compare(recorded_result("release"))
            self.assertEqual(regressions, [])

        def test_the_recorded_unoptimised_build_is_below_the_bounds(self):
            # The check's sensitivity, stated: a real ~22% slowdown does not fire.
            rows, regressions = compare(recorded_result("unoptimised"))
            self.assertEqual(regressions, [])
            ratio = {(r[0], r[1]): r[2] / r[3] for r in rows}
            self.assertGreater(ratio[("streaming", "mean")], 1.2)

        def test_cli_fires_on_the_recorded_race_result(self):
            with tempfile.TemporaryDirectory() as d:
                cur = Path(d) / "race.json"
                cur.write_text(json.dumps(recorded_result("race")))
                gating = run_cli("compare", "--current", str(cur))
                warning = run_cli("compare", "--current", str(cur), "--warn-only", "--out", str(Path(d) / "t.md"))
                table = (Path(d) / "t.md").read_text()
            self.assertEqual(gating.returncode, 1, gating.stdout + gating.stderr)
            self.assertIn("::error::streaming success_rate 45.97% is past its bound 99.00%", gating.stdout)
            self.assertEqual(warning.returncode, 0, warning.stdout + warning.stderr)
            self.assertIn("::warning::non_streaming mean 7180.43 ms is past its bound 1.71 ms", warning.stdout)
            self.assertNotIn("::error::", warning.stdout)
            self.assertIn("| streaming | p50 | 14499.73 ms | 0.53 ms | 27342.51 | 0.69 ms | REGRESSED | - |", table)

        def test_the_same_build_on_both_sides_is_within_bounds(self):
            _, regressions = compare(result(flat, flat))
            self.assertEqual(regressions, [])

        def test_the_largest_same_runner_noise_measured_does_not_fire(self):
            # Each metric at its largest measured ratio and difference at once.
            noisy = {"non_streaming": {"mean": 1408.0, "p50": 682.0, "p95": 736.0, "p99": 34000.0},
                     "streaming": {"mean": 648.0, "p50": 665.0, "p95": 732.0, "p99": 3000.0}}
            _, regressions = compare(result(noisy, flat))
            self.assertEqual(regressions, [])

        def test_both_the_ratio_and_the_floor_must_be_passed(self):
            tiny = {"non_streaming": {"mean": 100.0, "p50": 50.0, "p95": 60.0, "p99": 500.0},
                    "streaming": {"mean": 100.0, "p50": 50.0, "p95": 60.0, "p99": 500.0}}
            double = {m: {k: v * 2 for k, v in d.items()} for m, d in tiny.items()}
            _, regressions = compare(result(double, tiny))
            self.assertEqual(regressions, [])  # x2 on sub-floor numbers: under every floor

        def test_a_body_regression_fires_on_p50_and_p95(self):
            slower = {m: dict(d) for m, d in flat.items()}
            slower["non_streaming"]["p50"] = 550.0 * 1.6
            slower["non_streaming"]["p95"] = 650.0 * 1.6
            _, regressions = compare(result(slower, flat))
            self.assertEqual({(m, n) for m, n, *_ in regressions},
                             {("non_streaming", "p50"), ("non_streaming", "p95")})

        def test_no_control_is_reported_and_passes(self):
            rows, regressions = compare(result(flat, None))
            self.assertEqual(regressions, [])
            self.assertEqual({r[5] for r in rows}, {"not measured"})
            proc = run_cli("compare", "--current", "/dev/stdin", stdin=json.dumps(result(flat, None)))
            self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
            self.assertIn("::warning::no control build was measured", proc.stdout)

        def test_check_result_accepts_only_a_complete_candidate(self):
            with tempfile.TemporaryDirectory() as d:
                full, part = Path(d) / "full.json", Path(d) / "part.json"
                full.write_text(json.dumps(result(flat, None)))
                part.write_text(json.dumps(result({"streaming": flat["streaming"]}, None)))
                self.assertEqual(run_cli("check-result", str(full)).returncode, 0)
                self.assertEqual(run_cli("check-result", str(part)).returncode, 1)
                self.assertEqual(run_cli("check-result", str(Path(d) / "absent.json")).returncode, 1)

        def test_pick_control_takes_the_release_depth_back_and_its_recording(self):
            with tempfile.TemporaryDirectory() as d:
                listing = [{"tagName": f"transports/v2.4.0-sync.{n}", "isDraft": False,
                            "createdAt": f"2026-11-{n:02d}T00:00:00Z"} for n in range(8, 14)]
                listing += [{"tagName": "helm-chart-v2.1.52-sync.13", "isDraft": False,
                             "createdAt": "2026-11-30T00:00:00Z"},
                            {"tagName": "transports/v2.4.0-sync.14", "isDraft": True,
                             "createdAt": "2026-11-29T00:00:00Z"}]
                (Path(d) / "listing.json").write_text(json.dumps(listing))
                (Path(d) / "transports_v2.4.0-sync.10").write_text(json.dumps(result(flat, None)))
                gh = Path(d) / "gh"
                gh.write_text(f"""#!/usr/bin/env python3
import shutil, sys
a = sys.argv[1:]
if a[:2] == ["release", "list"]:
    print(open({str(Path(d) / "listing.json")!r}).read()); sys.exit(0)
if a[:2] == ["release", "download"]:
    try:
        shutil.copy({str(d)!r} + "/" + a[2].replace("/", "_"), a[a.index("-O") + 1])
    except FileNotFoundError:
        sys.exit(1)
    sys.exit(0)
sys.exit(2)
""")
                gh.chmod(0o755)
                env = dict(os.environ, PATH=f"{d}:{os.environ['PATH']}")
                rec = Path(d) / "recorded.json"
                # sync.13 is the release being made: the control is three before it.
                proc = run_cli("pick-control", "--repo", "o/r", "--exclude-tag", "transports/v2.4.0-sync.13",
                               "--recorded-out", str(rec), env=env)
                self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
                self.assertEqual(proc.stdout.strip(), "transports/v2.4.0-sync.10")
                self.assertTrue(complete(json.loads(rec.read_text())))
                # One back has no recording: the tag is still the control, with nothing recorded.
                proc = run_cli("pick-control", "--repo", "o/r", "--exclude-tag", "transports/v2.4.0-sync.13",
                               "--depth", "1", "--recorded-out", str(rec), env=env)
                self.assertEqual(proc.stdout.strip(), "transports/v2.4.0-sync.12")
                self.assertFalse(rec.exists())
                # Fewer releases than the depth: the oldest one.
                proc = run_cli("pick-control", "--repo", "o/r", "--depth", "99", env=env)
                self.assertEqual(proc.stdout.strip(), "transports/v2.4.0-sync.8")
                (Path(d) / "listing.json").write_text("not json")
                proc = run_cli("pick-control", "--repo", "o/r", env=env)
                self.assertEqual(proc.returncode, 1)
                self.assertIn("::warning::cannot list the releases", proc.stderr)

        def test_the_release_pipeline_records_and_compares_without_gating(self):
            import yaml
            jobs = yaml.safe_load((Path(script).parent.parent / "release-pipeline.yml").read_text())["jobs"]
            load_job = jobs["test-load-performance"]
            steps = load_job["steps"]
            run_step = next(s for s in steps if s.get("name") == "Run load performance test")
            self.assertGreaterEqual(int(run_step["env"]["OVERHEAD_SAMPLES"]), 3)
            self.assertIn("CONTROL_BIFROST_BIN", run_step["env"])
            control = next(s for s in steps if "load-control.sh" in s.get("run", ""))
            self.assertIn("load-trend.py pick-control", control["run"])
            trend = next(s for s in steps if "load-trend.py compare" in s.get("run", ""))
            self.assertIn("--warn-only", trend["run"])
            self.assertTrue(load_job.get("continue-on-error"))
            release = jobs["bifrost-http-release"]
            self.assertIn("test-load-performance", release["needs"])
            self.assertNotIn("test-load-performance", release["if"])
            record = next(s for s in release["steps"] if "gh release upload" in s.get("run", ""))
            self.assertIn(ASSET, record["run"])
            self.assertIn("load-trend.py check-result", record["run"])
            self.assertTrue(record.get("continue-on-error"))
            self.assertIn("uploads.github.com:443", release["steps"][0]["with"]["allowed-endpoints"])
            for name, job in jobs.items():
                with self.subTest(job=name):
                    self.assertNotIn("needs.test-load-performance", job.get("if", "") or "")

    result_ = unittest.TextTestRunner(verbosity=2).run(
        unittest.defaultTestLoader.loadTestsFromTestCase(LoadTrendTest))
    return 0 if result_.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
