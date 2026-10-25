import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { gzipSync, gunzipSync } from 'node:zlib';

const input = 'oj residual snapshot source';
if (gunzipSync(gzipSync(input)).toString() !== input) {
  throw new Error('oj residual node:zlib did not round-trip');
}

const isGeneratedHot = (id) => id.endsWith('/hot.js') && id.includes('/bazel-bin/');

export default {
  plugins: [{
    name: 'generated-hot-load',
    enforce: 'pre',
    load(id) {
      if (!isGeneratedHot(id)) return null;
      return readFileSync(id, 'utf8').replace('DISK_ONLY', 'GENERATED');
    },
    transform(code, id) {
      if (!isGeneratedHot(id)) return null;
      const modules = this.environment.moduleGraph.getModulesByFile(`${this.environment.config.root}/app.ts`);
      const urls = [...(modules ?? [])].map((module) => module.url).join(',');
      if (urls !== '/app.ts') throw new Error(`wrong application graph URL: ${urls}`);
      return {
        code: code + '\nexport const transformed = "TRANSFORM_ONE";' +
          '\nexport const graphURL = ' + JSON.stringify('APPLICATION_GRAPH_URL=' + urls) + ';',
        map: {
          version: 3, names: [], sources: ['original-hot.ts'],
          sourcesContent: ['export const value = "ORIGINAL_HOT";'], mappings: 'AAAA',
        },
      };
    },
  }, {
    name: 'pre-resolve-require',
    enforce: 'pre',
    resolveId: {
      filter: { id: /^(zod|cjs-claimed|external-claimed)$/ },
      handler(id, importer) {
        if (id === 'external-claimed') return { id: 'https://cdn.invalid/external.js', external: true };
        if (id === 'cjs-claimed') return createRequire(importer).resolve('zod');
        if (importer?.endsWith('/npm_elsewhere_entry.js')) {
          return this.resolve(id, path.join(path.dirname(importer), '../../vendor/importer.js'), { skipSelf: true });
        }
        return importer?.endsWith('/npm_require_entry.js') ? createRequire(importer).resolve(id) : null;
      },
    },
  }, {
    name: 'pre-resolve-passthrough',
    enforce: 'pre',
    resolveId: {
      filter: { id: /^zod$/ },
      handler(id, importer, options) {
        return this.resolve(id, importer, { ...options, skipSelf: true });
      },
    },
  }, {
    name: 'id-alias',
    async buildStart() {
      if (!(await this.resolve('zod'))) throw new Error('this.resolve with no importer found no zod');
    },
    resolveId: {
      filter: { id: /^zod-via-id$/ },
      handler(_id, importer) {
        return this.resolve('zod', importer, { skipSelf: true });
      },
    },
  }, {
    name: 'generated-hot-transform',
    transform(code, id) {
      if (!isGeneratedHot(id)) return null;
      return {
        code: code + '\nexport const composed = "TRANSFORM_TWO";',
        map: { version: 3, names: [], sources: [id], sourcesContent: [code], mappings: 'AAAA' },
      };
    },
  }],
};
