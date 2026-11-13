package datasheet

import (
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// scaleUsageCostFlatFees are the categories scaleUsageCost leaves unscaled on purpose: a flat
// per-request fee and a per-query fee are not usage (see scaleUsageCost).
var scaleUsageCostFlatFees = map[string]bool{
	"InputCostDetails.RequestCost":        true,
	"OutputCostDetails.SearchQueriesCost": true,
}

// TestScaleUsageCostScalesEveryUsageCategory fails when a cost category is added to the
// breakdown without scaleUsageCost scaling it (TXE-7373), which would leave a discount or
// regional multiplier unapplied to that category while the side total moved.
func TestScaleUsageCostScalesEveryUsageCategory(t *testing.T) {
	const v, m = 0.4, 0.5
	for _, details := range []any{&schemas.InputCostDetails{}, &schemas.OutputCostDetails{}} {
		typ := reflect.TypeOf(details).Elem()
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Name() + "." + typ.Field(i).Name
			d := reflect.New(typ)
			d.Elem().Field(i).SetFloat(v)
			cost := &schemas.BifrostCost{TotalCost: v}
			switch dd := d.Interface().(type) {
			case *schemas.InputCostDetails:
				cost.InputCost, cost.InputCostDetails = v, dd
			case *schemas.OutputCostDetails:
				cost.OutputCost, cost.OutputCostDetails = v, dd
			}
			scaleUsageCost(cost, m)
			want := v * m
			if scaleUsageCostFlatFees[name] {
				want = v
			}
			if got := d.Elem().Field(i).Float(); got != want {
				t.Errorf("scaleUsageCost left %s at %v, want %v", name, got, want)
			}
			if cost.TotalCost != want {
				t.Errorf("%s: TotalCost %v, want %v", name, cost.TotalCost, want)
			}
		}
	}
}
