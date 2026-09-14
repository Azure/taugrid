// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { productionLicenses } from './production-licenses';

export default defineConfig({
  plugins: [react(), productionLicenses(fileURLToPath(new URL('./package.json', import.meta.url)))],
  base: '/portal/',
  build: { outDir: '../internal/portalapi/assets', emptyOutDir: true },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/stellar': 'http://localhost:8080',
    },
  },
});
