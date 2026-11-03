# Tokun contracts (copy from txe)

The generated Go code of the Tokun contracts: the route plan and its signature,
attempt records, task events, the planner contracts, the catalog projections
and the events. The source of truth is txe's `tokun/proto` (buf-managed protos
and `gen/go`); this directory holds a byte-identical copy of the `.go` files
under `tokun/proto/gen/go/tokun/`, at the same paths under `tokun/`. txe's
`BUILD.bazel` files there are not copied.

- **Location**: `plugins/tokun/` is where the tokun routing plugin lives; this
  copy is the nested module `plugins/tokun/txeproto`, listed in `go.work`.
- **Module path**: `github.com/capsohq/txe/tokun/proto/gen/go`, txe's import
  path for `gen/go`, so the copied files compile here without edits. Import a
  package as `github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1`.
- **Never edit the copy.** Change the `.proto` files in txe, run `buf generate`
  there, then:

  ```bash
  plugins/tokun/txeproto/sync.sh <txe checkout>
  ```

- **Comparison**: `copy_test.go` reads every `.go` file under
  `$TXE_REPO/tokun/proto/gen/go/tokun/` and under `tokun/` here, and
  fails on a file missing on either side or on any byte difference. `TXE_REPO`
  is required:

  ```bash
  cd plugins/tokun/txeproto
  TXE_REPO=<txe checkout> go test ./...
  ```

  Point `TXE_REPO` at a checkout of txe's `new-tokun`, or at the txe worktree of
  a change that goes with this one.
