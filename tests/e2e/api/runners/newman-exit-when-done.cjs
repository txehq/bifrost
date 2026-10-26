// Preloaded into the newman CLI (node --require <this> "$(command -v newman)" run ...).
//
// newman 6 never calls process.exit: when a run finishes it only sets process.exitCode
// and waits for the event loop to drain. On the Linux release runner the request-guard
// run printed its summary with every assertion passing and then never exited, holding
// the job until it was cancelled (TXE-5180). This hook wraps newman.run: once newman's
// own completion callback has set the exit code, a run that has not exited by itself
// within a grace period names whatever is still keeping it alive and exits with that
// code. Run results are unchanged, and a run that exits normally is not touched.
'use strict';

const fs = require('fs');
const path = require('path');

const GRACE_MS = 10000;

const newman = require(path.join(path.dirname(fs.realpathSync(process.argv[1])), '..'));
const run = newman.run;

newman.run = function (options, callback) {
    return run.call(this, options, function () {
        if (typeof callback === 'function') {
            callback.apply(this, arguments);
        }
        // unref: this timer never keeps the process alive on its own.
        setTimeout(() => {
            const open = process.getActiveResourcesInfo().filter((r) => r !== 'TTYWrap');
            console.error(`[newman-exit-when-done] run finished ${GRACE_MS}ms ago but the process is still held open by: ${open.join(', ') || 'unknown'}; exiting with the run's result`);
            const code = process.exitCode ?? 0;
            process.stdout.write('', () => process.stderr.write('', () => process.exit(code)));
        }, GRACE_MS).unref();
    });
};
