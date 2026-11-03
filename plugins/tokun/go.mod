module github.com/capsohq/bifrost/plugins/tokun

go 1.27.3

require (
	github.com/capsohq/bifrost/core v1.11.2
	github.com/capsohq/txe/tokun/proto/gen/go v0.0.0-00010101000000-000000000000
	github.com/tidwall/gjson v1.18.0
	github.com/tidwall/sjson v1.2.5
	gonum.org/v1/gonum v0.17.0
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af
)

// The contracts are the copy in ./txeproto (a module of its own, listed in go.work).
replace github.com/capsohq/txe/tokun/proto/gen/go => ./txeproto
