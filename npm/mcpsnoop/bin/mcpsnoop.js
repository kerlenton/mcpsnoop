#!/usr/bin/env node
'use strict'

// The launcher for the npm distribution of mcpsnoop.
//
// mcpsnoop is a Go binary. This package ships none of it: the six platform
// packages below each carry one build, and npm installs the single one matching
// the machine through their os and cpu fields. So the download is the ordinary
// registry one, which works behind a proxy, from a cache, and under
// --ignore-scripts, where a postinstall that fetched from GitHub would not.
//
// What this file does is find that binary and get out of the way. mcpsnoop is a
// shim that sits in the pipe between an MCP client and its server, so the three
// things below are not incidental.

const { spawn } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')

// platformPackage names the package holding this machine's build. The Go side
// spells the same pair differently, so the mapping is written out rather than
// derived, and an unknown platform is named rather than guessed at.
const platformPackage = {
  'darwin-arm64': '@mcpsnoop/darwin-arm64',
  'darwin-x64': '@mcpsnoop/darwin-x64',
  'linux-arm64': '@mcpsnoop/linux-arm64',
  'linux-x64': '@mcpsnoop/linux-x64',
  'win32-arm64': '@mcpsnoop/win32-arm64',
  'win32-x64': '@mcpsnoop/win32-x64',
}

function resolveBinary() {
  const key = `${process.platform}-${process.arch}`
  const pkg = platformPackage[key]
  if (!pkg) {
    fail(
      `mcpsnoop has no prebuilt binary for ${key}.`,
      '',
      'The npm package ships builds for ' + Object.keys(platformPackage).sort().join(', ') + '.',
      'Install from source instead with: go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest'
    )
  }
  const file = process.platform === 'win32' ? 'mcpsnoop.exe' : 'mcpsnoop'
  try {
    return require.resolve(`${pkg}/bin/${file}`)
  } catch {
    // npm skipped or dropped the optional dependency. It does that on purpose
    // for a platform that does not match, which is handled above, and by
    // accident when a lockfile was written on another platform or the install
    // ran with --no-optional. Both are fixed the same way and neither is worth
    // a stack trace.
    fail(
      `mcpsnoop is installed but ${pkg} is not, so there is no binary to run.`,
      '',
      'npm leaves out an optional dependency when the install ran with',
      '--no-optional, and sometimes when a lockfile written on another platform',
      'is installed as it stands. Clearing what remembers the wrong answer fixes',
      'it.',
      '',
      '  in a project   rm -rf node_modules package-lock.json && npm install',
      '  through npx    npm cache clean --force',
      '',
      'Or install from source with: go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest'
    )
  }
}

// fail writes to stderr and stops.
//
// Synchronously, and not through console.error. On macOS and Windows a pipe is
// written asynchronously, and process.exit does not wait for that, so a message
// followed by an exit can simply vanish. An MCP client captures its server's
// stderr into a log, which is a pipe, and the whole point of these messages is
// that someone reads them there.
function fail(...lines) {
  try {
    fs.writeSync(2, lines.join('\n') + '\n')
  } catch {
    // Nothing useful is left to do about a stderr that will not take bytes.
  }
  process.exit(1)
}

// start runs the binary, putting back an execute bit that the install lost.
//
// npm carries the mode through, and the packaging step and its tests keep the
// bit on. Other clients have not always, and a binary unpacked without it cannot
// be started at all. Telling someone to chmod a file inside node_modules is a
// poor answer when the fix is one call.
//
// The check has to come first. spawn reports EACCES through the error event
// rather than by throwing, so by the time it is known the moment to fix it and
// try again has passed.
function start(binary) {
  try {
    fs.accessSync(binary, fs.constants.X_OK)
  } catch {
    try {
      fs.chmodSync(binary, 0o755)
    } catch {
      // Let spawn report it. Its error names the file and the reason, and a
      // read-only install is not something this can talk its way out of.
    }
  }
  return spawn(binary, process.argv.slice(2), spawnOptions)
}

const spawnOptions = {
  // The real file descriptors, not pipes. mcpsnoop stands between a client and a
  // server on stdin and stdout, and its TUI needs the terminal, so anything that
  // copies bytes through this process would add buffering to a pipe whose whole
  // purpose is to be transparent.
  stdio: 'inherit',
  windowsHide: true,
}

const child = start(resolveBinary())

// A client stops its server by signalling it. Without this the signal reaches
// the wrapper and not the shim, and mcpsnoop's own shutdown never runs.
//
// Installing a handler also takes away Node's own default, which is to die. That
// is what makes the wrapper wait for the shim rather than abandoning it, and it
// is why the close handler below has to put the default back before raising the
// signal on itself.
const forwarded = ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGBREAK']
for (const signal of forwarded) {
  process.on(signal, () => {
    if (child.exitCode === null && child.signalCode === null) child.kill(signal)
  })
}

child.on('error', (err) => {
  // spawn reports most failures here rather than by throwing, EACCES included
  // when the binary is found but cannot be executed.
  if (err.code === 'EACCES') {
    fail(
      `mcpsnoop cannot run ${err.path || 'its binary'}, which arrived without its execute bit.`,
      '',
      'Reinstalling usually restores it:',
      '',
      '  npm install mcpsnoop --force'
    )
  }
  fail(`mcpsnoop could not start: ${err.message}`)
})

child.on('close', (code, signal) => {
  if (signal) {
    // Die the way the child died, so a caller reading the wait status sees the
    // signal rather than an invented exit code.
    //
    // The handlers have to come off first. While one is installed the signal is
    // delivered to it instead of killing the process, so raising it here would
    // only call that handler again, and the wrapper would sit in its event loop
    // for ever holding a client that is waiting for it to exit.
    for (const s of forwarded) process.removeAllListeners(s)
    process.kill(process.pid, signal)
    // Only reached if the signal turned out not to be fatal after all.
    process.exit(128 + (os.constants.signals[signal] || 0))
    return
  }
  // mcpsnoop reports the wrapped server's exit code as its own, and a wrapper
  // that flattened it would throw away the one number a caller came for.
  process.exit(code === null ? 1 : code)
})
