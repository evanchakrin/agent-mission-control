#!/usr/bin/env node
// Build "Agent Mission Control.exe" — a real Windows program, not a .lnk shortcut.
//
// WHY THIS EXISTS. The desktop entry used to be a shortcut pointing at wscript.exe
// pointing at a .vbs. That chain has four ways to fail and it hit three of them: the
// icon rendered blank because Explorer cached a miss against the .ico path, the
// shortcut file silently disappeared, and double-clicking produced "cannot find".
// None of it is fixable from inside a .lnk, because a .lnk is a reference to a thing
// rather than the thing. An .exe carries its icon inside the file, cannot hold a
// stale cache entry keyed to some other path, does not need Windows Script Host
// switched on, and wears no shortcut arrow.
//
// Zero new dependencies: csc.exe ships with the .NET Framework, present on every
// Windows 10/11 machine. Nothing is downloaded and nothing is installed.
//
//   node tools/make-launcher.js --out <dir> [--icon <ico>] [--server <server.js>]
//                               [--port 4173] [--token <token>] [--name <exe>]

const fs = require('fs'), path = require('path'), os = require('os');
const { execFileSync } = require('child_process');

function arg(name, def) {
  const i = process.argv.indexOf('--' + name);
  return i > -1 && process.argv[i + 1] ? process.argv[i + 1] : def;
}
if (process.platform !== 'win32') {
  console.error('This launcher is Windows-only; nothing to build on ' + process.platform + '.');
  process.exit(0);
}

const REPO = path.resolve(__dirname, '..');
const outDir = path.resolve(arg('out', path.join(process.env.LOCALAPPDATA || os.homedir(), 'AgentMissionControl')));
const server = path.resolve(arg('server', path.join(REPO, 'server.js')));
const icon = path.resolve(arg('icon', path.join(REPO, 'public', 'amc.ico')));
const port = arg('port', '4173');
const token = arg('token', '');
const exeName = arg('name', 'Agent Mission Control.exe');
// --mode start builds the STARTUP launcher instead: a windowless program that
// starts the service hidden at logon. It replaced a .vbs in the Startup folder
// the day Windows 11 24H2 shipped without VBScript and the .vbs opened in
// Notepad at every boot of the ERP server (2026-09-01).
const mode = arg('mode', 'launch');

function findCsc() {
  const win = process.env.SystemRoot || 'C:\\Windows';
  const roots = [path.join(win, 'Microsoft.NET', 'Framework64'), path.join(win, 'Microsoft.NET', 'Framework')];
  const found = [];
  for (const r of roots) {
    let dirs = [];
    try { dirs = fs.readdirSync(r); } catch { continue; }
    for (const d of dirs) {
      const c = path.join(r, d, 'csc.exe');
      if (fs.existsSync(c)) found.push(c);
    }
  }
  found.sort();
  const v4 = found.filter(f => f.indexOf('v4.') > -1);   // v4 is what modern Windows carries
  return (v4.length ? v4 : found).pop() || null;
}

// The launcher itself. Every failure path ends in a plain-language message box
// rather than silence — whoever double-clicks this is not going to read a log.
const CS_TEMPLATE = [
  'using System;',
  'using System.Diagnostics;',
  'using System.IO;',
  'using System.Net;',
  'using System.Threading;',
  'using System.Windows.Forms;',
  '',
  'static class Launcher {',
  '    const string URL = "http://localhost:__PORT__";',
  '    const string HEALTH = URL + "/api/meta";',
  '',
  '    // A launcher.cfg beside the exe wins, so an install can move without a rebuild.',
  '    static string Setting(string key, string fallback) {',
  '        try {',
  '            string cfg = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "launcher.cfg");',
  '            if (File.Exists(cfg)) {',
  '                foreach (string raw in File.ReadAllLines(cfg)) {',
  '                    string line = raw.Trim();',
  '                    if (line.StartsWith(key + "=")) return line.Substring(key.Length + 1).Trim();',
  '                }',
  '            }',
  '        } catch { }',
  '        return fallback;',
  '    }',
  '',
  '    // PATH first, then the standard installer locations. Looking in the obvious',
  '    // places beats telling someone "node is not on your PATH".',
  '    static string FindNode() {',
  '        try {',
  '            string p = Environment.GetEnvironmentVariable("PATH");',
  '            if (p != null) {',
  '                foreach (string dir in p.Split(new char[] { (char)59 })) {',
  '                    if (dir.Trim().Length == 0) continue;',
  '                    try {',
  '                        string c = Path.Combine(dir.Trim(), "node.exe");',
  '                        if (File.Exists(c)) return c;',
  '                    } catch { }',
  '                }',
  '            }',
  '        } catch { }',
  '        string[] guesses = {',
  '            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "nodejs", "node.exe"),',
  '            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86), "nodejs", "node.exe"),',
  '            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Programs", "nodejs", "node.exe"),',
  '        };',
  '        foreach (string g in guesses) { try { if (File.Exists(g)) return g; } catch { } }',
  '        return null;',
  '    }',
  '',
  '    static bool IsUp(int timeoutMs) {',
  '        try {',
  '            HttpWebRequest req = (HttpWebRequest)WebRequest.Create(HEALTH);',
  '            req.Timeout = timeoutMs;',
  '            req.ReadWriteTimeout = timeoutMs;',
  '            req.Method = "GET";',
  '            using (HttpWebResponse r = (HttpWebResponse)req.GetResponse()) { return true; }',
  '        } catch (WebException we) {',
  '            // A reply of any kind means something is listening, which is all we asked.',
  '            return we.Response != null;',
  '        } catch { return false; }',
  '    }',
  '',
  '    static void Say(string text, MessageBoxIcon icon) {',
  '        MessageBox.Show(text, "Agent Mission Control", MessageBoxButtons.OK, icon);',
  '    }',
  '',
  '    [STAThread]',
  '    static void Main() {',
  '        if (IsUp(1500)) { Open(); return; }',
  '',
  '        string server = Setting("server", @"__SERVER__");',
  '        if (!File.Exists(server)) {',
  '            Say("Agent Mission Control can\'t start because its program file is missing." +',
  '                "\\n\\nIt looked for it here:\\n" + server +',
  '                "\\n\\nIf you moved or renamed that folder, open launcher.cfg next to this " +',
  '                "program and point server= at the new location.", MessageBoxIcon.Error);',
  '            return;',
  '        }',
  '',
  '        string node = FindNode();',
  '        if (node == null) {',
  '            Say("Agent Mission Control needs Node.js, and it isn\'t installed on this computer." +',
  '                "\\n\\nInstall it from nodejs.org, then try again.", MessageBoxIcon.Error);',
  '            return;',
  '        }',
  '',
  '        try {',
  '            ProcessStartInfo psi = new ProcessStartInfo(node);',
  '            string tok = Setting("token", "__TOKEN__");',
  '            psi.Arguments = "\\"" + server + "\\"" + (tok.Length > 0 ? " --token \\"" + tok + "\\"" : "");',
  '            psi.UseShellExecute = false;',
  '            psi.CreateNoWindow = true;',
  '            psi.WindowStyle = ProcessWindowStyle.Hidden;',
  '            psi.WorkingDirectory = Path.GetDirectoryName(server);',
  '            Process.Start(psi);',
  '        } catch (Exception ex) {',
  '            Say("Agent Mission Control couldn\'t start.\\n\\n" + ex.Message, MessageBoxIcon.Error);',
  '            return;',
  '        }',
  '',
  '        // Up to 30s. A cold start reads every transcript on the machine, so the old',
  '        // 10-second ceiling gave up while it was still perfectly healthy.',
  '        for (int i = 0; i < 60; i++) {',
  '            Thread.Sleep(500);',
  '            if (IsUp(1000)) { Open(); return; }',
  '        }',
  '        Say("Agent Mission Control was started but hasn\'t answered yet.\\n\\n" +',
  '            "It may still be reading through your history. Try this in your browser in a " +',
  '            "moment:\\n" + URL, MessageBoxIcon.Warning);',
  '    }',
  '',
  '    static void Open() {',
  '        try {',
  '            ProcessStartInfo psi = new ProcessStartInfo(URL);',
  '            psi.UseShellExecute = true;   // hand it to the default browser',
  '            Process.Start(psi);',
  '        } catch (Exception ex) {',
  '            Say("The dashboard is running, but your browser didn\'t open.\\n\\nGo to " + URL +',
  '                " and you\'ll find it there.\\n\\n(" + ex.Message + ")", MessageBoxIcon.Warning);',
  '        }',
  '    }',
  '}',
].join('\n');

// The startup launcher: no window, no message boxes (nobody is watching at
// logon), everything it needs in start.cfg beside it so a reinstall or a moved
// Node never needs a rebuild. Output goes to a log via cmd's redirection, which
// keeps a parent alive for node's stdio without this program hanging around.
const CS_START_TEMPLATE = [
  'using System;',
  'using System.Diagnostics;',
  'using System.IO;',
  'using System.Net;',
  '',
  'static class Starter {',
  '    static string Setting(string key) {',
  '        try {',
  '            string cfg = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "start.cfg");',
  '            if (!File.Exists(cfg)) cfg = Setting2(key);',
  '            if (cfg != null && File.Exists(cfg)) {',
  '                foreach (string raw in File.ReadAllLines(cfg)) {',
  '                    string line = raw.Trim();',
  '                    if (line.StartsWith(key + "=")) return line.Substring(key.Length + 1).Trim();',
  '                }',
  '            }',
  '        } catch { }',
  '        return null;',
  '    }',
  '    // A copy of this exe in the Startup folder finds its cfg in the install dir.',
  '    static string Setting2(string key) {',
  '        try { return Path.Combine(@"__DEST__", "start.cfg"); } catch { return null; }',
  '    }',
  '    static bool IsUp(string url) {',
  '        try {',
  '            HttpWebRequest req = (HttpWebRequest)WebRequest.Create(url);',
  '            req.Timeout = 1500; req.ReadWriteTimeout = 1500; req.Method = "GET";',
  '            using (HttpWebResponse r = (HttpWebResponse)req.GetResponse()) { return true; }',
  '        } catch (WebException we) { return we.Response != null; } catch { return false; }',
  '    }',
  '    static void Main() {',
  '        string command = Setting("command");',
  '        if (command == null || command.Length == 0) return;',
  '        string health = Setting("health");',
  '        if (health != null && health.Length > 0 && IsUp(health)) return;   // already running: do not start a second one',
  '        try {',
  '            ProcessStartInfo psi = new ProcessStartInfo("cmd.exe", "/d /c " + command);',
  '            psi.UseShellExecute = false;',
  '            psi.CreateNoWindow = true;',
  '            psi.WindowStyle = ProcessWindowStyle.Hidden;',
  '            string wd = Setting("cwd");',
  '            if (wd != null && wd.Length > 0) psi.WorkingDirectory = wd;',
  '            Process.Start(psi);',
  '        } catch { }',
  '    }',
  '}',
].join('\n');

const cs = mode === 'start'
  ? CS_START_TEMPLATE.replace(/__DEST__/g, outDir.replace(/"/g, '""'))
  : CS_TEMPLATE
    .replace(/__PORT__/g, port)
    .replace(/__SERVER__/g, server.replace(/"/g, '""'))     // verbatim string: "" escapes a quote
    .replace(/__TOKEN__/g, token.replace(/\\/g, '\\\\').replace(/"/g, '\\"'));

const csc = findCsc();
if (!csc) {
  console.error('Could not find csc.exe. The .NET Framework normally ships it at ' +
    '%SystemRoot%\\Microsoft.NET\\Framework64\\v4.0.30319\\csc.exe');
  process.exit(1);
}

fs.mkdirSync(outDir, { recursive: true });
const tmp = path.join(os.tmpdir(), 'amc-launcher-' + process.pid + '.cs');
fs.writeFileSync(tmp, cs, 'utf8');
const exePath = path.join(outDir, exeName);

const args = ['/nologo', '/target:winexe', '/optimize+', '/out:' + exePath,
  '/reference:System.dll', '/reference:System.Windows.Forms.dll'];
if (fs.existsSync(icon)) args.push('/win32icon:' + icon);
else console.warn('note: no icon at ' + icon + ' — building without an embedded icon');
args.push(tmp);

try {
  execFileSync(csc, args, { stdio: 'pipe', encoding: 'utf8' });
} catch (e) {
  console.error('compile failed:\n' + (e.stdout || '') + (e.stderr || ''));
  process.exit(1);
} finally {
  try { fs.unlinkSync(tmp); } catch { /* ignore */ }
}

if (mode !== 'start') {
fs.writeFileSync(path.join(outDir, 'launcher.cfg'),
  '# Agent Mission Control launcher settings.\n' +
  '# server = full path to the server.js this button should run.\n' +
  'server=' + server + '\n' +
  (token ? 'token=' + token + '\n' : ''), 'utf8');
}

console.log('built ' + exePath + '  (' + fs.statSync(exePath).size + ' bytes)');
console.log('  runs  : ' + server);
console.log('  opens : http://localhost:' + port);
console.log('  icon  : ' + (fs.existsSync(icon) ? icon : '(none embedded)'));
