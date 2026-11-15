# Patched copy of github.com/bytedance/sonic/loader

Upstream version: v0.5.2 (the version every module in this repository requires).

This is the module zip from the Go module proxy. Every file is unchanged except the three below.
Every go.mod in this repository that requires the loader replaces it with this directory, and
`go.work` uses it.

## The race (TXE-7504)

sonic's JIT registers each compiled function with the Go runtime as a module appended to the
runtime's module list (`runtime.firstmoduledata` … `runtime.lastmoduledatap`). Upstream's
lock-free `registerModuleLockFree` does this in two steps:

1. CAS `lastmoduledatap` from the old tail to the new module, then
2. store the new module in `oldTail.next`.

A goroutine preempted between steps 1 and 2 leaves its module unreachable from the head of
the list. A second registration in that gap appends to the orphan, returns, and runs JIT code
that `runtime.findfunc` cannot find. This can happen because the encoder and decoder program
caches lock separately. The next stack growth through such a frame aborts the process with
`fatal error: unknown caller pc`, which `recover` cannot catch. CI run 43732264194 hit it
(Go 1.27.3, amd64).

A 2 ms sleep injected between steps 1 and 2 crashed a linux/amd64 stress program 2/2 with that
signature. The same sleep inside the patched critical section survived 2/2. The race is not
specific to a Go version, and every upstream release up to sonic v1.15.5 / loader v0.5.3 has it.

## The patch

- `register.go`: registration takes a mutex, links the new module into `oldTail.next`, then
  publishes it as the tail. Upstream's `bytedance_tango` variant (`register_tango.go`) already
  uses this order. A nil-by-default `registerGapHook` lets the tests hold a registration
  between the two steps.
- `register_test.go`: two regression tests. One holds a registration in the gap and asserts
  that another cannot return with an unreachable module; it fails on the upstream ordering.
  The other checks reachability and order under 16-way concurrency. Both files are gofmt-ed.
- `README.md`: this file.

## When to remove it

Remove this directory once an upstream loader release fixes the race, which means its
`loader/register.go` links before publishing or serializes registration. The upstream report
that tracks this is https://github.com/bytedance/sonic/issues/1014. Then delete this
directory, the `replace github.com/bytedance/sonic/loader => …` line in every go.mod, the
`./third_party/sonic-loader` line in `go.work` and `setup-go-workspace.sh`, the `COPY third_party`
lines in `transports/Dockerfile*`, the `MODULES` entry in `go-unit-shards.py`, and the sonic
loader guards in `core/providers/utils/fasthttpreplace_test.go`.

If upstream has not fixed it and a sonic upgrade moves the required loader version, those
guards fail. In that case, copy the new loader version here, re-apply the `register.go` patch,
and update the version above.
