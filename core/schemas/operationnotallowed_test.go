package schemas

import (
	"errors"
	"fmt"
	"testing"
)

func TestAsOperationNotAllowed(t *testing.T) {
	refused := &OperationNotAllowedError{RequestType: LiveRequest, Provider: "custom"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the error itself", refused, true},
		{"wrapped with %w", fmt.Errorf("select key: %w", refused), true},
		{"twice wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", refused)), true},
		{"joined", errors.Join(errors.New("other"), refused), true},
		{"another error", errors.New("live is not supported by custom provider"), false},
		{"wrapped another error", fmt.Errorf("x: %w", errors.New("y")), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		got, ok := AsOperationNotAllowed(tc.err)
		if ok != tc.want {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.want)
		}
		if ok && got != refused {
			t.Errorf("%s: returned %v, want the refusal in the chain", tc.name, got)
		}
		var viaAs *OperationNotAllowedError
		if errors.As(tc.err, &viaAs) != tc.want {
			t.Errorf("%s: disagrees with errors.As", tc.name)
		}
	}
}

func TestAsOperationNotAllowedAllocatesNothingForAnotherError(t *testing.T) {
	other := fmt.Errorf("x: %w", errors.New("no keys found"))
	if allocs := testing.AllocsPerRun(100, func() { _, _ = AsOperationNotAllowed(other) }); allocs != 0 {
		t.Fatalf("AsOperationNotAllowed allocated %v times for an error that is not a refusal", allocs)
	}
}
