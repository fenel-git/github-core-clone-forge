import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const filename = process.arch === 'arm64' ? './forge-local'
  : process.arch === 'x64' ? './forge-linux-amd64' : null;
if (filename === null) {
  process.stderr.write(`[entry] unsupported architecture: ${process.arch}\n`);
  process.exit(1);
}
const child = spawn(fileURLToPath(new URL(filename, import.meta.url)), process.argv.slice(2), {
  cwd: fileURLToPath(new URL('.', import.meta.url)),
  env: process.env,
  stdio: 'inherit',
});
for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => { if (!child.killed) child.kill(signal); });
}
child.on('error', (error) => {
  process.stderr.write(`[entry] ${error.message}\n`);
  process.exitCode = 1;
});
child.on('exit', (code, signal) => {
  process.exitCode = code ?? (signal === 'SIGINT' ? 130 : 1);
});
