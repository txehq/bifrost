#!/usr/bin/env bash
# Sourced, not run: serves a gateway's two pricing datasheets on loopback for an offline run
# (TXE-7424).
#
# A fresh gateway downloads its pricing and model-parameter datasheets from getbifrost.ai at
# start, and exits when it cannot. Under scripts/test-offline.sh (BIFROST_TEST_OFFLINE=1) every
# dial off loopback is refused, so a script that starts a gateway there points
# framework.pricing at these instead. Both are empty unless the caller passes the two files to
# serve: a suite that checks costs installs its own pricing overrides, and a suite that needs
# catalog prices passes a checked-in snapshot (test-api-integrations.sh, TXE-7464).
#
#   source .github/workflows/scripts/offline-datasheets.sh
#   serve_offline_datasheets "$LOG_DIR" [pricing.json model-parameters.json]
#   ...                                    # sets DATASHEET_ORIGIN and DATASHEET_PID
#   ...                                    # pricing_url: $DATASHEET_ORIGIN/pricing.json
#   kill "$DATASHEET_PID"                  #   model_parameters_url: $DATASHEET_ORIGIN/model-parameters.json

serve_offline_datasheets() {
  local log_dir="$1" pricing="${2:-}" params="${3:-}" sheets
  sheets="$(mktemp -d)"
  if [ -n "$pricing" ] || [ -n "$params" ]; then
    cp "${pricing:?serve_offline_datasheets: pass both datasheets}" "$sheets/pricing.json" || return 1
    cp "${params:?serve_offline_datasheets: pass both datasheets}" "$sheets/model-parameters.json" || return 1
  else
    echo '{}' > "$sheets/pricing.json"
    echo '{}' > "$sheets/model-parameters.json"
  fi
  # Port 0: the kernel picks a free port, so parallel runs never collide.
  python3 -c '
import functools, http.server, os, sys
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
with open(sys.argv[2] + ".tmp", "w") as f:
    f.write(str(server.server_address[1]))
os.rename(sys.argv[2] + ".tmp", sys.argv[2])
server.serve_forever()
' "$sheets" "$sheets/port" > "$log_dir/offline-datasheets.log" 2>&1 &
  DATASHEET_PID=$!
  for _ in $(seq 1 50); do
    [ -s "$sheets/port" ] && break
    sleep 0.1
  done
  if [ ! -s "$sheets/port" ]; then
    echo "❌ the loopback datasheet server did not start" >&2
    cat "$log_dir/offline-datasheets.log" >&2 || true
    return 1
  fi
  DATASHEET_ORIGIN="http://127.0.0.1:$(cat "$sheets/port")"
  echo "📄 Offline run: pricing datasheets from $DATASHEET_ORIGIN"
}
