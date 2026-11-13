// The one place harness runners and their tests pick a loopback port to listen on (TXE-7326).
//
// Ports come from below every common ephemeral range (Linux 32768-60999, macOS and Windows
// 49152-65535). A port checked free inside that range can still be handed out before the process that
// wants it binds it: a gateway opens loopback connections at startup (model listing, the object store's
// ping) before it listens, and the kernel gives each one an ephemeral local port. Picked there, the
// gateway's own startup took its listen port once in CI ("bind: address already in use" on
// 127.0.0.1:57598). fixture-folders.test.mjs fails when a runner picks a random port by hand instead.
import net from "node:net";

export const PORT_RANGE = { low: 20000, high: 32000 };

// randomLoopbackPort returns a port in PORT_RANGE that leaves room for `extra` consecutive ports above
// it, without checking that any is free.
export function randomLoopbackPort(extra = 0) {
	return PORT_RANGE.low + Math.floor(Math.random() * (PORT_RANGE.high - PORT_RANGE.low - extra));
}

const canBind = (port) =>
	new Promise((ok) => {
		const s = net.createServer();
		s.once("error", () => ok(false));
		s.listen(port, "127.0.0.1", () => s.close(() => ok(true)));
	});

// freeLoopbackPort returns a port in PORT_RANGE that, with every offset in span, can be bound on
// 127.0.0.1 right now.
export async function freeLoopbackPort(span = [0]) {
	const extra = Math.max(0, ...span);
	for (let i = 0; i < 200; i++) {
		const base = randomLoopbackPort(extra);
		let ok = true;
		for (const d of span) if (!(await canBind(base + d))) ok = false;
		if (ok) return base;
	}
	throw new Error("no free loopback port found");
}
