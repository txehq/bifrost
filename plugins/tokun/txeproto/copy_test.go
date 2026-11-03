package txeproto_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	catalogv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/catalog/v1"
	eventsv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/events/v1"
	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// The copy under tokun/ equals the .go files of txe's tokun/proto/gen/go/tokun/
// file for file and byte for byte. TXE_REPO names the txe checkout to compare
// with: the new-tokun branch, or the worktree of a txe change that goes with
// this one.
func TestCopyEqualsTxe(t *testing.T) {
	txe := os.Getenv("TXE_REPO")
	if txe == "" {
		t.Fatal("TXE_REPO is not set; set it to a txe checkout whose tokun/proto/gen/go/tokun is the source of this copy")
	}
	want := readTree(t, filepath.Join(txe, "tokun", "proto", "gen", "go", "tokun"))
	got := readTree(t, "tokun")

	for _, name := range sortedKeys(want) {
		data, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s is in txe and missing here; run sync.sh", name)
		case !bytes.Equal(data, want[name]):
			t.Errorf("%s differs from txe; run sync.sh", name)
		}
	}
	for _, name := range sortedKeys(got) {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is here and not in txe; run sync.sh", name)
		}
	}
}

// Every contract file registers under its tokun/ path, so the copy cannot
// collide with another proto file a Bifrost binary links.
func TestContractsRegisterUnderTokunPaths(t *testing.T) {
	files := []protoreflect.FileDescriptor{
		catalogv1.File_tokun_catalog_v1_changeset_proto,
		eventsv1.File_tokun_events_v1_charge_proto,
		plannerv1.File_tokun_planner_v1_planner_proto,
		routingv1.File_tokun_routing_v1_route_plan_proto,
	}
	for _, fd := range files {
		got, err := protoregistry.GlobalFiles.FindFileByPath(fd.Path())
		if err != nil {
			t.Fatalf("%s is not registered: %v", fd.Path(), err)
		}
		if got != fd {
			t.Errorf("%s is registered by another descriptor", fd.Path())
		}
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s has no files", root)
	}
	return files
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
