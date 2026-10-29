package secrets

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func restoreKeyringFunctions(t *testing.T) {
	t.Helper()
	get, set, remove := keyringGet, keyringSet, keyringDelete
	t.Cleanup(func() {
		keyringGet, keyringSet, keyringDelete = get, set, remove
	})
}

func TestKeyForProfileSeparatesKindsAndProfiles(t *testing.T) {
	if got := keyForProfile("engineering", VirtualKey); got != "profile:engineering:virtual-key" {
		t.Fatalf("key = %q", got)
	}
	if keyForProfile("engineering", VirtualKey) == keyForProfile("production", VirtualKey) ||
		keyForProfile("engineering", VirtualKey) == keyForProfile("engineering", ManagementKey) {
		t.Fatal("profile key does not isolate profile and credential kind")
	}
}

func TestGetTreatsMissingCredentialAsEmpty(t *testing.T) {
	restoreKeyringFunctions(t)
	keyringGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	value, err := Get("engineering", AgentToken)
	if err != nil || value != "" {
		t.Fatalf("Get() = %q, %v", value, err)
	}
}

func TestGetWrapsBackendFailure(t *testing.T) {
	restoreKeyringFunctions(t)
	keyringGet = func(string, string) (string, error) { return "", errors.New("backend unavailable") }
	_, err := Get("engineering", AgentToken)
	if err == nil || !strings.Contains(err.Error(), "read agent-token") || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("Get() error = %v", err)
	}
}

func TestSetTrimsCredentialAndEmptyDeletes(t *testing.T) {
	restoreKeyringFunctions(t)
	var stored, deleted string
	keyringSet = func(_, account, value string) error {
		stored = account + "=" + value
		return nil
	}
	keyringDelete = func(_, account string) error {
		deleted = account
		return keyring.ErrNotFound
	}
	if err := Set("engineering", VirtualKey, "  sk-bf-test  "); err != nil {
		t.Fatal(err)
	}
	if stored != "profile:engineering:virtual-key=sk-bf-test" {
		t.Fatalf("stored = %q", stored)
	}
	if err := Set("engineering", VirtualKey, "  "); err != nil {
		t.Fatal(err)
	}
	if deleted != "profile:engineering:virtual-key" {
		t.Fatalf("deleted = %q", deleted)
	}
}

// TestProfileCredentialKindsCoversEveryKind pins that every credential kind this
// package declares is in ProfileCredentialKinds, the list cleared when a profile's
// gateway URL changes or the profile is deleted. A kind added later without
// joining the list would otherwise survive a URL change and reach the new gateway.
func TestProfileCredentialKindsCoversEveryKind(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "secrets.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []Kind
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "Kind" {
				continue
			}
			for _, lit := range value.Values {
				basic, ok := lit.(*ast.BasicLit)
				if !ok || basic.Kind != token.STRING {
					t.Fatalf("Kind constant with a non-literal value: %T", lit)
				}
				declared = append(declared, Kind(strings.Trim(basic.Value, `"`)))
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Kind constants in secrets.go")
	}
	for _, kind := range declared {
		if !slices.Contains(ProfileCredentialKinds, kind) {
			t.Errorf("credential kind %q is not in ProfileCredentialKinds, so it would survive a gateway URL change", kind)
		}
	}
	if len(ProfileCredentialKinds) != len(declared) {
		t.Errorf("ProfileCredentialKinds has %d entries, secrets.go declares %d kinds", len(ProfileCredentialKinds), len(declared))
	}
}
