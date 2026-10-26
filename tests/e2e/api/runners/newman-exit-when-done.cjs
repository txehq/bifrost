// Preloaded into the newman CLI (node --require <this> "$(command -v newman)" run ...).
//
// newman 6 never calls process.exit: when a run finishes it only sets process.exitCode
// and waits for the event loop to drain. On the Linux release runner the request-guard
// run printed its summary with every assertion passing and then never exited, holding
// the job until it was cancelled (TXE-5180).
//
// This hook wraps newman.run. newman calls the run callback only after the runtime's
// `done` (every request and assertion finished), after `beforeDone` (where reporters
// queue their exports) and after every queued export file is written; a runtime or
// export error reaches the callback as `err`, and the CLI's callback turns it, or any
// failed assertion, into exitCode 1. The hook acts only after that callback: if the
// process has not exited by itself within a grace period, it names what is holding it
// open and exits with newman's code, or with 1 when file or write I/O is still pending,
// so unfinished output is never reported as success. A run that never reaches its
// callback is left alone, and a run that exits by itself is not touched. A file call
// blocked in libuv's thread pool also blocks process.exit, so that case stays hung:
// still never a success.
'use strict';

const fs = require('fs');
const path = require('path');

const GRACE_MS = Number(process.env.NEWMAN_EXIT_GRACE_MS) || 10000;
// File requests and unfinished socket/pipe writes (WriteWrap, SimpleWriteWrap, ShutdownWrap).
const PENDING_IO = /^(FSReq|FileHandle)|(WriteWrap|ShutdownWrap)$/;

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
            const pendingIO = open.some((r) => PENDING_IO.test(r));
            const code = pendingIO ? 1 : (process.exitCode ?? 0);
            console.error(`[newman-exit-when-done] run finished ${GRACE_MS}ms ago but the process is still held open by: ${open.join(', ') || 'unknown'}; exiting with ${code}${pendingIO ? ' (file or write I/O still pending)' : " (the run's result)"}`);
            process.stdout.write('', () => process.stderr.write('', () => process.exit(code)));
        }, GRACE_MS).unref();
    });
};
