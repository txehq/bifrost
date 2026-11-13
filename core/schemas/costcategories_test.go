package schemas

import (
	"reflect"
	"testing"
)

// TestEveryCostDetailFieldIsExported fails when a category is added to the cost
// breakdown without the rest of its wiring (TXE-7373: video input cost was reported
// inside TextCost because the breakdown had no video category). For every field of
// InputCostDetails, OutputCostDetails and AdditionalCostDetails:
//
//   - ExportFixtureCost carries a non-zero value for it, so AssertCostBreakdown, which
//     every connector's export test runs, sees the category;
//   - CostAttributes emits it as exactly one span attribute;
//   - CostAttributeLookup maps that attribute to a category its side sums;
//   - Add and deepCopy keep it.
func TestEveryCostDetailFieldIsExported(t *testing.T) {
	fixture := ExportFixtureCost()
	sides := []struct {
		side    CostCategory
		fixture any
		build   func(field string, v float64) *BifrostCost
	}{
		{CostInput, fixture.InputCostDetails, func(field string, v float64) *BifrostCost {
			d := &InputCostDetails{}
			reflect.ValueOf(d).Elem().FieldByName(field).SetFloat(v)
			return &BifrostCost{InputCostDetails: d}
		}},
		{CostOutput, fixture.OutputCostDetails, func(field string, v float64) *BifrostCost {
			d := &OutputCostDetails{}
			reflect.ValueOf(d).Elem().FieldByName(field).SetFloat(v)
			return &BifrostCost{OutputCostDetails: d}
		}},
		{CostAdditional, fixture.AdditionalCostDetails, func(field string, v float64) *BifrostCost {
			d := &AdditionalCostDetails{}
			reflect.ValueOf(d).Elem().FieldByName(field).SetFloat(v)
			return &BifrostCost{AdditionalCostDetails: d}
		}},
	}
	for _, s := range sides {
		fv := reflect.ValueOf(s.fixture).Elem()
		for i := 0; i < fv.NumField(); i++ {
			field := fv.Type().Field(i)
			if field.Type.Kind() != reflect.Float64 {
				t.Errorf("%s.%s is not a float64 cost; extend this check for it", fv.Type().Name(), field.Name)
				continue
			}
			name := fv.Type().Name() + "." + field.Name
			if fv.Field(i).Float() == 0 {
				t.Errorf("%s is zero in ExportFixtureCost, so no connector's export test sees it", name)
			}

			const v = 0.0123
			one := s.build(field.Name, v)
			var emitted []string
			for key, got := range CostAttributes(one) {
				if key != AttrUsageCost && got == v {
					emitted = append(emitted, key)
				}
			}
			if len(emitted) != 1 {
				t.Errorf("%s is emitted as %d span attributes %v, want exactly one", name, len(emitted), emitted)
				continue
			}
			// The attribute must read back, through CostAttributeLookup, as a category its side sums.
			only := map[string]any{emitted[0]: v}
			var categories []CostCategory
			for _, c := range costSides[s.side] {
				if got, ok := CostAttributeLookup(only)(c); ok && got == v {
					categories = append(categories, c)
				}
			}
			if len(categories) != 1 {
				t.Errorf("%s's attribute %q reads back as %d categories of the %q side %v, want exactly one", name, emitted[0], len(categories), s.side, categories)
			}

			sum := one.Add(s.build(field.Name, v))
			if got := reflect.ValueOf(detailsOf(sum, s.side)).Elem().FieldByName(field.Name).Float(); got != 2*v {
				t.Errorf("Add drops %s: got %v, want %v", name, got, 2*v)
			}
			if got := reflect.ValueOf(detailsOf(one.DeepCopy(), s.side)).Elem().FieldByName(field.Name).Float(); got != v {
				t.Errorf("deepCopy drops %s: got %v, want %v", name, got, v)
			}
		}
	}
}

func detailsOf(c *BifrostCost, side CostCategory) any {
	switch side {
	case CostInput:
		return c.InputCostDetails
	case CostOutput:
		return c.OutputCostDetails
	default:
		return c.AdditionalCostDetails
	}
}
