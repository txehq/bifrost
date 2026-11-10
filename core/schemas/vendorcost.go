package schemas

// VendorCostUnit names what VendorCost.Amount counts.
type VendorCostUnit string

// The units a vendor reports money or credits in (fork, TXE-6518, contract C5).
const (
	VendorCostUnitUSD      VendorCostUnit = "usd"       // US dollars
	VendorCostUnitUSDTicks VendorCostUnit = "usd_ticks" // xAI cost_in_usd_ticks, 1e-10 USD each
	VendorCostUnitCredit   VendorCostUnit = "credit"    // vendor credits (BFL; ElevenLabs character-cost)
	VendorCostUnitUnit     VendorCostUnit = "unit"      // Kling units
)

// IsValid reports whether u is one of the four units the contract defines.
func (u VendorCostUnit) IsValid() bool {
	switch u {
	case VendorCostUnitUSD, VendorCostUnitUSDTicks, VendorCostUnitCredit, VendorCostUnitUnit:
		return true
	default:
		return false
	}
}

// VendorCost is the money or credits a vendor reported for one request or task, copied from the
// vendor's own response in the vendor's own unit (fork, TXE-6518, contract C5 BF6). It is never
// computed by Bifrost: the catalog-priced usage.cost is a separate field. Amount carries no
// omitempty, so a vendor that reports a zero charge is told apart from one that reports nothing
// (whose usage has no vendor_cost at all).
type VendorCost struct {
	Amount float64        `json:"amount"`
	Unit   VendorCostUnit `json:"unit"`
}

// SumVendorCosts is the vendor's charge for a request made of several upstream calls (an MCP agent's turns),
// fork TXE-6967: the sum when every call reported a charge in the same unit, otherwise nil. A partial sum
// would understate what the vendor charged, and nil is what contract C5 agreement two means by "the
// vendor did not report it", so the caller measures instead.
func SumVendorCosts(base, add *VendorCost) *VendorCost {
	if base == nil || add == nil || base.Unit != add.Unit {
		return nil
	}
	return &VendorCost{Amount: base.Amount + add.Amount, Unit: base.Unit}
}

// sumReportedInt64 is SumVendorCosts for a raw counter such as cost_in_usd_ticks: the sum when both calls
// reported it, otherwise nil.
func sumReportedInt64(base, add *int64) *int64 {
	if base == nil || add == nil {
		return nil
	}
	sum := *base + *add
	return &sum
}

// vendorCostFromUSDTicks is xAI's reported charge, cost_in_usd_ticks, as a VendorCost in the vendor's own
// unit (fork, TXE-6967, contract C5/C10): nil when the vendor reported no ticks, and a reported zero stays
// amount 0, unlike costFromUSDTicks, which feeds pricing and skips non-positive counts.
func vendorCostFromUSDTicks(ticks *int64) *VendorCost {
	if ticks == nil {
		return nil
	}
	return &VendorCost{Amount: float64(*ticks), Unit: VendorCostUnitUSDTicks}
}
