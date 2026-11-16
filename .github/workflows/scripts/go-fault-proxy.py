#!/usr/bin/env python3
"""A Go module proxy that serves one synthetic module and fails each path's first requests.

go-proxy-retry.test.sh points the go command at it to prove that the GOPROXY list from
.github/actions/setup-go/goproxy-list.sh rides out a transient proxy failure (TXE-7621). It never
dials out: the module, example.test/retrydep v1.0.0, is built in memory.

A failed request is the CI failure on 16 Nov 2026 in miniature: a .zip gets its headers and half
its body, then the connection closes (proxy.golang.org's HTTP/2 stream INTERNAL_ERROR arrived
mid-body); every other path answers 500.

Usage: go-fault-proxy.py --port-file FILE [--fail-times N]
  Binds 127.0.0.1 on a free port, writes the port to FILE, and serves until killed. The first N
  requests for each path fail (default 1). Each request is logged to stderr, a failed one as
  "INJECTED <path>".
"""
import argparse
import http.server
import io
import json
import sys
import threading
import zipfile

MODULE = "example.test/retrydep"
VERSION = "v1.0.0"
GO_MOD = f"module {MODULE}\n\ngo 1.21\n"
SOURCE = "package retrydep\n\n// Answer is what the consumer module prints.\nconst Answer = 42\n"


def module_zip():
    buf = io.BytesIO()
    prefix = f"{MODULE}@{VERSION}/"
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr(prefix + "go.mod", GO_MOD)
        zf.writestr(prefix + "retrydep.go", SOURCE)
    return buf.getvalue()


def routes():
    base = f"/{MODULE}/@v/"
    return {
        base + "list": (b"v1.0.0\n", "text/plain"),
        base + VERSION + ".info": (json.dumps({"Version": VERSION, "Time": "2026-11-16T00:00:00Z"}).encode(), "application/json"),
        base + VERSION + ".mod": (GO_MOD.encode(), "text/plain"),
        base + VERSION + ".zip": (module_zip(), "application/zip"),
    }


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--port-file", required=True)
    p.add_argument("--fail-times", type=int, default=1)
    args = p.parse_args()
    served = routes()
    counts = {}
    lock = threading.Lock()

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *a):
            sys.stderr.write("go-fault-proxy: " + (fmt % a) + "\n")

        def do_GET(self):
            path = self.path.split("?", 1)[0]
            with lock:
                counts[path] = counts.get(path, 0) + 1
                attempt = counts[path]
            if path not in served:
                self.send_response(404)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            body, ctype = served[path]
            if attempt <= args.fail_times:
                sys.stderr.write(f"go-fault-proxy: INJECTED {path} (attempt {attempt})\n")
                if path.endswith(".zip"):
                    self.send_response(200)
                    self.send_header("Content-Type", ctype)
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body[: len(body) // 2])
                    self.wfile.flush()
                    self.close_connection = True
                    return
                self.send_response(500)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    with open(args.port_file, "w") as fh:
        fh.write(str(srv.server_address[1]))
    srv.serve_forever()


if __name__ == "__main__":
    main()
