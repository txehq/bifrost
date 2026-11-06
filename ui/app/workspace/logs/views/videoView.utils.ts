import { VideoOutput as VideoAsset } from "@/lib/types/logs";

// A video job can return images among its outputs: a Seedance task created with
// return_last_frame lists its final frame after the video. A <video> element cannot play
// those, so the log view shows them as images.
export function isImageOutput(video: VideoAsset): boolean {
	return !!video.content_type?.startsWith("image/");
}

export function getVideoSrc(video: VideoAsset): string | null {
	if (video.url) return video.url;
	if (video.base64) return `data:${video.content_type || "video/mp4"};base64,${video.base64}`;
	return null;
}

// splitVideoOutputs drops outputs with nothing to show and separates the playable videos
// from the image frames, each in the order the job returned them.
export function splitVideoOutputs(outputs?: VideoAsset[] | null): { videos: VideoAsset[]; images: VideoAsset[] } {
	const shown = outputs?.filter((video) => video.url || video.base64) ?? [];
	return {
		videos: shown.filter((video) => !isImageOutput(video)),
		images: shown.filter(isImageOutput),
	};
}