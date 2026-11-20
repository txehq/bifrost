#!/usr/bin/env bash
# Fixture tests for check-go-setup.py (TXE-7621). Each case writes a scratch repository with one
# workflow (and the scripts it calls) and asserts the check passes it or names the rule it breaks.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-go-setup.py"
WORK="$(mktemp -d)"
trap 'rm -r "$WORK"' EXIT

fails=0
n=0

# case <name> <want: pass|rule N> ; reads the workflow from stdin. Files the workflow calls are
# written before the call with put.
put() { mkdir -p "$(dirname "$WORK/$1/$2")"; cat >"$WORK/$1/$2"; }
case_() {
  local name="$1" want="$2" root="$WORK/$1"
  mkdir -p "$root/.github/workflows"
  cat >"$root/.github/workflows/ci.yml"
  n=$((n + 1))
  local out rc=0
  out="$(python3 "$CHECK" --root "$root" 2>&1)" || rc=$?
  if [[ "$want" == pass ]]; then
    if (( rc == 0 )); then echo "ok: $name passes"; else echo "FAIL: $name should pass: $out"; fails=$((fails + 1)); fi
  else
    if (( rc != 0 )) && grep -q "($want)" <<<"$out"; then
      echo "ok: $name fails on $want"
    else
      echo "FAIL: $name should fail on $want (rc $rc): $out"; fails=$((fails + 1))
    fi
  fi
}

case_ good pass <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - uses: ./.github/actions/setup-go
      - run: go test ./...
      - run: echo done
        env:
          GOPROXY: "off"
EOF

case_ direct-setup-go 'rule 1' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
        with: {go-version: "1.27.3", cache: false}
      - run: go build ./...
EOF

case_ go-without-wrapper 'rule 2' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: |
          cd core && GOWORK=off go vet ./...
EOF

put make-without-wrapper Makefile <<'EOF'
all:
	go build ./...
EOF
case_ make-without-wrapper 'rule 2' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: make all
EOF

put called-script .github/workflows/scripts/outer.sh <<'EOF'
#!/usr/bin/env bash
bash "$SCRIPT_DIR/inner.sh"
EOF
put called-script .github/workflows/scripts/inner.sh <<'EOF'
#!/usr/bin/env bash
if ! go test ./...; then exit 1; fi
EOF
case_ called-script 'rule 2' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: ./.github/workflows/scripts/outer.sh
EOF

put wrapped-python .github/workflows/scripts/shards.py <<'EOF'
import subprocess
subprocess.run(["go", "test", "./..."], check=True)
EOF
case_ wrapped-python 'rule 2' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: scripts/test-offline.sh python3 .github/workflows/scripts/shards.py run
EOF

put node-script tests/run.mjs <<'EOF'
run('go', ['build', '-o', 'bin', '.'])
EOF
case_ node-script 'rule 2' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: CI=true node tests/run.mjs
EOF

# Not running go: a script that greps another, one that writes a stub go, one that writes a
# fixture holding a go command into a heredoc, a Python script that names scripts as data.
put data-only .github/workflows/scripts/build.sh <<'EOF'
#!/usr/bin/env bash
go build ./...
EOF
put data-only .github/workflows/scripts/greps.test.sh <<'EOF'
#!/usr/bin/env bash
grep -q 'go build' "$ROOT/.github/workflows/scripts/build.sh"
EOF
put data-only .github/workflows/scripts/stub.test.sh <<'EOF'
#!/usr/bin/env bash
cat >"$ROOT/bin/go" <<'STUB'
#!/usr/bin/env bash
echo fake
STUB
bash "$SCRIPT_DIR/build.sh"
EOF
put data-only .github/workflows/scripts/fixture.test.sh <<'EOF'
#!/usr/bin/env bash
cat >"$WORK/ci.yml" <<'YAML'
      - run: ./.github/workflows/scripts/build.sh
YAML
EOF
put data-only .github/workflows/scripts/names.py <<'EOF'
SCRIPTS = ["build.sh", "make run"]
EOF
case_ data-only pass <<'EOF'
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: ./.github/workflows/scripts/greps.test.sh
      - run: ./.github/workflows/scripts/stub.test.sh
      - run: ./.github/workflows/scripts/fixture.test.sh
      - run: python3 .github/workflows/scripts/names.py
      - run: echo "run go test ./... locally"
EOF

case_ wrapper-before-checkout 'rule 3' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/setup-go
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - run: go test ./...
EOF

case_ goproxy-job-env 'rule 4' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    env:
      GOPROXY: https://proxy.golang.org,direct
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - uses: ./.github/actions/setup-go
      - run: go test ./...
EOF

case_ goproxy-assigned 'rule 4' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
      - uses: ./.github/actions/setup-go
      - run: |
          export GOPROXY=https://goproxy.example
          go test ./...
EOF

put composite-action .github/actions/other/action.yml <<'EOF'
name: other
runs:
  using: composite
  steps:
    - uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0
EOF
case_ composite-action 'rule 1' <<'EOF'
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
EOF

# Rule 5 (TXE-8021): the wrapper's GOPROXY write is GitHub-hosted only, and it is there.
wrapper() {
  local name="$1" cond="$2"
  {
    printf 'name: Set up Go\nruns:\n  using: composite\n  steps:\n'
    printf '    - uses: actions/setup-go@4b73464bb391d4059bd26b0524d20df3927bd417 # v6.3.0\n'
    if [[ "$cond" != absent ]]; then
      printf '    - name: Make module fetches retry\n'
      [[ -n "$cond" ]] && printf '      if: %s\n' "$cond"
      printf '      shell: bash\n      run: |\n'
      printf '        list="$("$GITHUB_ACTION_PATH/goproxy-list.sh")"\n'
      printf '        echo "GOPROXY=$list" >> "$GITHUB_ENV"\n'
    fi
  } | put "$name" .github/actions/setup-go/action.yml
}
checkout_only() {
  printf 'jobs:\n  build:\n    runs-on: ubuntu-24.04\n    steps:\n'
  printf '      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2\n'
}
wrapper wrapper-hosted-only "runner.environment == 'github-hosted'"
checkout_only | case_ wrapper-hosted-only pass
wrapper wrapper-unguarded ""
checkout_only | case_ wrapper-unguarded 'rule 5'
wrapper wrapper-other-condition "runner.environment != 'self-hosted' || github.event_name == 'push'"
checkout_only | case_ wrapper-other-condition 'rule 5'
wrapper wrapper-without-list absent
checkout_only | case_ wrapper-without-list 'rule 5'

# The repository itself.
n=$((n + 1))
if out="$(python3 "$CHECK" 2>&1)"; then
  echo "ok: the repository passes ($out)"
else
  echo "FAIL: the repository fails check-go-setup.py: $out"; fails=$((fails + 1))
fi

if (( fails > 0 )); then
  echo "check-go-setup_test.sh: $fails of $n cases failed"
  exit 1
fi
echo "check-go-setup_test.sh: all $n cases pass"
