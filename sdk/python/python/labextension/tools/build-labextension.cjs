const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const root = path.resolve(__dirname, '..');
let corePath = process.env.JUPYTERLAB_CORE_PATH;
if (!corePath) {
  const local = path.resolve(root, '../.venv/Lib/site-packages/jupyterlab/staging');
  if (fs.existsSync(path.join(local, 'package.json'))) {
    corePath = local;
  } else {
    const python = process.env.PYTHON || (process.platform === 'win32' ? 'python' : 'python3');
    const discovery = spawnSync(python, ['-c', 'import pathlib, jupyterlab; print(pathlib.Path(jupyterlab.__file__).parent / "staging")'], { encoding: 'utf8' });
    if (discovery.status === 0) corePath = discovery.stdout.trim();
  }
}
if (!corePath || !fs.existsSync(path.join(corePath, 'package.json'))) {
  console.error('Set JUPYTERLAB_CORE_PATH to the installed JupyterLab staging directory, or install JupyterLab in the active Python environment.');
  process.exit(1);
}
const builder = require.resolve('@jupyterlab/builder/lib/build-labextension.js');
const result = spawnSync(process.execPath, [builder, '--core-path', path.resolve(corePath), root], { cwd: root, stdio: 'inherit' });
if (result.error) console.error(result.error.message);
process.exit(result.status === null ? 1 : result.status);
