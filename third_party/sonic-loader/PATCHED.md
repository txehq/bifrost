# Patched copy of github.com/bytedance/sonic/loader

Upstream version: v0.5.2 (the version every module in this repository requires).
Source: the module zip from the Go module proxy, unchanged except for the files listed below.

## Patch (TXE-7504)

- `register.go`: module registration is serialized under a mutex and links the new module into
  the runtime's module list before publishing it as the tail. Upstream's lock-free version could
  publish a JIT module that `runtime.findfunc` could not reach. A stack growth through such a frame
  aborted the process with `fatal error: unknown caller pc` (seen in CI run 43732264194). The
  race is in every upstream release up to and including loader v0.5.3 / sonic v1.15.5.
- `register_test.go`: regression tests for that ordering.
- `PATCHED.md`: this file.

## Upgrading sonic

`core/providers/utils/sonicloader_test.go` fails when a go.mod requires a loader version other
than the one above, or drops the replace to this directory. To move to a newer loader: if the new
upstream version fixed registration (check `loader/register.go`), delete this directory and the
replaces and the guard. Otherwise copy the new version here, re-apply the `register.go` patch,
and update the version in this file and in the guard.
