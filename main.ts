// main.ts — thin launcher for the vendored Go forge binary.
//
// The platform starts this file with `node /bot/main.ts`. All it does is
// hand over to the static binary that sits beside it: find `./forge`
// relative to this file (never relative to the working directory), run it
// with the same arguments and the same stdio, pass signals through, and
// leave with whatever exit code the binary leaves with. There is no server
// logic here and no environment handling — the Go program owns all of that.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const binaryPath = path.join(scriptDir, 'forge');

const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: 'inherit',
});

const forward = (signal: NodeJS.Signals): void => {
  if (child.pid !== undefined) {
    try {
      child.kill(signal);
    } catch {
      // The child already went away; nothing left to forward to.
    }
  }
};

process.on('SIGINT', () => forward('SIGINT'));
process.on('SIGTERM', () => forward('SIGTERM'));

child.on('error', (err: Error) => {
  console.error(`failed to start ${binaryPath}: ${err.message}`);
  process.exit(1);
});

child.on('exit', (code: number | null, signal: NodeJS.Signals | null) => {
  if (signal !== null) {
    // Killed by a signal: report it the way a shell would.
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
