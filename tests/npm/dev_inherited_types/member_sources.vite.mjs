import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import path from 'node:path';

const transformed = [];

export default {
  plugins: [{
    name: 'member-sources',
    // Server environments that discover deps to pre-bundle, as worker and SSR framework plugins configure them.
    config: () => process.env.MEMBER_SOURCES_SERVER_OPTIMIZERS && {
      environments: {
        ssr: { optimizeDeps: { noDiscovery: false } },
        worker: { consumer: 'server', resolve: { noExternal: true }, optimizeDeps: { noDiscovery: false } },
      },
    },
    async transform(code, id) {
      transformed.push(id);
      // Like most transforms, it leaves node_modules alone; one that resolves for
      // a sibling of the module it transforms, as import checkers do.
      if (id.includes('/node_modules/')) return;
      const sibling = await this.resolve('zod', path.join(path.dirname(id), 'sibling.ts'));
      // Derived from the module's path, as a StyleX class hash is.
      const hash = createHash('sha256').update(id.split('?')[0]).digest('hex').slice(0, 12);
      return `${code}\n// FIRST_PARTY_TRANSFORM ${sibling ? 'SIBLING_RESOLVED' : 'SIBLING_UNRESOLVED'} ${hash}\n// SIBLING_ID ${sibling?.id}\n`;
    },
    configureServer(server) {
      server.middlewares.use('/member-sources', (_req, res) => res.end(JSON.stringify({
        sources: JSON.parse(process.env.BAZEL_MEMBER_SOURCES ?? 'null'),
        transformed,
      })));
      server.middlewares.use('/environment-transform', async (req, res) => {
        const query = new URL(req.url, 'http://localhost').searchParams;
        try {
          res.end((await server.environments[query.get('environment')].transformRequest(query.get('url')))?.code ?? '');
        } catch (error) {
          res.statusCode = 500;
          res.end(String(error?.stack ?? error));
        }
      });
    },
  }, {
    name: 'pre-claims',
    enforce: 'pre',
    resolveId: {
      filter: { id: /^(external|dependency)-claimed$/ },
      handler: (id, importer) => id === 'external-claimed'
        ? { id: 'https://cdn.invalid/external.js', external: true }
        : createRequire(importer).resolve('zod'),
    },
  }, {
    name: 'virtual-member-user',
    resolveId: (id) => (id === 'virtual:member-user' ? '\0member-user' : null),
    load: (id) => (id === '\0member-user' ? 'export { tagline } from "shared";' : null),
  }, {
    // A normal-order resolver, which Vite reaches only for an import its own resolver leaves.
    name: 'late-resolver',
    resolveId: (id) => (id === 'zod' ? '\0late-zod' : null),
    load: (id) => (id === '\0late-zod' ? 'export const z = "LATE_RESOLVER";' : null),
  }],
};
