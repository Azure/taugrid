// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import type { Plugin } from 'vite';

interface PackageMetadata {
  name: string;
  version: string;
  license?: string;
  dependencies?: Record<string, string>;
}

function readPackage(path: string): PackageMetadata {
  const metadata: PackageMetadata = JSON.parse(readFileSync(path, 'utf8'));
  if (typeof metadata.name !== 'string' || typeof metadata.version !== 'string') {
    throw new Error(`Package metadata lacks a name or version: ${path}`);
  }
  return metadata;
}

function resolvePackage(name: string, parentManifest: string): string {
  const require = createRequire(parentManifest);
  try {
    return require.resolve(name + '/package.json');
  } catch (error) {
    if (!(error instanceof Error) || !('code' in error) ||
        (error.code !== 'ERR_PACKAGE_PATH_NOT_EXPORTED' && error.code !== 'MODULE_NOT_FOUND')) throw error;
    // Some packages intentionally do not export package.json.
    let directory = dirname(require.resolve(name));
    while (directory !== dirname(directory)) {
      const manifest = join(directory, 'package.json');
      if (existsSync(manifest) && readPackage(manifest).name === name) return manifest;
      directory = dirname(directory);
    }
    throw new Error(`Cannot locate license owner for dependency ${name}`);
  }
}

export function productionLicenseNotices(rootManifest: string): string {
  const packages = new Map<string, string>();
  const visited = new Set<string>();
  function visit(manifest: string) {
    if (visited.has(manifest)) return;
    visited.add(manifest);
    const metadata = readPackage(manifest);
    const directory = dirname(manifest);
    const files = readdirSync(directory).filter(name => /^(licen[cs]e|notice|copying)(?:[.-]|$)/i.test(name)).sort();
    if (!files.some(name => /^(licen[cs]e|copying)(?:[.-]|$)/i.test(name))) {
      throw new Error(`Missing upstream license file for ${metadata.name}@${metadata.version}`);
    }
    const identity = `${metadata.name}@${metadata.version}`;
    const notices = [
      `Package: ${identity}`,
      `License: ${metadata.license || 'See upstream license below'}`,
      ...files.map(name => `\n--- ${name} ---\n${readFileSync(join(directory, name), 'utf8').replace(/\r\n/g, '\n').trimEnd()}`),
    ].join('\n');
    if (packages.has(identity) && packages.get(identity) !== notices) {
      throw new Error(`Conflicting upstream license notices for ${identity}`);
    }
    packages.set(identity, notices);
    for (const name of Object.keys(metadata.dependencies || {}).sort()) {
      visit(resolvePackage(name, manifest));
    }
  }
  const root = readPackage(rootManifest);
  for (const name of Object.keys(root.dependencies || {}).sort()) {
    visit(resolvePackage(name, rootManifest));
  }
  return [
    'TauGrid Portal — third-party production dependency notices',
    '',
    'Generated from installed production packages and their transitive dependencies.',
    'Upstream copyright and license notices are reproduced below without modification.',
    '',
    ...[...packages.keys()].sort().map(name => packages.get(name)),
    '',
  ].join('\n\n').trimEnd() + '\n';
}

export function productionLicenses(rootManifest: string): Plugin {
  return {
    name: 'taugrid-production-license-notices',
    apply: 'build',
    generateBundle() {
      this.emitFile({
        type: 'asset',
        fileName: 'THIRD_PARTY_LICENSES.txt',
        source: productionLicenseNotices(rootManifest),
      });
    },
  };
}
