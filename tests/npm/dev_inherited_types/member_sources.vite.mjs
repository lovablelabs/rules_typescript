const transformed = [];

export default {
  plugins: [{
    name: 'member-sources',
    transform(_code, id) {
      transformed.push(id);
    },
    configureServer(server) {
      server.middlewares.use('/member-sources', (_req, res) => res.end(JSON.stringify({
        sources: JSON.parse(process.env.BAZEL_MEMBER_SOURCES ?? 'null'),
        transformed,
      })));
    },
  }],
};
