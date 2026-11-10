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
