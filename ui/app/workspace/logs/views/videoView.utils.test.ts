import { describe, expect, it } from "vitest";
import { VideoOutput } from "@/lib/types/logs";
import { getVideoSrc, isImageOutput, splitVideoOutputs } from "./videoView.utils";

const clip: VideoOutput = { type: "url", url: "https://cdn.example/v.mp4", content_type: "video/mp4" };
const lastFrame: VideoOutput = { type: "url", url: "https://cdn.example/last.jpeg", content_type: "image/jpeg" };

describe("isImageOutput", () => {
	it("is true only for an image content type", () => {
		expect(isImageOutput(lastFrame)).toBe(true);
		expect(isImageOutput(clip)).toBe(false);
		expect(isImageOutput({ type: "url", url: "https://cdn.example/v.mov", content_type: "video/quicktime" })).toBe(false);
	});

	it("treats an output with no content type as a video", () => {
		expect(isImageOutput({ type: "url", url: "https://cdn.example/v" })).toBe(false);
	});
});

describe("splitVideoOutputs", () => {
	it("keeps a Seedance task's last frame out of the playable videos", () => {
		expect(splitVideoOutputs([clip, lastFrame])).toEqual({ videos: [clip], images: [lastFrame] });
	});

	it("leaves a job with only videos unchanged", () => {
		const second: VideoOutput = { type: "url", url: "https://cdn.example/v2.mp4", content_type: "video/mp4" };
		expect(splitVideoOutputs([clip, second])).toEqual({ videos: [clip, second], images: [] });
	});

	it("drops outputs with neither a URL nor inline data", () => {
		expect(splitVideoOutputs([{ type: "url", content_type: "video/mp4" }, { type: "url", content_type: "image/jpeg" }, clip])).toEqual({
			videos: [clip],
			images: [],
		});
	});

	it("is empty for a job with no outputs", () => {
		expect(splitVideoOutputs(undefined)).toEqual({ videos: [], images: [] });
		expect(splitVideoOutputs(null)).toEqual({ videos: [], images: [] });
		expect(splitVideoOutputs([])).toEqual({ videos: [], images: [] });
	});
});

describe("getVideoSrc", () => {
	it("prefers the URL", () => {
		expect(getVideoSrc({ ...clip, base64: "AAAA" })).toBe("https://cdn.example/v.mp4");
	});

	it("builds a data URI from inline data with the output's own type", () => {
		expect(getVideoSrc({ type: "base64", base64: "AAAA", content_type: "image/jpeg" })).toBe("data:image/jpeg;base64,AAAA");
		expect(getVideoSrc({ type: "base64", base64: "AAAA" })).toBe("data:video/mp4;base64,AAAA");
	});

	it("is null when there is nothing to show", () => {
		expect(getVideoSrc({ type: "url" })).toBeNull();
	});
});