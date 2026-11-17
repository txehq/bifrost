import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alertDialog";

interface Props {
	/** UI tab id of the connector being deleted ("otel" or "prometheus"), or null when closed. */
	connector: "otel" | "prometheus" | null;
	isDeleting: boolean;
	onCancel: () => void;
	onConfirm: () => void;
}

// Deleting the Prometheus connector deletes the "telemetry" plugin, which also serves the
// /metrics scrape endpoint, so its warning says so (TXE-7746).
const COPY = {
	otel: {
		title: "Delete OpenTelemetry connector",
		description: "This removes the OpenTelemetry connector and its configuration. Bifrost stops sending traces and metrics to your collectors.",
	},
	prometheus: {
		title: "Delete Prometheus connector",
		description:
			"This removes the telemetry plugin and its configuration. Bifrost stops serving the /metrics endpoint, so Prometheus can no longer scrape it, and stops pushing to the Push Gateway.",
	},
} as const;

export default function ConfirmDeleteConnectorDialog({ connector, isDeleting, onCancel, onConfirm }: Props) {
	const copy = connector ? COPY[connector] : null;
	return (
		<AlertDialog
			open={connector !== null}
			onOpenChange={(open) => {
				if (!open && !isDeleting) onCancel();
			}}
		>
			<AlertDialogContent data-testid="observability-connector-delete-dialog">
				<AlertDialogHeader>
					<AlertDialogTitle>{copy?.title}</AlertDialogTitle>
					<AlertDialogDescription data-testid="observability-connector-delete-description">
						{copy?.description} This cannot be undone.
					</AlertDialogDescription>
				</AlertDialogHeader>
				<AlertDialogFooter>
					<AlertDialogCancel disabled={isDeleting} data-testid="observability-connector-delete-cancel">
						Cancel
					</AlertDialogCancel>
					<AlertDialogAction
						onClick={(e) => {
							// Keep the dialog open until the request settles.
							e.preventDefault();
							onConfirm();
						}}
						disabled={isDeleting}
						data-testid="observability-connector-delete-confirm"
					>
						{isDeleting ? "Deleting..." : "Delete"}
					</AlertDialogAction>
				</AlertDialogFooter>
			</AlertDialogContent>
		</AlertDialog>
	);
}
