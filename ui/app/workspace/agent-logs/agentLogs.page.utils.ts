/**
 * Whether the Agent logs page shows its onboarding empty state instead of the
 * history view. The probe only covers the last 30 days, so its being empty
 * does not prove there are no logs: the history view stays whenever the
 * selected range returned rows (an absolute range or this month can reach past
 * the probe), and an explicit absolute range keeps its date controls even when
 * empty.
 */
export function showAgentLogsOnboarding(state: { probeLoaded: boolean; probeTotal: number; pageTotal: number; period: string }): boolean {
	return state.probeLoaded && state.probeTotal === 0 && state.pageTotal === 0 && state.period !== "";
}