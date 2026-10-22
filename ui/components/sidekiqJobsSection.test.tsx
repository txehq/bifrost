import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import SidekiqJobsSection from "./sidekiqJobsSection";

const noop = () => {};

describe("background jobs section", () => {
	it("says there are no jobs only once a list has arrived", () => {
		expect(renderToStaticMarkup(<SidekiqJobsSection jobs={[]} onCancel={noop} onDismiss={noop} />)).toContain("No background jobs");
		const loading = renderToStaticMarkup(<SidekiqJobsSection jobs={[]} loading onCancel={noop} onDismiss={noop} />);
		expect(loading).not.toContain("No background jobs");
		expect(loading).toContain("Loading background jobs");
		const failed = renderToStaticMarkup(<SidekiqJobsSection jobs={[]} failed onRetry={noop} onCancel={noop} onDismiss={noop} />);
		expect(failed).not.toContain("No background jobs");
		expect(failed).toContain("Couldn&#x27;t load background jobs");
		expect(failed).toContain("sidekiq-jobs-retry");
	});
});
