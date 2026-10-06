import { spawn } from 'node:child_process';
import { join } from 'node:path';

export function startServer({ tsserverJs, workspaceRoot, plugin, deadline, env = {} }) {
  const args = [tsserverJs, '--disableAutomaticTypingAcquisition'];
  if (plugin === 'global') args.push('--globalPlugins', '@rules_typescript/tsserver-plugin');
  if (plugin) args.push('--pluginProbeLocations', join(workspaceRoot, '.bazel'));
  const proc = spawn(process.execPath, args, {
    cwd: workspaceRoot,
    env: { ...process.env, ...env },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  let stderr = '';
  proc.stderr.on('data', (chunk) => (stderr += chunk));
  const pending = new Map();
  let failure;
  function rejectPending(error) {
    failure = error;
    for (const waiter of pending.values()) waiter.reject(error);
    pending.clear();
  }
  proc.on('error', rejectPending);
  proc.stdin.on('error', rejectPending);
  const closed = new Promise((resolve) => {
    proc.on('close', (code, signal) => {
      rejectPending(new Error(`tsserver exited: code=${code}, signal=${signal}\n${stderr}`));
      resolve({ code, signal });
    });
  });
  let buffered = '';
  proc.stdout.setEncoding('utf8');
  proc.stdout.on('data', (chunk) => {
    buffered += chunk;
    for (let nl = buffered.indexOf('\n'); nl !== -1; nl = buffered.indexOf('\n')) {
      const line = buffered.slice(0, nl).trim();
      buffered = buffered.slice(nl + 1);
      if (!line.startsWith('{')) continue;
      try {
        const message = JSON.parse(line);
        const waiter = pending.get(message.request_seq);
        if (waiter) {
          pending.delete(message.request_seq);
          waiter.resolve(message);
        }
      } catch (error) {
        rejectPending(error);
      }
    }
  });
  let seq = 0;
  function send(command, args, wanted = ++seq) {
    if (failure) throw failure;
    proc.stdin.write(
      JSON.stringify({ seq: wanted, type: 'request', command, arguments: args }) + '\n'
    );
  }
  function withinDeadline(promise, command) {
    if (!deadline) return promise;
    let timer;
    return Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(
          () => {
            const error = new Error(`test deadline awaiting tsserver ${command}`);
            rejectPending(error);
            proc.kill();
            reject(error);
          },
          Math.max(0, deadline - Date.now())
        );
      }),
    ]).finally(() => clearTimeout(timer));
  }
  function request(command, args) {
    return withinDeadline(
      new Promise((resolve, reject) => {
        const wanted = ++seq;
        pending.set(wanted, {
          reject,
          resolve(message) {
            if (message.type === 'response' && message.success === true) {
              resolve(message.body);
            } else if (
              message.success === false &&
              message.message === 'No content available.' &&
              (command === 'definition' || command === 'completionInfo')
            ) {
              resolve(undefined);
            } else {
              reject(
                new Error(
                  `tsserver ${command} failed: ${message.message || JSON.stringify(message)}`
                )
              );
            }
          },
        });
        send(command, args, wanted);
      }),
      command
    );
  }
  return {
    request,
    pid: proc.pid,
    alive: () => !failure && proc.exitCode === null && proc.signalCode === null,
    stderr: () => stderr,
    open: (file) => send('open', { file }),
    async diagnostics(file) {
      const body = await request('semanticDiagnosticsSync', { file });
      if (!Array.isArray(body)) throw new Error('tsserver returned malformed diagnostics');
      return body;
    },
    async stop() {
      if (failure) proc.kill();
      else send('exit');
      const { code } = await withinDeadline(closed, 'exit');
      if (code !== 0) throw failure;
    },
  };
}
